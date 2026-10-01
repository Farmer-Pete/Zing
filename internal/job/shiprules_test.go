// shiprules_test.go tests M3 task 4's pure shipping rules (design section
// 8.3, 8.4, 8.5, 8.7, shiprules.go): EvaluateCI, pollFingerprint,
// nextInterval, shippingGate, ciLogText, and parsePRNumber.
package job

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"zing/internal/orchestrator"
)

// Test-only stand-ins, reused across this file's cases so each raw string
// appears once: a commit sha, the one required-check context most cases
// use, a second check name (not "lint" -- building.go's own check command
// already carries that name), and two commit-status contexts.
const (
	ciSHA          = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRequiredCI = "ci"
	testCheckB     = "typecheck"
	testStatusA    = "deploy"
	testStatusB    = "codecov"
	testInProgress = "in_progress"
)

// -----------------------------------------------------------------------
// Pure: EvaluateCI
// -----------------------------------------------------------------------

func TestEvaluateCI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		runs      []orchestrator.CheckRun
		statuses  []orchestrator.CommitStatus
		required  []orchestrator.RequiredCheck
		wantState CIState
		wantMiss  []string
	}{
		{
			name: "completed success and skipped is green",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess},
				{ID: 2, Name: testCheckB, Status: ghCompleted, Conclusion: ghSkipped},
			},
			required:  []orchestrator.RequiredCheck{{Context: testRequiredCI}},
			wantState: CIGreen,
		},
		{
			name: "still in progress is pending",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testRequiredCI, Status: testInProgress},
			},
			required:  []orchestrator.RequiredCheck{{Context: testRequiredCI}},
			wantState: CIPending,
		},
		{
			name: "a pending status holds back an otherwise green run",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess},
			},
			statuses:  []orchestrator.CommitStatus{{Context: "deploy/preview", State: "pending"}},
			required:  []orchestrator.RequiredCheck{{Context: testRequiredCI}},
			wantState: CIPending,
		},
		{
			name: "a required context with no run or status is pending and missing",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testCheckB, Status: ghCompleted, Conclusion: ghSuccess},
			},
			required:  []orchestrator.RequiredCheck{{Context: testRequiredCI}},
			wantState: CIPending,
			wantMiss:  []string{testRequiredCI},
		},
		{
			name: "one failed run fails the whole set, even with another still running",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghFailure},
				{ID: 2, Name: testCheckB, Status: testInProgress},
			},
			required:  []orchestrator.RequiredCheck{{Context: testRequiredCI}},
			wantState: CIFailed,
		},
		{
			name: "a status error fails CI even though it is not required",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess},
			},
			statuses:  []orchestrator.CommitStatus{{Context: testStatusB, State: ghError}},
			required:  []orchestrator.RequiredCheck{{Context: testRequiredCI}},
			wantState: CIFailed,
		},
		{
			name: "no required checks is unprotected even with a passing run",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess},
			},
			wantState: CIUnprotected,
		},
		{
			name: "the newest run per name wins the reduction",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghFailure},
				{ID: 2, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess},
			},
			required:  []orchestrator.RequiredCheck{{Context: testRequiredCI}},
			wantState: CIGreen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EvaluateCI(tc.runs, tc.statuses, tc.required)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			if diff := cmp.Diff(tc.wantMiss, got.Missing, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Missing mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEvaluateCIFailsClosed(t *testing.T) {
	t.Parallel()

	required := []orchestrator.RequiredCheck{{Context: testRequiredCI}}

	cases := []struct {
		name     string
		runs     []orchestrator.CheckRun
		statuses []orchestrator.CommitStatus
	}{
		{
			name: "empty conclusion",
			runs: []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ""}},
		},
		{
			name: "an unrecognized conclusion",
			runs: []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: "superseded"}},
		},
		{
			name: "an unrecognized status",
			runs: []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, Status: "in_limbo", Conclusion: ""}},
		},
		{
			name:     "an empty status state",
			runs:     []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess}},
			statuses: []orchestrator.CommitStatus{{Context: testStatusA, State: ""}},
		},
		{
			name:     "an unrecognized status state",
			runs:     []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess}},
			statuses: []orchestrator.CommitStatus{{Context: testStatusA, State: "archived"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EvaluateCI(tc.runs, tc.statuses, required)
			if got.State != CIPending {
				t.Errorf("State = %q, want pending (fail closed)", got.State)
			}
		})
	}
}

func TestEvaluateCISpoofedCheck(t *testing.T) {
	t.Parallel()

	required := []orchestrator.RequiredCheck{{Context: testRequiredCI, AppID: new(int64(15368))}}

	t.Run("a same-name run from another app does not satisfy it", func(t *testing.T) {
		t.Parallel()
		runs := []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, AppID: 99, Status: ghCompleted, Conclusion: ghSuccess}}
		got := EvaluateCI(runs, nil, required)
		if got.State != CIPending {
			t.Errorf("State = %q, want pending", got.State)
		}
		if diff := cmp.Diff([]string{testRequiredCI}, got.Missing); diff != "" {
			t.Errorf("Missing mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("the bound app's run satisfies it", func(t *testing.T) {
		t.Parallel()
		runs := []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, AppID: 15368, Status: ghCompleted, Conclusion: ghSuccess}}
		statuses := []orchestrator.CommitStatus{{Context: testRequiredCI, State: ghSuccess}}
		got := EvaluateCI(runs, statuses, required)
		if got.State != CIGreen {
			t.Errorf("State = %q, want green", got.State)
		}
	})
}

func TestEvaluateCINewerOtherAppRun(t *testing.T) {
	t.Parallel()

	required := []orchestrator.RequiredCheck{{Context: testRequiredCI, AppID: new(int64(15368))}}

	t.Run("both are kept and the bound run stays green", func(t *testing.T) {
		t.Parallel()
		runs := []orchestrator.CheckRun{
			{ID: 10, Name: testRequiredCI, AppID: 15368, Status: ghCompleted, Conclusion: ghSuccess},
			{ID: 11, Name: testRequiredCI, AppID: 99, Status: ghCompleted, Conclusion: ghSuccess},
		}
		got := EvaluateCI(runs, nil, required)
		if got.State != CIGreen {
			t.Errorf("State = %q, want green", got.State)
		}
	})

	t.Run("a failed run from the other app still fails CI", func(t *testing.T) {
		t.Parallel()
		runs := []orchestrator.CheckRun{
			{ID: 10, Name: testRequiredCI, AppID: 15368, Status: ghCompleted, Conclusion: ghSuccess},
			{ID: 11, Name: testRequiredCI, AppID: 99, Status: ghCompleted, Conclusion: ghFailure},
		}
		got := EvaluateCI(runs, nil, required)
		if got.State != CIFailed {
			t.Errorf("State = %q, want failed", got.State)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: pollFingerprint
// -----------------------------------------------------------------------

func basePR() orchestrator.PRState {
	return orchestrator.PRState{State: "open", Merged: false, Draft: true, HeadSHA: ciSHA}
}

func TestPollFingerprintStable(t *testing.T) {
	t.Parallel()

	pr := basePR()
	runs := []orchestrator.CheckRun{
		{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess},
		{ID: 2, Name: testCheckB, Status: ghCompleted, Conclusion: ghSuccess},
	}
	statuses := []orchestrator.CommitStatus{
		{Context: testStatusA, State: ghSuccess},
		{Context: testStatusB, State: ghSuccess},
	}
	required := []orchestrator.RequiredCheck{{Context: testRequiredCI}, {Context: testCheckB}}
	threads := []PollThread{
		{TID: "t2", IsResolved: false, LastCommentDigest: "dig2"},
		{TID: "t1", IsResolved: true, LastCommentDigest: "dig1"},
	}

	got1 := pollFingerprint(pr, runs, statuses, required, threads)

	runsReordered := []orchestrator.CheckRun{runs[1], runs[0]}
	statusesReordered := []orchestrator.CommitStatus{statuses[1], statuses[0]}
	requiredReordered := []orchestrator.RequiredCheck{required[1], required[0]}
	threadsReordered := []PollThread{threads[1], threads[0]}

	got2 := pollFingerprint(pr, runsReordered, statusesReordered, requiredReordered, threadsReordered)

	if got1 != got2 {
		t.Errorf("fingerprint depends on input order: %q != %q", got1, got2)
	}
}

func TestFingerprintChangesOnCommentEdit(t *testing.T) {
	t.Parallel()

	pr := basePR()
	runs := []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess}}
	required := []orchestrator.RequiredCheck{{Context: testRequiredCI}}

	before := pollFingerprint(pr, runs, nil, required, []PollThread{{TID: "t1", LastCommentDigest: "before"}})
	after := pollFingerprint(pr, runs, nil, required, []PollThread{{TID: "t1", LastCommentDigest: "after"}})

	if before == after {
		t.Errorf("fingerprint did not change when a thread's last comment digest changed")
	}
}

func TestFingerprintChangesOnProtectionOnly(t *testing.T) {
	t.Parallel()

	pr := basePR()
	runs := []orchestrator.CheckRun{
		{ID: 1, Name: testRequiredCI, AppID: 15368, Status: ghCompleted, Conclusion: ghSuccess},
		{ID: 2, Name: testCheckB, Status: ghCompleted, Conclusion: ghSuccess},
	}
	threads := []PollThread{{TID: "t1", IsResolved: true, LastCommentDigest: "dig1"}}

	fp1 := pollFingerprint(pr, runs, nil, []orchestrator.RequiredCheck{{Context: testRequiredCI}}, threads)
	fp2 := pollFingerprint(pr, runs, nil, []orchestrator.RequiredCheck{{Context: testRequiredCI}, {Context: testCheckB}}, threads)
	fp3 := pollFingerprint(pr, runs, nil, []orchestrator.RequiredCheck{{Context: testRequiredCI, AppID: new(int64(15368))}, {Context: testCheckB}}, threads)

	if fp1 == fp2 {
		t.Errorf("fingerprint did not change when a required check was added")
	}
	if fp2 == fp3 {
		t.Errorf("fingerprint did not change when a required check was bound to another app id")
	}
	if fp1 == fp3 {
		t.Errorf("fingerprint collided across two different protection states")
	}

	if got := nextInterval(&fp1, new(120), fp2); got != 30 {
		t.Errorf("nextInterval across a protection-only change = %d, want 30", got)
	}
}

// -----------------------------------------------------------------------
// Pure: nextInterval
// -----------------------------------------------------------------------

func TestNextInterval(t *testing.T) {
	t.Parallel()

	fpA := "A"
	fpB := "B"

	// Worked example, design section 8.3: polls at t = 0, 30, 90, 210,
	// 450, 750, 1050, the first six with fingerprint A, the last with B.
	var prevFP *string
	var prevInterval *int

	steps := []struct {
		fp   string
		want int
	}{
		{fpA, 30},
		{fpA, 60},
		{fpA, 120},
		{fpA, 240},
		{fpA, 300},
		{fpA, 300},
		{fpB, 30},
	}

	for i, step := range steps {
		got := nextInterval(prevFP, prevInterval, step.fp)
		if got != step.want {
			t.Errorf("step %d: nextInterval = %d, want %d", i, got, step.want)
		}
		f := step.fp
		prevFP = &f
		prevInterval = &got
	}
}

// -----------------------------------------------------------------------
// Pure: shippingGate
// -----------------------------------------------------------------------

func TestShippingGate(t *testing.T) {
	t.Parallel()

	// Worked example, design section 8.7: CI fix (k 0), respond fix
	// (k 1), CI fix (k 2) all proceed under max_loops 3; the next CI
	// failure at k 3 hits the gate.
	for k := range 3 {
		ok, what, why, tried := shippingGate(k, 3, string(FixKindCILog), 0, "the log")
		if !ok {
			t.Errorf("k=%d: shippingGate refused under the gate", k)
		}
		if what != "" || why != "" || tried != "" {
			t.Errorf("k=%d: shippingGate under the gate returned non-empty fields: %q %q %q", k, what, why, tried)
		}
	}

	ok, what, why, tried := shippingGate(3, 3, string(FixKindCILog), 0, "the log")
	if ok {
		t.Fatal("shippingGate allowed a fix at the gate")
	}
	if what != "shipping needed more than 3 fix runs" {
		t.Errorf("what = %q", what)
	}
	if why != "CI fixes and review-thread fixes share a limit of 3" {
		t.Errorf("why = %q", why)
	}
	if tried != "ci_log\nthe log" {
		t.Errorf("tried = %q", tried)
	}

	t.Run("threads names the respond artifact on line 2", func(t *testing.T) {
		t.Parallel()
		ok, _, _, tried := shippingGate(3, 3, "threads", 42, "the thread fix text")
		if ok {
			t.Fatal("shippingGate allowed a threads fix at the gate")
		}
		want := "threads\nrespond 42\nthe thread fix text"
		if tried != want {
			t.Errorf("tried = %q, want %q", tried, want)
		}
	})
}

// -----------------------------------------------------------------------
// Pure (over a fake Checks): ciLogText
// -----------------------------------------------------------------------

type fakeLogChecks struct {
	tail func(ctx context.Context, owner, repo string, jobID int64, lines int) (string, error)
}

func (f fakeLogChecks) ListCheckRuns(context.Context, string, string, string) ([]orchestrator.CheckRun, error) {
	return nil, nil
}

func (f fakeLogChecks) ListStatuses(context.Context, string, string, string) ([]orchestrator.CommitStatus, error) {
	return nil, nil
}

func (f fakeLogChecks) RequiredCheckRules(context.Context, string, string, string) ([]orchestrator.RequiredCheck, error) {
	return nil, nil
}

func (f fakeLogChecks) JobLogTail(ctx context.Context, owner, repo string, jobID int64, lines int) (string, error) {
	return f.tail(ctx, owner, repo, jobID, lines)
}

func TestCILogText(t *testing.T) {
	t.Parallel()

	checks := fakeLogChecks{
		tail: func(_ context.Context, _, _ string, jobID int64, lines int) (string, error) {
			if lines != 200 {
				t.Errorf("JobLogTail lines = %d, want 200", lines)
			}
			switch jobID {
			case 111:
				return "line one\nline two", nil
			case 222:
				return "", errors.New("signed url expired")
			default:
				t.Fatalf("unexpected job id %d", jobID)
				return "", nil
			}
		},
	}

	failedRuns := []orchestrator.CheckRun{
		{Name: testRequiredCI, AppSlug: ghGitHubActions, Conclusion: ghFailure, DetailsURL: "https://github.com/o/r/actions/runs/1/job/111"},
		{Name: "unreadable", AppSlug: ghGitHubActions, Conclusion: ghFailure, DetailsURL: "https://github.com/o/r/actions/runs/2/job/222"},
		{Name: "other-app", AppSlug: "circleci", Conclusion: ghFailure, DetailsURL: "https://circleci.com/gh/o/r/9"},
		{Name: "zzz-fourth", AppSlug: ghGitHubActions, Conclusion: ghFailure, DetailsURL: "https://github.com/o/r/actions/runs/3/job/333"},
	}
	failedStatuses := []orchestrator.CommitStatus{
		{Context: testStatusB, State: ghError, TargetURL: "https://codecov.io/x"},
	}

	got := ciLogText(t.Context(), checks, "o", "r", failedRuns, failedStatuses)

	if want := "check ci (failure)\nline one\nline two"; !strings.Contains(got, want) {
		t.Errorf("missing the readable Actions log block; got:\n%s", got)
	}
	if want := "check other-app (failure) https://circleci.com/gh/o/r/9"; !strings.Contains(got, want) {
		t.Errorf("missing the non-Actions fallback line; got:\n%s", got)
	}
	if want := "check unreadable (failure): the log could not be read: signed url expired"; !strings.Contains(got, want) {
		t.Errorf("missing the unreadable-log line; got:\n%s", got)
	}
	if strings.Contains(got, "zzz-fourth") {
		t.Errorf("more than 3 failed runs appear; got:\n%s", got)
	}
	if want := "status codecov (error) https://codecov.io/x"; !strings.Contains(got, want) {
		t.Errorf("missing the failed status line; got:\n%s", got)
	}
}

// -----------------------------------------------------------------------
// Pure: parsePRNumber
// -----------------------------------------------------------------------

func TestParsePRNumber(t *testing.T) {
	t.Parallel()

	n, err := parsePRNumber("https://github.com/Farmer-Pete/zing-sandbox/pull/41")
	if err != nil {
		t.Fatalf("parsePRNumber: %v", err)
	}
	if n != 41 {
		t.Errorf("n = %d, want 41", n)
	}

	cases := []string{
		"https://github.com/Farmer-Pete/zing-sandbox/pull/0",
		"https://github.com/Farmer-Pete/zing-sandbox/pull/",
		"https://github.com/Farmer-Pete/zing-sandbox/pull/01",
		"https://github.com/Farmer-Pete/zing-sandbox/issues/41",
		"not a url",
		"",
	}
	for _, url := range cases {
		if _, err := parsePRNumber(url); err == nil {
			t.Errorf("parsePRNumber(%q): want an error, got none", url)
		}
	}

	wantErr := "job: shipping: pr url https://example.com/not/a/pr has no number"
	if _, err := parsePRNumber("https://example.com/not/a/pr"); err == nil || err.Error() != wantErr {
		t.Errorf("error = %v, want %q", err, wantErr)
	}
}
