package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"zing/internal/response"
)

// startHandshakeStub is the path to the POSIX shell script the
// start-handshake tests exec (design section 7.1, #45). It is simpler than
// fake_claude.sh/fake_codex.sh, which record a run's full argv/env for
// assertion elsewhere: this stub marks when it started and when its stdin
// reached EOF, and at EOF it records ("ordered") whether OnStart's own
// marker file already existed, so a test can prove the prompt arrived only
// after Go's OnStart callback returned.
//
// Codex's own argv carries "-o <path>", where its final message must be
// written; a Claude invocation never does, so the stub writes its scripted
// result to that path when present, and to stdout (Claude's own result
// channel) otherwise. Both results name job "classify" so
// parseFinalMessage's job check passes for either runtime.
//
// The script is committed under testdata, not written fresh per test: a
// test that os.WriteFiles an executable script and execs it right after can
// lose a race to a parallel test's fork, which inherits the still-open
// write descriptor and fails its own exec with ETXTBSY (#23). A committed
// file has no writer to race.
const startHandshakeStub = "testdata/start_handshake_stub.sh"

// markOnStartDone creates the marker the stub checks for once its stdin
// closes. OnStart creates it as its last act, so the stub finds it only if
// the prompt was written after OnStart returned. This is a causal check: a
// file mtime comparison against time.Now flakes on Linux, whose mtime
// clock is coarser than Go's.
func markOnStartDone(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "onstart_done"), nil, 0o600); err != nil {
		t.Errorf("write onstart_done: %v", err)
	}
}

// assertStdinClosedAfterOnStart checks the stub saw stdin close and found
// OnStart's marker at that moment.
func assertStdinClosedAfterOnStart(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "stdin_done")); err != nil {
		t.Fatalf("stub never reached stdin EOF: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ordered")); err != nil {
		t.Error("stdin reached EOF before OnStart returned, want the prompt written only after OnStart")
	}
}

// TestClaudeOnStartRunsBeforePromptIsWritten proves the start handshake
// (design section 7.1, #45): req.OnStart runs, and returns, strictly
// before the prompt reaches the child's stdin. OnStart sleeps 200ms, then
// creates its marker file as its last act; the stub must find that marker
// when its stdin reaches EOF. The sleep is what makes an early EOF (a
// prompt written before OnStart returned) reliably miss the marker.
func TestClaudeOnStartRunsBeforePromptIsWritten(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()

	var gotPID int
	req := RunRequest{
		Job:    response.JobClassify,
		Model:  testModel,
		Prompt: testPrompt,
		Env:    []string{"STUB_DIR=" + dir},
		OnStart: func(info StartInfo) {
			gotPID = info.PID
			time.Sleep(200 * time.Millisecond)
			markOnStartDone(t, dir)
		},
	}

	c := NewClaude(startHandshakeStub, testOAuthToken)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotPID <= 0 {
		t.Errorf("OnStart PID = %d, want > 0", gotPID)
	}
	assertStdinClosedAfterOnStart(t, dir)
}

// TestCodexOnStartRunsBeforePromptIsWritten is TestClaudeOnStartRunsBeforePromptIsWritten's
// Codex twin (design section 7.1, #45): the same handshake, driven through
// Codex's own argv shape (the "-o" output file instead of stdout), and
// proving SessionID is "" on an unresumed first turn, per StartInfo's own
// contract for Codex.
func TestCodexOnStartRunsBeforePromptIsWritten(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()

	var gotPID int
	var gotSessionID string
	req := RunRequest{
		Job:    response.JobClassify,
		Model:  testModel,
		Prompt: testPrompt,
		Env:    []string{"STUB_DIR=" + dir},
		OnStart: func(info StartInfo) {
			gotPID = info.PID
			gotSessionID = info.SessionID
			time.Sleep(200 * time.Millisecond)
			markOnStartDone(t, dir)
		},
	}

	c := NewCodex(startHandshakeStub)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotPID <= 0 {
		t.Errorf("OnStart PID = %d, want > 0", gotPID)
	}
	if gotSessionID != "" {
		t.Errorf("OnStart SessionID = %q, want \"\" on an unresumed first turn", gotSessionID)
	}
	assertStdinClosedAfterOnStart(t, dir)
}

// TestAgentWithoutPromptExits proves the handshake's own failure mode
// (design section 7.1, #45): if the parent closes the child's stdin
// without ever writing the prompt -- exactly what happens when serve dies
// between cmd.Start and the prompt-writing goroutine -- the child reads an
// immediately empty stdin and does no work, rather than hanging or
// fabricating a result.
func TestAgentWithoutPromptExits(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()

	// PR review fix G2: t.Context() alone is canceled only when the test
	// returns, so a hang -- the very regression this test guards against --
	// would stall the whole package until go test's own -timeout kills the
	// binary, rather than failing this one test fast with a diagnosis.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, startHandshakeStub)
	cmd.Env = append(os.Environ(), "STUB_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "no_work")); err != nil {
		t.Errorf("no_work marker missing: %v", err)
	}
}
