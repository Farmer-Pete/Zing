package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestStartToken_SelfIsNonEmptyAndStable proves StartToken returns a
// non-empty token for the calling process, and that two calls against the
// same pid agree (design section 6.1): the process has not restarted
// between them, so its start time cannot have changed.
func TestStartToken_SelfIsNonEmptyAndStable(t *testing.T) {
	t.Parallel()
	first, err := StartToken(os.Getpid())
	if err != nil {
		t.Fatalf("StartToken(self): %v", err)
	}
	if first == "" {
		t.Fatal("StartToken(self) = \"\", want non-empty")
	}

	second, err := StartToken(os.Getpid())
	if err != nil {
		t.Fatalf("StartToken(self) second call: %v", err)
	}
	if second != first {
		t.Errorf("StartToken(self) = %q then %q, want stable", first, second)
	}
}

// TestStartToken_ChildDiffersFromSelf proves two different live processes
// get different tokens (design section 6.1).
func TestStartToken_ChildDiffersFromSelf(t *testing.T) {
	t.Parallel()
	self, err := StartToken(os.Getpid())
	if err != nil {
		t.Fatalf("StartToken(self): %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "sleep", "5")
	if err = cmd.Start(); err != nil {
		t.Skipf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill() //nolint:errcheck // best-effort teardown; the process may already be gone
		_ = cmd.Wait()         //nolint:errcheck // best-effort teardown; only reaping the child matters here
	}()

	child, err := StartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("StartToken(child): %v", err)
	}
	if child == self {
		t.Errorf("StartToken(child) = %q, same as self %q, want different", child, self)
	}
}

// TestStartToken_ReapedChildReturnsErrNoProcess proves a pid whose process
// has already exited and been reaped returns ErrNoProcess (design section
// 6.1): the exact signal reclaimForeign (a later milestone) uses to tell a
// dead leader from a live one.
func TestStartToken_ReapedChildReturnsErrNoProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "true")
	if err := cmd.Start(); err != nil {
		t.Skipf("start true: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if _, err := StartToken(pid); !errors.Is(err, ErrNoProcess) {
		t.Errorf("StartToken(reaped child) err = %v, want ErrNoProcess", err)
	}
}

// startGroupLeader starts "sleep 5" as its own process group leader
// (pgid == pid) and returns the running command; the caller is responsible
// for killing and waiting on it.
func startGroupLeader(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "5")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("start sleep: %v", err)
	}
	return cmd
}

// TestGroupAlive_TrueForLiveGroupLeader proves GroupAlive sees a running
// group leader as alive (design section 6.1, 6.3).
func TestGroupAlive_TrueForLiveGroupLeader(t *testing.T) {
	t.Parallel()
	cmd := startGroupLeader(t)
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
		_ = cmd.Wait()                                      //nolint:errcheck // best-effort teardown
	}()

	if !GroupAlive(cmd.Process.Pid) {
		t.Error("GroupAlive(live group) = false, want true")
	}
}

// TestGroupAlive_FalseAfterGroupExits proves GroupAlive sees a group with
// no members as gone, after the leader is killed and reaped (design section
// 6.1, 6.3).
func TestGroupAlive_FalseAfterGroupExits(t *testing.T) {
	t.Parallel()
	cmd := startGroupLeader(t)
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill group: %v", err)
	}
	_ = cmd.Wait() //nolint:errcheck // expected: the process was just killed

	// GroupAlive can race the kernel's own cleanup by a few scheduler ticks
	// right after the kill; a reaped Wait means the leader is gone, but the
	// pgid entry itself can take one more instant to clear.
	deadline := time.Now().Add(2 * time.Second)
	for GroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if GroupAlive(pgid) {
		t.Error("GroupAlive(exited group) = true, want false")
	}
}

// TestKillGroup_KillsGroupAndIsIdempotent proves KillGroup ends a live
// group and that a second call against the now-gone group is a no-op, not
// an error (design section 6.1, 6.3): reclaimForeign retries it every pass
// until the group is actually gone.
func TestKillGroup_KillsGroupAndIsIdempotent(t *testing.T) {
	t.Parallel()
	cmd := startGroupLeader(t)
	pgid := cmd.Process.Pid

	if err := KillGroup(pgid); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	_ = cmd.Wait() //nolint:errcheck // expected: the process was just killed

	deadline := time.Now().Add(2 * time.Second)
	for GroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if GroupAlive(pgid) {
		t.Fatal("group still alive after KillGroup")
	}

	if err := KillGroup(pgid); err != nil {
		t.Errorf("second KillGroup on an already-gone group: %v, want nil (ESRCH is not an error)", err)
	}
}

// TestStartToken_ZombieReturnsErrNoProcess proves PR review fix A2: an
// unreaped zombie -- a process that has already exited but whose parent
// has not yet called wait on it -- still has a stable, readable process
// table entry (on Linux, /proc/<pid>/stat with state "Z"; on Darwin, the
// same kinfo_proc sysctl with P_stat SZOMB), but it is not alive. Without
// this, a SIGKILL'd serve whose parent has not yet reaped it would block
// takeover of its own stale lock forever, and reclaim would wait on an
// orphaned agent's zombie group leader as though it were still running.
func TestStartToken_ZombieReturnsErrNoProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "true")
	if err := cmd.Start(); err != nil {
		t.Skipf("start true: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }() //nolint:errcheck // reap the zombie at teardown; the exit itself is expected

	// "true" exits almost immediately; poll until StartToken itself
	// observes the zombie, rather than guessing how long that takes.
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if _, err = StartToken(pid); errors.Is(err, ErrNoProcess) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("StartToken(zombie) err = %v, want ErrNoProcess within 2s", err)
}
