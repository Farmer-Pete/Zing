package job

// This file tests retryTimeout and its callers (runJobWith, runAndRoute),
// the same way codex_failure_test.go tests retryTransient: against
// runAndRoute's and routeFailure's unexported machinery, so it lives in
// package job rather than job_test.

import (
	"context"
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
