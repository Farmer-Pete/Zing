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
			res: runtime.RunResult{Stdout: []byte("first attempt stdout\n"), FailureDetail: "first attempt detail"},
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
	if rr.Res.FailureDetail != "first attempt detail" {
		t.Errorf("rr.Res.FailureDetail = %q, want %q (the first attempt's)", rr.Res.FailureDetail, "first attempt detail")
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
