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

// startHandshakeStub is a POSIX shell script this file writes fresh for the
// start-handshake tests (design section 7.1, #45). It is simpler than
// fake_claude.sh/fake_codex.sh, which record a run's full argv/env for
// assertion elsewhere: this stub only needs to mark two moments -- when it
// started, and when its stdin reached EOF -- so a test can prove the
// second happens only after Go's own OnStart callback returned.
//
// Codex's own argv carries "-o <path>", where its final message must be
// written; a Claude invocation never does, so the stub writes its scripted
// result to that path when present, and to stdout (Claude's own result
// channel) otherwise. Both results name job "classify" so
// parseFinalMessage's job check passes for either runtime.
const startHandshakeStub = `#!/bin/sh
set -eu
dir="${STUB_DIR:?STUB_DIR not set}"
: > "$dir/started"

outfile=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    outfile="$arg"
  fi
  prev="$arg"
done

stdin="$(cat)"
: > "$dir/stdin_done"
if [ -z "$stdin" ]; then
  : > "$dir/no_work"
  exit 0
fi

if [ -n "$outfile" ]; then
  printf '%s' '<zing job="classify" outcome="bug"><reason>ok</reason></zing>' > "$outfile"
else
  printf '%s' '{"result":"<zing job=\"classify\" outcome=\"bug\"><reason>ok</reason></zing>"}'
fi
`

// writeStartHandshakeStub writes startHandshakeStub to dir as an executable
// file and returns its path.
func writeStartHandshakeStub(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(path, []byte(startHandshakeStub), 0o755); err != nil { //nolint:gosec // G306: a test fixture script needs to be executable
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// fileModTime stats path and returns its modification time, the ordering
// signal these tests compare against Go's own wall-clock OnStart return
// time: both modern macOS (APFS) and Linux (ext4) file systems this repo
// targets keep sub-second mtime precision, well inside the 200ms gap
// OnStart's own sleep below creates.
func fileModTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime()
}

// TestClaudeOnStartRunsBeforePromptIsWritten proves the start handshake
// (design section 7.1, #45): req.OnStart runs, and returns, strictly
// before the prompt reaches the child's stdin. OnStart sleeps 200ms before
// recording its own return time, so the stub's own stdin-EOF marker (the
// earliest moment it could have received the prompt and the stdin close
// that follows it) must postdate it by at least that long.
func TestClaudeOnStartRunsBeforePromptIsWritten(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	stub := writeStartHandshakeStub(t, dir)

	var onStartReturnedAt time.Time
	var gotPID int
	req := RunRequest{
		Job:    response.JobClassify,
		Model:  testModel,
		Prompt: testPrompt,
		Env:    []string{"STUB_DIR=" + dir},
		OnStart: func(info StartInfo) {
			gotPID = info.PID
			time.Sleep(200 * time.Millisecond)
			onStartReturnedAt = time.Now()
		},
	}

	c := NewClaude(stub, testOAuthToken)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotPID <= 0 {
		t.Errorf("OnStart PID = %d, want > 0", gotPID)
	}
	stdinDoneAt := fileModTime(t, filepath.Join(dir, "stdin_done"))
	if !stdinDoneAt.After(onStartReturnedAt) {
		t.Errorf("stdin reached EOF at %v, want after OnStart returned at %v", stdinDoneAt, onStartReturnedAt)
	}
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
	stub := writeStartHandshakeStub(t, dir)

	var onStartReturnedAt time.Time
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
			onStartReturnedAt = time.Now()
		},
	}

	c := NewCodex(stub)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotPID <= 0 {
		t.Errorf("OnStart PID = %d, want > 0", gotPID)
	}
	if gotSessionID != "" {
		t.Errorf("OnStart SessionID = %q, want \"\" on an unresumed first turn", gotSessionID)
	}
	stdinDoneAt := fileModTime(t, filepath.Join(dir, "stdin_done"))
	if !stdinDoneAt.After(onStartReturnedAt) {
		t.Errorf("stdin reached EOF at %v, want after OnStart returned at %v", stdinDoneAt, onStartReturnedAt)
	}
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
	stub := writeStartHandshakeStub(t, dir)

	cmd := exec.CommandContext(t.Context(), stub)
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
