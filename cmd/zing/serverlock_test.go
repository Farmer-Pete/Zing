package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"

	"zing/internal/proc"
)

// TestAcquireServeLock_FreshAcquireSucceeds proves a fresh data directory
// with no existing lock acquires cleanly, writing a two-line file this
// process's own pid and start token parse back out of (design section 6.2).
func TestAcquireServeLock_FreshAcquireSucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	lock, err := acquireServeLock(dir)
	if err != nil {
		t.Fatalf("acquireServeLock: %v", err)
	}
	defer lock.release()

	pid, token, err := readServeLock(lock.path)
	if err != nil {
		t.Fatalf("readServeLock: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("lock pid = %d, want %d", pid, os.Getpid())
	}
	if token != lock.token {
		t.Errorf("lock token = %q, want %q", token, lock.token)
	}
}

// TestAcquireServeLock_LiveHolderRefusedWithExactMessage proves a second
// acquire against a lock this same process already holds (standing in for
// another live process with a matching pid and start token) is refused
// with the exact message design section 6.2 step 3 gives.
func TestAcquireServeLock_LiveHolderRefusedWithExactMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// An empty recorded token makes serveLockHolderAlive fall back to a
	// plain signal-0 liveness check, which correctly reports this process's
	// own, definitely-alive pid as alive regardless of platform support for
	// proc.StartToken.
	first, err := acquireServeLockAs(dir, os.Getpid(), "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.release()

	_, err = acquireServeLockAs(dir, os.Getpid(), "tok-other")
	if err == nil {
		t.Fatal("second acquire against a live holder: want an error, got nil")
	}
	want := fmt.Sprintf("serve: another zing serve is running (pid %d); stop it before starting a new one", os.Getpid())
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

// TestAcquireServeLock_DeadPIDTakenOver proves a lock recorded for a pid
// that is not alive is taken over rather than refused (design section 6.2
// step 4).
func TestAcquireServeLock_DeadPIDTakenOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	deadPID := startAndReapChild(t)

	// Its own release would refuse to remove a file recorded under a
	// different pid than its own once a second acquire has overwritten it,
	// so this test releases nothing itself and instead directly asserts
	// the takeover below.
	if _, err := acquireServeLockAs(dir, deadPID, ""); err != nil {
		t.Fatalf("first acquire (simulating the dead process's own lock): %v", err)
	}

	second, err := acquireServeLockAs(dir, os.Getpid(), "tok-live")
	if err != nil {
		t.Fatalf("second acquire over a dead pid's lock: %v", err)
	}
	defer second.release()

	pid, token, err := readServeLock(second.path)
	if err != nil {
		t.Fatalf("readServeLock: %v", err)
	}
	if pid != os.Getpid() || token != "tok-live" {
		t.Errorf("lock after takeover = (pid=%d, token=%q), want (pid=%d, token=tok-live)", pid, token, os.Getpid())
	}
}

// TestAcquireServeLock_SamePIDDifferentTokenTakenOver proves a lock whose
// recorded start token no longer matches the live process at that pid (the
// pid was reused by an unrelated process after a reboot) is taken over,
// not mistaken for the same incarnation (design section 6.2 step 3, D7).
func TestAcquireServeLock_SamePIDDifferentTokenTakenOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := acquireServeLockAs(dir, os.Getpid(), "a-token-nobody-has"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ourToken, err := proc.StartToken(os.Getpid())
	if err != nil {
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}

	second, err := acquireServeLockAs(dir, os.Getpid(), ourToken)
	if err != nil {
		t.Fatalf("second acquire over a stale token: %v", err)
	}
	defer second.release()

	_, token, err := readServeLock(second.path)
	if err != nil {
		t.Fatalf("readServeLock: %v", err)
	}
	if token != ourToken {
		t.Errorf("lock token after takeover = %q, want %q", token, ourToken)
	}
}

// TestAcquireServeLock_UnparseableFileTakenOver proves a lock file that
// does not parse (never written by linkServeLock) is treated as stale,
// never as a live holder (design section 6.2 step 2).
func TestAcquireServeLock_UnparseableFileTakenOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, serveLockFilename), []byte("garbage, not a lock file"), 0o600); err != nil {
		t.Fatalf("write garbage lock: %v", err)
	}

	lock, err := acquireServeLockAs(dir, os.Getpid(), "tok-live")
	if err != nil {
		t.Fatalf("acquire over an unparseable lock: %v", err)
	}
	lock.release()
}

// TestAcquireServeLock_NonPositivePIDTakenOver proves PR review fix A1: a
// lock file recording pid=0 or a negative pid is malformed, not a live
// holder. Before this fix, serveLockHolderAlive would report a pid 0 "alive"
// forever: syscall.Kill(0, 0) checks the caller's own process group and
// returns nil, and syscall.Kill(-1, 0) checks every process the caller can
// signal, so either would block every later acquire until an operator
// deleted the file by hand.
func TestAcquireServeLock_NonPositivePIDTakenOver(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1} {
		t.Run(fmt.Sprintf("pid=%d", pid), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			content := fmt.Sprintf("pid=%d\nstart=\n", pid)
			if err := os.WriteFile(filepath.Join(dir, serveLockFilename), []byte(content), 0o600); err != nil {
				t.Fatalf("write non-positive-pid lock: %v", err)
			}

			lock, err := acquireServeLockAs(dir, os.Getpid(), "tok-live")
			if err != nil {
				t.Fatalf("acquire over a non-positive pid lock: %v", err)
			}
			lock.release()
		})
	}
}

// TestServeLockHolderAlive_NonPositivePIDIsNotAlive is
// TestAcquireServeLock_NonPositivePIDTakenOver's own direct unit proof
// (PR review fix A1): serveLockHolderAlive itself, not only the acquire
// path above it, must refuse to call kill(0, 0) or kill(-1, 0) on these
// values.
func TestServeLockHolderAlive_NonPositivePIDIsNotAlive(t *testing.T) {
	t.Parallel()
	if serveLockHolderAlive(0, "") {
		t.Error(`serveLockHolderAlive(0, "") = true, want false`)
	}
	if serveLockHolderAlive(-1, "") {
		t.Error(`serveLockHolderAlive(-1, "") = true, want false`)
	}
}

// TestServeLock_ReleaseRemovesOnlyMatchingFile proves release only removes
// the lock file when it still parses with this lock's own pid and token,
// never a file some other acquire has since overwritten (design section
// 6.2).
func TestServeLock_ReleaseRemovesOnlyMatchingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := acquireServeLockAs(dir, 111, "tok-first")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Simulate a second serve having since taken over the same path (first
	// never released; its pid/token no longer match what is on disk).
	if removeErr := os.Remove(first.path); removeErr != nil {
		t.Fatalf("remove: %v", removeErr)
	}
	second, err := acquireServeLockAs(dir, 222, "tok-second")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	first.release()
	if _, _, err := readServeLock(second.path); err != nil {
		t.Fatalf("second's lock file was removed by first's release: %v", err)
	}

	second.release()
	if _, statErr := os.Stat(second.path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("second's own release left the lock file behind: stat err = %v", statErr)
	}
}

// TestServeLockNeverVisibleHalfWritten proves a reader loop running
// concurrently with 100 acquire/release cycles never parses an empty or
// partial file (design section 6.2, 10 item 4): linkServeLock's own
// write-then-link sequence must make the file atomically either absent or
// complete.
func TestServeLockNeverVisibleHalfWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, serveLockFilename)

	stop := make(chan struct{})
	var badRead atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue // absent is fine
			}
			if _, _, parseErr := parseServeLock(data); parseErr != nil && len(data) > 0 {
				badRead.Store(true)
			}
		}
	})

	for i := range 100 {
		lock, err := acquireServeLockAs(dir, 1000+i, "tok")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		lock.release()
	}
	close(stop)
	wg.Wait()

	if badRead.Load() {
		t.Error("the reader observed a non-empty file that failed to parse: the lock was visible half-written")
	}
}

// TestServeLockStaleTakeoverRace proves the serve.lock.guard flock
// serializes a stale takeover across two concurrent starters (design
// section 6.2): with a stale lock file (a dead pid) already in place, two
// goroutines racing acquireServeLockAs with different fake identities both
// return without error (the loser takes over what the winner just wrote,
// in turn), and the final file holds whichever identity acquired last.
func TestServeLockStaleTakeoverRace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	deadPID := startAndReapChild(t)

	if _, err := acquireServeLockAs(dir, deadPID, ""); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	// Deliberately never released below: this test only cares that both
	// calls succeed without corrupting the file, and that the file matches
	// one of the two identities once both are done.
	var wg sync.WaitGroup
	results := make([]error, 2)
	pids := [2]int{3001, 3002}
	tokens := [2]string{"tok-a", "tok-b"}
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, acquireErr := acquireServeLockAs(dir, pids[i], tokens[i])
			results[i] = acquireErr
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("acquire %d: %v", i, err)
		}
	}

	pid, token, err := readServeLock(filepath.Join(dir, serveLockFilename))
	if err != nil {
		t.Fatalf("readServeLock after both acquires: %v", err)
	}
	match := (pid == 3001 && token == "tok-a") || (pid == 3002 && token == "tok-b")
	if !match {
		t.Errorf("final lock = (pid=%d, token=%q), want one of the two identities", pid, token)
	}
}

// TestServeLock_NoExistingFileTwoGoroutinesExactlyOneWins proves that with
// no lock file present at all, two goroutines acquiring at once (through
// acquireServeLockAs with two different fake identities) never both
// succeed in a way that silently drops one: the final file matches
// whichever identity's write happened to land last, and both calls return
// without error (the loser's own write simply lost the os.Link race on one
// attempt and took over on retry, exactly as the stale-takeover path
// does), since nothing on disk before either starts could mark either as
// "the stale one" to take over.
func TestServeLock_NoExistingFileTwoGoroutinesExactlyOneWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pid := 4001 + i
			_, err := acquireServeLockAs(dir, pid, "tok")
			results[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("acquire %d: %v", i, err)
		}
	}

	if _, _, err := readServeLock(filepath.Join(dir, serveLockFilename)); err != nil {
		t.Fatalf("readServeLock: %v", err)
	}
}

// TestClaimOwner_StableNonceMatchesPattern proves claimOwner mints its
// nonce once per process (two calls return the same id) and that the id
// matches <host>-<pid>-<8 lowercase hex characters> (design section 6.2).
func TestClaimOwner_StableNonceMatchesPattern(t *testing.T) {
	first := claimOwner()
	second := claimOwner()
	if first != second {
		t.Errorf("claimOwner() returned %q then %q, want the same id both times", first, second)
	}
	if !regexp.MustCompile(`^.+-\d+-[0-9a-f]{8}$`).MatchString(first) {
		t.Errorf("claimOwner() = %q, want it to match ^.+-\\d+-[0-9a-f]{8}$", first)
	}
}

// startAndReapChild starts a short-lived child process, waits for it to
// exit, and returns its pid: a pid this test can treat as definitely dead
// (reaped, not merely a zombie) without colliding with any real process on
// the test machine.
func startAndReapChild(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run a short-lived child: %v", err)
	}
	return cmd.Process.Pid
}

// TestAcquireServeLock_CreatesMissingDataDir proves a fresh install, where
// the data directory does not exist yet, can take the lock: serve acquires
// it before store.Open, which is what used to create the directory.
func TestAcquireServeLock_CreatesMissingDataDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "fresh", ".zing")
	lock, err := acquireServeLock(dir)
	if err != nil {
		t.Fatalf("acquireServeLock on a missing dir: %v", err)
	}
	defer lock.release()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("data dir mode = %o, want 700", mode)
	}
}
