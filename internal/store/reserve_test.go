package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// reserveInput builds the (owner, expires) pair claimForCommit already
// establishes on ticketID, so a Reserve test's fence argument matches the
// live claim exactly (design section 4.5: Reserve's fence is the same
// exact-lease check CommitHandlerResult's final UPDATE makes).
func reserveInput(t *testing.T, s *Store, ticketID int64) (owner string, expires time.Time) {
	t.Helper()
	return claimForCommit(t, s, ticketID)
}

// TestReserve_TwoReservesOnOneSessionYieldSequentialTurns proves the turn
// computation (design section 4.5: COALESCE(MAX(turn), -1) + 1): a session's
// first reservation gets turn 0, and a second reservation against the same
// session id gets turn 1.
func TestReserve_TwoReservesOnOneSessionYieldSequentialTurns(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := reserveInput(t, s, ticketID)

	first, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	if first.Turn != 0 {
		t.Errorf("first Reserve turn = %d, want 0", first.Turn)
	}

	second, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{ID: &first.SessionID}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("second Reserve: %v", err)
	}
	if second.SessionID != first.SessionID {
		t.Errorf("second Reserve session = %d, want the same session %d", second.SessionID, first.SessionID)
	}
	if second.Turn != 1 {
		t.Errorf("second Reserve turn = %d, want 1", second.Turn)
	}
	if second.RunID == first.RunID {
		t.Error("second Reserve run id = first run id, want a distinct row")
	}
}

// TestReserve_WrongOwnerReturnsErrClaimLostAndWritesNothing proves the fence
// (design section 4.5): a Reserve whose owner does not match the ticket's
// live claim_owner returns ErrClaimLost and leaves no session or run row
// behind, even though the ticket really is claimed (just not by this owner).
func TestReserve_WrongOwnerReturnsErrClaimLostAndWritesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	_, expires := reserveInput(t, s, ticketID)

	_, err := s.Reserve(ctx, ticketID, testOwnerOther, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Reserve with the wrong owner: err = %v, want ErrClaimLost", err)
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM sessions WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("sessions after a lost-claim Reserve = %d, want 0", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM runs`); n != 0 {
		t.Errorf("runs after a lost-claim Reserve = %d, want 0", n)
	}
}

// TestReserve_WrongExpiryReturnsErrClaimLost proves the fence checks the
// exact expires, not just the owner: a Reserve call whose expires no longer
// matches the ticket's live claim_expires_at (for example, a stale lease
// after a renewal) also returns ErrClaimLost.
func TestReserve_WrongExpiryReturnsErrClaimLost(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := reserveInput(t, s, ticketID)

	staleExpires := expires.Add(-time.Minute)
	_, err := s.Reserve(ctx, ticketID, owner, staleExpires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Reserve with a stale expires: err = %v, want ErrClaimLost", err)
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM sessions WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("sessions after a lost-claim Reserve = %d, want 0", n)
	}
}

// TestReserve_NilSessionIDCreatesSessionWithNullExternalID proves the
// su.ID == nil path (design section 4.5): Reserve inserts a fresh session
// row whose external_id is NULL, leaving SessionUpsert's own commit to fill
// it in once the runtime call returns one.
func TestReserve_NilSessionIDCreatesSessionWithNullExternalID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := reserveInput(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	var gotJob, gotRuntime string
	var externalID sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT job, runtime, external_id FROM sessions WHERE id = ?`, reserved.SessionID).
		Scan(&gotJob, &gotRuntime, &externalID); err != nil {
		t.Fatalf("read inserted session: %v", err)
	}
	if gotJob != testStatePlanning || gotRuntime != testRuntimeFake {
		t.Errorf("session (job, runtime) = (%s, %s), want (%s, fake)", gotJob, gotRuntime, testStatePlanning)
	}
	if externalID.Valid {
		t.Errorf("session external_id = %q, want NULL", externalID.String)
	}
}

// TestReserve_SessionIDForAnotherTicketErrors proves Reserve verifies
// ownership before reusing an existing session id (design section 4.5, the
// same verifySessionForTicket check CommitHandlerResult's resume path
// makes): a su.ID that names a real session, but on a different ticket,
// errors rather than reserving a run under it.
func TestReserve_SessionIDForAnotherTicketErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketB, testStatePlanning)
	sessionOnA := insertSession(t, s, ticketA, testStatePlanning)

	owner, expires := reserveInput(t, s, ticketB)

	_, err := s.Reserve(ctx, ticketB, owner, expires, SessionUpsert{ID: &sessionOnA}, RunSeed{Model: testModelClaudeX})
	if err == nil {
		t.Fatal("Reserve with another ticket's session id: want an error, got nil")
	}
	if errors.Is(err, ErrClaimLost) {
		t.Errorf("Reserve with another ticket's session id: err = %v, want a plain ownership error, not ErrClaimLost", err)
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM runs WHERE session_id = ?`, sessionOnA); n != 0 {
		t.Errorf("runs on session %d after a rejected Reserve = %d, want 0", sessionOnA, n)
	}
}

// TestReserve_InsertsRunWithNullOutcomeExitCodeAndSetModel proves the run row
// Reserve inserts is the placeholder design section 4.5 describes: outcome,
// exit_code, and agent_seconds all NULL until something terminalizes it, with
// only model set at reserve time.
func TestReserve_InsertsRunWithNullOutcomeExitCodeAndSetModel(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := reserveInput(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelOpus48})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want the reserved run")
	}
	if run.ID != reserved.RunID {
		t.Errorf("FirstRun.ID = %d, want the reserved run id %d", run.ID, reserved.RunID)
	}
	if run.Outcome != nil {
		t.Errorf("run.Outcome = %v, want nil", run.Outcome)
	}
	if run.ExitCode != nil {
		t.Errorf("run.ExitCode = %v, want nil", run.ExitCode)
	}
	if run.AgentSeconds != nil {
		t.Errorf("run.AgentSeconds = %v, want nil", run.AgentSeconds)
	}
	if run.Model == nil || *run.Model != testModelOpus48 {
		t.Errorf("run.Model = %v, want claude-opus-4-8", run.Model)
	}
}

// sessionResumes reads sessions.resumes directly: the column Reserve now
// writes through when su.ID != nil && su.BumpResumes (design section 4.2),
// and CommitHandlerResult's upsertSessionTx still writes through for a
// commit that reserved nothing.
func sessionResumes(t *testing.T, s *Store, sessionID int64) int {
	t.Helper()
	var resumes int
	if err := s.db.QueryRowContext(t.Context(), `SELECT resumes FROM sessions WHERE id = ?`, sessionID).Scan(&resumes); err != nil {
		t.Fatalf("read resumes for session %d: %v", sessionID, err)
	}
	return resumes
}

// TestReserveWritesTaskN proves RunSeed.TaskN lands on the reserved run's
// own task_n column (design section 4.2): nil for every job but a build or
// perimeter task unit, and the exact task number when one is given.
func TestReserveWritesTaskN(t *testing.T) {
	s := newTestStore(t)

	t.Run("nil for a non-build job", func(t *testing.T) {
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := reserveInput(t, s, ticketID)

		reserved, err := s.Reserve(ctx, ticketID, owner, expires,
			SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		run, ok, err := s.FirstRun(ctx, reserved.SessionID)
		if err != nil || !ok {
			t.Fatalf("FirstRun: ok=%v err=%v", ok, err)
		}
		if run.TaskN != nil {
			t.Errorf("run.TaskN = %v, want nil", run.TaskN)
		}
	})

	t.Run("set for a build task unit", func(t *testing.T) {
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "2")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := reserveInput(t, s, ticketID)

		n := 2
		reserved, err := s.Reserve(ctx, ticketID, owner, expires,
			SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &n})
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		run, ok, err := s.FirstRun(ctx, reserved.SessionID)
		if err != nil || !ok {
			t.Fatalf("FirstRun: ok=%v err=%v", ok, err)
		}
		if run.TaskN == nil || *run.TaskN != 2 {
			t.Errorf("run.TaskN = %v, want 2", run.TaskN)
		}
	})
}

// TestReserveChargesResume proves Reserve itself charges a resume (design
// section 4.2): a first turn (su.ID nil) never bumps resumes, and a
// reserved resume (su.ID set, BumpResumes true) raises it by exactly one.
func TestReserveChargesResume(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := reserveInput(t, s, ticketID)

	first, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	if got := sessionResumes(t, s, first.SessionID); got != 0 {
		t.Errorf("resumes after a first turn = %d, want 0", got)
	}

	if _, err = s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{ID: &first.SessionID, BumpResumes: true}, RunSeed{Model: testModelClaudeX}); err != nil {
		t.Fatalf("resume Reserve: %v", err)
	}
	if got := sessionResumes(t, s, first.SessionID); got != 1 {
		t.Errorf("resumes after a reserved resume = %d, want 1", got)
	}
}

// TestInterruptedResumeStaysCharged proves the point of moving the charge
// to Reserve (design section 4.2): once a resume is reserved, its charge
// survives even when no terminalizing commit ever runs -- here simulated by
// ExpireClaims's reconcile, the same path a crash or a shutdown takes
// (design section 14).
func TestInterruptedResumeStaysCharged(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := reserveInput(t, s, ticketID)

	first, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	if _, err = s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{ID: &first.SessionID, BumpResumes: true}, RunSeed{Model: testModelClaudeX}); err != nil {
		t.Fatalf("resume Reserve: %v", err)
	}

	if _, err := s.ExpireClaims(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}

	if got := sessionResumes(t, s, first.SessionID); got != 1 {
		t.Errorf("resumes after an interrupted resume = %d, want 1 (still charged)", got)
	}
}

// TestCommittedResumeChargedOnce proves the charge lands exactly once when
// the resume does complete and terminalize normally (design section 4.2):
// Reserve charges it, and resumeSessionRecord no longer sets BumpResumes on
// the terminalizing commit, so CommitHandlerResult does not charge it
// again.
func TestCommittedResumeChargedOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := reserveInput(t, s, ticketID)

	first, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	resumed, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{ID: &first.SessionID, BumpResumes: true}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("resume Reserve: %v", err)
	}

	outcome := "ready"
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs:    []Run{{ID: resumed.RunID, Outcome: &outcome}},
		Session: &SessionUpsert{ID: &first.SessionID}, // resumeSessionRecord's own shape: no BumpResumes
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	if got := sessionResumes(t, s, first.SessionID); got != 1 {
		t.Errorf("resumes after reserve then commit = %d, want 1 (charged exactly once)", got)
	}
}
