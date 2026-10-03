package dispatch_test

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/proc"
	"zing/internal/store"
)

// reclaimTestJob is the job name every test in this file seeds its open run
// under: a code-only state, so claimTimeoutFor's own default applies and
// this file need not depend on machine.toml naming a specific job.
const reclaimTestJob = "build"

// seedForeignClaim claims ticketID under owner/expires (standing in for a
// dead serve's own claim) and returns it ready for reclaimForeign to see
// through store.ForeignClaims.
func seedForeignClaim(t *testing.T, s *store.Store, ticketID int64, owner string, expires time.Time) {
	t.Helper()
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedForeignClaim: claimed=%v err=%v", claimed, err)
	}
}

// seedOpenRun reserves one run on ticketID under the exact (owner, expires)
// already claimed (seedForeignClaim), then records its process identity
// through RecordRunStart, standing in for #45's own start handshake (design
// section 7.1). pgid 0 means "no agent was ever recorded" (PGID stays
// NULL); procStart "" means no start token was recorded for a real pgid.
func seedOpenRun(t *testing.T, s *store.Store, ticketID int64, owner string, expires time.Time, pgid int, procStart string, startedAt time.Time) {
	t.Helper()
	rsv, err := s.Reserve(t.Context(), ticketID, owner, expires,
		store.SessionUpsert{Job: reclaimTestJob, Runtime: testRuntimeFake},
		store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("seedOpenRun: Reserve: %v", err)
	}
	if err := s.RecordRunStart(t.Context(), rsv.RunID, pgid, procStart, startedAt, ""); err != nil {
		t.Fatalf("seedOpenRun: RecordRunStart: %v", err)
	}
}

// startGroupLeader starts "sleep 30" as its own process group leader
// (pgid == pid), the same pattern internal/proc's own tests use, and
// returns the running command; the caller must kill and wait on it.
func startGroupLeader(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("start sleep: %v", err)
	}
	return cmd
}

func killGroup(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
	_ = cmd.Wait()                                      //nolint:errcheck // best-effort teardown
}

// waitGroupGone polls proc.GroupAlive(pgid) until it reports false, bounded
// so a group that never actually dies fails the test instead of hanging it
// (mirrors internal/proc's own tests: GroupAlive can lag a kill by a few
// scheduler ticks).
func waitGroupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for proc.GroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if proc.GroupAlive(pgid) {
		t.Fatalf("group %d still alive after the deadline", pgid)
	}
}

// TestReclaimForeign_NoOpenRunsReclaimsAtOnce proves a dead serve's claim
// with no open run at all (no agent was ever recorded for it) is reclaimed
// on the very first pass (design section 6.3).
func TestReclaimForeign_NoOpenRunsReclaimsAtOnce(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	foreignOwner := "dead-serve-1"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil && *final.ClaimOwner == foreignOwner {
		t.Errorf("final ticket claim owner = %v, want reclaimed (no longer %q)", final.ClaimOwner, foreignOwner)
	}
}

// TestReclaimForeign_WaitsForLiveOrphan proves a dead serve's claim is kept
// while its orphaned agent's process group is still alive, and is
// reclaimed once that group actually exits (design section 6.3).
func TestReclaimForeign_WaitsForLiveOrphan(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	cmd := startGroupLeader(t)
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		killGroup(t, cmd)
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}

	foreignOwner := "dead-serve-2"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)
	seedOpenRun(t, s, ticketID, foreignOwner, expires, pgid, token, time.Now())

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (group still alive): %v", err)
	}

	kept := getTicket(t, s, ticketID)
	if kept.ClaimOwner == nil || *kept.ClaimOwner != foreignOwner {
		t.Fatalf("ticket claim owner = %v while the orphan's group is still alive, want unchanged %q", kept.ClaimOwner, foreignOwner)
	}

	killGroup(t, cmd)
	waitGroupGone(t, pgid)

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (group now gone): %v", err)
	}
	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil && *final.ClaimOwner == foreignOwner {
		t.Errorf("final ticket claim owner = %v, want reclaimed once the orphan's group exited", final.ClaimOwner)
	}
}

// TestReclaimForeign_LiveDescendantAfterLeaderExit proves a group whose
// leader has exited but still has a live descendant is treated as live,
// not dead, even though proc.StartToken(pgid) now reports ErrNoProcess
// (design section 6.3): a process group id cannot be reused while any
// member is alive, so GroupAlive, not StartToken alone, is what actually
// decides this case. This is the fix TestReclaimForeign_WaitsForLiveOrphan
// exposed: classifyOpenRun originally treated every ErrNoProcess as live
// unconditionally, which missed reclaiming a truly empty group (no
// descendant at all) once its own leader had exited and been reaped.
func TestReclaimForeign_LiveDescendantAfterLeaderExit(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	// The leader execs a backgrounded sleep (its own group, inherited from
	// the shell) then exits immediately on its own; the descendant keeps
	// the group alive.
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 30 & exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("start sh: %v", err)
	}
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
	}()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for the leader to exit: %v", err)
	}

	// The leader is gone and reaped; the backgrounded sleep keeps the
	// group alive.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := proc.StartToken(pgid); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("leader pid never reported ErrNoProcess")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !proc.GroupAlive(pgid) {
		t.Fatal("group reported gone right after the leader exited, want the backgrounded sleep to keep it alive")
	}

	foreignOwner := "dead-serve-5"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)
	seedOpenRun(t, s, ticketID, foreignOwner, expires, pgid, token, time.Now())

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner == nil || *final.ClaimOwner != foreignOwner {
		t.Errorf("final ticket claim owner = %v, want unchanged %q (the live descendant must keep the claim held)", final.ClaimOwner, foreignOwner)
	}
}

// TestReclaimForeign_ReusedPidIsDead proves a recorded start token that no
// longer matches the live process at that process group id -- the id was
// reused by an unrelated process -- is treated as dead and reclaimed at
// once, even though the group itself is very much alive (design section
// 6.3).
func TestReclaimForeign_ReusedPidIsDead(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	cmd := startGroupLeader(t)
	defer killGroup(t, cmd)
	pgid := cmd.Process.Pid
	if _, err := proc.StartToken(pgid); err != nil {
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}

	foreignOwner := "dead-serve-3"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)
	seedOpenRun(t, s, ticketID, foreignOwner, expires, pgid, "a-token-nobody-really-has", time.Now())

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil && *final.ClaimOwner == foreignOwner {
		t.Errorf("final ticket claim owner = %v, want reclaimed (the recorded token no longer matches the live group)", final.ClaimOwner)
	}
}

// TestReclaimForeign_UnverifiedGroupNeverKilled proves a live group with no
// recorded start token (the platform cannot verify it, or the read failed
// at start) is never killed, even once its job's deadline has long passed:
// the claim is still reclaimed (so the next tick can resume the session),
// but the group itself is left alone, since Zing cannot tell it apart from
// an unrelated group that happens to reuse the same id (design section 6.3,
// 11).
func TestReclaimForeign_UnverifiedGroupNeverKilled(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	cmd := startGroupLeader(t)
	defer killGroup(t, cmd)
	pgid := cmd.Process.Pid

	foreignOwner := "dead-serve-4"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)
	// No recorded token (""), and StartedAt long in the past: past any
	// job's deadline, so a verified orphan here would already be killed.
	seedOpenRun(t, s, ticketID, foreignOwner, expires, pgid, "", time.Now().Add(-2*time.Hour))

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !proc.GroupAlive(pgid) {
		t.Error("the unverified group was killed, want it left alone")
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil && *final.ClaimOwner == foreignOwner {
		t.Errorf("final ticket claim owner = %v, want reclaimed (an unverified group past its deadline still frees the claim)", final.ClaimOwner)
	}
}

// TestReclaimForeign_KillsOrphanPastDeadline proves a dead serve's
// verified-live orphan, once its own job's deadline (plus claimGrace) has
// passed, is killed outright rather than left to wait forever -- and that
// the claim it was guarding is reclaimed once evaluateOrphan reports it no
// longer live (design section 6.3, dispatch.go's evaluateOrphan KillGroup
// branch).
func TestReclaimForeign_KillsOrphanPastDeadline(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	cmd := startGroupLeader(t)
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		killGroup(t, cmd)
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}

	foreignOwner := "dead-serve-6"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)
	// StartedAt far in the past: well past any job's deadline plus
	// claimGrace, so a verified-live group here must be killed, not
	// waited on (TestReclaimForeign_WaitsForLiveOrphan's own mirror).
	seedOpenRun(t, s, ticketID, foreignOwner, expires, pgid, token, time.Now().Add(-2*time.Hour))

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// evaluateOrphan's KillGroup already delivered SIGKILL synchronously
	// inside Tick; reap the leader so GroupAlive (a kill(pgid, 0) probe,
	// which a zombie still answers) actually reports it gone, the same
	// reaping every other test's own killGroup helper does right after it
	// signals a group itself.
	// The kill pass keeps the claim: SIGKILL does not wait for the group
	// to exit, so reclaiming in the same pass could resume the session
	// while the old agent still runs.
	if mid := getTicket(t, s, ticketID); mid.ClaimOwner == nil || *mid.ClaimOwner != foreignOwner {
		t.Fatalf("claim owner right after the kill = %v, want still %q", mid.ClaimOwner, foreignOwner)
	}
	_ = cmd.Wait() //nolint:errcheck // best-effort reap, SIGKILL already delivered
	waitGroupGone(t, pgid)
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil && *final.ClaimOwner == foreignOwner {
		t.Errorf("final ticket claim owner = %v, want reclaimed once the overdue orphan was killed", final.ClaimOwner)
	}
}

// TestReclaimForeign_ExpiredForeignClaimWithLiveOrphanIsKept proves fill's
// own reconcile ordering (design section 4.2 step 1, 6.3): reclaimForeign
// runs before ExpireClaims, and ExpireClaims is scoped to this process's
// own owner (d.cfg.Owner), so a foreign claim is never swept by
// ExpireClaims just because its claim_expires_at has already passed --
// only reclaimForeign, gated on the orphan actually being dead, may ever
// free it. A regression to ExpireClaims(ctx, now, "") (every owner) would
// sweep this claim out from under the still-live orphan and fail this
// test.
func TestReclaimForeign_ExpiredForeignClaimWithLiveOrphanIsKept(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	cmd := startGroupLeader(t)
	defer killGroup(t, cmd)
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}

	foreignOwner := "dead-serve-7"
	// claim_expires_at already in the past: an unscoped ExpireClaims would
	// treat this as fair game.
	expires := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)
	// StartedAt recent: well inside the job's deadline, so the orphan is
	// live and not yet killable.
	seedOpenRun(t, s, ticketID, foreignOwner, expires, pgid, token, time.Now())

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if tickErr := d.Tick(t.Context()); tickErr != nil {
		t.Fatalf("Tick: %v", tickErr)
	}

	kept := getTicket(t, s, ticketID)
	if kept.ClaimOwner == nil || *kept.ClaimOwner != foreignOwner {
		t.Fatalf("ticket claim owner = %v, want unchanged %q (a live orphan's foreign claim must survive even past its own claim_expires_at)", kept.ClaimOwner, foreignOwner)
	}

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != nil {
		t.Errorf("runs = %+v, want exactly one run still open (Outcome nil)", runs)
	}
}

// TestReclaimForeign_AllLensOrphansMustExit proves reclaimForeign's own
// anyLive aggregation (dispatch.go's reclaimForeign loop) over every open
// run of a foreign claim, the shape a review round's parallel lens
// sessions leave behind: the claim is kept as long as even one of several
// open runs is still live, and is only reclaimed once every one of them
// has actually exited (design section 6.3).
func TestReclaimForeign_AllLensOrphansMustExit(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	cmd := startGroupLeader(t)
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		killGroup(t, cmd)
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}

	foreignOwner := "dead-serve-8"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, foreignOwner, expires)
	// Two open runs under the one claim, the live one seeded (and so
	// scanned) first and the dead one (no agent was ever recorded, pgid 0)
	// second: an aggregation bug that just overwrites anyLive with each
	// run's own verdict, rather than OR-ing them together, would let this
	// dead second run erase the live first run's own true and still pass
	// if the two were seeded the other way around.
	seedOpenRun(t, s, ticketID, foreignOwner, expires, pgid, token, time.Now())
	seedOpenRun(t, s, ticketID, foreignOwner, expires, 0, "", time.Now())

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (one lens still alive): %v", err)
	}

	kept := getTicket(t, s, ticketID)
	if kept.ClaimOwner == nil || *kept.ClaimOwner != foreignOwner {
		t.Fatalf("ticket claim owner = %v while one lens orphan is still alive, want unchanged %q", kept.ClaimOwner, foreignOwner)
	}

	killGroup(t, cmd)
	waitGroupGone(t, pgid)

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (both lenses now gone): %v", err)
	}
	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil && *final.ClaimOwner == foreignOwner {
		t.Errorf("final ticket claim owner = %v, want reclaimed once every lens orphan exited", final.ClaimOwner)
	}
}

// ---- orphaned CHECK commands (#55 plan D10) --------------------------------

// checkDeadline is how far past budget_started_at reclaim waits on a live
// CHECK command: machine.toml's build timeout (45m) plus claimGrace (5m).
const checkDeadline = 50 * time.Minute

// seedCheckProc records a running CHECK command for ticketID under the
// exact (owner, expires) claim, standing in for a dead serve's CHECK.
func seedCheckProc(t *testing.T, s *store.Store, ticketID int64, owner string, expires time.Time, pgid int, procStart string, budgetStartedAt time.Time) {
	t.Helper()
	if err := s.RecordCheckStart(t.Context(), ticketID, owner, expires, "test", pgid, procStart, budgetStartedAt, budgetStartedAt); err != nil {
		t.Fatalf("seedCheckProc: RecordCheckStart: %v", err)
	}
}

// hasCheckProc reports whether ticketID still records a CHECK command.
func hasCheckProc(t *testing.T, s *store.Store, ticketID int64) bool {
	t.Helper()
	_, ok, err := s.CheckProc(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("CheckProc: %v", err)
	}
	return ok
}

// reclaimingDispatcher is a dispatcher that reclaims foreign claims.
func reclaimingDispatcher(t *testing.T, s *store.Store) *dispatch.Dispatcher {
	t.Helper()
	return newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner, ReclaimForeign: true,
	})
}

// claimOwnerIs reports whether ticketID is still claimed by owner.
func claimOwnerIs(t *testing.T, s *store.Store, ticketID int64, owner string) bool {
	t.Helper()
	tk := getTicket(t, s, ticketID)
	return tk.ClaimOwner != nil && *tk.ClaimOwner == owner
}

// TestReclaimForeign_WaitsForLiveCheckCommand proves a dead serve's claim
// is kept while its orphaned CHECK command is alive, and is reclaimed, with
// the row deleted, once the command's group exits.
func TestReclaimForeign_WaitsForLiveCheckCommand(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	cmd := startGroupLeader(t)
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		killGroup(t, cmd)
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}
	const owner = "dead-check-1"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, owner, expires)
	seedCheckProc(t, s, ticketID, owner, expires, pgid, token, time.Now())

	d := reclaimingDispatcher(t, s)
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (command alive): %v", err)
	}
	if !claimOwnerIs(t, s, ticketID, owner) {
		t.Fatal("claim reclaimed while the CHECK command is alive, want it kept")
	}

	killGroup(t, cmd)
	waitGroupGone(t, pgid)
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (command gone): %v", err)
	}
	if claimOwnerIs(t, s, ticketID, owner) {
		t.Error("claim kept after the CHECK command exited, want reclaimed")
	}
	if hasCheckProc(t, s, ticketID) {
		t.Error("check_procs row kept after the reclaim, want it deleted")
	}
}

// TestReclaimForeign_CheckLiveDescendantAfterLeaderExit proves a CHECK
// command whose leader exited but whose descendant lives keeps the claim.
func TestReclaimForeign_CheckLiveDescendantAfterLeaderExit(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 30 & exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("start sh: %v", err)
	}
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
	}()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for the leader to exit: %v", err)
	}
	if !proc.GroupAlive(pgid) {
		t.Fatal("group gone right after the leader exited, want the backgrounded sleep to keep it alive")
	}

	const owner = "dead-check-2"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, owner, expires)
	seedCheckProc(t, s, ticketID, owner, expires, pgid, token, time.Now())

	if err := reclaimingDispatcher(t, s).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !claimOwnerIs(t, s, ticketID, owner) {
		t.Error("claim reclaimed while the CHECK command's descendant lives, want it kept")
	}
}

// TestReclaimForeign_CheckReusedPidIsDead proves a recorded start token that
// no longer matches the live group is a dead CHECK command: reclaimed at
// once, row deleted.
func TestReclaimForeign_CheckReusedPidIsDead(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	cmd := startGroupLeader(t)
	defer killGroup(t, cmd)
	pgid := cmd.Process.Pid
	if _, err := proc.StartToken(pgid); err != nil {
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}
	const owner = "dead-check-3"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, owner, expires)
	seedCheckProc(t, s, ticketID, owner, expires, pgid, "a-token-nobody-really-has", time.Now())

	if err := reclaimingDispatcher(t, s).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if claimOwnerIs(t, s, ticketID, owner) {
		t.Error("claim kept for a reused pid, want reclaimed")
	}
	if hasCheckProc(t, s, ticketID) {
		t.Error("check_procs row kept, want it deleted")
	}
}

// TestReclaimForeign_CheckUnverifiedNeverKilled proves a CHECK command with
// no start token is never killed: past its deadline the claim is reclaimed
// and the group left alone.
func TestReclaimForeign_CheckUnverifiedNeverKilled(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	cmd := startGroupLeader(t)
	defer killGroup(t, cmd)
	pgid := cmd.Process.Pid
	const owner = "dead-check-4"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, owner, expires)
	seedCheckProc(t, s, ticketID, owner, expires, pgid, "", time.Now().Add(-2*time.Hour))

	if err := reclaimingDispatcher(t, s).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !proc.GroupAlive(pgid) {
		t.Error("the unverified CHECK group was killed, want it left alone")
	}
	if claimOwnerIs(t, s, ticketID, owner) {
		t.Error("claim kept for an unverified command past its deadline, want reclaimed")
	}
}

// TestReclaimForeign_KillsCheckCommandPastDeadline proves a verified CHECK
// command past budget_started_at + 45m + 5m is killed with the claim kept
// on that pass, and reclaimed with its row deleted once the group exits.
func TestReclaimForeign_KillsCheckCommandPastDeadline(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	cmd := startGroupLeader(t)
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		killGroup(t, cmd)
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}
	const owner = "dead-check-5"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, owner, expires)
	seedCheckProc(t, s, ticketID, owner, expires, pgid, token, time.Now().Add(-(checkDeadline + time.Second)))

	d := reclaimingDispatcher(t, s)
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !claimOwnerIs(t, s, ticketID, owner) {
		t.Fatal("claim reclaimed on the kill pass, want it kept until the group exits")
	}
	_ = cmd.Wait() //nolint:errcheck // best-effort reap, SIGKILL already delivered
	waitGroupGone(t, pgid)
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if claimOwnerIs(t, s, ticketID, owner) {
		t.Error("claim kept after the killed CHECK command exited, want reclaimed")
	}
	if hasCheckProc(t, s, ticketID) {
		t.Error("check_procs row kept, want it deleted")
	}
}

// TestReclaimForeign_CheckAndAgentBothMustExit proves a dead open run does
// not free a claim whose CHECK command still lives.
func TestReclaimForeign_CheckAndAgentBothMustExit(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	cmd := startGroupLeader(t)
	defer killGroup(t, cmd)
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}
	const owner = "dead-check-6"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedForeignClaim(t, s, ticketID, owner, expires)
	seedOpenRun(t, s, ticketID, owner, expires, 0, "", time.Now())
	seedCheckProc(t, s, ticketID, owner, expires, pgid, token, time.Now())

	if err := reclaimingDispatcher(t, s).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !claimOwnerIs(t, s, ticketID, owner) {
		t.Error("claim reclaimed while the CHECK command lives, want it kept")
	}
}

// TestExpireClaims_KeepsExpiredClaimWithLiveCheckCommand proves ordinary
// claim expiry respects a live recorded CHECK command the same way reclaim
// does (review finding 2): an expired claim, foreign or this serve's own,
// is kept while the command's verified group lives inside its deadline,
// and expires, with the row deleted, once the group is gone.
func TestExpireClaims_KeepsExpiredClaimWithLiveCheckCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		owner          string
		reclaimForeign bool
	}{
		{"any owner, no reclaim", "crashed-check-serve", false},
		{"own claim, with reclaim", testOwner, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newDispatchTestStore(t)
			ticketID := seedQueuedTicket(t, s, testFixtureRef)
			cmd := startGroupLeader(t)
			pgid := cmd.Process.Pid
			token, err := proc.StartToken(pgid)
			if err != nil {
				killGroup(t, cmd)
				t.Skipf("proc.StartToken unsupported on this platform: %v", err)
			}
			expires := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
			seedForeignClaim(t, s, ticketID, tc.owner, expires)
			seedCheckProc(t, s, ticketID, tc.owner, expires, pgid, token, time.Now())

			d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{
				MaxParallel: 1, Owner: testOwner, ReclaimForeign: tc.reclaimForeign,
			})
			if err := d.Tick(t.Context()); err != nil {
				t.Fatalf("Tick (command alive): %v", err)
			}
			if !claimOwnerIs(t, s, ticketID, tc.owner) {
				t.Fatal("expired claim cleared while its CHECK command lives, want it kept")
			}

			killGroup(t, cmd)
			waitGroupGone(t, pgid)
			if err := d.Tick(t.Context()); err != nil {
				t.Fatalf("Tick (command gone): %v", err)
			}
			if tk := getTicket(t, s, ticketID); tk.ClaimExpiresAt != nil && tk.ClaimExpiresAt.Equal(expires) {
				t.Error("the expired claim was kept after the CHECK command exited, want it cleared")
			}
			if hasCheckProc(t, s, ticketID) {
				t.Error("check_procs row kept, want it deleted")
			}
		})
	}
}
