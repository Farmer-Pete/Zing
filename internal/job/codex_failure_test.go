package job

// This file tests runAndRoute against the real Codex runtime (package job,
// so it can reach runAndRoute's and routeFailure's unexported machinery
// directly, the same reason runjob_test.go lives here rather than in
// job_test).

import (
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
		Env: []string{"FAKE_CODEX_DIR=" + fakeDir, "FAKE_CODEX_MODE=error_event"},
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
