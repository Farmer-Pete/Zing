package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// countingLockRunner is runCommonLocked's own test double (design section
// 8): it fails its first failCount calls with git's exact lock-file
// contention message, then succeeds, so a test can drive the retry loop
// deterministically without a real git lock file.
type countingLockRunner struct {
	failCount int
	calls     int
}

func (r *countingLockRunner) Run(_ context.Context, _, _ string, _ ...string) (string, error) {
	r.calls++
	if r.calls <= r.failCount {
		return "fatal: Unable to create '/x/.git/config.lock': File exists.", errors.New("exit status 128")
	}
	return "ok", nil
}

func (r *countingLockRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.Run(ctx, dir, name, args...)
}

// TestRunCommonRetriesLockContention proves runCommonLocked's own retry
// rule (design section 8): a command that fails twice with git's lock-file
// contention message, then succeeds, runs exactly three times and reports
// success.
func TestRunCommonRetriesLockContention(t *testing.T) {
	t.Parallel()
	r := &countingLockRunner{failCount: 2}

	out, err := runCommonLocked(t.Context(), r, "/x", "config", "--get", "foo")
	if err != nil {
		t.Fatalf("runCommonLocked: %v, want success on the third attempt", err)
	}
	if out != "ok" {
		t.Errorf("out = %q, want ok", out)
	}
	if r.calls != 3 {
		t.Errorf("calls = %d, want 3 (fail, fail, succeed)", r.calls)
	}
}

// TestRunCommonGivesUpAfterFourConsecutiveFailures proves the retry budget
// is bounded (design section 8: "up to 3 times"): a command that always
// fails with lock contention runs exactly four times in total (the first
// attempt plus three retries) and returns the failure.
func TestRunCommonGivesUpAfterFourConsecutiveFailures(t *testing.T) {
	t.Parallel()
	r := &countingLockRunner{failCount: 1000}

	_, err := runCommonLocked(t.Context(), r, "/x", "config", "--get", "foo")
	if err == nil {
		t.Fatal("runCommonLocked: want an error after exhausting the retry budget, got nil")
	}
	if r.calls != commonLockRetries+1 {
		t.Errorf("calls = %d, want %d (the initial attempt plus %d retries)", r.calls, commonLockRetries+1, commonLockRetries)
	}
}

// TestRunCommonReturnsAtOnceOnAnOrdinaryFailure proves runCommonLocked
// retries only the exact lock-contention shape, not any failure: an
// ordinary git error (no lock-file message in the output) returns after
// one call.
func TestRunCommonReturnsAtOnceOnAnOrdinaryFailure(t *testing.T) {
	t.Parallel()
	calls := 0
	countingFail := runnerFunc{
		run: func(_ context.Context, _, _ string, _ ...string) (string, error) {
			calls++
			return "fatal: some other error", errors.New("exit status 1")
		},
	}

	if _, err := runCommonLocked(t.Context(), countingFail, "/x", "status"); err == nil {
		t.Fatal("runCommonLocked: want the ordinary failure returned, got nil")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry for a non-lock failure)", calls)
	}
}

// runnerFunc adapts a Run func alone into a Runner, for
// TestRunCommonReturnsAtOnceOnAnOrdinaryFailure, which has no need for
// Output.
type runnerFunc struct {
	run func(ctx context.Context, dir, name string, args ...string) (string, error)
}

func (r runnerFunc) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.run(ctx, dir, name, args...)
}

func (r runnerFunc) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.run(ctx, dir, name, args...)
}

// TestCommonLockSharedAcrossOrchestratorsOfOneRepo proves commonLockFor's
// own registry (design section 8): two Orchestrator values for the same
// repository -- one reached directly, one through a symlinked path --
// resolve to the identical *sync.Mutex, while an orchestrator for a
// different repository gets a different one.
func TestCommonLockSharedAcrossOrchestratorsOfOneRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	symlinkDir := filepath.Join(t.TempDir(), "via-symlink")
	if err := os.Symlink(repo, symlinkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	oDirect := newTestOrchestrator(t, repo, execRunner{})
	oSymlink := newTestOrchestrator(t, symlinkDir, execRunner{})

	muDirect, err := oDirect.resolveCommonMu(ctx)
	if err != nil {
		t.Fatalf("resolveCommonMu(direct): %v", err)
	}
	muSymlink, err := oSymlink.resolveCommonMu(ctx)
	if err != nil {
		t.Fatalf("resolveCommonMu(symlink): %v", err)
	}
	if muDirect != muSymlink {
		t.Error("orchestrators for the same repository (one reached through a symlink) got different mutexes")
	}

	otherRepo := newTestRepo(t)
	oOther := newTestOrchestrator(t, otherRepo, execRunner{})
	muOther, err := oOther.resolveCommonMu(ctx)
	if err != nil {
		t.Fatalf("resolveCommonMu(other): %v", err)
	}
	if muOther == muDirect {
		t.Error("orchestrators for two different repositories got the same mutex")
	}
}

// recordedGitCall is one call TestOrchestratorSerializesCommonGitWrites'
// own recordingRunner observed.
type recordedGitCall struct {
	args   []string
	locked bool
}

// recordingRunner wraps a real execRunner (so the lifecycle it drives
// actually works) and records every call's argv and whether commonMu was
// held at that moment (via CommonMuHeldForTest, export_test.go). o is set
// after the Orchestrator it backs is constructed, since the Orchestrator
// needs this Runner to exist first.
type recordingRunner struct {
	o *Orchestrator

	mu     sync.Mutex
	events []recordedGitCall
}

func (r *recordingRunner) record(ctx context.Context, args []string) {
	locked := r.o.CommonMuHeldForTest(ctx)
	r.mu.Lock()
	r.events = append(r.events, recordedGitCall{args: append([]string(nil), args...), locked: locked})
	r.mu.Unlock()
}

func (r *recordingRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	r.record(ctx, args)
	return execRunner{}.Run(ctx, dir, name, args...)
}

func (r *recordingRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	r.record(ctx, args)
	return execRunner{}.Output(ctx, dir, name, args...)
}

// snapshot returns a copy of the events recorded so far.
func (r *recordingRunner) snapshot() []recordedGitCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedGitCall(nil), r.events...)
}

// ticketLifecycle drives one ticket through create-worktree, commit, push,
// judge checkout, and cleanup against o -- the sequence
// TestOrchestratorSerializesCommonGitWrites runs twice at once, for two
// different tickets sharing one repository.
func ticketLifecycle(ctx context.Context, t *testing.T, o *Orchestrator, ticketID int64) error {
	t.Helper()

	wt, err := o.PrepareWorktree(ctx, ticketID, "", nil)
	if err != nil {
		return fmt.Errorf("ticket %d: PrepareWorktree: %w", ticketID, err)
	}

	fileName := fmt.Sprintf("file-%d.txt", ticketID)
	writeTestFile(t, filepath.Join(wt.Dir(), fileName), "content\n")

	msg := CommitMessage{Title: fmt.Sprintf("ticket %d", ticketID), FuncLines: []string{testFuncLine}}
	sha, err := o.CommitTask(ctx, wt, []string{fileName}, msg)
	if err != nil {
		return fmt.Errorf("ticket %d: CommitTask: %w", ticketID, err)
	}

	if err = o.Push(ctx, wt); err != nil {
		return fmt.Errorf("ticket %d: Push: %w", ticketID, err)
	}

	jt, err := o.JudgeWorktree(ctx, ticketID, sha)
	if err != nil {
		return fmt.Errorf("ticket %d: JudgeWorktree: %w", ticketID, err)
	}
	if err := jt.Remove(ctx); err != nil {
		return fmt.Errorf("ticket %d: JudgeTree.Remove: %w", ticketID, err)
	}

	if err := o.RemoveWorktree(ctx, wt); err != nil {
		return fmt.Errorf("ticket %d: RemoveWorktree: %w", ticketID, err)
	}
	return nil
}

// TestOrchestratorSerializesCommonGitWrites drives the full per-ticket
// lifecycle -- create worktree, commit, push, judge checkout, cleanup --
// from two goroutines for two tickets sharing one repository (design
// section 8). It asserts every recorded call whose argv matches the
// inventory's own shared list (SharedGitSubcommandsForTest) ran with
// commonMu held: since sync.Mutex has no goroutine affinity, a "held" read
// taken from inside the Runner call runCommon invokes can only be true
// when runCommon's own Lock is still in effect, so this is also the proof
// that no two shared calls -- across either goroutine -- ever overlapped.
// It also asserts every shared subcommand was actually exercised at least
// once, so the held-lock assertion above is not vacuously true.
//
// cone is left empty on both tickets' PrepareWorktree calls, so "sparse-
// checkout init --cone" (the one shared call built on a locally-constructed
// execRunner rather than o.run, worktree.go's PrepareWorktree) is not
// exercised here; TestRunCommonLocksARunnerPassedExplicitly covers that
// call site directly instead.
func TestOrchestratorSerializesCommonGitWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)

	// Warm resolveCommonMu's cache with a plain, uninstrumented Runner
	// first: GitCommonDir's own resolution call would otherwise run
	// through rec below, which probes CommonMuHeldForTest, which itself
	// calls resolveCommonMu -- a reentrant call into the very
	// sync.Once.Do that is still running, which deadlocks. Once
	// commonMuOnce has fired, every later resolveCommonMu call (including
	// the ones CommonMuHeldForTest makes from inside rec's own Run/Output)
	// just returns the cached mutex, no further git call involved.
	o := newTestOrchestrator(t, repo, execRunner{})
	if _, err := o.resolveCommonMu(ctx); err != nil {
		t.Fatalf("resolveCommonMu: %v", err)
	}

	rec := &recordingRunner{o: o}
	o.run = rec

	const ticketA, ticketB = 2001, 2002

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []int64{ticketA, ticketB} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			errs <- ticketLifecycle(ctx, t, o, id)
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ticket lifecycle: %v", err)
		}
	}

	events := rec.snapshot()
	seen := make(map[string]int)
	for _, ev := range events {
		if !isSharedGitCommand(ev.args) {
			continue
		}
		seen[sharedKey(ev.args)]++
		if !ev.locked {
			t.Errorf("shared call %v ran without commonMu held", ev.args)
		}
	}

	for _, prefix := range SharedGitSubcommandsForTest() {
		key := sharedKeyFromPrefix(prefix)
		if seen[key] == 0 && key != "sparse-checkout init" {
			t.Errorf("shared subcommand %v was never exercised by this test", prefix)
		}
	}
}

// sharedKey and sharedKeyFromPrefix give TestOrchestratorSerializesCommonGitWrites
// a map key per shared subcommand, for its own "every shared subcommand was
// exercised" sanity check.
func sharedKey(args []string) string {
	for _, prefix := range SharedGitSubcommandsForTest() {
		if len(args) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if args[i] != p {
				match = false
				break
			}
		}
		if match {
			return sharedKeyFromPrefix(prefix)
		}
	}
	return ""
}

func sharedKeyFromPrefix(prefix []string) string {
	return strings.Join(prefix, " ")
}

// TestRunCommonLocksARunnerPassedExplicitly proves runCommon holds
// commonMu around whichever Runner a call site passes it, not only o.run
// (design section 8): worktree.go's PrepareWorktree builds its own
// execRunner for "sparse-checkout init --cone" (the one shared call that
// needs a worktree's own filter-driver override) and passes that value to
// runCommon rather than calling it directly.
func TestRunCommonLocksARunnerPassedExplicitly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := newTestRepo(t)
	o := newTestOrchestrator(t, repo, execRunner{})

	probe := &recordingRunner{o: o}
	if _, err := o.runCommon(ctx, probe, repo, "status"); err != nil {
		t.Fatalf("runCommon: %v", err)
	}

	events := probe.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %v, want exactly one", events)
	}
	if !events[0].locked {
		t.Error("runCommon ran the explicitly-passed Runner without commonMu held")
	}
}
