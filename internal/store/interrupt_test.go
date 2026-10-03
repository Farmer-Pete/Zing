package store

import (
	"testing"
	"time"
)

// TestRecordRunStart_WritesIdentityAndStartedAt proves the basic write path
// (design section 5.3, 7.1): pgid, proc_start, and started_at all land on
// the named run, and a non-empty sessionExternalID fills the session's
// still-null external_id.
func TestRecordRunStart_WritesIdentityAndStartedAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	startedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	if err = s.RecordRunStart(ctx, reserved.RunID, 4242, "123.000456", startedAt, testExternalID1); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.PGID == nil || *run.PGID != 4242 {
		t.Errorf("run.PGID = %v, want 4242", run.PGID)
	}
	if run.ProcStart == nil || *run.ProcStart != "123.000456" {
		t.Errorf("run.ProcStart = %v, want 123.000456", run.ProcStart)
	}
	if run.StartedAt == nil || !run.StartedAt.Equal(startedAt) {
		t.Errorf("run.StartedAt = %v, want %v", run.StartedAt, startedAt)
	}

	sess, ok, err := s.OpenSession(ctx, ticketID, testStatePlanning)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !ok {
		t.Fatal("OpenSession: ok = false, want true")
	}
	if sess.ExternalID == nil || *sess.ExternalID != testExternalID1 {
		t.Errorf("session.ExternalID = %v, want %s", sess.ExternalID, testExternalID1)
	}
}

// TestRecordRunStart_PGIDZeroRecordsOnlyStartedAt proves the fake runtime's
// own path (design section 5.3, 7.1): pgid 0 leaves pgid and proc_start
// NULL (the columns' own CHECK forbids pgid = 0) and records only
// started_at and the session external id.
func TestRecordRunStart_PGIDZeroRecordsOnlyStartedAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	startedAt := time.Now().UTC().Truncate(time.Second)
	if err = s.RecordRunStart(ctx, reserved.RunID, 0, "", startedAt, testExternalID1); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.PGID != nil {
		t.Errorf("run.PGID = %v, want nil for pgid 0", run.PGID)
	}
	if run.ProcStart != nil {
		t.Errorf("run.ProcStart = %v, want nil for pgid 0", run.ProcStart)
	}
	if run.StartedAt == nil || !run.StartedAt.Equal(startedAt) {
		t.Errorf("run.StartedAt = %v, want %v", run.StartedAt, startedAt)
	}
}

// TestRecordRunStartKeepsIdentityOnExternalIDConflict proves design section
// 5.4's own guard: a stored external_id that differs from what this start
// reports is never overwritten, only logged -- the process identity (pgid,
// started_at) still commits, and RecordRunStart still returns nil.
func TestRecordRunStartKeepsIdentityOnExternalIDConflict(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	stored := "abc"
	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err = s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &reserved.SessionID, ExternalID: &stored},
	}); err != nil {
		t.Fatalf("seed stored external_id: %v", err)
	}
	// CommitHandlerResult above cleared the ticket's claim (no Next set still
	// clears it, same as every commit); reclaim the lease so RecordRunStart's
	// own transaction, which does not fence on it, has nothing to do with
	// that -- RecordRunStart never checks the claim at all, only the run.

	startedAt := time.Now().UTC().Truncate(time.Second)
	if err = s.RecordRunStart(ctx, reserved.RunID, 777, "1.000", startedAt, "xyz"); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.PGID == nil || *run.PGID != 777 {
		t.Errorf("run.PGID = %v, want 777 (identity still committed)", run.PGID)
	}
	if run.StartedAt == nil || !run.StartedAt.Equal(startedAt) {
		t.Errorf("run.StartedAt = %v, want %v (identity still committed)", run.StartedAt, startedAt)
	}

	sess, ok, err := s.OpenSession(ctx, ticketID, testStatePlanning)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !ok {
		t.Fatal("OpenSession: ok = false, want true")
	}
	if sess.ExternalID == nil || *sess.ExternalID != stored {
		t.Errorf("session.ExternalID = %v, want unchanged %s", sess.ExternalID, stored)
	}
}

// TestCommitHandlerResult_AcceptsExternalIDAlreadySetByRecordRunStart proves
// design section 5.4's second guarantee: the terminal commit that writes a
// session's external id accepts a session whose external id RecordRunStart
// already set to that same value -- the upsertSessionTx guard ("WHERE
// external_id IS NULL") already makes the second write a no-op rather than
// an error, this just pins it for the new start-time writer.
func TestCommitHandlerResult_AcceptsExternalIDAlreadySetByRecordRunStart(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err = s.RecordRunStart(ctx, reserved.RunID, 100, "1.000", time.Now(), "abc"); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	externalID := "abc"
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &reserved.SessionID, ExternalID: &externalID},
		Runs:    []Run{{ID: reserved.RunID, Outcome: new(testOutcomeBug)}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
}

// TestInterruptRuns_TerminalizesOpenRunsAndClearsClaim proves the main path
// (design section 5.3, 7.2): every run of ticketID's sessions with a null
// outcome becomes outcome=error, interrupted=1, exit_code=-1, and the
// ticket's claim is cleared, all fenced on the exact owner and expires.
func TestInterruptRuns_TerminalizesOpenRunsAndClearsClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	applied, err := s.InterruptRuns(ctx, ticketID, owner, expires)
	if err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.Outcome == nil || *run.Outcome != testOutcomeError {
		t.Errorf("run.Outcome = %v, want error", run.Outcome)
	}
	if !run.Interrupted {
		t.Error("run.Interrupted = false, want true")
	}
	if run.ExitCode == nil || *run.ExitCode != -1 {
		t.Errorf("run.ExitCode = %v, want -1", run.ExitCode)
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner != nil || got.ClaimExpiresAt != nil {
		t.Errorf("ticket claim = (%v, %v), want cleared", got.ClaimOwner, got.ClaimExpiresAt)
	}
}

// TestInterruptRuns_AgentSecondsFromStartedAtToNow proves the agent_seconds
// computation when started_at is set (design section 5.3): whole seconds
// elapsed since started_at, so an interrupted run's work still counts
// against the ticket's agent-minutes budget.
func TestInterruptRuns_AgentSecondsFromStartedAtToNow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	startedAt := time.Now().Add(-30 * time.Second)
	if err = s.RecordRunStart(ctx, reserved.RunID, 100, "1.000", startedAt, ""); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	if _, err = s.InterruptRuns(ctx, ticketID, owner, expires); err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.AgentSeconds == nil || *run.AgentSeconds < 29 || *run.AgentSeconds > 35 {
		t.Errorf("run.AgentSeconds = %v, want roughly 30", run.AgentSeconds)
	}
}

// TestInterruptRuns_FallsBackToExistingAgentSecondsWithNoStartedAt proves
// the fallback branch (design section 5.3): a run with no started_at (the
// start handshake never ran, or RecordRunStart itself failed) keeps its
// existing agent_seconds, floored at 0 when that too is NULL.
func TestInterruptRuns_FallsBackToExistingAgentSecondsWithNoStartedAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err = s.InterruptRuns(ctx, ticketID, owner, expires); err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.AgentSeconds == nil || *run.AgentSeconds != 0 {
		t.Errorf("run.AgentSeconds = %v, want 0 (no started_at, no prior agent_seconds)", run.AgentSeconds)
	}
}

// TestInterruptRuns_KeepsExistingAgentSecondsWithNoStartedAt is
// TestInterruptRuns_FallsBackToExistingAgentSecondsWithNoStartedAt's own
// sibling for the other half of the same fallback (PR review fix E2): a
// run that already carries a non-NULL agent_seconds before it is
// interrupted, with no started_at to compute a fresh span from, keeps that
// value rather than having it replaced by the floor-at-0 case above. The
// fixture above never seeded agent_seconds (Reserve leaves it NULL), so it
// could only ever exercise the floor, never this preservation branch.
func TestInterruptRuns_KeepsExistingAgentSecondsWithNoStartedAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, seedErr := s.db.ExecContext(ctx, `UPDATE runs SET agent_seconds = 5 WHERE id = ?`, reserved.RunID); seedErr != nil {
		t.Fatalf("seed agent_seconds: %v", seedErr)
	}

	if _, err = s.InterruptRuns(ctx, ticketID, owner, expires); err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.AgentSeconds == nil || *run.AgentSeconds != 5 {
		t.Errorf("run.AgentSeconds = %v, want 5 (no started_at: the existing value survives)", run.AgentSeconds)
	}
}

// TestInterruptRuns_CapsAgentSecondsAtClaimExpiry proves design section
// 5.3's cap (PR review fix E1): a reclaim that happens long after the
// claim's own lease lapsed must never charge the ticket for how long the
// dead serve sat down, only for the time the lease actually covered -- the
// agent cannot legitimately run longer than started_at plus the lease.
func TestInterruptRuns_CapsAgentSecondsAtClaimExpiry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner := testOwner
	expires := time.Now().Add(-time.Hour).UTC().Truncate(time.Second) // the lease lapsed long ago
	claimed, err := s.Claim(ctx, ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	startedAt := expires.Add(-10 * time.Second) // ran for 10s inside its own lease
	if err = s.RecordRunStart(ctx, reserved.RunID, 100, "1.000", startedAt, ""); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	// "now" (real time.Now(), inside interruptClaimedRuns) is long after
	// expires: standing in for a replacement serve reclaiming hours after
	// the dead serve's lease lapsed. Without the cap this would charge
	// nearly an hour of agent_seconds for a run that only ever covered 10s
	// of its own lease.
	if _, err = s.InterruptRuns(ctx, ticketID, owner, expires); err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.AgentSeconds == nil || *run.AgentSeconds < 9 || *run.AgentSeconds > 11 {
		t.Errorf("run.AgentSeconds = %v, want roughly 10 (capped at the claim's own expiry, not charged for how long the lease sat lapsed)", run.AgentSeconds)
	}
}

// TestInterruptRuns_NoOpWhenFenceMismatch proves the fence (design section
// 5.3): a claim already moved on (wrong owner or expiry) makes InterruptRuns
// report applied=false with no error, and writes nothing.
func TestInterruptRuns_NoOpWhenFenceMismatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	applied, err := s.InterruptRuns(ctx, ticketID, testOwnerOther, expires)
	if err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}
	if applied {
		t.Fatal("InterruptRuns with the wrong owner: applied = true, want false")
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.Outcome != nil {
		t.Errorf("run.Outcome = %v, want still nil (fence mismatch writes nothing)", run.Outcome)
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner == nil || *got.ClaimOwner != owner {
		t.Errorf("ticket claim owner = %v, want unchanged %s", got.ClaimOwner, owner)
	}
}

// TestForeignClaims_ExcludesSelfOwnedClaims proves the self filter (design
// section 5.3, 6.3): a ticket claimed by self never appears, even though it
// is claimed.
func TestForeignClaims_ExcludesSelfOwnedClaims(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	if _, err := s.Claim(ctx, ticketID, "self-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	claims, err := s.ForeignClaims(ctx, "self-1")
	if err != nil {
		t.Fatalf("ForeignClaims: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("ForeignClaims(self=self-1) = %v, want empty (self-owned)", claims)
	}
}

// TestForeignClaims_ReturnsOtherOwnersWithOpenRuns proves the main path
// (design section 5.3, 6.3): a ticket claimed by another owner is returned
// with its open run's identity, in a shape reclaimForeign can walk.
func TestForeignClaims_ReturnsOtherOwnersWithOpenRuns(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := s.Claim(ctx, ticketID, "other-1", expires); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	reserved, err := s.Reserve(ctx, ticketID, "other-1", expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err = s.RecordRunStart(ctx, reserved.RunID, 5555, "99.000111", time.Now(), ""); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	claims, err := s.ForeignClaims(ctx, "self-1")
	if err != nil {
		t.Fatalf("ForeignClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("ForeignClaims = %v, want exactly one claim", claims)
	}
	claim := claims[0]
	if claim.TicketID != ticketID || claim.Owner != "other-1" {
		t.Errorf("claim = (ticket %d, owner %s), want (%d, other-1)", claim.TicketID, claim.Owner, ticketID)
	}
	if !claim.Expires.Equal(expires) {
		t.Errorf("claim.Expires = %v, want %v", claim.Expires, expires)
	}
	if len(claim.Open) != 1 {
		t.Fatalf("claim.Open = %v, want exactly one open run", claim.Open)
	}
	open := claim.Open[0]
	if open.RunID != reserved.RunID || open.Job != testStatePlanning {
		t.Errorf("open run = (id %d, job %s), want (%d, %s)", open.RunID, open.Job, reserved.RunID, testStatePlanning)
	}
	if open.PGID == nil || *open.PGID != 5555 {
		t.Errorf("open.PGID = %v, want 5555", open.PGID)
	}
	if open.ProcStart == nil || *open.ProcStart != "99.000111" {
		t.Errorf("open.ProcStart = %v, want 99.000111", open.ProcStart)
	}
	if open.StartedAt == nil {
		t.Error("open.StartedAt = nil, want set")
	}
}

// TestReclaimClaim_AppliesAndClearsForeignClaim proves ReclaimClaim shares
// interruptClaimedRuns with InterruptRuns (design section 5.3): fenced on
// the foreign owner and expiry, it terminalizes the open run and clears the
// claim exactly like InterruptRuns does for self.
func TestReclaimClaim_AppliesAndClearsForeignClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := s.Claim(ctx, ticketID, "other-1", expires); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	reserved, err := s.Reserve(ctx, ticketID, "other-1", expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	applied, err := s.ReclaimClaim(ctx, ticketID, "other-1", expires, nil)
	if err != nil {
		t.Fatalf("ReclaimClaim: %v", err)
	}
	if !applied {
		t.Fatal("ReclaimClaim: applied = false, want true")
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner != nil {
		t.Errorf("ticket claim owner = %v, want cleared", got.ClaimOwner)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if !run.Interrupted {
		t.Error("run.Interrupted = false, want true")
	}
}

// TestReclaimClaim_NoOpWhenClaimAlreadyGone proves the fence applies to
// ReclaimClaim too (design section 5.3): a ticket no longer claimed by the
// exact (owner, expires) ForeignClaims read returns applied=false, nil.
func TestReclaimClaim_NoOpWhenClaimAlreadyGone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	applied, err := s.ReclaimClaim(ctx, ticketID, "other-1", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("ReclaimClaim: %v", err)
	}
	if applied {
		t.Error("ReclaimClaim on an unclaimed ticket: applied = true, want false")
	}
}
