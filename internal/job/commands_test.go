package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"zing/internal/sandbox"
)

const commandRunnerTimeout = 10 * time.Second

// pollUntil polls cond every 10ms until it reports true or deadline
// elapses, failing t once the deadline passes (review F029/F030/F031: a
// poll for a positive signal, in place of a fixed sleep before checking
// that a different, later action never happened).
func pollUntil(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		if cond() {
			return
		}
		if time.Now().After(end) {
			t.Fatal("condition did not become true before the deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readPGID polls for path (a shell command's own "echo $$", written as its
// first statement) and parses it as a process group id: Setpgid gives a
// child its own pid as its group id, so this is also the id the whole
// group -- including any backgrounded grandchild -- shares.
func readPGID(t *testing.T, path string) int {
	t.Helper()
	var data []byte
	pollUntil(t, 5*time.Second, func() bool {
		d, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		data = d
		return true
	})
	pgid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pgid from %s: %v", path, err)
	}
	return pgid
}

// requirePGIDGone polls (10ms interval, 5s deadline) until sending signal 0
// to -pgid reports ESRCH: every process in the group has exited and been
// reaped, so nothing sharing pgid can still be running (review
// F029/F030/F031). A bare timed sleep cannot stand in for this: these
// tests prove a negative (a killed command never reaches its own delayed
// "touch canary" line), and no amount of waiting proves a thing never
// happens, only that it did not happen yet. Waiting for the whole group to
// provably end first removes the race a marker written by a backgrounded
// grandchild would otherwise run against the very kill this test proves.
func requirePGIDGone(t *testing.T, pgid int) {
	t.Helper()
	pollUntil(t, 5*time.Second, func() bool {
		return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
	})
}

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
// actually killed rather than left running. It proves a negative (the
// killed command never reaches its own delayed "touch canary" line), so
// nothing here can wait a fixed time and then check the canary and call
// that proof (review F029): the shell command records its own pid (also
// its process group id, since Setpgid gives it its own) as its first
// statement, and the test polls until that whole group is confirmed gone
// before it ever looks at the canary.
func TestCommandRunnerTimeout(t *testing.T) {
	t.Parallel()
	r := NewCommandRunner(sandbox.Off(), false)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	canary := filepath.Join(dir, "canary")

	shellCmd := fmt.Sprintf("echo $$ >%s; sleep 2; touch %s", pidFile, canary)
	exitCode, err := r.Run(t.Context(), dir, "", shellCmd, 200*time.Millisecond)
	if !errors.Is(err, ErrCommandTimeout) {
		t.Fatalf("err = %v, want ErrCommandTimeout", err)
	}
	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", exitCode)
	}

	requirePGIDGone(t, readPGID(t, pidFile))
	if _, statErr := os.Stat(canary); statErr == nil {
		t.Error("canary file exists: the timed-out command kept running past the timeout")
	}
}

// TestCommandRunnerKillsGroupChild proves the whole process group is killed
// once the command returns, on a clean exit, the same discipline
// runtime.Claude's own killProcessGroup applies: a grandchild forked in the
// background, its own stdout/stderr redirected away so Wait does not block
// on it, must not survive Run returning.
//
// Like TestCommandRunnerTimeout above, this proves a negative (review
// F030), so it cannot wait a fixed time and then check the canary. It
// cannot poll a marker the grandchild itself writes, either: Run kills the
// whole group the instant Wait returns here, with no grace period, so a
// marker the grandchild writes as its own first statement races that kill
// and loses far more often than not (measured: 5/5 runs on this machine
// never saw it). The parent shell's own pid, echoed as this command's
// first statement before it ever backgrounds anything, carries no such
// race -- Run only starts killing once this shell has already run to
// completion -- so the test uses that to learn the group id and polls
// until the whole group, including the grandchild, is confirmed gone.
func TestCommandRunnerKillsGroupChild(t *testing.T) {
	t.Parallel()
	r := NewCommandRunner(sandbox.Off(), false)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	canary := filepath.Join(dir, "canary")

	shellCmd := fmt.Sprintf("echo $$ >%s; ( sleep 2; touch %s ) >/dev/null 2>&1 & disown; exit 0", pidFile, canary)
	exitCode, err := r.Run(t.Context(), dir, "", shellCmd, commandRunnerTimeout)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}

	requirePGIDGone(t, readPGID(t, pidFile))
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

// TestCommandRunnerFilteredEnv proves the unsandboxed runner's environment
// is runtime.FilteredEnv(extraEnv), not os.Environ() plus extraEnv (review
// F051): a secret-shaped variable inherited from this test process must not
// reach the command, while the allowlisted parent variables still do. Not
// parallel: t.Setenv cannot combine with t.Parallel.
func TestCommandRunnerFilteredEnv(t *testing.T) {
	t.Setenv("EVIL_TOKEN", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	r := NewCommandRunner(sandbox.Off(), false)
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")

	exitCode, err := r.Run(t.Context(), dir, "", "env >"+out, commandRunnerTimeout)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}

	got, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("read %s: %v", out, readErr)
	}
	env := string(got)
	if strings.Contains(env, "EVIL_TOKEN") {
		t.Error("EVIL_TOKEN reached the command despite the filter")
	}
	if strings.Contains(env, "AWS_SECRET_ACCESS_KEY") {
		t.Error("AWS_SECRET_ACCESS_KEY reached the command despite the filter")
	}
	if !strings.Contains(env, "PATH=") {
		t.Error("PATH did not reach the command; FilteredEnv should still allow it")
	}
	if !strings.Contains(env, "HOME=") {
		t.Error("HOME did not reach the command; FilteredEnv should still allow it")
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
