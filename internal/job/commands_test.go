package job

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zing/internal/sandbox"
)

const commandRunnerTimeout = 10 * time.Second

// TestCommandRunnerExitCode proves the unsandboxed runner returns a
// command's real exit code with a nil error.
func TestCommandRunnerExitCode(t *testing.T) {
	t.Parallel()
	r := NewCommandRunner(sandbox.Off(), false)

	exitCode, err := r.Run(t.Context(), t.TempDir(), "", "exit 3", commandRunnerTimeout)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 3 {
		t.Errorf("exitCode = %d, want 3", exitCode)
	}
}

// TestCommandRunnerTimeout proves a command that outlives its timeout
// returns ErrCommandTimeout with exitCode -1, and that the process is
// actually killed rather than left running.
func TestCommandRunnerTimeout(t *testing.T) {
	t.Parallel()
	r := NewCommandRunner(sandbox.Off(), false)
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary")

	exitCode, err := r.Run(t.Context(), dir, "", "sleep 2; touch "+canary, 200*time.Millisecond)
	if !errors.Is(err, ErrCommandTimeout) {
		t.Fatalf("err = %v, want ErrCommandTimeout", err)
	}
	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", exitCode)
	}

	time.Sleep(2 * time.Second)
	if _, statErr := os.Stat(canary); statErr == nil {
		t.Error("canary file exists: the timed-out command kept running past the timeout")
	}
}

// TestCommandRunnerKillsGroupChild proves the whole process group is killed
// once the command returns, on a clean exit, the same discipline
// runtime.Claude's own killProcessGroup applies: a grandchild forked in the
// background, its own stdout/stderr redirected away so Wait does not block
// on it, must not survive Run returning.
func TestCommandRunnerKillsGroupChild(t *testing.T) {
	t.Parallel()
	r := NewCommandRunner(sandbox.Off(), false)
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary")

	shellCmd := "( sleep 2; touch " + canary + " ) >/dev/null 2>&1 & disown; exit 0"
	exitCode, err := r.Run(t.Context(), dir, "", shellCmd, commandRunnerTimeout)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}

	time.Sleep(3 * time.Second)
	if _, statErr := os.Stat(canary); statErr == nil {
		t.Error("canary file exists: the backgrounded grandchild survived Run returning")
	}
}

// TestCommandRunnerStartFailure proves a command that cannot even start (an
// invalid working directory) returns a non-timeout, non-ErrSandbox error
// with exitCode -1.
func TestCommandRunnerStartFailure(t *testing.T) {
	t.Parallel()
	r := NewCommandRunner(sandbox.Off(), false)

	exitCode, err := r.Run(t.Context(), filepath.Join(t.TempDir(), "does-not-exist"), "", "true", commandRunnerTimeout)
	if err == nil {
		t.Fatal("Run with a nonexistent working directory: want an error, got nil")
	}
	if errors.Is(err, ErrCommandTimeout) || errors.Is(err, ErrSandbox) {
		t.Errorf("err = %v, want neither ErrCommandTimeout nor ErrSandbox", err)
	}
	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", exitCode)
	}
}

// TestSandboxedCommandsErrSandbox proves the real CommandRunner refuses to
// run anything when the sandbox is unavailable and required (design section
// 5.5).
func TestSandboxedCommandsErrSandbox(t *testing.T) {
	t.Parallel()
	r := NewCommandRunner(sandbox.Off(), true)

	exitCode, err := r.Run(t.Context(), t.TempDir(), "", "true", commandRunnerTimeout)
	if !errors.Is(err, ErrSandbox) {
		t.Fatalf("err = %v, want ErrSandbox", err)
	}
	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", exitCode)
	}
}
