package store

import (
	"testing"
	"time"
)

// testOwner and testOwnerOther are the claim owners repeated across this
// file's fence tests. testWaitingQuestions, testStateBuilding, and
// testReasonPlanReady collect literals this file repeats often enough that
// goconst would otherwise ask for a constant (store_test.go's
// testTypeQuestion, testTypeState, and testStatePlanning, and commit.go's
// questionStateOpen and questionStateAnswered, cover the rest).
const (
	testOwner      = "host-1"
	testOwnerOther = "host-2"

	testWaitingQuestions = "questions"
	testStateBuilding    = "building"
	testReasonPlanReady  = "plan ready"
)

// claimForCommit claims ticketID for testOwner with a lease truncated to
// second precision (SQLite's TEXT timestamp round-trips at second
// precision, section 6.2's formatTime), so the returned expires compares
// equal to what a later GetTicket reads back. Tests reuse this same expires
// value as HandlerCommit.Expires, exactly as the dispatcher reuses the lease
// it claimed with (section 6.8).
func claimForCommit(t *testing.T, s *Store, ticketID int64) (owner string, expires time.Time) {
	t.Helper()
	expires = time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, claimErr := s.Claim(t.Context(), ticketID, testOwner, expires)
	if claimErr != nil {
		t.Fatalf("Claim: %v", claimErr)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}
	return testOwner, expires
}

// setWaitingQuestions sets ticketID's waiting_on directly to "questions",
// reaching past the spine helpers the way reads_test.go's setTicketWaiting
// does, for a fixture this file always arranges the same way.
func setWaitingQuestions(t *testing.T, s *Store, ticketID int64) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE tickets SET waiting_on = ? WHERE id = ?`, testWaitingQuestions, ticketID); err != nil {
		t.Fatalf("set ticket waiting_on: %v", err)
	}
}

// insertQuestionRun inserts a turn-0, outcome-question runs row directly,
// bypassing CommitHandlerResult, so a resume or answer test can arrange a
// pre-existing run for its questions to attach to.
func insertQuestionRun(t *testing.T, s *Store, sessionID int64) int64 {
	t.Helper()
	res, err := s.db.ExecContext(t.Context(),
		`INSERT INTO runs (session_id, turn, outcome) VALUES (?, 0, ?)`, sessionID, testTypeQuestion)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return id
}

// insertOpenQuestion inserts a "question" message directly (fixture setup,
// not the code under test), attached to runID, in the "open" lifecycle
// state, with a two-option payload (keys a and b).
func insertOpenQuestion(t *testing.T, s *Store, ticketID, runID int64, key string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, RunID: &runID, Type: testTypeQuestion, Author: testAuthorZing,
		State: new(questionStateOpen), Body: key, Payload: questionPayload(key),
	})
	if err != nil {
		t.Fatalf("insert open question %s: %v", key, err)
	}
	return id
}

// zeroOptionQuestionPayload is a QuestionPayload with no options: the
// free-text case AnswerQuestion must reject.
func zeroOptionQuestionPayload(key string) []byte {
	return []byte(`{"key":"` + key + `","kind":"question","state":"open","recommended":"free text","options":[]}`)
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count rows %q: %v", query, err)
	}
	return n
}

// --- CommitHandlerResult ----------------------------------------------------

func TestCommitHandlerResult_FirstEntryPlanningAppliesAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	externalID := "ext-1"

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Session: &SessionUpsert{Job: testStatePlanning, Runtime: "fake", ExternalID: &externalID},
		Runs:    []Run{{Turn: 0, Outcome: new(testTypeQuestion)}},
		Messages: []Message{{
			TicketID: ticketID, Type: testTypeQuestion, Author: testAuthorZing,
			State: new(questionStateOpen), Body: "Q1 heading\n\nbody", Payload: questionPayload("Q1"),
		}},
		AttachRunToMsgs: true,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStatePlanning {
		t.Errorf("ticket state = %q, want unchanged %q (Next was empty)", got.State, testStatePlanning)
	}
	if got.WaitingOn == nil || *got.WaitingOn != testWaitingQuestions {
		t.Errorf("ticket waiting_on = %v, want %s", got.WaitingOn, testWaitingQuestions)
	}
	if got.ClaimOwner != nil || got.ClaimExpiresAt != nil {
		t.Errorf("ticket claim = (%v, %v), want cleared", got.ClaimOwner, got.ClaimExpiresAt)
	}

	sess, ok, sessErr := s.OpenSession(ctx, ticketID, testStatePlanning)
	if sessErr != nil {
		t.Fatalf("OpenSession: %v", sessErr)
	}
	if !ok {
		t.Fatal("OpenSession: ok = false, want true (session inserted)")
	}
	if sess.ExternalID == nil || *sess.ExternalID != externalID {
		t.Errorf("session.ExternalID = %v, want %s", sess.ExternalID, externalID)
	}

	var runID int64
	var runTurn int
	var runOutcome string
	if scanErr := s.db.QueryRowContext(ctx, `SELECT id, turn, outcome FROM runs WHERE session_id = ?`, sess.ID).
		Scan(&runID, &runTurn, &runOutcome); scanErr != nil {
		t.Fatalf("read inserted run: %v", scanErr)
	}
	if runTurn != 0 || runOutcome != testTypeQuestion {
		t.Errorf("run = (turn %d, outcome %s), want (0, %s)", runTurn, runOutcome, testTypeQuestion)
	}

	open, qErr := s.QuestionsByState(ctx, ticketID, questionStateOpen)
	if qErr != nil {
		t.Fatalf("QuestionsByState: %v", qErr)
	}
	if len(open) != 1 {
		t.Fatalf("open questions = %d, want 1", len(open))
	}
	if open[0].RunID == nil || *open[0].RunID != runID {
		t.Errorf("question.RunID = %v, want %d (AttachRunToMsgs)", open[0].RunID, runID)
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ?`, ticketID, msgTypeState); n != 0 {
		t.Errorf("state messages = %d, want 0 (Next was empty, no transition)", n)
	}
}

// TestCommitHandlerResult_TruncatesExpiresLikeClaim proves the second-based
// truncation moved to the store boundary (section 6.3): a caller that hands
// the identical, sub-second-precision time.Time to both Claim and
// CommitHandlerResult, truncating neither itself, still gets a fence that
// matches, because each store method truncates its own incoming expires the
// same way.
func TestCommitHandlerResult_TruncatesExpiresLikeClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	rawExpires := time.Now().Add(10 * time.Minute).Add(123456789 * time.Nanosecond)

	claimed, err := s.Claim(ctx, ticketID, testOwner, rawExpires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: testOwner, Expires: rawExpires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult with the same untruncated expires Claim used: applied = false, want true")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStateBuilding {
		t.Errorf("ticket state = %q, want building", got.State)
	}
}

func TestCommitHandlerResult_StaleOwnerAppliesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	_, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: testOwnerOther, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if applied {
		t.Error("CommitHandlerResult with a stale owner: applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStatePlanning {
		t.Errorf("ticket state = %q, want unchanged planning", got.State)
	}
	if got.ClaimOwner == nil || *got.ClaimOwner != testOwner {
		t.Errorf("ticket claim owner = %v, want unchanged %s", got.ClaimOwner, testOwner)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("messages after a refused commit = %d, want 0", n)
	}
}

func TestCommitHandlerResult_ChangedExpiryAppliesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)
	differentExpires := expires.Add(time.Minute)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: differentExpires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if applied {
		t.Error("CommitHandlerResult with a changed expiry: applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStatePlanning {
		t.Errorf("ticket state = %q, want unchanged planning", got.State)
	}
	if got.ClaimExpiresAt == nil || !got.ClaimExpiresAt.Equal(expires) {
		t.Errorf("ticket claim expiry = %v, want unchanged %v", got.ClaimExpiresAt, expires)
	}
}

func TestCommitHandlerResult_AttachRunToMsgsRejectsNonSingleRun(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	externalID := "ext-1"
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{Job: testStatePlanning, Runtime: "fake", ExternalID: &externalID},
		Runs:    nil, // zero runs, AttachRunToMsgs still set
		Messages: []Message{{
			TicketID: ticketID, Type: testTypeQuestion, Author: testAuthorZing,
			State: new(questionStateOpen), Body: "Q1", Payload: questionPayload("Q1"),
		}},
		AttachRunToMsgs: true,
	})
	if err == nil {
		t.Error("CommitHandlerResult with AttachRunToMsgs and zero runs: want error, got nil")
	}
	if applied {
		t.Error("CommitHandlerResult with AttachRunToMsgs and zero runs: applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.ClaimOwner == nil {
		t.Error("ticket claim was cleared despite the rejected commit, want it held")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("messages after a rejected commit = %d, want 0", n)
	}
}

func TestCommitHandlerResult_ResumeClearsWaitAndTransitions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	setWaitingQuestions(t, s, ticketID)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	q1ID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")
	q2ID := insertOpenQuestion(t, s, ticketID, run0ID, "Q2")

	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Session:          &SessionUpsert{ID: &sessID, BumpResumes: true},
		Runs:             []Run{{Turn: 1, Outcome: new("ready")}},
		ResolveQuestions: []int64{q1ID, q2ID},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStateBuilding {
		t.Errorf("ticket state = %q, want %s", got.State, testStateBuilding)
	}
	if got.WaitingOn != nil {
		t.Errorf("ticket waiting_on = %v, want nil", *got.WaitingOn)
	}
	if got.ClaimOwner != nil {
		t.Error("ticket claim was not cleared")
	}

	var resumes int
	if scanErr := s.db.QueryRowContext(ctx, `SELECT resumes FROM sessions WHERE id = ?`, sessID).Scan(&resumes); scanErr != nil {
		t.Fatalf("read session resumes: %v", scanErr)
	}
	if resumes != 1 {
		t.Errorf("session.resumes = %d, want 1", resumes)
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM runs WHERE session_id = ? AND turn = 1`, sessID); n != 1 {
		t.Errorf("turn-1 runs for session = %d, want 1", n)
	}

	resolved, qErr := s.QuestionsByState(ctx, ticketID, "resolved")
	if qErr != nil {
		t.Fatalf("QuestionsByState(resolved): %v", qErr)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved questions = %d, want 2", len(resolved))
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ? AND parent_id = ?`, ticketID, msgTypeResolved, q1ID); n != 1 {
		t.Errorf("resolved messages for q1 = %d, want 1", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ? AND parent_id = ?`, ticketID, msgTypeResolved, q2ID); n != 1 {
		t.Errorf("resolved messages for q2 = %d, want 1", n)
	}

	var from, to, reason string
	stateErr := s.db.QueryRowContext(ctx,
		`SELECT json_extract(payload,'$.from'), json_extract(payload,'$.to'), json_extract(payload,'$.reason')
		 FROM messages WHERE ticket_id = ? AND type = ?`, ticketID, msgTypeState).Scan(&from, &to, &reason)
	if stateErr != nil {
		t.Fatalf("read state message: %v", stateErr)
	}
	if from != testStatePlanning || to != testStateBuilding || reason != testReasonPlanReady {
		t.Errorf("state message = (%s, %s, %s), want (%s, %s, %s)", from, to, reason, testStatePlanning, testStateBuilding, testReasonPlanReady)
	}
}

// TestCommitHandlerResult_RejectsSessionFromAnotherTicket proves every write
// in one commit is scoped to c.TicketID (section 6.3): a commit naming
// another ticket's session errors and writes nothing, rather than silently
// updating a session that belongs to a different ticket.
func TestCommitHandlerResult_RejectsSessionFromAnotherTicket(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketA, testStatePlanning)
	setTicketState(t, s, ticketB, testStatePlanning)
	sessA := insertSession(t, s, ticketA, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketB)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Session: &SessionUpsert{ID: &sessA, BumpResumes: true},
	})
	if err == nil {
		t.Error("CommitHandlerResult naming another ticket's session: want error, got nil")
	}
	if applied {
		t.Error("CommitHandlerResult naming another ticket's session: applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketB)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStatePlanning {
		t.Errorf("ticket B state = %q, want unchanged planning", got.State)
	}
	if got.ClaimOwner == nil {
		t.Error("ticket B claim was cleared despite the rejected commit, want it held")
	}

	var resumes int
	if scanErr := s.db.QueryRowContext(ctx, `SELECT resumes FROM sessions WHERE id = ?`, sessA).Scan(&resumes); scanErr != nil {
		t.Fatalf("read session A resumes: %v", scanErr)
	}
	if resumes != 0 {
		t.Errorf("session A resumes = %d, want unchanged 0 (the commit must not touch another ticket's session)", resumes)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketB); n != 0 {
		t.Errorf("ticket B messages after a rejected commit = %d, want 0", n)
	}
}

// TestCommitHandlerResult_RejectsResolveQuestionFromAnotherTicket proves the
// same scoping for ResolveQuestions (section 6.3): a commit that names
// another ticket's question id errors and writes nothing, including no
// partial resolution of the question it did not own.
func TestCommitHandlerResult_RejectsResolveQuestionFromAnotherTicket(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketA, testStatePlanning)
	setTicketState(t, s, ticketB, testStatePlanning)

	sessA := insertSession(t, s, ticketA, testStatePlanning)
	runA := insertQuestionRun(t, s, sessA)
	qA := insertOpenQuestion(t, s, ticketA, runA, "Q1")

	owner, expires := claimForCommit(t, s, ticketB)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		ResolveQuestions: []int64{qA},
	})
	if err == nil {
		t.Error("CommitHandlerResult naming another ticket's question: want error, got nil")
	}
	if applied {
		t.Error("CommitHandlerResult naming another ticket's question: applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketB)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStatePlanning {
		t.Errorf("ticket B state = %q, want unchanged planning", got.State)
	}

	qGot, getMsgErr := s.GetMessage(ctx, qA)
	if getMsgErr != nil {
		t.Fatalf("GetMessage(qA): %v", getMsgErr)
	}
	if qGot.State == nil || *qGot.State != questionStateOpen {
		t.Errorf("question A state = %v, want unchanged open (the commit must not resolve another ticket's question)", qGot.State)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketB); n != 0 {
		t.Errorf("ticket B messages after a rejected commit = %d, want 0", n)
	}
}

// TestCommitHandlerResult_ForcesMessageTicketID proves the commit boundary
// overrides a handler-supplied message TicketID rather than trusting it
// (section 6.3): every inserted message lands under c.TicketID regardless of
// what the commit's Messages carried.
func TestCommitHandlerResult_ForcesMessageTicketID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketB, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketB)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: owner, Expires: expires,
		Messages: []Message{{
			TicketID: ticketA, // a misbehaving handler naming the wrong ticket
			Type:     testTypeUpdate, Author: testAuthorZing, Body: "progress",
		}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	msgs, listErr := s.ListMessages(ctx, ticketB)
	if listErr != nil {
		t.Fatalf("ListMessages(ticketB): %v", listErr)
	}
	if len(msgs) != 1 {
		t.Fatalf("ticketB messages = %d, want 1 (forced to c.TicketID)", len(msgs))
	}
	if msgs[0].TicketID != ticketB {
		t.Errorf("message.TicketID = %d, want %d (forced, not the handler's %d)", msgs[0].TicketID, ticketB, ticketA)
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketA); n != 0 {
		t.Errorf("ticketA messages = %d, want 0 (the handler-supplied ticket id must not be trusted)", n)
	}
}

func TestCommitHandlerResult_CodeHandlerTransitionWritesStateMessage(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, "reviewing")
	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: "judging", Reason: "review clean",
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != "judging" {
		t.Errorf("ticket state = %q, want judging", got.State)
	}

	msgs, listErr := s.ListMessages(ctx, ticketID)
	if listErr != nil {
		t.Fatalf("ListMessages: %v", listErr)
	}
	if len(msgs) != 1 || msgs[0].Type != msgTypeState {
		t.Fatalf("messages = %+v, want exactly one state message", msgs)
	}
}

// --- AnswerQuestion ----------------------------------------------------------

func TestAnswerQuestion_AcceptsValidAnswerAndClearsWaitOnLastOfBatch(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	setWaitingQuestions(t, s, ticketID)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	q1ID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")
	q2ID := insertOpenQuestion(t, s, ticketID, run0ID, "Q2")

	res, err := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: q1ID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion(q1): %v", err)
	}
	if !res.Accepted {
		t.Fatalf("AnswerQuestion(q1): Accepted = false, Conflict = %q, want accepted", res.Conflict)
	}
	if res.WaitCleared {
		t.Error("AnswerQuestion(q1): WaitCleared = true, want false (q2 still open)")
	}

	q1, getErr := s.GetMessage(ctx, q1ID)
	if getErr != nil {
		t.Fatalf("GetMessage(q1): %v", getErr)
	}
	if q1.State == nil || *q1.State != questionStateAnswered {
		t.Errorf("q1.State = %v, want %s", q1.State, questionStateAnswered)
	}

	ticket, ticketErr := s.GetTicket(ctx, ticketID)
	if ticketErr != nil {
		t.Fatalf("GetTicket: %v", ticketErr)
	}
	if ticket.WaitingOn == nil || *ticket.WaitingOn != testWaitingQuestions {
		t.Errorf("ticket.WaitingOn after answering one of two = %v, want %s (still waiting)", ticket.WaitingOn, testWaitingQuestions)
	}

	res2, err2 := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: q2ID, Option: "b"})
	if err2 != nil {
		t.Fatalf("AnswerQuestion(q2): %v", err2)
	}
	if !res2.Accepted {
		t.Fatalf("AnswerQuestion(q2): Accepted = false, Conflict = %q, want accepted", res2.Conflict)
	}
	if !res2.WaitCleared {
		t.Error("AnswerQuestion(q2): WaitCleared = false, want true (batch complete)")
	}

	ticket, ticketErr = s.GetTicket(ctx, ticketID)
	if ticketErr != nil {
		t.Fatalf("GetTicket: %v", ticketErr)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn after the whole batch answered = %v, want nil", *ticket.WaitingOn)
	}

	msgs, listErr := s.ListMessages(ctx, ticketID)
	if listErr != nil {
		t.Fatalf("ListMessages: %v", listErr)
	}
	var answers int
	for _, m := range msgs {
		if m.Type == msgTypeAnswer {
			answers++
			if m.State == nil || *m.State != answerStateSent {
				t.Errorf("answer message state = %v, want %s", m.State, answerStateSent)
			}
		}
	}
	if answers != 2 {
		t.Errorf("answer messages = %d, want 2", answers)
	}
}

// TestAnswerQuestion_LeavesANonQuestionsWaitUntouched proves the wait-clear
// is conditional on waiting_on = 'questions' (section 6.3): answering the
// last open question of a batch never clears some other wait flag the
// ticket happens to carry, and WaitCleared reports that it did not clear
// anything.
func TestAnswerQuestion_LeavesANonQuestionsWaitUntouched(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	const otherWait = "split" // one of the eight waiting flags, not "questions"
	if _, err := s.db.ExecContext(ctx, `UPDATE tickets SET waiting_on = ? WHERE id = ?`, otherWait, ticketID); err != nil {
		t.Fatalf("set ticket waiting_on: %v", err)
	}

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")

	res, err := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", res.Conflict)
	}
	if res.WaitCleared {
		t.Error("AnswerQuestion with the ticket waiting on something else: WaitCleared = true, want false")
	}

	ticket, ticketErr := s.GetTicket(ctx, ticketID)
	if ticketErr != nil {
		t.Fatalf("GetTicket: %v", ticketErr)
	}
	if ticket.WaitingOn == nil || *ticket.WaitingOn != otherWait {
		t.Errorf("ticket.WaitingOn after the last question answered = %v, want unchanged %q", ticket.WaitingOn, otherWait)
	}
}

func TestAnswerQuestion_RejectsWrongTicket(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketA, testStatePlanning)

	sessID := insertSession(t, s, ticketA, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketA, runID, "Q1")

	res, err := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketB, QuestionID: qID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if res.Accepted {
		t.Error("AnswerQuestion against the wrong ticket: Accepted = true, want false")
	}
	if res.Conflict == "" {
		t.Error("AnswerQuestion against the wrong ticket: Conflict is empty, want a named conflict")
	}

	q, getErr := s.GetMessage(ctx, qID)
	if getErr != nil {
		t.Fatalf("GetMessage: %v", getErr)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state after a wrong-ticket answer = %v, want unchanged %s", q.State, questionStateOpen)
	}
}

func TestAnswerQuestion_RejectsMissingOption(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")

	res, err := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "z"})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if res.Accepted {
		t.Error("AnswerQuestion with an option not on the question: Accepted = true, want false")
	}
	if res.Conflict == "" {
		t.Error("AnswerQuestion with an unknown option: Conflict is empty, want a named conflict")
	}

	q, getErr := s.GetMessage(ctx, qID)
	if getErr != nil {
		t.Fatalf("GetMessage: %v", getErr)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state after a missing-option answer = %v, want unchanged %s", q.State, questionStateOpen)
	}
}

func TestAnswerQuestion_RejectsClosedQuestion(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET state = ? WHERE id = ?`, questionStateResolved, qID); err != nil {
		t.Fatalf("resolve question directly: %v", err)
	}

	res, err := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if res.Accepted {
		t.Error("AnswerQuestion against a resolved question: Accepted = true, want false")
	}
	if res.Conflict == "" {
		t.Error("AnswerQuestion against a resolved question: Conflict is empty, want a named conflict")
	}
}

func TestAnswerQuestion_IdempotentOnRepeat(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	setWaitingQuestions(t, s, ticketID)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")

	first, err := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"})
	if err != nil {
		t.Fatalf("first AnswerQuestion: %v", err)
	}
	if !first.Accepted {
		t.Fatalf("first AnswerQuestion: Accepted = false, Conflict = %q, want accepted", first.Conflict)
	}

	second, err2 := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"})
	if err2 != nil {
		t.Fatalf("second AnswerQuestion: %v", err2)
	}
	if second.Accepted {
		t.Error("repeat AnswerQuestion: Accepted = true, want false")
	}
	if second.Conflict != "already answered" {
		t.Errorf("repeat AnswerQuestion: Conflict = %q, want %q", second.Conflict, "already answered")
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ?`, ticketID, msgTypeAnswer); n != 1 {
		t.Errorf("answer messages after a repeat send = %d, want 1 (no duplicate write)", n)
	}
}

func TestAnswerQuestion_RejectsZeroOptionQuestion(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID, insErr := s.InsertMessage(ctx, Message{
		TicketID: ticketID, RunID: &runID, Type: testTypeQuestion, Author: testAuthorZing,
		State: new(questionStateOpen), Body: "Q1", Payload: zeroOptionQuestionPayload("Q1"),
	})
	if insErr != nil {
		t.Fatalf("insert zero-option question: %v", insErr)
	}

	res, err := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if res.Accepted {
		t.Error("AnswerQuestion against a zero-option question: Accepted = true, want false")
	}
	if res.Conflict != "free-text not supported" {
		t.Errorf("Conflict = %q, want %q", res.Conflict, "free-text not supported")
	}
}
