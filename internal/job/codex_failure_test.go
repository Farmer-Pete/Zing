package job

// This file tests runAndRoute against the real Codex runtime (package job,
// so it can reach runAndRoute's and routeFailure's unexported machinery
// directly, the same reason runjob_test.go lives here rather than in
// job_test).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// seedCodexFailureTestTicket inserts one queued ticket on runJobTestProject,
// with LocalPath overridden to a real temp directory in place of its own
// placeholder "/tmp/zing": this file's tests run the real Codex runtime,
// whose exec.CommandContext sets cmd.Dir to it and fails to start against a
// directory that does not exist.
func seedCodexFailureTestTicket(t *testing.T, s *store.Store) int64 {
	t.Helper()
	ctx := t.Context()
	proj := runJobTestProject
	proj.LocalPath = t.TempDir()
	projectID, err := s.EnsureProject(ctx, proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: "fake#1", Title: "t", State: stateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

// readTranscriptFile reads path as a string, failing the test on error.
func readTranscriptFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// codexFailureTestScript points at the shared fake codex binary
// internal/runtime's own tests already drive (internal/runtime/codex_test.go),
// resolved relative to this package's directory.
const codexFailureTestScript = "../runtime/testdata/fake_codex.sh"

// fakeCodexModeErrorEvent is fake_codex.sh's error_event mode, named once
// since three tests below set it (goconst).
const fakeCodexModeErrorEvent = "FAKE_CODEX_MODE=error_event"

// newCodexFailureTestDeps builds the Deps a runAndRoute call against the
// real Codex runtime needs: a fresh store and ticket, a claimed owner and
// lease, a real Reserve, the checked-in machine.toml (whose "planreview"
// job names runtime "codex" and model alias "codex"), the codex alias in
// Models, and a real DataDir -- planreview names no sandbox profile, so
// runJobWith gives it a private temp root under DataDir (PKG9-PLAN.md
// section 7.3), which ErrConfig's if DataDir is empty.
func newCodexFailureTestDeps(t *testing.T) (Deps, store.Ticket) {
	t.Helper()
	s := newRunJobTestStore(t)
	ticketID := seedCodexFailureTestTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	// The real Codex runtime execs this path with cmd.Dir set to the
	// ticket's project (below, a real temp directory): a relative path is
	// resolved against that directory, not this test binary's own working
	// directory, so it must be made absolute first.
	absScript, err := filepath.Abs(codexFailureTestScript)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", codexFailureTestScript, err)
	}
	set, err := runtime.NewSet(map[string]runtime.Runtime{
		testRuntimeCodex: runtime.NewCodex(absScript),
	})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t),
		Models: map[string]string{testRuntimeCodex: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}
	return d, ticket
}

// failIfSuccessCalled is runAndRoute's success callback for a test that
// expects routeFailure, not success, to build the commit: a call here means
// the fake codex binary did not fail the way the test set it up to.
func failIfSuccessCalled(t *testing.T) func(runResult) (store.HandlerCommit, error) {
	t.Helper()
	return func(_ runResult) (store.HandlerCommit, error) {
		t.Fatalf("runAndRoute: success callback called, want a routed failure")
		return store.HandlerCommit{}, nil
	}
}

// TestRunAndRoute_CodexErrorEventQuotedInTried is this task's named test
// (design: TestRunAndRoute_CodexErrorEventQuotedInTried): a fake Codex that
// writes one error event to stdout and exits 1, with empty stderr and no -o
// content, must escalate runtime_exec_failed with Tried quoting that
// event's message, and the run's stored TranscriptPath must point at the
// stdout file runJobWith wrote, which must itself contain the event line.
func TestRunAndRoute_CodexErrorEventQuotedInTried(t *testing.T) {
	t.Parallel()
	d, ticket := newCodexFailureTestDeps(t)

	const wantMessage = "unexpected status 400 Bad Request: model not supported"
	fakeDir := t.TempDir()
	req := runtime.RunRequest{
		Job: response.JobPlanreview,
		Env: []string{"FAKE_CODEX_DIR=" + fakeDir, fakeCodexModeErrorEvent},
	}

	su := store.SessionUpsert{Job: jobPlanreviewName, Runtime: testRuntimeCodex}
	commit, err := runAndRoute(t.Context(), d, ticket, jobPlanreviewName, su, req, 0,
		freshSessionRecord, nil, response.EscalationOriginPlanreview,
		failIfSuccessCalled(t), nil, 0)
	if err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}

	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want a runtime_exec_failed escalation")
	}
	if got := commit.Escalation.Payload.Code; got != string(response.EscalationCodeRuntimeExecFailed) {
		t.Errorf("Escalation.Payload.Code = %q, want %q", got, response.EscalationCodeRuntimeExecFailed)
	}
	if got := commit.Escalation.Payload.Tried; got != wantMessage {
		t.Errorf("Escalation.Payload.Tried = %q, want %q", got, wantMessage)
	}
	if commit.Escalation.RunID == nil {
		t.Fatal("Escalation.RunID = nil, want the reserved run id")
	}

	_, ev, err := d.Store.RunEvidenceByID(t.Context(), *commit.Escalation.RunID)
	if err != nil {
		t.Fatalf("RunEvidenceByID: %v", err)
	}
	if ev.TranscriptPath == nil {
		t.Fatal("TranscriptPath = nil, want the stdout file runJobWith wrote")
	}
	wantPath := filepath.Join(d.DataDir, "runs", fmt.Sprintf("run-%d-stdout.jsonl", *commit.Escalation.RunID))
	if *ev.TranscriptPath != wantPath {
		t.Errorf("TranscriptPath = %q, want %q", *ev.TranscriptPath, wantPath)
	}

	data := readTranscriptFile(t, *ev.TranscriptPath)
	if !strings.Contains(data, `"type":"error"`) {
		t.Errorf("transcript file %s does not contain a type error event line:\n%s", *ev.TranscriptPath, data)
	}
	if !strings.Contains(data, wantMessage) {
		t.Errorf("transcript file %s does not contain %q:\n%s", *ev.TranscriptPath, wantMessage, data)
	}
}

// readFakeCodexCalls counts the lines in FAKE_CODEX_DIR/calls, the one
// fake_codex.sh appends on every invocation (fake_codex.sh): the number of
// times the fake binary actually ran, independent of what each run printed.
func readFakeCodexCalls(t *testing.T, fakeDir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fakeDir, "calls"))
	if err != nil {
		t.Fatalf("read calls file: %v", err)
	}
	return strings.Count(string(data), "\n")
}

// TestRunAndRoute_CodexTransientRetriedOnce is this task's named test
// (design: TestRunAndRoute_CodexTransientRetriedOnce): a fake Codex whose
// first call sleeps 1s and fails with a 503 event, transient_once mode,
// must be retried once on the same run, and the retry's success must reach
// runAndRoute's own success callback with no escalation at all.
func TestRunAndRoute_CodexTransientRetriedOnce(t *testing.T) {
	t.Parallel()
	d, ticket := newCodexFailureTestDeps(t)

	fakeDir := t.TempDir()
	req := runtime.RunRequest{
		Job: response.JobPlanreview,
		Env: []string{"FAKE_CODEX_DIR=" + fakeDir, "FAKE_CODEX_MODE=transient_once"},
	}

	successCalled := false
	var reservedRunID int64
	success := func(rr runResult) (store.HandlerCommit, error) {
		successCalled = true
		reservedRunID = rr.Reserved.RunID
		// The second attempt alone takes milliseconds: an AgentTime of at
		// least 1s proves the first attempt's own 1s sleep (transient_once)
		// is still counted in, not dropped when the retry replaced the
		// result (design goal: "The run's AgentTime is the sum of both
		// attempts").
		if rr.Res.AgentTime < time.Second {
			t.Errorf("rr.Res.AgentTime = %v, want at least 1s", rr.Res.AgentTime)
		}
		return store.HandlerCommit{}, nil
	}

	su := store.SessionUpsert{Job: jobPlanreviewName, Runtime: testRuntimeCodex}
	commit, err := runAndRoute(t.Context(), d, ticket, jobPlanreviewName, su, req, 0,
		freshSessionRecord, nil, response.EscalationOriginPlanreview,
		success, nil, 0)
	if err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}
	if !successCalled {
		t.Fatal("success callback never ran")
	}
	if commit.Escalation != nil {
		t.Fatalf("commit.Escalation = %+v, want nil", commit.Escalation)
	}

	if got := readFakeCodexCalls(t, fakeDir); got != 2 {
		t.Errorf("FAKE_CODEX_DIR/calls holds %d lines, want 2", got)
	}

	_, ev, err := d.Store.RunEvidenceByID(t.Context(), reservedRunID)
	if err != nil {
		t.Fatalf("RunEvidenceByID: %v", err)
	}
	if ev.TranscriptPath == nil {
		t.Fatal("TranscriptPath = nil, want the stdout file runJobWith wrote")
	}
	data := readTranscriptFile(t, *ev.TranscriptPath)
	if !strings.Contains(data, "unexpected status 503 Service Unavailable") {
		t.Errorf("transcript file does not contain the first attempt's 503 event:\n%s", data)
	}
	if !strings.Contains(data, "fake-codex-default-thread-id") {
		t.Errorf("transcript file does not contain the second attempt's thread.started event:\n%s", data)
	}
}

// TestRunAndRoute_CodexTransientTwiceEscalatesWithRetryNote is this task's
// named test (design: TestRunAndRoute_CodexTransientTwiceEscalatesWithRetryNote):
// a fake Codex that fails the same transient way on every call (error_event
// mode, with a 503 message in place of the mode's own default) is still
// retried exactly once, and the eventual escalation's Tried names the retry
// and quotes the second attempt's own error.
func TestRunAndRoute_CodexTransientTwiceEscalatesWithRetryNote(t *testing.T) {
	t.Parallel()
	d, ticket := newCodexFailureTestDeps(t)

	const wantMessage = "unexpected status 503 Service Unavailable"
	fakeDir := t.TempDir()
	req := runtime.RunRequest{
		Job: response.JobPlanreview,
		Env: []string{"FAKE_CODEX_DIR=" + fakeDir, fakeCodexModeErrorEvent, "FAKE_CODEX_ERROR_MESSAGE=" + wantMessage},
	}

	su := store.SessionUpsert{Job: jobPlanreviewName, Runtime: testRuntimeCodex}
	commit, err := runAndRoute(t.Context(), d, ticket, jobPlanreviewName, su, req, 0,
		freshSessionRecord, nil, response.EscalationOriginPlanreview,
		failIfSuccessCalled(t), nil, 0)
	if err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}

	if got := readFakeCodexCalls(t, fakeDir); got != 2 {
		t.Errorf("FAKE_CODEX_DIR/calls holds %d lines, want 2", got)
	}

	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want a runtime_exec_failed escalation")
	}
	if got := commit.Escalation.Payload.Code; got != string(response.EscalationCodeRuntimeExecFailed) {
		t.Errorf("Escalation.Payload.Code = %q, want %q", got, response.EscalationCodeRuntimeExecFailed)
	}
	tried := commit.Escalation.Payload.Tried
	const wantPrefix = `retried once after a transient failure matching "503"`
	if !strings.HasPrefix(tried, wantPrefix) {
		t.Errorf("Tried = %q, want prefix %q", tried, wantPrefix)
	}
	if !strings.HasSuffix(tried, wantMessage) {
		t.Errorf("Tried = %q, want suffix %q", tried, wantMessage)
	}
}

// TestRunAndRoute_CodexPermanentErrorRunsOnce is this task's named test
// (design: TestRunAndRoute_CodexPermanentErrorRunsOnce): a fake Codex error
// event that matches no transient pattern (error_event mode's own default
// 400 message) is never retried, so the fake binary runs exactly once.
func TestRunAndRoute_CodexPermanentErrorRunsOnce(t *testing.T) {
	t.Parallel()
	d, ticket := newCodexFailureTestDeps(t)

	fakeDir := t.TempDir()
	req := runtime.RunRequest{
		Job: response.JobPlanreview,
		Env: []string{"FAKE_CODEX_DIR=" + fakeDir, fakeCodexModeErrorEvent},
	}

	su := store.SessionUpsert{Job: jobPlanreviewName, Runtime: testRuntimeCodex}
	if _, err := runAndRoute(t.Context(), d, ticket, jobPlanreviewName, su, req, 0,
		freshSessionRecord, nil, response.EscalationOriginPlanreview,
		failIfSuccessCalled(t), nil, 0); err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}

	if got := readFakeCodexCalls(t, fakeDir); got != 1 {
		t.Errorf("FAKE_CODEX_DIR/calls holds %d lines, want 1", got)
	}
}

// wantFirstAttemptDetail is the FailureDetail every scriptedTransientRuntime
// test below gives a first attempt that fails transiently, named once since
// several tests assert it comes through unchanged (goconst).
const wantFirstAttemptDetail = "first attempt detail"

// scriptedTransientRuntime is a runtime.Runtime that returns one scripted
// (RunResult, error) pair per call, by call order, repeating its last entry
// for any call past len(results): the two runJob-level retry tests below
// drive it directly, in place of the real Codex runtime, to control exactly
// what each of the two attempts returns -- real bytes and a real
// *runtime.ExecError with Transient set, but no real process -- and to
// count how many times rt.Run actually ran.
type scriptedTransientRuntime struct {
	results []struct {
		res runtime.RunResult
		err error
	}
	calls int
}

func (s *scriptedTransientRuntime) Run(context.Context, runtime.RunRequest) (runtime.RunResult, error) {
	i := min(s.calls, len(s.results)-1)
	s.calls++
	return s.results[i].res, s.results[i].err
}

// TestRunJob_TransientRetrySkippedWhenContextEnds is this task's named test
// (design: TestRunJob_TransientRetrySkippedWhenContextEnds): canceling the
// parent context 100ms into retryTransient's 2s wait must skip the second
// attempt, keep the first attempt's own result, set ExitCode to -1, and
// return runtime.ErrCanceled.
func TestRunJob_TransientRetrySkippedWhenContextEnds(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	stub := &scriptedTransientRuntime{results: []struct {
		res runtime.RunResult
		err error
	}{
		{
			res: runtime.RunResult{Stdout: []byte("first attempt stdout\n"), FailureDetail: wantFirstAttemptDetail},
			err: &runtime.ExecError{ExitCode: 1, Transient: "503"},
		},
	}}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	rr, err := runJob(ctx, d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)

	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (no second attempt)", stub.calls)
	}
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}
	if rr.Res.FailureDetail != wantFirstAttemptDetail {
		t.Errorf("rr.Res.FailureDetail = %q, want %q (the first attempt's)", rr.Res.FailureDetail, wantFirstAttemptDetail)
	}
	if rr.Res.ExitCode != -1 {
		t.Errorf("rr.Res.ExitCode = %d, want -1", rr.Res.ExitCode)
	}

	_, ev, err := s.RunEvidenceByID(t.Context(), rr.Reserved.RunID)
	if err != nil {
		t.Fatalf("RunEvidenceByID: %v", err)
	}
	if ev.TranscriptPath == nil {
		t.Fatal("TranscriptPath = nil, want the first attempt's stdout file")
	}
}

// TestRunJob_TransientRetryCapsStdout is this task's named test (design:
// TestRunJob_TransientRetryCapsStdout): joining two attempts' stdout, 40KiB
// each, must cut the result to exactly maxTranscriptBytes (64KiB), keeping
// the retry's own bytes whole at the end and only the tail of the first
// attempt's bytes at the start.
func TestRunJob_TransientRetryCapsStdout(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	const chunk = 40 * 1024
	aBytes := bytes.Repeat([]byte("a"), chunk)
	bBytes := bytes.Repeat([]byte("b"), chunk)
	stub := &scriptedTransientRuntime{results: []struct {
		res runtime.RunResult
		err error
	}{
		{res: runtime.RunResult{Stdout: aBytes}, err: &runtime.ExecError{ExitCode: 1, Transient: "503"}},
		{res: runtime.RunResult{Stdout: bBytes}},
	}}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if stub.calls != 2 {
		t.Errorf("stub.calls = %d, want 2", stub.calls)
	}

	_, ev, err := s.RunEvidenceByID(t.Context(), rr.Reserved.RunID)
	if err != nil {
		t.Fatalf("RunEvidenceByID: %v", err)
	}
	if ev.TranscriptPath == nil {
		t.Fatal("TranscriptPath = nil")
	}
	data := []byte(readTranscriptFile(t, *ev.TranscriptPath))
	if len(data) != maxTranscriptBytes {
		t.Fatalf("len(data) = %d, want %d", len(data), maxTranscriptBytes)
	}
	if !bytes.Equal(data[len(data)-chunk:], bBytes) {
		t.Error("the last 40960 bytes are not all b")
	}
	const keptFromFirst = maxTranscriptBytes - chunk
	if !bytes.Equal(data[:keptFromFirst], bytes.Repeat([]byte("a"), keptFromFirst)) {
		t.Error("the first 24576 bytes are not all a")
	}
}

// newTransientRetryStubDeps is the Deps and ticket every retryTransient test
// below needs: a fresh store and claimed ticket, the scripted stub runtime
// registered under every runtime name a job config might name, and a real
// DataDir, exactly the shape TestRunJob_TransientRetrySkippedWhenContextEnds
// and TestRunJob_TransientRetryCapsStdout each build inline.
func newTransientRetryStubDeps(t *testing.T, stub *scriptedTransientRuntime) (Deps, store.Ticket) {
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

// TestRunJob_TransientRetrySkippedWhenJobDeadlineEnds is
// TestRunJob_TransientRetrySkippedWhenContextEnds' sibling for the other way
// runCtx can end during retryTransient's 2s wait (design goals: "returns
// runtime.ErrCanceled for a parent cancel or runtime.ErrTimeout for the job
// deadline"): a context.WithTimeout parent whose own deadline, not a
// cancel, ends during the wait must still skip the second attempt, keep the
// first attempt's result, set ExitCode to -1, and return
// runtime.ErrTimeout, not runtime.ErrCanceled. deleting or swapping
// retryTransient's errors.Is(ctx.Err(), context.DeadlineExceeded) mapping
// would turn this red while leaving the sibling cancel test green.
func TestRunJob_TransientRetrySkippedWhenJobDeadlineEnds(t *testing.T) {
	t.Parallel()
	stub := &scriptedTransientRuntime{results: []struct {
		res runtime.RunResult
		err error
	}{
		{
			res: runtime.RunResult{Stdout: []byte("first attempt stdout\n"), FailureDetail: wantFirstAttemptDetail},
			err: &runtime.ExecError{ExitCode: 1, Transient: "503"},
		},
	}}
	d, ticket := newTransientRetryStubDeps(t, stub)

	// Shorter than transientRetryDelay (2s), so retryTransient's select
	// hits ctx.Done() from this deadline, not the 2s wait, while rt.Run's
	// own first call (synchronous, no sleep) still has time to complete
	// first.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	rr, err := runJob(ctx, d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)

	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (no second attempt)", stub.calls)
	}
	if !errors.Is(err, runtime.ErrTimeout) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrTimeout)", err)
	}
	if rr.Res.FailureDetail != wantFirstAttemptDetail {
		t.Errorf("rr.Res.FailureDetail = %q, want %q (the first attempt's)", rr.Res.FailureDetail, wantFirstAttemptDetail)
	}
	if rr.Res.ExitCode != -1 {
		t.Errorf("rr.Res.ExitCode = %d, want -1", rr.Res.ExitCode)
	}
}

// TestRunAndRoute_CodexTransientSkippedByJobDeadlineEscalatesFirstDetail
// proves routeFailure's ErrTimeout branch, reached through retryTransient's
// own skip, escalates runtime_exec_failed with Tried set to the skipped
// retry's first-attempt FailureDetail (design goal: "An ErrTimeout
// escalation's Tried is the first attempt's FailureDetail"), the same way
// execFailureCommit already quotes it for an un-retried run.
func TestRunAndRoute_CodexTransientSkippedByJobDeadlineEscalatesFirstDetail(t *testing.T) {
	t.Parallel()
	stub := &scriptedTransientRuntime{results: []struct {
		res runtime.RunResult
		err error
	}{
		{
			res: runtime.RunResult{FailureDetail: wantFirstAttemptDetail},
			err: &runtime.ExecError{ExitCode: 1, Transient: "503"},
		},
	}}
	d, ticket := newTransientRetryStubDeps(t, stub)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	su := store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude}
	commit, err := runAndRoute(ctx, d, ticket, testJobClassify, su, runtime.RunRequest{Job: response.JobClassify}, 0,
		freshSessionRecord, nil, response.EscalationOriginClassify,
		failIfSuccessCalled(t), nil, 0)
	if err != nil {
		t.Fatalf("runAndRoute: %v", err)
	}

	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (no second attempt)", stub.calls)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want a runtime_exec_failed escalation")
	}
	if got := commit.Escalation.Payload.Code; got != string(response.EscalationCodeRuntimeExecFailed) {
		t.Errorf("Escalation.Payload.Code = %q, want %q", got, response.EscalationCodeRuntimeExecFailed)
	}
	if got := commit.Escalation.Payload.Tried; got != wantFirstAttemptDetail {
		t.Errorf("Escalation.Payload.Tried = %q, want %q", got, wantFirstAttemptDetail)
	}
}

// TestRunJob_TransientRetryLogsStartAndSuccess proves retryTransient's two
// happy-path log lines (design goals: "the start is INFO runtime transient
// retry, success is INFO runtime transient retry succeeded ... each with
// ticket_id, run_id, job, and match") and the nongoal that the error text
// itself is never logged ("Logs name the matched pattern and the run, ...
// raw output is never logged"). Not t.Parallel: it swaps slog's
// process-wide default to capture the records.
func TestRunJob_TransientRetryLogsStartAndSuccess(t *testing.T) {
	const firstDetail = "first attempt super secret detail"
	stub := &scriptedTransientRuntime{results: []struct {
		res runtime.RunResult
		err error
	}{
		{res: runtime.RunResult{FailureDetail: firstDetail}, err: &runtime.ExecError{ExitCode: 1, Transient: "503"}},
		{res: runtime.RunResult{FinalMessage: "ok"}},
	}}
	d, ticket := newTransientRetryStubDeps(t, stub)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	recs := jsonLogRecords(t, &logBuf)

	start := findLogRecord(t, recs, "runtime transient retry")
	if start["level"] != slogLevelInfo {
		t.Errorf("start level = %v, want %s", start["level"], slogLevelInfo)
	}
	if got := logRecordInt64(t, start, "ticket_id"); got != ticket.ID {
		t.Errorf("start ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, start, "run_id"); got != rr.Reserved.RunID {
		t.Errorf("start run_id = %d, want %d", got, rr.Reserved.RunID)
	}
	if got := logRecordString(t, start, "job"); got != testJobClassify {
		t.Errorf("start job = %q, want %q", got, testJobClassify)
	}
	if got := logRecordString(t, start, "match"); got != "503" {
		t.Errorf("start match = %q, want %q", got, "503")
	}

	success := findLogRecord(t, recs, "runtime transient retry succeeded")
	if success["level"] != slogLevelInfo {
		t.Errorf("succeeded level = %v, want %s", success["level"], slogLevelInfo)
	}
	if got := logRecordInt64(t, success, "ticket_id"); got != ticket.ID {
		t.Errorf("succeeded ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, success, "run_id"); got != rr.Reserved.RunID {
		t.Errorf("succeeded run_id = %d, want %d", got, rr.Reserved.RunID)
	}
	if got := logRecordString(t, success, "job"); got != testJobClassify {
		t.Errorf("succeeded job = %q, want %q", got, testJobClassify)
	}
	if got := logRecordString(t, success, "match"); got != "503" {
		t.Errorf("succeeded match = %q, want %q", got, "503")
	}

	if strings.Contains(logBuf.String(), firstDetail) {
		t.Errorf("log buffer contains the first attempt's own failure detail text:\n%s", logBuf.String())
	}
}

// TestRunJob_TransientRetryLogsFailure proves the retry-failed branch's WARN
// line (design goals: "failure is WARN runtime transient retry failed with
// err_kind and exit_code", each retry branch line also carrying match) and
// that neither attempt's own failure detail text ever reaches the log. Not
// t.Parallel: it swaps slog's process-wide default to capture the records.
func TestRunJob_TransientRetryLogsFailure(t *testing.T) {
	const firstDetail = "first attempt super secret detail"
	const secondDetail = "second attempt super secret detail"
	stub := &scriptedTransientRuntime{results: []struct {
		res runtime.RunResult
		err error
	}{
		{res: runtime.RunResult{FailureDetail: firstDetail}, err: &runtime.ExecError{ExitCode: 1, Transient: "503"}},
		{res: runtime.RunResult{FailureDetail: secondDetail, ExitCode: 7}, err: &runtime.ExecError{ExitCode: 7}},
	}}
	d, ticket := newTransientRetryStubDeps(t, stub)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err == nil {
		t.Fatal("runJob: err = nil, want the second attempt's own ExecError")
	}

	recs := jsonLogRecords(t, &logBuf)
	failed := findLogRecord(t, recs, "runtime transient retry failed")
	if failed["level"] != slogLevelWarn {
		t.Errorf("level = %v, want %s", failed["level"], slogLevelWarn)
	}
	if got := logRecordInt64(t, failed, "ticket_id"); got != ticket.ID {
		t.Errorf("ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, failed, "run_id"); got != rr.Reserved.RunID {
		t.Errorf("run_id = %d, want %d", got, rr.Reserved.RunID)
	}
	if got := logRecordString(t, failed, "job"); got != testJobClassify {
		t.Errorf("job = %q, want %q", got, testJobClassify)
	}
	if got := logRecordString(t, failed, "match"); got != "503" {
		t.Errorf("match = %q, want %q", got, "503")
	}
	if got := logRecordString(t, failed, "err_kind"); got != "ExecError" {
		t.Errorf("err_kind = %q, want %q", got, "ExecError")
	}
	if got := logRecordInt64(t, failed, "exit_code"); got != 7 {
		t.Errorf("exit_code = %d, want 7", got)
	}

	if strings.Contains(logBuf.String(), firstDetail) {
		t.Errorf("log buffer contains the first attempt's own failure detail text:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), secondDetail) {
		t.Errorf("log buffer contains the second attempt's own failure detail text:\n%s", logBuf.String())
	}
}

// TestRunJob_TransientRetryLogsSkipped proves the context-ended branch's
// WARN line (design goals: "a skip is WARN runtime transient retry skipped
// with err_kind when the run context ends during the 2s wait", each retry
// branch line also carrying match) and that the first attempt's own failure
// detail text never reaches the log. Not t.Parallel: it swaps slog's
// process-wide default to capture the records.
func TestRunJob_TransientRetryLogsSkipped(t *testing.T) {
	const firstDetail = "first attempt super secret detail"
	stub := &scriptedTransientRuntime{results: []struct {
		res runtime.RunResult
		err error
	}{
		{res: runtime.RunResult{FailureDetail: firstDetail}, err: &runtime.ExecError{ExitCode: 1, Transient: "503"}},
	}}
	d, ticket := newTransientRetryStubDeps(t, stub)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	rr, err := runJob(ctx, d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	recs := jsonLogRecords(t, &logBuf)
	skipped := findLogRecord(t, recs, "runtime transient retry skipped")
	if skipped["level"] != slogLevelWarn {
		t.Errorf("level = %v, want %s", skipped["level"], slogLevelWarn)
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
	if got := logRecordString(t, skipped, "match"); got != "503" {
		t.Errorf("match = %q, want %q", got, "503")
	}
	if got := logRecordString(t, skipped, "err_kind"); got != "ErrCanceled" {
		t.Errorf("err_kind = %q, want %q", got, "ErrCanceled")
	}

	if strings.Contains(logBuf.String(), firstDetail) {
		t.Errorf("log buffer contains the first attempt's own failure detail text:\n%s", logBuf.String())
	}
}
