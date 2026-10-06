// shiprules_test.go tests M3 task 4's pure shipping rules (design section
// 8.3, 8.4, 8.5, 8.7, shiprules.go): EvaluateCI, pollFingerprint,
// nextInterval, shippingGate, ciLogTextFrom, decideCIRerun,
// failedTestNames, rerunPassedNotes, and parsePRNumber.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// Test-only stand-ins, reused across this file's cases so each raw string
// appears once: a commit sha, the one required-check context most cases
// use, a second check name (not "lint" -- building.go's own check command
// already carries that name), two commit-status contexts, and a two-line
// retry-notes text used by TestBaseMergeRequestPointRoundTrip.
const (
	ciSHA                   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRequiredCI          = "ci"
	testCheckB              = "typecheck"
	testStatusA             = "deploy"
	testStatusB             = "codecov"
	testInProgress          = "in_progress"
	testTwoLineText         = "line one\nline two"
	testFlakyTestName       = "TestX"
	testTriedThreeInfraRuns = "re-ran workflow runs 10, 11, 12"
	mkNonActionsAppSlug     = "circleci"
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
	return orchestrator.PRState{State: shipPRStateOpen, Merged: false, Draft: true, HeadSHA: ciSHA}
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

func (f fakeLogChecks) RerunJob(context.Context, string, string, int64) error {
	return nil
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
			case 333:
				// readFailedChecks reads every failed Actions run's log,
				// not just the 3 ciLogTextFrom later keeps.
				return "fourth log", nil
			default:
				t.Fatalf("unexpected job id %d", jobID)
				return "", nil
			}
		},
	}

	failedRuns := []orchestrator.CheckRun{
		{Name: testRequiredCI, AppSlug: ghGitHubActions, Conclusion: ghFailure, DetailsURL: "https://github.com/o/r/actions/runs/1/job/111"},
		{Name: "unreadable", AppSlug: ghGitHubActions, Conclusion: ghFailure, DetailsURL: "https://github.com/o/r/actions/runs/2/job/222"},
		{Name: "other-app", AppSlug: mkNonActionsAppSlug, Conclusion: ghFailure, DetailsURL: "https://circleci.com/gh/o/r/9"},
		{Name: "zzz-fourth", AppSlug: ghGitHubActions, Conclusion: ghFailure, DetailsURL: "https://github.com/o/r/actions/runs/3/job/333"},
	}
	failedStatuses := []orchestrator.CommitStatus{
		{Context: testStatusB, State: ghError, TargetURL: "https://codecov.io/x"},
	}

	got := ciLogTextFrom(readFailedChecks(t.Context(), checks, "o", "r", failedRuns), failedStatuses)

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
// Pure: decideCIRerun
// -----------------------------------------------------------------------

// decideCIRerunNow is the fixed "now" every TestDecideCIRerun case
// measures its prior events' At against.
var decideCIRerunNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// mkFailedCheck builds a failedCheck for a github-actions run with
// checkRunID, runID, and jobID all consistent with actionsJobIDPattern's
// own DetailsURL shape.
func mkFailedCheck(name string, checkRunID, runID, jobID int64, conclusion, log string, logErr error) failedCheck {
	return failedCheck{
		Run: orchestrator.CheckRun{
			ID:         checkRunID,
			Name:       name,
			Status:     ghCompleted,
			Conclusion: conclusion,
			AppSlug:    ghGitHubActions,
			DetailsURL: fmt.Sprintf("https://github.com/o/r/actions/runs/%d/job/%d", runID, jobID),
		},
		RunID:  runID,
		JobID:  jobID,
		Log:    log,
		LogErr: logErr,
	}
}

// mkNonActionsFailedCheck builds a failedCheck for a check run from an app
// other than github-actions, so it never carries Actions ids.
func mkNonActionsFailedCheck(name string, checkRunID int64, conclusion string) failedCheck {
	return failedCheck{
		Run: orchestrator.CheckRun{
			ID:         checkRunID,
			Name:       name,
			Status:     ghCompleted,
			Conclusion: conclusion,
			AppSlug:    mkNonActionsAppSlug,
			DetailsURL: "https://circleci.com/gh/o/r/9",
		},
	}
}

// mkPrior builds one priorRerun for TestDecideCIRerun, agoFromNow before
// decideCIRerunNow.
func mkPrior(check string, checkRunID, runID int64, reason response.RerunReason, agoFromNow time.Duration) priorRerun {
	return priorRerun{
		Event: response.CheckRerunEvent{Check: check, SHA: ciSHA, RunID: runID, CheckRunID: checkRunID, Reason: reason},
		At:    decideCIRerunNow.Add(-agoFromNow),
	}
}

func TestDecideCIRerun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		failed         []failedCheck
		failedStatuses int
		prior          []priorRerun
		inFlight       map[int64]bool
		want           rerunDecision
	}{
		{
			name:   "first failure is re-run as flaky",
			failed: []failedCheck{mkFailedCheck("ci", 1, 10, 100, ghFailure, "--- FAIL: TestX", nil)},
			want: rerunDecision{
				Action: rerunNow,
				Reruns: []plannedRerun{{
					Event: response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonFlaky, Tests: []string{testFlakyTestName}},
					JobID: 100,
				}},
			},
		},
		{
			name:   "a spent flaky re-run on a new failure gives fix",
			failed: []failedCheck{mkFailedCheck("ci", 2, 11, 101, ghFailure, "--- FAIL: TestX", nil)},
			prior:  []priorRerun{mkPrior("ci", 1, 10, response.RerunReasonFlaky, 20*time.Minute)},
			want:   rerunDecision{Action: rerunFix},
		},
		{
			name:   "the same check run under 10 minutes old waits",
			failed: []failedCheck{mkFailedCheck("ci", 1, 10, 100, ghFailure, "", nil)},
			prior:  []priorRerun{mkPrior("ci", 1, 10, response.RerunReasonFlaky, 5*time.Minute)},
			want:   rerunDecision{Action: rerunWait},
		},
		{
			name:   "the same check run id past 10 minutes gives fix",
			failed: []failedCheck{mkFailedCheck("ci", 1, 10, 100, ghFailure, "--- FAIL: TestY", nil)},
			prior:  []priorRerun{mkPrior("ci", 1, 10, response.RerunReasonFlaky, 11*time.Minute)},
			want:   rerunDecision{Action: rerunFix},
		},
		{
			name:   "an infra conclusion with no prior re-runs is re-run",
			failed: []failedCheck{mkFailedCheck("ci", 1, 10, 100, ghCancelled, "", nil)},
			want: rerunDecision{
				Action: rerunNow,
				Reruns: []plannedRerun{{
					Event: response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonInfra},
					JobID: 100,
				}},
			},
		},
		{
			name:   "an infra conclusion with 2 prior re-runs is still re-run",
			failed: []failedCheck{mkFailedCheck("ci", 3, 12, 102, ghCancelled, "", nil)},
			prior: []priorRerun{
				mkPrior("ci", 1, 10, response.RerunReasonInfra, 2*time.Hour),
				mkPrior("ci", 2, 11, response.RerunReasonInfra, time.Hour),
			},
			want: rerunDecision{
				Action: rerunNow,
				Reruns: []plannedRerun{{
					Event: response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 12, CheckRunID: 3, Reason: response.RerunReasonInfra},
					JobID: 102,
				}},
			},
		},
		{
			name:   "an infra conclusion with 3 prior re-runs escalates",
			failed: []failedCheck{mkFailedCheck("ci", 4, 13, 103, ghCancelled, "", nil)},
			prior: []priorRerun{
				mkPrior("ci", 1, 10, response.RerunReasonInfra, 3*time.Hour),
				mkPrior("ci", 2, 11, response.RerunReasonInfra, 2*time.Hour),
				mkPrior("ci", 3, 12, response.RerunReasonInfra, time.Hour),
			},
			want: rerunDecision{
				Action: rerunEscalate,
				What:   rerunInfraWhat,
				Why:    "ci ended cancelled on bbbbbbb after 3 re-runs",
				Tried:  testTriedThreeInfraRuns,
			},
		},
		{
			name:   "an infra conclusion on a non-Actions check escalates immediately",
			failed: []failedCheck{mkNonActionsFailedCheck("ci", 1, ghCancelled)},
			want: rerunDecision{
				Action: rerunEscalate,
				What:   rerunInfraWhat,
				Why:    "ci ended cancelled on bbbbbbb and is not a GitHub Actions job, so Zing cannot re-run it",
			},
		},
		{
			name:   "a first unreadable log is re-run as no_log",
			failed: []failedCheck{mkFailedCheck("ci", 1, 10, 100, ghFailure, "", errors.New("boom"))},
			want: rerunDecision{
				Action: rerunNow,
				Reruns: []plannedRerun{{
					Event: response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonNoLog},
					JobID: 100,
				}},
			},
		},
		{
			name:   "a second unreadable log escalates",
			failed: []failedCheck{mkFailedCheck("ci", 2, 11, 101, ghFailure, "", errors.New("boom again"))},
			prior:  []priorRerun{mkPrior("ci", 1, 10, response.RerunReasonNoLog, 20*time.Minute)},
			want: rerunDecision{
				Action: rerunEscalate,
				What:   rerunNoLogWhat,
				Why:    "ci failed again on bbbbbbb and its log could not be read: boom again",
				Tried:  "re-ran workflow runs 10",
			},
		},
		{
			name:   "a non-Actions failure with a readable conclusion gives fix",
			failed: []failedCheck{mkNonActionsFailedCheck("ci", 1, ghFailure)},
			want:   rerunDecision{Action: rerunFix},
		},
		{
			name: "an escalating check beats a re-runnable one",
			failed: []failedCheck{
				mkFailedCheck("a", 4, 13, 103, ghCancelled, "", nil),
				mkFailedCheck("b", 1, 20, 200, ghFailure, "--- FAIL: TestZ", nil),
			},
			prior: []priorRerun{
				mkPrior("a", 1, 10, response.RerunReasonInfra, 3*time.Hour),
				mkPrior("a", 2, 11, response.RerunReasonInfra, 2*time.Hour),
				mkPrior("a", 3, 12, response.RerunReasonInfra, time.Hour),
			},
			want: rerunDecision{
				Action: rerunEscalate,
				What:   rerunInfraWhat,
				Why:    "a ended cancelled on bbbbbbb after 3 re-runs",
				Tried:  testTriedThreeInfraRuns,
			},
		},
		{
			name: "a re-runnable check beats one already due for fix",
			failed: []failedCheck{
				mkFailedCheck("a", 1, 10, 100, ghFailure, "--- FAIL: TestZ", nil),
				mkFailedCheck("b", 2, 11, 101, ghFailure, "--- FAIL: TestW", nil),
			},
			prior: []priorRerun{mkPrior("b", 9, 90, response.RerunReasonFlaky, 20*time.Minute)},
			want: rerunDecision{
				Action: rerunNow,
				Reruns: []plannedRerun{{
					Event: response.CheckRerunEvent{Check: "a", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonFlaky, Tests: []string{"TestZ"}},
					JobID: 100,
				}},
			},
		},
		{
			name:           "a failed status with no failed checks gives fix",
			failedStatuses: 1,
			want:           rerunDecision{Action: rerunFix},
		},
		{
			name:     "a sibling job still running in the same workflow run waits",
			failed:   []failedCheck{mkFailedCheck("ci", 1, 10, 100, ghFailure, "--- FAIL: TestX", nil)},
			inFlight: map[int64]bool{10: true},
			want:     rerunDecision{Action: rerunWait},
		},
		{
			name: "two failed checks sharing a workflow run only plan one re-run",
			failed: []failedCheck{
				mkFailedCheck("a", 1, 10, 100, ghFailure, "--- FAIL: TestA", nil),
				mkFailedCheck("b", 2, 10, 101, ghFailure, "--- FAIL: TestB", nil),
			},
			want: rerunDecision{
				Action: rerunNow,
				Reruns: []plannedRerun{{
					Event: response.CheckRerunEvent{Check: "a", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonFlaky, Tests: []string{"TestA"}},
					JobID: 100,
				}},
			},
		},
		{
			name:   "an infra conclusion with a too-long name escalates without re-running",
			failed: []failedCheck{mkFailedCheck(strings.Repeat("x", 201), 1, 10, 100, ghCancelled, "", nil)},
			want: rerunDecision{
				Action: rerunEscalate,
				What:   rerunNameInvalidWhat,
				Why:    "a check ended cancelled on bbbbbbb with a name 201 runes long, so Zing cannot record a re-run of it",
			},
		},
		{
			name:   "a flaky failure with a too-long name gives fix instead of re-running",
			failed: []failedCheck{mkFailedCheck(strings.Repeat("x", 201), 1, 10, 100, ghFailure, "--- FAIL: TestX", nil)},
			want:   rerunDecision{Action: rerunFix},
		},
		{
			name:     "a spent flaky budget gives fix even with a sibling job in flight",
			failed:   []failedCheck{mkFailedCheck("ci", 2, 11, 101, ghFailure, "--- FAIL: TestX", nil)},
			prior:    []priorRerun{mkPrior("ci", 1, 10, response.RerunReasonFlaky, 20*time.Minute)},
			inFlight: map[int64]bool{11: true},
			want:     rerunDecision{Action: rerunFix},
		},
		{
			name:     "an infra cap already hit escalates even with a sibling job in flight",
			failed:   []failedCheck{mkFailedCheck("ci", 4, 13, 103, ghCancelled, "", nil)},
			inFlight: map[int64]bool{13: true},
			prior: []priorRerun{
				mkPrior("ci", 1, 10, response.RerunReasonInfra, 3*time.Hour),
				mkPrior("ci", 2, 11, response.RerunReasonInfra, 2*time.Hour),
				mkPrior("ci", 3, 12, response.RerunReasonInfra, time.Hour),
			},
			want: rerunDecision{
				Action: rerunEscalate,
				What:   rerunInfraWhat,
				Why:    "ci ended cancelled on bbbbbbb after 3 re-runs",
				Tried:  testTriedThreeInfraRuns,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideCIRerun(decideCIRerunNow, ciSHA, tc.failed, tc.failedStatuses, tc.prior, tc.inFlight)
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("decideCIRerun() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: workflowRunsInFlight
// -----------------------------------------------------------------------

func TestWorkflowRunsInFlight(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		runs []orchestrator.CheckRun
		want map[int64]bool
	}{
		{
			name: "an in-progress Actions run counts",
			runs: []orchestrator.CheckRun{
				{AppSlug: ghGitHubActions, Status: testInProgress, DetailsURL: fmt.Sprintf("https://github.com/o/r/actions/runs/%d/job/%d", 10, 11)},
			},
			want: map[int64]bool{10: true},
		},
		{
			name: "a completed run does not count",
			runs: []orchestrator.CheckRun{
				{AppSlug: ghGitHubActions, Status: ghCompleted, Conclusion: ghFailure, DetailsURL: fmt.Sprintf("https://github.com/o/r/actions/runs/%d/job/%d", 20, 21)},
			},
			want: map[int64]bool{},
		},
		{
			name: "a non-Actions app does not count",
			runs: []orchestrator.CheckRun{
				{AppSlug: mkNonActionsAppSlug, Status: testInProgress, DetailsURL: fmt.Sprintf("https://github.com/o/r/actions/runs/%d/job/%d", 30, 31)},
			},
			want: map[int64]bool{},
		},
		{
			name: "a DetailsURL that does not match does not count",
			runs: []orchestrator.CheckRun{
				{AppSlug: ghGitHubActions, Status: testInProgress, DetailsURL: "https://example.com/not-actions"},
			},
			want: map[int64]bool{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := workflowRunsInFlight(tc.runs)
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("workflowRunsInFlight() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: failedTestNames
// -----------------------------------------------------------------------

func TestFailedTestNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "dedups in first-seen order and keeps subtests",
			text: "--- FAIL: TestA (0.1s)\n--- FAIL: TestA/sub (0s)\n--- FAIL: TestA (0.1s)",
			want: []string{"TestA", "TestA/sub"},
		},
		{
			name: "caps at 20 names",
			text: func() string {
				var b strings.Builder
				for i := range 25 {
					fmt.Fprintf(&b, "--- FAIL: Test%d (0s)\n", i)
				}
				return b.String()
			}(),
			want: func() []string {
				want := make([]string, 20)
				for i := range 20 {
					want[i] = fmt.Sprintf("Test%d", i)
				}
				return want
			}(),
		},
		{
			name: "cuts a long name to 200 bytes",
			text: "--- FAIL: " + strings.Repeat("x", 300) + " (0s)",
			want: []string{strings.Repeat("x", 200)},
		},
		{
			name: "empty input gives nil",
			text: "",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := failedTestNames(tc.text)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("failedTestNames() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: rerunPassedNotes
// -----------------------------------------------------------------------

func TestRerunPassedNotes(t *testing.T) {
	t.Parallel()

	flakyEvent := response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonFlaky, Tests: []string{testFlakyTestName}}
	infraEvent := response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonInfra}

	cases := []struct {
		name    string
		runs    []orchestrator.CheckRun
		reruns  []response.CheckRerunEvent
		passed  []response.CheckRerunPassedEvent
		wantLen int
	}{
		{
			name:    "a newer successful run gives a note",
			runs:    []orchestrator.CheckRun{{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess}},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			wantLen: 1,
		},
		{
			name:    "the same check run id gives no note",
			runs:    []orchestrator.CheckRun{{ID: 1, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess}},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			wantLen: 0,
		},
		{
			name:    "a newer run still in progress gives no note",
			runs:    []orchestrator.CheckRun{{ID: 2, Name: "ci", Status: testInProgress}},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			wantLen: 0,
		},
		{
			name:    "a newer run that failed again gives no note",
			runs:    []orchestrator.CheckRun{{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghFailure}},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			wantLen: 0,
		},
		{
			name:    "an existing passed event gives no second note",
			runs:    []orchestrator.CheckRun{{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess}},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			passed:  []response.CheckRerunPassedEvent{{Check: "ci", SHA: ciSHA, Tests: []string{testFlakyTestName}}},
			wantLen: 0,
		},
		{
			name:    "an infra-only re-run gives no note",
			runs:    []orchestrator.CheckRun{{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess}},
			reruns:  []response.CheckRerunEvent{infraEvent},
			wantLen: 0,
		},
		{
			name: "the newest run by id wins even listed before the old one",
			runs: []orchestrator.CheckRun{
				{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess},
				{ID: 1, Name: "ci", Status: ghCompleted, Conclusion: ghFailure},
			},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			wantLen: 1,
		},
		{
			name: "the newest run by id wins even listed after the old one",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: "ci", Status: ghCompleted, Conclusion: ghFailure},
				{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess},
			},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			wantLen: 1,
		},
		{
			name: "a newer failure beats an older success and gives no note",
			runs: []orchestrator.CheckRun{
				{ID: 1, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess},
				{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghFailure},
			},
			reruns:  []response.CheckRerunEvent{flakyEvent},
			wantLen: 0,
		},
		{
			name:    "a no_log re-run with a newer successful run gives a note",
			runs:    []orchestrator.CheckRun{{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess}},
			reruns:  []response.CheckRerunEvent{{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonNoLog, Tests: []string{testFlakyTestName}}},
			wantLen: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := rerunPassedNotes(99, ciSHA, tc.runs, tc.reruns, tc.passed)
			if err != nil {
				t.Fatalf("rerunPassedNotes: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len(got) = %d, want %d", len(got), tc.wantLen)
			}
			if tc.wantLen == 0 {
				return
			}
			var payload response.CheckRerunPassedEvent
			if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			want := response.CheckRerunPassedEvent{Check: "ci", SHA: ciSHA, Tests: []string{testFlakyTestName}}
			if !cmp.Equal(payload, want) {
				t.Errorf("payload = %+v, want %+v", payload, want)
			}
			if got[0].TicketID != 99 {
				t.Errorf("TicketID = %d, want 99", got[0].TicketID)
			}
			if got[0].EventKind == nil || *got[0].EventKind != store.EventKindCheckRerunPassed {
				t.Errorf("EventKind = %v, want %s", got[0].EventKind, store.EventKindCheckRerunPassed)
			}
		})
	}

	t.Run("the newest of several events for one check wins its Tests", func(t *testing.T) {
		t.Parallel()
		older := response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonFlaky, Tests: []string{"TestOld"}}
		newer := response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 11, CheckRunID: 2, Reason: response.RerunReasonFlaky, Tests: []string{"TestNew"}}
		runs := []orchestrator.CheckRun{{ID: 3, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess}}

		got, err := rerunPassedNotes(99, ciSHA, runs, []response.CheckRerunEvent{older, newer}, nil)
		if err != nil {
			t.Fatalf("rerunPassedNotes: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got = %+v, want one message", got)
		}
		var payload response.CheckRerunPassedEvent
		if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if !cmp.Equal(payload.Tests, newer.Tests) {
			t.Fatalf("Tests = %v, want %v", payload.Tests, newer.Tests)
		}
	})

	t.Run("messages are sorted by check name", func(t *testing.T) {
		t.Parallel()
		ciEvent := response.CheckRerunEvent{Check: "ci", SHA: ciSHA, RunID: 10, CheckRunID: 1, Reason: response.RerunReasonFlaky}
		zzEvent := response.CheckRerunEvent{Check: "zz", SHA: ciSHA, RunID: 20, CheckRunID: 2, Reason: response.RerunReasonFlaky}
		runs := []orchestrator.CheckRun{
			{ID: 3, Name: "zz", Status: ghCompleted, Conclusion: ghSuccess},
			{ID: 4, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess},
		}

		got, err := rerunPassedNotes(99, ciSHA, runs, []response.CheckRerunEvent{zzEvent, ciEvent}, nil)
		if err != nil {
			t.Fatalf("rerunPassedNotes: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got = %+v, want two messages", got)
		}
		var first, second response.CheckRerunPassedEvent
		if err := json.Unmarshal(got[0].Payload, &first); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if err := json.Unmarshal(got[1].Payload, &second); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if first.Check != "ci" || second.Check != "zz" {
			t.Fatalf("checks = %q, %q, want ci then zz", first.Check, second.Check)
		}
	})
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

// -----------------------------------------------------------------------
// Pure: mergeDecision (M4 task 8)
// -----------------------------------------------------------------------

// mergeTestFileX is one ordinary, never-special changed path TestMergeDecision's
// table reuses across cases (goconst).
const mergeTestFileX = "internal/x.go"

// mergeTestRule is config.Merge's own default manual_paths and
// dependency_files (internal/config/config.go), reused here so
// TestMergeDecision's table matches design section 8.8's own worked
// examples exactly.
var mergeTestRule = MergeRule{
	Auto:            true,
	Method:          shipMergeMethodSquash,
	ManualPaths:     []string{"deploy/**", "**/migrations/**", "Dockerfile", ".github/workflows/**"},
	DependencyFiles: []string{"go.mod", "go.sum", "package.json", "pyproject.toml"},
}

func TestMergeDecision(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		rule    MergeRule
		changed []string
		auto    bool
		reason  string
	}{
		{
			name:    "auto off always asks",
			rule:    MergeRule{Auto: false},
			changed: []string{mergeTestFileX},
			auto:    false,
			reason:  "merge.auto is off",
		},
		{
			name:    "auto on, no rule blocks",
			rule:    mergeTestRule,
			changed: []string{mergeTestFileX, "internal/x_test.go"},
			auto:    true,
			reason:  "merge.auto is on and no rule blocks it",
		},
		{
			name:    "dependency file by exact path",
			rule:    mergeTestRule,
			changed: []string{"go.mod", "go.sum", mergeTestFileX},
			auto:    false,
			reason:  "the diff changes a dependency file: go.mod, go.sum",
		},
		{
			name:    "dependency file by base name",
			rule:    mergeTestRule,
			changed: []string{"web/package.json"},
			auto:    false,
			reason:  "the diff changes a dependency file: web/package.json",
		},
		{
			name:    "manual-deploy path, migrations glob",
			rule:    mergeTestRule,
			changed: []string{"internal/store/migrations/0005_x.sql"},
			auto:    false,
			reason:  "the diff touches a manual-deploy path: internal/store/migrations/0005_x.sql",
		},
		{
			name:    "manual-deploy path, workflows glob",
			rule:    mergeTestRule,
			changed: []string{".github/workflows/ci.yml"},
			auto:    false,
			reason:  "the diff touches a manual-deploy path: .github/workflows/ci.yml",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			auto, reason := mergeDecision(c.rule, c.changed)
			if auto != c.auto || reason != c.reason {
				t.Errorf("mergeDecision(%+v, %v) = (%v, %q), want (%v, %q)",
					c.rule, c.changed, auto, reason, c.auto, c.reason)
			}
		})
	}
}

// TestEvaluateCIReportedGreen is a regression test for a live PR: a
// required AI-review check (cubic) never reports on a draft, so CI stayed
// pending and Zing never marked the draft ready, the step that would have
// let the check run. ReportedGreen is true when every check that has
// reported passed, whatever is still missing.
func TestEvaluateCIReportedGreen(t *testing.T) {
	t.Parallel()
	required := []orchestrator.RequiredCheck{{Context: testRequiredCI}, {Context: testCheckB}}

	onlyMissing := EvaluateCI([]orchestrator.CheckRun{
		{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess},
	}, nil, required)
	if onlyMissing.State != CIPending || !onlyMissing.ReportedGreen {
		t.Errorf("only a missing check: State %v ReportedGreen %v, want pending and true", onlyMissing.State, onlyMissing.ReportedGreen)
	}

	if none := EvaluateCI(nil, nil, required); none.ReportedGreen {
		t.Error("nothing reported yet: ReportedGreen true, want false")
	}

	running := EvaluateCI([]orchestrator.CheckRun{
		{ID: 1, Name: testRequiredCI, Status: testInProgress},
	}, nil, required)
	if running.ReportedGreen {
		t.Error("a running check: ReportedGreen true, want false")
	}
}

// TestAttestAppStatuses is a regression test for a live PR: the ruleset
// required "CodeRabbit" from app 347564, CodeRabbit reports a commit status
// rather than a check run, and EvaluateCI matches an app-bound check only
// by a check run, so CI stayed pending forever although GitHub reported
// the pull request mergeable ("clean"). GitHub enforces the app identity
// of a required status itself, so once it says clean, a successful status
// of the same context counts as that app's run. Not clean, it does not.
func TestAttestAppStatuses(t *testing.T) {
	t.Parallel()
	app := int64(347564)
	required := []orchestrator.RequiredCheck{{Context: testRequiredCI}, {Context: testCheckB, AppID: &app}}
	runs := []orchestrator.CheckRun{{ID: 1, Name: testRequiredCI, Status: ghCompleted, Conclusion: ghSuccess}}
	statuses := []orchestrator.CommitStatus{{Context: testCheckB, State: ghSuccess}}

	if got := EvaluateCI(attestAppStatuses(runs, statuses, required, "clean"), statuses, required); got.State != CIGreen {
		t.Errorf("clean: State = %v (missing %v), want green", got.State, got.Missing)
	}
	if got := EvaluateCI(attestAppStatuses(runs, statuses, required, "blocked"), statuses, required); got.State != CIPending {
		t.Errorf("blocked: State = %v, want pending (a status alone never satisfies an app-bound check)", got.State)
	}
}

// -----------------------------------------------------------------------
// Pure: the base merge request markers
// -----------------------------------------------------------------------

func TestParseBaseMergeRequest(t *testing.T) {
	t.Parallel()

	poll := baseMergeRequest{MessageID: 7, AfterRunID: 3, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("a", 40)}
	t.Run("poll request round-trips", func(t *testing.T) {
		t.Parallel()
		row := store.MessageRow{ID: poll.MessageID, Message: store.Message{Body: poll.body()}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		got, err := parseBaseMergeRequest(row)
		if err != nil {
			t.Fatalf("parseBaseMergeRequest() error = %v", err)
		}
		if got != poll {
			t.Errorf("parseBaseMergeRequest() = %+v, want %+v", got, poll)
		}
	})

	retry := baseMergeRequest{MessageID: 9, AfterRunID: 4, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("b", 40), RetryOf: 7, Notes: "line one\nline two"}
	t.Run("retry request round-trips", func(t *testing.T) {
		t.Parallel()
		row := store.MessageRow{ID: retry.MessageID, Message: store.Message{Body: retry.body()}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		got, err := parseBaseMergeRequest(row)
		if err != nil {
			t.Fatalf("parseBaseMergeRequest() error = %v", err)
		}
		if got != retry {
			t.Errorf("parseBaseMergeRequest() = %+v, want %+v", got, retry)
		}
	})

	malformed := []struct {
		name string
		body string
	}{
		{"bad first line", "not a request line\nbase main " + strings.Repeat("a", 40)},
		{"short sha", "base merge requested after run 1\nbase main " + strings.Repeat("a", 39)},
		{"uppercase sha", "base merge requested after run 1\nbase main " + strings.Repeat("A", 40)},
		{"missing base line", "base merge requested after run 1\nnot a base line"},
		{"line 3 not a retry line", "base merge requested after run 1\nbase main " + strings.Repeat("a", 40) + "\nnot a retry line"},
	}
	for _, c := range malformed {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			row := store.MessageRow{ID: 42, Message: store.Message{Body: c.body}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
			_, err := parseBaseMergeRequest(row)
			wantErr := "job: base merge request 42: malformed marker"
			if err == nil || err.Error() != wantErr {
				t.Errorf("parseBaseMergeRequest() error = %v, want %q", err, wantErr)
			}
			if !errors.Is(err, ErrMalformedBaseMergeRequest) {
				t.Errorf("errors.Is(%v, ErrMalformedBaseMergeRequest) = false, want true (review thread ta15433844ef97a61)", err)
			}
		})
	}
}

// TestBaseMergeRequestPointRoundTrip proves the Point field round-trips
// through body/parseBaseMergeRequest for each syncPoint, with and without a
// retry, and that an unrecognized point name is malformed the same way the
// existing malformed cases are.
func TestBaseMergeRequestPointRoundTrip(t *testing.T) {
	t.Parallel()

	for _, point := range []syncPoint{syncPointReview, syncPointJudge, syncPointCI} {
		t.Run(string(point)+" request round-trips", func(t *testing.T) {
			t.Parallel()
			req := baseMergeRequest{MessageID: 7, AfterRunID: 3, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("a", 40), Point: point}
			row := store.MessageRow{ID: req.MessageID, Message: store.Message{Body: req.body()}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
			got, err := parseBaseMergeRequest(row)
			if err != nil {
				t.Fatalf("parseBaseMergeRequest() error = %v", err)
			}
			if got != req {
				t.Errorf("parseBaseMergeRequest() = %+v, want %+v", got, req)
			}
		})
	}

	retry := baseMergeRequest{MessageID: 9, AfterRunID: 4, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("b", 40), Point: syncPointCI, RetryOf: 7, Notes: testTwoLineText}
	t.Run("point ci retry request round-trips", func(t *testing.T) {
		t.Parallel()
		row := store.MessageRow{ID: retry.MessageID, Message: store.Message{Body: retry.body()}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		got, err := parseBaseMergeRequest(row)
		if err != nil {
			t.Fatalf("parseBaseMergeRequest() error = %v", err)
		}
		if got != retry {
			t.Errorf("parseBaseMergeRequest() = %+v, want %+v", got, retry)
		}
	})

	t.Run("bogus point name is malformed", func(t *testing.T) {
		t.Parallel()
		body := "base merge requested after run 1\nbase main " + strings.Repeat("a", 40) + "\npoint bogus"
		row := store.MessageRow{ID: 42, Message: store.Message{Body: body}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		_, err := parseBaseMergeRequest(row)
		wantErr := "job: base merge request 42: malformed marker"
		if err == nil || err.Error() != wantErr {
			t.Errorf("parseBaseMergeRequest() error = %v, want %q", err, wantErr)
		}
		if !errors.Is(err, ErrMalformedBaseMergeRequest) {
			t.Errorf("errors.Is(%v, ErrMalformedBaseMergeRequest) = false, want true", err)
		}
	})
}

func TestOpenBaseMergeRequest(t *testing.T) {
	t.Parallel()

	req1 := baseMergeRequest{MessageID: 1, AfterRunID: 1, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("a", 40)}
	req2 := baseMergeRequest{MessageID: 4, AfterRunID: 2, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("b", 40), RetryOf: 1}
	otherOpen := baseMergeRequest{MessageID: 5, AfterRunID: 1, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("c", 40)}

	row := func(id int64, body string) store.MessageRow {
		return store.MessageRow{ID: id, Message: store.Message{Body: body}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
	}

	cases := []struct {
		name     string
		rows     []store.MessageRow
		want     baseMergeRequest
		wantOpen bool
		wantErr  string
	}{
		{name: "no rows"},
		{
			name:     "a lone request is open",
			rows:     []store.MessageRow{row(req1.MessageID, req1.body())},
			want:     req1,
			wantOpen: true,
		},
		{
			name: "landed closes it",
			rows: []store.MessageRow{
				row(req1.MessageID, req1.body()),
				row(2, fmt.Sprintf("base merge landed %d sha %s", req1.MessageID, strings.Repeat("d", 40))),
			},
		},
		{
			name: "closed closes it",
			rows: []store.MessageRow{
				row(req1.MessageID, req1.body()),
				row(2, fmt.Sprintf("base merge closed %d", req1.MessageID)),
			},
		},
		{
			name: "closed followed by a retry request leaves the retry open",
			rows: []store.MessageRow{
				row(req1.MessageID, req1.body()),
				row(2, fmt.Sprintf("base merge closed %d", req1.MessageID)),
				row(req2.MessageID, req2.body()),
			},
			want:     req2,
			wantOpen: true,
		},
		{
			name: "two open requests is an error",
			rows: []store.MessageRow{
				row(req1.MessageID, req1.body()),
				row(otherOpen.MessageID, otherOpen.body()),
			},
			wantErr: "job: ticket has two open base merge requests",
		},
		{
			name: "unrelated rows are ignored",
			rows: []store.MessageRow{
				row(99, "merge asked "+strings.Repeat("e", 40)),
				row(req1.MessageID, req1.body()),
			},
			want:     req1,
			wantOpen: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, open, err := openBaseMergeRequest(c.rows)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("openBaseMergeRequest() error = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("openBaseMergeRequest() unexpected error = %v", err)
			}
			if open != c.wantOpen || got != c.want {
				t.Errorf("openBaseMergeRequest() = (%+v, %v), want (%+v, %v)", got, open, c.want, c.wantOpen)
			}
		})
	}
}

func TestPollMergeCount(t *testing.T) {
	t.Parallel()

	pollA := baseMergeRequest{MessageID: 1, AfterRunID: 1, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("a", 40)}
	pollB := baseMergeRequest{MessageID: 2, AfterRunID: 2, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("b", 40)}
	retry := baseMergeRequest{MessageID: 3, AfterRunID: 3, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("c", 40), RetryOf: 1}

	rows := []store.MessageRow{
		{ID: pollA.MessageID, Message: store.Message{Body: pollA.body()}}, //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: pollB.MessageID, Message: store.Message{Body: pollB.body()}}, //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: retry.MessageID, Message: store.Message{Body: retry.body()}}, //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
	}
	if got := pollMergeCount(rows); got != 2 {
		t.Errorf("pollMergeCount() = %d, want 2", got)
	}
}

// TestPollMergeCountSkipsPointRequests proves a baseSync point's own
// requests (a point line, with or without a retry line) never count toward
// POLL's own budget; a malformed row still counts, as today.
func TestPollMergeCountSkipsPointRequests(t *testing.T) {
	t.Parallel()

	pollA := baseMergeRequest{MessageID: 1, AfterRunID: 1, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("a", 40)}
	pollB := baseMergeRequest{MessageID: 2, AfterRunID: 2, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("b", 40)}
	pointReview := baseMergeRequest{MessageID: 3, AfterRunID: 3, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("c", 40), Point: syncPointReview}
	pointJudgeRetry := baseMergeRequest{MessageID: 4, AfterRunID: 4, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("d", 40), Point: syncPointJudge, RetryOf: 3}
	malformed := store.MessageRow{ID: 5, Message: store.Message{Body: "base merge requested after run 5\nnot a base line"}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped

	rows := []store.MessageRow{
		{ID: pollA.MessageID, Message: store.Message{Body: pollA.body()}},                     //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: pollB.MessageID, Message: store.Message{Body: pollB.body()}},                     //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: pointReview.MessageID, Message: store.Message{Body: pointReview.body()}},         //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: pointJudgeRetry.MessageID, Message: store.Message{Body: pointJudgeRetry.body()}}, //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		malformed,
	}
	if got := pollMergeCount(rows); got != 3 {
		t.Errorf("pollMergeCount() = %d, want 3", got)
	}
}

// TestPointMergeCount proves pointMergeCount counts only the rows a given
// point opened (a point line naming it, no retry line), apart per point.
func TestPointMergeCount(t *testing.T) {
	t.Parallel()

	reviewA := baseMergeRequest{MessageID: 1, AfterRunID: 1, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("a", 40), Point: syncPointReview}
	reviewB := baseMergeRequest{MessageID: 2, AfterRunID: 2, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("b", 40), Point: syncPointReview}
	reviewRetry := baseMergeRequest{MessageID: 3, AfterRunID: 3, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("c", 40), Point: syncPointReview, RetryOf: 1}
	judgeA := baseMergeRequest{MessageID: 4, AfterRunID: 4, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("d", 40), Point: syncPointJudge}
	pollA := baseMergeRequest{MessageID: 5, AfterRunID: 5, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("e", 40)}

	rows := []store.MessageRow{
		{ID: reviewA.MessageID, Message: store.Message{Body: reviewA.body()}},         //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: reviewB.MessageID, Message: store.Message{Body: reviewB.body()}},         //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: reviewRetry.MessageID, Message: store.Message{Body: reviewRetry.body()}}, //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: judgeA.MessageID, Message: store.Message{Body: judgeA.body()}},           //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: pollA.MessageID, Message: store.Message{Body: pollA.body()}},             //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
	}

	if got := pointMergeCount(rows, syncPointReview); got != 2 {
		t.Errorf("pointMergeCount(review) = %d, want 2", got)
	}
	if got := pointMergeCount(rows, syncPointJudge); got != 1 {
		t.Errorf("pointMergeCount(judge) = %d, want 1", got)
	}
	if got := pointMergeCount(rows, syncPointCI); got != 0 {
		t.Errorf("pointMergeCount(ci) = %d, want 0", got)
	}
}

func TestBaseMergeTriedID(t *testing.T) {
	t.Parallel()

	if id, ok := baseMergeTriedID("base merge 12\nmore"); !ok || id != 12 {
		t.Errorf("baseMergeTriedID() = (%d, %v), want (12, true)", id, ok)
	}
	for _, tried := range []string{"", "base merge x", "ci_log\nsomething", "base merge 0"} {
		if _, ok := baseMergeTriedID(tried); ok {
			t.Errorf("baseMergeTriedID(%q) ok = true, want false", tried)
		}
	}
}

func TestMergeCheckText(t *testing.T) {
	t.Parallel()

	if got := mergeCheckText(nil, nil, nil); got != "" {
		t.Errorf("mergeCheckText() = %q, want empty", got)
	}

	results := []commandResult{{Kind: checkKindTest, Cmd: "go test ./...", Exit: 1, Output: "FAIL"}}
	markers := []string{"a.go"}
	outside := []string{"b.go"}
	want := strings.Join([]string{
		checkInputText(results),
		"conflict markers remain in: a.go",
		"these paths are outside the merge; restore or delete them: b.go",
	}, "\n\n")
	if got := mergeCheckText(results, markers, outside); got != want {
		t.Errorf("mergeCheckText() = %q, want %q", got, want)
	}
}

func TestMergeFuncLines(t *testing.T) {
	t.Parallel()

	req := baseMergeRequest{BaseBranch: pbFixtureDefaultBranch, BaseSHA: "abc1234567890123456789012345678901234567"}
	if got := mergeTitle(req); got != "Merge main into the ticket branch" {
		t.Errorf("mergeTitle() = %q, want %q", got, "Merge main into the ticket branch")
	}

	lines := mergeFuncLines(req, []string{"a.go", "b\nbad.go", "", "c.go"})
	want := []string{"Merges main at abc1234", "Resolves a.go", "Resolves c.go"}
	if diff := cmp.Diff(want, lines); diff != "" {
		t.Errorf("mergeFuncLines() mismatch (-want +got):\n%s", diff)
	}

	qs := []response.Question{{Title: "t1", Body: "b1", Recommended: "r1"}}
	wantQ := "t1\nb1\nRecommended: r1"
	if got := mergeQuestionText(qs); got != wantQ {
		t.Errorf("mergeQuestionText() = %q, want %q", got, wantQ)
	}
}

// TestBaseMergePrefix proves baseMergePrefix, the "base merge " family
// Store.MarkersWithPrefix reads, matches every marker this file renders: a
// request's own body, a landed marker, and a closed marker.
func TestBaseMergePrefix(t *testing.T) {
	t.Parallel()

	req := baseMergeRequest{MessageID: 1, AfterRunID: 1, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("a", 40)}
	landed := fmt.Sprintf("base merge landed %d sha %s", req.MessageID, strings.Repeat("b", 40))
	closed := fmt.Sprintf("base merge closed %d", req.MessageID)

	for _, body := range []string{req.body(), landed, closed} {
		if !strings.HasPrefix(body, baseMergePrefix) {
			t.Errorf("%q does not have prefix %q", body, baseMergePrefix)
		}
	}
}

// -----------------------------------------------------------------------
// Pure: reviewBotAction
// -----------------------------------------------------------------------

// TestReviewBotAction proves reviewBotAction's own rule (design "Shape",
// review bot clock markers): no since marker starts the clock; under wait
// since since just waits; past wait with no nudge yet nudges; nudged but
// still under wait (measured from the nudge) waits again; nudged and past
// wait escalates; a wait exactly reached counts as reached, for both since
// and nudgedAt.
func TestReviewBotAction(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	wait := 20 * time.Minute

	tm := func(d time.Duration) *time.Time {
		t := now.Add(d)
		return &t
	}

	tests := []struct {
		name     string
		since    *time.Time
		nudgedAt *time.Time
		want     reviewBotStep
	}{
		{"no since starts", nil, nil, reviewBotStart},
		{"under wait, no nudge: wait", tm(-10 * time.Minute), nil, reviewBotWait},
		{"past wait, no nudge: nudge", tm(-21 * time.Minute), nil, reviewBotNudge},
		{"exactly at wait, no nudge: nudge", tm(-wait), nil, reviewBotNudge},
		{"nudged under wait: wait", tm(-40 * time.Minute), tm(-10 * time.Minute), reviewBotWait},
		{"nudged past wait: escalate", tm(-50 * time.Minute), tm(-21 * time.Minute), reviewBotEscalate},
		{"nudged exactly at wait: escalate", tm(-50 * time.Minute), tm(-wait), reviewBotEscalate},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := reviewBotAction(now, tc.since, tc.nudgedAt, wait)
			if got != tc.want {
				t.Errorf("reviewBotAction = %v, want %v", got, tc.want)
			}
		})
	}
}
