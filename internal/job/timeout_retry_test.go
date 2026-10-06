package job

// This file tests retryTimeout and its callers (runJobWith, runAndRoute),
// the same way codex_failure_test.go tests retryTransient: against
// runAndRoute's and routeFailure's unexported machinery, so it lives in
// package job rather than job_test.

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// timeoutStubRuntime is a runtime.Runtime that returns one scripted
// (RunResult, error) pair per call, by call order, repeating its last entry
// for any call past len(results) -- the same shape as
// scriptedTransientRuntime (codex_failure_test.go), reusing its
// scriptedAttempt type. retryTimeout's own tests additionally need each
// call's RunRequest and its ctx's deadline, so a test can assert what the
// retry's prompt, session id, idle timeout, and deadline actually were.
// onRun, when set, runs synchronously after each call's result is chosen
// and before Run returns it, so a test can end the parent context the
// instant the attempt it cares about has actually run.
type timeoutStubRuntime struct {
	results   []scriptedAttempt
	calls     int
	onRun     func(callIndex int)
	requests  []runtime.RunRequest
	deadlines []time.Time
}

func (s *timeoutStubRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	i := min(s.calls, len(s.results)-1)
	s.calls++
	s.requests = append(s.requests, req)
	deadline, _ := ctx.Deadline()
	s.deadlines = append(s.deadlines, deadline)
	if s.onRun != nil {
		s.onRun(i)
	}
	return s.results[i].res, s.results[i].err
}

// newTimeoutRetryStubDeps is the Deps and ticket every retryTimeout test
// needs, built the same way newTransientRetryStubDeps (codex_failure_test.go)
// is: a fresh store and claimed ticket, the scripted stub runtime
// registered under every runtime name a job config might name, and a real
// DataDir.
func newTimeoutRetryStubDeps(t *testing.T, stub *timeoutStubRuntime) (Deps, store.Ticket) {
	t.Helper()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}
	return d, ticket
}

// TestRunAndRoute_NoTimeoutRetryEscalatesAtOnce is this task's named test
// (design goal: "A job with timeout_retries 0 escalates a timeout at once,
// as it does today"): classify's own TimeoutRetries is overridden to the
// build job's value, after asserting that value is 0, so an ErrTimeout from
// the one scripted attempt still escalates runtime_exec_failed at once,
// with no second attempt and a Tried that does not mention the automatic
// retry text task 4 adds.
func TestRunAndRoute_NoTimeoutRetryEscalatesAtOnce(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{FailureDetail: wantFirstAttemptDetail}, err: runtime.ErrTimeout},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	buildRetries := d.Machine.Jobs[jobBuildName].TimeoutRetries
	if buildRetries != 0 {
		t.Fatalf("build job's TimeoutRetries = %d, want 0", buildRetries)
	}
	jobs := make(map[string]machine.Job, len(d.Machine.Jobs))
	maps.Copy(jobs, d.Machine.Jobs)
	classifyCfg := jobs[testJobClassify]
	classifyCfg.TimeoutRetries = buildRetries
	jobs[testJobClassify] = classifyCfg
	machineCopy := *d.Machine
	machineCopy.Jobs = jobs
	d.Machine = &machineCopy

	su := store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude}
	commit, err := runAndRoute(t.Context(), d, ticket, testJobClassify, su, runtime.RunRequest{Job: response.JobClassify}, 0,
		freshSessionRecord, nil, response.EscalationOriginClassify,
		failIfSuccessCalled(t), nil, 0)
	if err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}

	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (no retry)", stub.calls)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want a runtime_exec_failed escalation")
	}
	if got := commit.Escalation.Payload.Code; got != string(response.EscalationCodeRuntimeExecFailed) {
		t.Errorf("Escalation.Payload.Code = %q, want %q", got, response.EscalationCodeRuntimeExecFailed)
	}
	tried := commit.Escalation.Payload.Tried
	if tried != wantFirstAttemptDetail {
		t.Errorf("Tried = %q, want %q", tried, wantFirstAttemptDetail)
	}
	if strings.Contains(tried, "retried once automatically") {
		t.Errorf("Tried = %q, want no automatic-retry text", tried)
	}
}

// withClassifyTimeoutRetries returns a copy of d whose Machine has
// classify's TimeoutRetries set to retries, leaving every other job
// untouched: several tests below need to force classify's own retry on or
// off against the real, checked-in machine.toml.
func withClassifyTimeoutRetries(d Deps, retries int) Deps {
	jobs := make(map[string]machine.Job, len(d.Machine.Jobs))
	maps.Copy(jobs, d.Machine.Jobs)
	classifyCfg := jobs[testJobClassify]
	classifyCfg.TimeoutRetries = retries
	jobs[testJobClassify] = classifyCfg
	machineCopy := *d.Machine
	machineCopy.Jobs = jobs
	d.Machine = &machineCopy
	return d
}

// lastEventStamp is the owner's own worked example's last-event time (design
// tests: TestRunAndRoute_TimeoutRetryFailsEscalatesOnce and
// TestRunAndRoute_FinalStallEscalatesAsExecFailure), named once since
// several tests below share it (goconst).
const lastEventStamp = "2026-10-05T19:32:07Z"

// TestRunAndRoute_TimeoutRetryFailsEscalatesOnce is this task's named test
// (design goal: "If it fails, exactly one runtime_exec_failed question is
// written, and its Tried starts with 'retried once automatically after a
// timeout'"): a first attempt that stalls and a retry that times out must
// escalate exactly once, with AgentTime summed across both attempts and
// Tried naming both what happened first and what the retry's own failure
// was.
func TestRunAndRoute_TimeoutRetryFailsEscalatesOnce(t *testing.T) {
	t.Parallel()
	lastEvent, err := time.Parse(time.RFC3339, lastEventStamp)
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{AgentTime: 125 * time.Second, LastEvent: lastEvent}, err: runtime.ErrStalled},
		{res: runtime.RunResult{AgentTime: 180 * time.Second}, err: runtime.ErrTimeout},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	su := store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude}
	commit, err := runAndRoute(t.Context(), d, ticket, testJobClassify, su, runtime.RunRequest{Job: response.JobClassify}, 0,
		freshSessionRecord, nil, response.EscalationOriginClassify,
		failIfSuccessCalled(t), nil, 0)
	if err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}

	if stub.calls != 2 {
		t.Fatalf("stub.calls = %d, want 2", stub.calls)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want a runtime_exec_failed escalation")
	}
	if got := commit.Escalation.Payload.Code; got != string(response.EscalationCodeRuntimeExecFailed) {
		t.Errorf("Escalation.Payload.Code = %q, want %q", got, response.EscalationCodeRuntimeExecFailed)
	}
	const wantTried = "retried once automatically after a timeout: the first attempt stalled after 125 s with no transcript growth since 2026-10-05T19:32:07Z; the retry failed with: runtime: job deadline exceeded"
	if got := commit.Escalation.Payload.Tried; got != wantTried {
		t.Errorf("Tried = %q, want %q", got, wantTried)
	}
	if len(commit.Runs) != 1 || commit.Runs[0].AgentSeconds == nil {
		t.Fatalf("commit.Runs = %+v, want one run with AgentSeconds set", commit.Runs)
	}
	if got := *commit.Runs[0].AgentSeconds; got != 305 {
		t.Errorf("AgentSeconds = %d, want 305", got)
	}
}

// TestRunAndRoute_TimeoutRetrySucceedsWithNoQuestion is a design acceptance
// test ("A perimeter run that times out once and succeeds on the retry
// writes no owner question"): classify stands in for perimeter here, since
// both set timeout_retries = 1 in the real machine.toml. A first attempt
// that times out and a retry that succeeds must reach the success callback
// once, with no escalation at all, and AgentTime summed across both
// attempts.
func TestRunAndRoute_TimeoutRetrySucceedsWithNoQuestion(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{AgentTime: 180 * time.Second}, err: runtime.ErrTimeout},
		{res: runtime.RunResult{AgentTime: 20 * time.Second, FinalMessage: "ok"}},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	successCalled := false
	success := func(rr runResult) (store.HandlerCommit, error) {
		successCalled = true
		if got := runtime.Seconds(rr.Res.AgentTime); got != 200 {
			t.Errorf("rr.Res.AgentTime = %d s, want 200 s", got)
		}
		return store.HandlerCommit{}, nil
	}

	su := store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude}
	commit, err := runAndRoute(t.Context(), d, ticket, testJobClassify, su, runtime.RunRequest{Job: response.JobClassify}, 0,
		freshSessionRecord, nil, response.EscalationOriginClassify,
		success, nil, 0)
	if err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}
	if stub.calls != 2 {
		t.Errorf("stub.calls = %d, want 2", stub.calls)
	}
	if !successCalled {
		t.Fatal("success callback never ran")
	}
	if commit.Escalation != nil {
		t.Fatalf("commit.Escalation = %+v, want nil", commit.Escalation)
	}
}

// TestRunAndRoute_FinalStallEscalatesAsExecFailure is a design acceptance
// test: a final ErrStalled, whether there was no retry or the retry itself
// stalled, must escalate runtime_exec_failed the same way an ErrTimeout
// does, with Tried naming the stall.
func TestRunAndRoute_FinalStallEscalatesAsExecFailure(t *testing.T) {
	t.Parallel()
	const stallDetail = "no transcript growth for 120 s; last transcript event: " + lastEventStamp

	t.Run("no retry", func(t *testing.T) {
		t.Parallel()
		stub := &timeoutStubRuntime{results: []scriptedAttempt{
			{res: runtime.RunResult{FailureDetail: stallDetail}, err: runtime.ErrStalled},
		}}
		d, ticket := newTimeoutRetryStubDeps(t, stub)
		d = withClassifyTimeoutRetries(d, 0)

		su := store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude}
		commit, err := runAndRoute(t.Context(), d, ticket, testJobClassify, su, runtime.RunRequest{Job: response.JobClassify}, 0,
			freshSessionRecord, nil, response.EscalationOriginClassify,
			failIfSuccessCalled(t), nil, 0)
		if err != nil {
			t.Fatalf("runAndRoute: %v", err)
		}
		if stub.calls != 1 {
			t.Errorf("stub.calls = %d, want 1 (no retry)", stub.calls)
		}
		if commit.Escalation == nil {
			t.Fatal("commit.Escalation = nil, want a runtime_exec_failed escalation")
		}
		if got := commit.Escalation.Payload.Code; got != string(response.EscalationCodeRuntimeExecFailed) {
			t.Errorf("Escalation.Payload.Code = %q, want %q", got, response.EscalationCodeRuntimeExecFailed)
		}
		if got := commit.Escalation.Payload.Tried; got != stallDetail {
			t.Errorf("Tried = %q, want %q", got, stallDetail)
		}
	})

	t.Run("stall on retry", func(t *testing.T) {
		t.Parallel()
		stub := &timeoutStubRuntime{results: []scriptedAttempt{
			{res: runtime.RunResult{AgentTime: 180 * time.Second}, err: runtime.ErrTimeout},
			{res: runtime.RunResult{FailureDetail: stallDetail}, err: runtime.ErrStalled},
		}}
		d, ticket := newTimeoutRetryStubDeps(t, stub)

		su := store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude}
		commit, err := runAndRoute(t.Context(), d, ticket, testJobClassify, su, runtime.RunRequest{Job: response.JobClassify}, 0,
			freshSessionRecord, nil, response.EscalationOriginClassify,
			failIfSuccessCalled(t), nil, 0)
		if err != nil {
			t.Fatalf("runAndRoute: %v", err)
		}
		if stub.calls != 2 {
			t.Errorf("stub.calls = %d, want 2", stub.calls)
		}
		if commit.Escalation == nil {
			t.Fatal("commit.Escalation = nil, want a runtime_exec_failed escalation")
		}
		const wantTried = "retried once automatically after a timeout: the first attempt timed out after 180 s; the retry failed with: " + stallDetail
		if got := commit.Escalation.Payload.Tried; got != wantTried {
			t.Errorf("Tried = %q, want %q", got, wantTried)
		}
	})
}

// TestRunJob_TimeoutRetrySkippedOnParentCancel is a design acceptance test
// (owner decision Q10: skipRetry returns runtime.ErrCanceled when the parent
// context is canceled): the stub's onRun hook cancels the parent context
// synchronously inside the first attempt's own call, so ctx.Err() is
// already set by the time retryTimeout checks it, before the 2s wait ever
// starts.
func TestRunJob_TimeoutRetrySkippedOnParentCancel(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{}, err: runtime.ErrTimeout},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stub.onRun = func(int) { cancel() }

	rr, err := runJob(ctx, d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)

	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (no retry)", stub.calls)
	}
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}
	if rr.Res.ExitCode != -1 {
		t.Errorf("rr.Res.ExitCode = %d, want -1", rr.Res.ExitCode)
	}
}

// TestRunJob_TimeoutRetrySkippedOnCancelDuringWait is
// TestRunJob_TimeoutRetrySkippedOnParentCancel's sibling for a cancel that
// lands during retryTimeout's 2s wait rather than before it starts.
func TestRunJob_TimeoutRetrySkippedOnCancelDuringWait(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{}, err: runtime.ErrTimeout},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stub.onRun = func(int) { time.AfterFunc(500*time.Millisecond, cancel) }

	rr, err := runJob(ctx, d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)

	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (no retry)", stub.calls)
	}
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}
	if rr.Res.ExitCode != -1 {
		t.Errorf("rr.Res.ExitCode = %d, want -1", rr.Res.ExitCode)
	}
}

// TestRunJob_TimeoutRetrySkippedOnParentDeadline is
// TestRunJob_TimeoutRetrySkippedOnParentCancel's sibling for the parent's
// own deadline (owner decision Q10: ErrTimeout, not ErrCanceled, for a
// deadline, so the owner still gets a question instead of a silent drop).
func TestRunJob_TimeoutRetrySkippedOnParentDeadline(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{}, err: runtime.ErrTimeout},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	ctx, fire := newFakeDeadlineCtx(t.Context())
	stub.onRun = func(int) { fire() }

	rr, err := runJob(ctx, d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)

	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (no retry)", stub.calls)
	}
	if !errors.Is(err, runtime.ErrTimeout) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrTimeout)", err)
	}
	if errors.Is(err, runtime.ErrCanceled) {
		t.Errorf("err = %v, want not errors.Is(err, runtime.ErrCanceled)", err)
	}
	if rr.Res.ExitCode != -1 {
		t.Errorf("rr.Res.ExitCode = %d, want -1", rr.Res.ExitCode)
	}
}
