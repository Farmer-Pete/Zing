package job

// This file tests retryTimeout and its callers (runJobWith, runAndRoute),
// the same way codex_failure_test.go tests retryTransient: against
// runAndRoute's and routeFailure's unexported machinery, so it lives in
// package job rather than job_test.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"zing/internal/machine"
	"zing/internal/prompt"
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
// as it does today"): classify's own TimeoutRetries is overridden to 0, so
// an ErrTimeout from the one scripted attempt still escalates
// runtime_exec_failed at once, with no second attempt and a Tried that does
// not mention the automatic retry text task 4 adds.
func TestRunAndRoute_NoTimeoutRetryEscalatesAtOnce(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{FailureDetail: wantFirstAttemptDetail}, err: runtime.ErrTimeout},
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

// TestRunJob_TimeoutRetryLogs is this task's named test (design goals: "Log
// both ... Each stall logs WARN 'run stalled'. Each retry logs its start and
// its end"): a first attempt that stalls and a retry that succeeds must log
// WARN "run stalled" for attempt 1, INFO "runtime timeout retry" before the
// wait, and INFO "runtime timeout retry succeeded" with outcome "ok", each
// carrying ticket_id, run_id, job and last_event. Not t.Parallel: it swaps
// slog's process-wide default to capture the records.
func TestRunJob_TimeoutRetryLogs(t *testing.T) {
	lastEvent, err := time.Parse(time.RFC3339, lastEventStamp)
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{LastEvent: lastEvent}, err: runtime.ErrStalled},
		{res: runtime.RunResult{FinalMessage: "ok"}},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, runErr := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if runErr != nil {
		t.Fatalf("runJob: %v", runErr)
	}
	if stub.calls != 2 {
		t.Fatalf("stub.calls = %d, want 2", stub.calls)
	}

	recs := jsonLogRecords(t, &logBuf)

	checkCommon := func(rec map[string]any) {
		if got := logRecordInt64(t, rec, "ticket_id"); got != ticket.ID {
			t.Errorf("ticket_id = %d, want %d", got, ticket.ID)
		}
		if got := logRecordInt64(t, rec, "run_id"); got != rr.Reserved.RunID {
			t.Errorf("run_id = %d, want %d", got, rr.Reserved.RunID)
		}
		if got := logRecordString(t, rec, "job"); got != testJobClassify {
			t.Errorf("job = %q, want %q", got, testJobClassify)
		}
		if got := logRecordString(t, rec, "last_event"); got != lastEventStamp {
			t.Errorf("last_event = %q, want %q", got, lastEventStamp)
		}
	}

	stall := findLogRecord(t, recs, "run stalled")
	if stall["level"] != slogLevelWarn {
		t.Errorf("stall level = %v, want %s", stall["level"], slogLevelWarn)
	}
	if got := logRecordInt64(t, stall, "attempt"); got != 1 {
		t.Errorf("stall attempt = %d, want 1", got)
	}
	checkCommon(stall)

	start := findLogRecord(t, recs, "runtime timeout retry")
	if start["level"] != slogLevelInfo {
		t.Errorf("start level = %v, want %s", start["level"], slogLevelInfo)
	}
	checkCommon(start)

	success := findLogRecord(t, recs, "runtime timeout retry succeeded")
	if success["level"] != slogLevelInfo {
		t.Errorf("succeeded level = %v, want %s", success["level"], slogLevelInfo)
	}
	if got := logRecordString(t, success, "outcome"); got != "ok" {
		t.Errorf("succeeded outcome = %q, want %q", got, "ok")
	}
	checkCommon(success)
}

// TestRunJob_TimeoutRetryFailureLogs proves the retry-failed branch's WARN
// lines (design goals: "Log both ... Each stall logs WARN 'run stalled'"):
// a first attempt that times out and a retry that itself stalls must log
// WARN "run stalled" for attempt 2, and WARN "runtime timeout retry failed"
// with outcome and err_kind "ErrStalled". Not t.Parallel: it swaps slog's
// process-wide default to capture the records.
func TestRunJob_TimeoutRetryFailureLogs(t *testing.T) {
	lastEvent, err := time.Parse(time.RFC3339, lastEventStamp)
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{LastEvent: lastEvent}, err: runtime.ErrTimeout},
		{res: runtime.RunResult{}, err: runtime.ErrStalled},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, runErr := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if !errors.Is(runErr, runtime.ErrStalled) {
		t.Fatalf("runJob: err = %v, want errors.Is(err, runtime.ErrStalled)", runErr)
	}
	if stub.calls != 2 {
		t.Fatalf("stub.calls = %d, want 2", stub.calls)
	}

	recs := jsonLogRecords(t, &logBuf)

	checkCommon := func(rec map[string]any) {
		if got := logRecordInt64(t, rec, "ticket_id"); got != ticket.ID {
			t.Errorf("ticket_id = %d, want %d", got, ticket.ID)
		}
		if got := logRecordInt64(t, rec, "run_id"); got != rr.Reserved.RunID {
			t.Errorf("run_id = %d, want %d", got, rr.Reserved.RunID)
		}
		if got := logRecordString(t, rec, "job"); got != testJobClassify {
			t.Errorf("job = %q, want %q", got, testJobClassify)
		}
	}

	stall := findLogRecord(t, recs, "run stalled")
	if stall["level"] != slogLevelWarn {
		t.Errorf("stall level = %v, want %s", stall["level"], slogLevelWarn)
	}
	if got := logRecordInt64(t, stall, "attempt"); got != 2 {
		t.Errorf("stall attempt = %d, want 2", got)
	}
	checkCommon(stall)

	failed := findLogRecord(t, recs, "runtime timeout retry failed")
	if failed["level"] != slogLevelWarn {
		t.Errorf("failed level = %v, want %s", failed["level"], slogLevelWarn)
	}
	if got := logRecordString(t, failed, "outcome"); got != "ErrStalled" {
		t.Errorf("failed outcome = %q, want %q", got, "ErrStalled")
	}
	if got := logRecordString(t, failed, "err_kind"); got != "ErrStalled" {
		t.Errorf("failed err_kind = %q, want %q", got, "ErrStalled")
	}
	if got := logRecordString(t, failed, "last_event"); got != lastEventStamp {
		t.Errorf("failed last_event = %q, want %q", got, lastEventStamp)
	}
	checkCommon(failed)
}

// TestRunJob_TimeoutRetrySkippedLogsCanceled proves the skipped branch's
// WARN line (design goals: "Log both"; owner decision Q10): a parent cancel
// before the 2s wait starts must log WARN "runtime timeout retry skipped"
// with err_kind "ErrCanceled". Not t.Parallel: it swaps slog's process-wide
// default to capture the records.
func TestRunJob_TimeoutRetrySkippedLogsCanceled(t *testing.T) {
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{}, err: runtime.ErrTimeout},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stub.onRun = func(int) { cancel() }

	rr, err := runJob(ctx, d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	recs := jsonLogRecords(t, &logBuf)
	skipped := findLogRecord(t, recs, "runtime timeout retry skipped")
	if skipped["level"] != slogLevelWarn {
		t.Errorf("skipped level = %v, want %s", skipped["level"], slogLevelWarn)
	}
	if got := logRecordString(t, skipped, "err_kind"); got != "ErrCanceled" {
		t.Errorf("skipped err_kind = %q, want %q", got, "ErrCanceled")
	}
	if got := logRecordInt64(t, skipped, "ticket_id"); got != ticket.ID {
		t.Errorf("ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, skipped, "run_id"); got != rr.Reserved.RunID {
		t.Errorf("run_id = %d, want %d", got, rr.Reserved.RunID)
	}
	if got := logRecordString(t, skipped, "job"); got != testJobClassify {
		t.Errorf("job = %q, want %q", got, testJobClassify)
	}
}

// TestRunJob_TimeoutRetryRequest is this task's named test (design shape:
// "the retry's deadline is whichever comes first: req.Timeout from now, or
// ctx's own deadline"; owner decision Q7: "a first turn keeps an empty
// session id, so Claude starts a fresh session"): a first turn whose first
// attempt times out must retry with the note line in front of the original
// prompt, an empty session id, classify's own IdleTimeout, and a deadline
// req.Timeout (classify's 5 minutes) out from the retry's own call.
func TestRunJob_TimeoutRetryRequest(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{AgentTime: 180 * time.Second}, err: runtime.ErrTimeout},
		{res: runtime.RunResult{FinalMessage: "ok"}},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	var secondCallTime time.Time
	stub.onRun = func(i int) {
		if i == 1 {
			secondCallTime = time.Now()
		}
	}

	_, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify, Prompt: "PROMPT"}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("stub.calls = %d, want 2", stub.calls)
	}

	const wantNote = "Note from Zing: the previous attempt at this turn timed out after 180 s. Zing is running the turn again."
	wantPrompt := wantNote + "\n\nPROMPT"
	if got := stub.requests[1].Prompt; got != wantPrompt {
		t.Errorf("second request Prompt = %q, want %q", got, wantPrompt)
	}
	if got := stub.requests[1].SessionID; got != "" {
		t.Errorf("second request SessionID = %q, want empty", got)
	}
	if got := stub.requests[1].IdleTimeout; got != 2*time.Minute {
		t.Errorf("second request IdleTimeout = %v, want 2m (classify's own idle_minutes)", got)
	}

	delta := stub.deadlines[1].Sub(secondCallTime)
	if delta < 4*time.Minute+58*time.Second || delta > 5*time.Minute {
		t.Errorf("second request deadline - call time = %v, want between 4m58s and 5m", delta)
	}
}

// TestRunJob_TimeoutRetryResumeKeepsSession is this task's named test (owner
// decision Q7: "a retried resume turn resumes the same session id with the
// note line in front of the resume input"): a resume turn's first attempt
// that stalls must retry on the same session id, with the note line naming
// the stall in front of the original (resume) prompt.
func TestRunJob_TimeoutRetryResumeKeepsSession(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{AgentTime: 125 * time.Second}, err: runtime.ErrStalled},
		{res: runtime.RunResult{FinalMessage: "ok"}},
	}}
	d, ticket := newTimeoutRetryStubDeps(t, stub)

	_, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify, Prompt: "RESUME", SessionID: "SESSION-1"}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("stub.calls = %d, want 2", stub.calls)
	}

	const wantNote = "Note from Zing: the previous attempt at this turn stalled after 125 s with no transcript growth since the process started. Zing is running the turn again."
	wantPrompt := wantNote + "\n\nRESUME"
	if got := stub.requests[1].Prompt; got != wantPrompt {
		t.Errorf("second request Prompt = %q, want %q", got, wantPrompt)
	}
	if got := stub.requests[1].SessionID; got != "SESSION-1" {
		t.Errorf("second request SessionID = %q, want %q", got, "SESSION-1")
	}
}

// TestTimeoutRetryRequest is this task's named test (design: "For
// jobBuildName with res.SessionID set, it returns req with SessionID =
// res.SessionID and Prompt = ... Otherwise it returns today's request,
// timeoutRetryNoteFmt in front of req.Prompt, and false"): a build turn
// whose attempt minted a session resumes that session with the fixed note
// as its whole prompt and no trace of the original prompt; a build turn
// with no session id, and any other job, keep #155's note-in-front
// behavior.
func TestTimeoutRetryRequest(t *testing.T) {
	t.Parallel()

	t.Run("build with session resumes", func(t *testing.T) {
		t.Parallel()
		req := runtime.RunRequest{Job: response.JobBuild, Prompt: "ORIGINAL BUILD PROMPT", Timeout: 45 * time.Minute}
		res := runtime.RunResult{SessionID: "sess-1"}
		now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

		got, resumed := timeoutRetryRequest(jobBuildName, req, res, "timed out after 180 s", now)

		if !resumed {
			t.Error("resumed = false, want true")
		}
		if got.SessionID != "sess-1" {
			t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-1")
		}
		if !strings.HasPrefix(got.Prompt, prompt.BuildResumeHeader) {
			t.Errorf("Prompt does not start with BuildResumeHeader: %q", got.Prompt)
		}
		for _, want := range []string{"git status", "git diff", "run only the tests named in your input"} {
			if !strings.Contains(got.Prompt, want) {
				t.Errorf("Prompt = %q, want it to contain %q", got.Prompt, want)
			}
		}
		if strings.Contains(got.Prompt, "ORIGINAL BUILD PROMPT") {
			t.Errorf("Prompt = %q, want none of the original prompt", got.Prompt)
		}
	})

	t.Run("build with no session keeps today's note", func(t *testing.T) {
		t.Parallel()
		req := runtime.RunRequest{Job: response.JobBuild, Prompt: "ORIGINAL BUILD PROMPT", Timeout: 45 * time.Minute}
		res := runtime.RunResult{}

		got, resumed := timeoutRetryRequest(jobBuildName, req, res, "timed out after 180 s", time.Now())

		if resumed {
			t.Error("resumed = true, want false")
		}
		wantPrompt := fmt.Sprintf(timeoutRetryNoteFmt, "timed out after 180 s") + "\n\nORIGINAL BUILD PROMPT"
		if got.Prompt != wantPrompt {
			t.Errorf("Prompt = %q, want %q", got.Prompt, wantPrompt)
		}
	})

	t.Run("classify keeps today's note even with a session id on res", func(t *testing.T) {
		t.Parallel()
		req := runtime.RunRequest{Job: response.JobClassify, Prompt: "ORIGINAL CLASSIFY PROMPT", Timeout: 5 * time.Minute, SessionID: "req-session"}
		res := runtime.RunResult{SessionID: "sess-1"}

		got, resumed := timeoutRetryRequest(testJobClassify, req, res, "timed out after 60 s", time.Now())

		if resumed {
			t.Error("resumed = true, want false")
		}
		wantPrompt := fmt.Sprintf(timeoutRetryNoteFmt, "timed out after 60 s") + "\n\nORIGINAL CLASSIFY PROMPT"
		if got.Prompt != wantPrompt {
			t.Errorf("Prompt = %q, want %q", got.Prompt, wantPrompt)
		}
		if got.SessionID != "req-session" {
			t.Errorf("SessionID = %q, want req's own unchanged %q", got.SessionID, "req-session")
		}
	})
}

// TestRetryTimeout_BuildResumesWithNote is this task's named test: a build
// turn's first attempt that times out after minting a session must retry by
// resuming that session, with a prompt holding the resume note (git status,
// git diff).
func TestRetryTimeout_BuildResumesWithNote(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{FinalMessage: "ok"}},
	}}
	req := runtime.RunRequest{Job: response.JobBuild, Prompt: "ORIGINAL BUILD PROMPT", Timeout: 45 * time.Minute}
	firstRes := runtime.RunResult{AgentTime: 180 * time.Second, SessionID: "sess-1"}

	_, err := retryTimeout(t.Context(), stub, req, 1, 1, jobBuildName, 1, firstRes, runtime.ErrTimeout)
	if err != nil {
		t.Fatalf("retryTimeout: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("stub.calls = %d, want 1", stub.calls)
	}
	if got := stub.requests[0].SessionID; got != "sess-1" {
		t.Errorf("retry request SessionID = %q, want %q", got, "sess-1")
	}
	if !strings.Contains(stub.requests[0].Prompt, "git status") || !strings.Contains(stub.requests[0].Prompt, "git diff") {
		t.Errorf("retry request Prompt = %q, want it to hold git status and git diff", stub.requests[0].Prompt)
	}
}

// TestRetryTimeout_BuildSecondTimeoutEscalates is this task's named test: a
// build turn whose resumed retry also times out must report that second
// timeout back to the caller (runJobWith turns it into the usual
// runtime_exec_failed escalation elsewhere), with the FailureDetail naming
// the automatic retry.
func TestRetryTimeout_BuildSecondTimeoutEscalates(t *testing.T) {
	t.Parallel()
	stub := &timeoutStubRuntime{results: []scriptedAttempt{
		{res: runtime.RunResult{AgentTime: 60 * time.Second}, err: runtime.ErrTimeout},
	}}
	req := runtime.RunRequest{Job: response.JobBuild, Prompt: "ORIGINAL BUILD PROMPT", Timeout: 45 * time.Minute}
	firstRes := runtime.RunResult{AgentTime: 180 * time.Second, SessionID: "sess-1"}

	retryRes, err := retryTimeout(t.Context(), stub, req, 1, 1, jobBuildName, 1, firstRes, runtime.ErrTimeout)
	if !errors.Is(err, runtime.ErrTimeout) {
		t.Fatalf("retryTimeout err = %v, want ErrTimeout", err)
	}
	if stub.calls != 1 {
		t.Fatalf("stub.calls = %d, want 1", stub.calls)
	}
	if got := stub.requests[0].SessionID; got != "sess-1" {
		t.Errorf("retry request SessionID = %q, want %q", got, "sess-1")
	}
	if !strings.HasPrefix(retryRes.FailureDetail, timeoutRetryPrefix) {
		t.Errorf("FailureDetail = %q, want it to start with %q", retryRes.FailureDetail, timeoutRetryPrefix)
	}
}
