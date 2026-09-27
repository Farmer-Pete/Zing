package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
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
	testStateDone        = "done"
	testReasonPlanReady  = "plan ready"
	testOutcomeBug       = "bug"
	testTypePlan         = "plan"
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

// markAnswered sets each of ids directly to the "answered" lifecycle state,
// arranging the fixture ResolveQuestions expects in real use (only an
// answered question's id ever reaches it) without going through the full
// AnswerQuestion flow.
func markAnswered(t *testing.T, s *Store, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.db.ExecContext(t.Context(),
			`UPDATE messages SET state = ? WHERE id = ?`, questionStateAnswered, id); err != nil {
			t.Fatalf("mark question %d answered: %v", id, err)
		}
	}
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

// --- scanMessage: nullable body ---------------------------------------------

// TestGetMessage_NullBodyReadsBackAsEmptyString proves scanMessage (rows.go)
// scans messages.body through a sql.NullString: the column is nullable
// (migrations/0001_init.sql), but InsertMessage always binds a Go string
// (never NULL), so a NULL body is only reachable through a raw insert, the
// same way this test arranges it. Before the fix, scanning straight into
// row.Body (a string) failed on a NULL row.
func TestGetMessage_NullBodyReadsBackAsEmptyString(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (ticket_id, type, author, body) VALUES (?, ?, ?, NULL)`,
		ticketID, testTypeUpdate, testAuthorZing)
	if err != nil {
		t.Fatalf("insert message with NULL body: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert message with NULL body: %v", err)
	}

	got, err := s.GetMessage(ctx, id)
	if err != nil {
		t.Fatalf("GetMessage with a NULL body: %v", err)
	}
	if got.Body != "" {
		t.Errorf("Body = %q, want \"\" for a NULL body column", got.Body)
	}
}

// --- CommitHandlerResult ----------------------------------------------------

func TestCommitHandlerResult_FirstEntryPlanningAppliesAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	externalID := testExternalID1

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Session: &SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake, ExternalID: &externalID},
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

	externalID := testExternalID1
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake, ExternalID: &externalID},
		Runs:    nil, // zero runs, AttachRunToMsgs still set
		Messages: []Message{{
			TicketID: ticketID, Type: testTypeQuestion, Author: testAuthorZing,
			State: new(questionStateOpen), Body: "Q1", Payload: questionPayload("Q1"),
		}},
		AttachRunToMsgs: true,
	})
	if err == nil || !strings.Contains(err.Error(), "attach needs exactly one run") {
		t.Errorf("CommitHandlerResult with AttachRunToMsgs and zero runs: err = %v, want containing %q", err, "attach needs exactly one run")
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
	// ResolveQuestions only ever resolves an answered question in real use
	// (skeleton.go's planningResume reads QuestionsByRun(..., answered)), and
	// resolveQuestionTx now enforces that at the store boundary too, so this
	// fixture answers both before the commit resolves them.
	markAnswered(t, s, q1ID, q2ID)

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

// TestCommitHandlerResult_SessionUpsertFillsNullExternalID proves the
// SessionUpsert extension (design section 4.5): a resume commit whose
// ExternalID is non-nil fills a session's still-null external_id -- the
// D13 case where Reserve created the session before the runtime call ran,
// and this terminalizing commit is the first to learn the runtime's session
// id -- and BumpResumes still increments alongside it.
func TestCommitHandlerResult_SessionUpsertFillsNullExternalID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sessID := insertSession(t, s, ticketID, testStatePlanning) // external_id NULL

	owner, expires := claimForCommit(t, s, ticketID)
	externalID := testExternalID1

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &sessID, ExternalID: &externalID, BumpResumes: true},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	var gotExternal sql.NullString
	var gotResumes int
	if scanErr := s.db.QueryRowContext(ctx, `SELECT external_id, resumes FROM sessions WHERE id = ?`, sessID).
		Scan(&gotExternal, &gotResumes); scanErr != nil {
		t.Fatalf("read session: %v", scanErr)
	}
	if !gotExternal.Valid || gotExternal.String != externalID {
		t.Errorf("session.external_id = %v, want %s", gotExternal, externalID)
	}
	if gotResumes != 1 {
		t.Errorf("session.resumes = %d, want 1 (BumpResumes)", gotResumes)
	}
}

// TestCommitHandlerResult_SessionUpsertDoesNotOverwriteSetExternalID proves
// the UPDATE ... WHERE external_id IS NULL guard: a commit's ExternalID
// never overwrites a session that already has one, so a later terminalizing
// commit on the same session (a second resume, say) cannot clobber the id
// the runtime returned on the first turn.
func TestCommitHandlerResult_SessionUpsertDoesNotOverwriteSetExternalID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sessID := insertSession(t, s, ticketID, testStatePlanning)
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET external_id = ? WHERE id = ?`, "ext-old", sessID); err != nil {
		t.Fatalf("seed external_id: %v", err)
	}

	owner, expires := claimForCommit(t, s, ticketID)
	newExternal := "ext-new"

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &sessID, ExternalID: &newExternal},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	var got string
	if scanErr := s.db.QueryRowContext(ctx, `SELECT external_id FROM sessions WHERE id = ?`, sessID).Scan(&got); scanErr != nil {
		t.Fatalf("read session: %v", scanErr)
	}
	if got != "ext-old" {
		t.Errorf("session.external_id = %q, want unchanged ext-old", got)
	}
}

// TestCommitHandlerResult_RejectsEmptyExternalID proves F035's write-side
// guard: a commit whose SessionUpsert.ExternalID points at "" fails the
// whole commit rather than storing an empty external_id (which is not a
// valid runtime id and not "no id yet" -- that is NULL).
func TestCommitHandlerResult_RejectsEmptyExternalID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	empty := ""
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake, ExternalID: &empty},
	})
	if err == nil {
		t.Error("CommitHandlerResult with ExternalID = \"\": want error, got nil")
	}
	if applied {
		t.Error("CommitHandlerResult with ExternalID = \"\": applied = true, want false")
	}

	sessions, sessErr := s.SessionsForTicket(ctx, ticketID)
	if sessErr != nil {
		t.Fatalf("SessionsForTicket: %v", sessErr)
	}
	if len(sessions) != 0 {
		t.Errorf("sessions for ticket %d = %v, want none (the whole commit must roll back)", ticketID, sessions)
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

// TestCommitHandlerResult_ResolveQuestionRejectsStillOpenQuestion proves
// resolveQuestionTx's state guard (section 6.3): a commit that tries to
// resolve a question still in the "open" state (never answered) errors, and
// the whole commit -- including its state transition and message -- rolls
// back rather than partially applying.
func TestCommitHandlerResult_ResolveQuestionRejectsStillOpenQuestion(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1") // left open: never answered

	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		ResolveQuestions: []int64{qID},
	})
	if err == nil {
		t.Error("CommitHandlerResult resolving a still-open question: want error, got nil")
	}
	if applied {
		t.Error("CommitHandlerResult resolving a still-open question: applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStatePlanning {
		t.Errorf("ticket state = %q, want unchanged planning (the whole commit rolled back)", got.State)
	}
	if got.ClaimOwner == nil {
		t.Error("ticket claim was cleared despite the rejected commit, want it held")
	}

	q, getMsgErr := s.GetMessage(ctx, qID)
	if getMsgErr != nil {
		t.Fatalf("GetMessage(q): %v", getMsgErr)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state = %v, want unchanged open (never resolved)", q.State)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ?`, ticketID, msgTypeState); n != 0 {
		t.Errorf("state messages after a rejected commit = %d, want 0 (the rejected transition wrote no state message)", n)
	}
}

// TestCommitHandlerResult_RejectsMessageParentFromAnotherTicket proves every
// inserted message's ParentID is scoped to c.TicketID (section 6.3): a
// commit whose message links onto another ticket's message errors and writes
// nothing.
func TestCommitHandlerResult_RejectsMessageParentFromAnotherTicket(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketB, testStatePlanning)

	otherMsgID, insErr := s.InsertMessage(ctx, Message{
		TicketID: ticketA, Type: testTypeUpdate, Author: testAuthorZing, Body: "on ticket A",
	})
	if insErr != nil {
		t.Fatalf("insert message on ticket A: %v", insErr)
	}

	owner, expires := claimForCommit(t, s, ticketB)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: owner, Expires: expires,
		Messages: []Message{{
			TicketID: ticketB, ParentID: &otherMsgID, Type: testTypeUpdate, Author: testAuthorZing, Body: "on ticket B",
		}},
	})
	if err == nil {
		t.Error("CommitHandlerResult with a message parented on another ticket: want error, got nil")
	}
	if applied {
		t.Error("CommitHandlerResult with a message parented on another ticket: applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketB)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.ClaimOwner == nil {
		t.Error("ticket B claim was cleared despite the rejected commit, want it held")
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

// --- CommitHandlerResult: Runs (ID > 0 updates, PKG7-PLAN.md section 4.5) ---

// scenarioPayload is a minimal, schema-valid "scenario" artifact payload
// (schemas/artifacts/scenario.json), used by every Artifacts test below that
// does not care about a whole-document type's version collisions.
func scenarioPayload(id string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"kind":"behavior","check_cmd":"go test ./...","given":"g","when":"w","then":"t"}`, id))
}

// planPayload is the checked-in "plan" artifact example
// (examples/artifacts/plan.json) verbatim: a whole-document type complex
// enough that hand-writing a second minimal valid payload is not worth it,
// so every version-collision test below reuses this one.
func planPayload() []byte {
	return []byte(`{
  "overview": {
    "objective": "Stop checkout from crashing on an empty cart.",
    "context": "internal/cart handles cart state; internal/checkout reads it at payment time.",
    "problem": {
      "text": "checkout panics when cart.Items is nil instead of an empty slice.",
      "loop": {
        "cmd": "go test ./internal/cart/... -run TestEmptyCart",
        "text": "fails: nil pointer dereference in checkout.Total"
      },
      "repro": "create a cart, call Checkout without adding items",
      "hypotheses": [
        {
          "rank": 1,
          "cause": "NewCart never initializes Items",
          "prediction": "initializing Items to []Item{} makes the loop pass"
        }
      ]
    },
    "goals": ["checkout never panics on an empty cart"],
    "nongoals": ["changing the checkout API"]
  },
  "design": {
    "demo": {
      "cmd": "go run ./cmd/demo -empty-cart",
      "text": "an empty cart checks out for zero dollars instead of crashing"
    },
    "shape": "NewCart initializes Items to an empty slice; checkout reads it unchanged.",
    "changes": [
      {
        "path": "internal/cart/cart.go",
        "symbol": "NewCart",
        "kind": "modified",
        "callers": "checkout.New",
        "callees": "none",
        "before": "Items field left at its zero value (nil)",
        "after": "Items: make([]Item, 0)"
      }
    ],
    "types": [],
    "migrations": { "migrations": [] }
  },
  "delivery": {
    "files": [
      { "path": "internal/cart/cart.go", "action": "modify", "reason": "initialize Items to an empty slice" }
    ],
    "deletions": { "deletions": [] },
    "tests": [
      {
        "name": "TestEmptyCart_ReturnsEmptyOrder",
        "seam": "cart.NewCart",
        "kind": "regression",
        "mocks": "",
        "asserts": "checkout of a freshly created cart returns a zero-item order, no panic"
      }
    ],
    "tasks": [
      { "n": 1, "test": "TestEmptyCart_ReturnsEmptyOrder", "demo": true, "text": "Initialize cart.Items to an empty slice in NewCart." }
    ]
  },
  "review": {
    "trust_root": "none",
    "alternatives": ["guard checkout.Total with a nil check instead of fixing the source"],
    "risks": ["other constructors that build a Cart by struct literal still skip this initializer"]
  }
}`)
}

// TestCommitHandlerResult_RunUpdateWritesOutcomeAndLeavesModelUntouched
// proves the Run.ID > 0 branch (design section 4.5): it writes outcome,
// exit_code, and agent_seconds on the run Reserve already placed, and never
// touches model, which Reserve set at reserve time.
func TestCommitHandlerResult_RunUpdateWritesOutcomeAndLeavesModelUntouched(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-opus-4-8")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []Run{{ID: reserved.RunID, Outcome: new(testOutcomeBug), ExitCode: new(0), AgentSeconds: new(5)}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	var outcome, model string
	var exitCode, agentSeconds int
	if scanErr := s.db.QueryRowContext(ctx,
		`SELECT outcome, exit_code, agent_seconds, model FROM runs WHERE id = ?`, reserved.RunID).
		Scan(&outcome, &exitCode, &agentSeconds, &model); scanErr != nil {
		t.Fatalf("read updated run: %v", scanErr)
	}
	if outcome != testOutcomeBug || exitCode != 0 || agentSeconds != 5 {
		t.Errorf("run = (outcome %s, exit %d, seconds %d), want (bug, 0, 5)", outcome, exitCode, agentSeconds)
	}
	if model != "claude-opus-4-8" {
		t.Errorf("run.model = %q, want unchanged claude-opus-4-8 (Reserve set it; an update must never touch it)", model)
	}
}

// TestCommitHandlerResult_RunUpdateRejectsRunFromAnotherTicketsSession proves
// the ownership subquery (design section 4.5): a Run.ID that belongs to a
// session on a different ticket is rejected with the exact text, and the
// run's own row is left untouched.
func TestCommitHandlerResult_RunUpdateRejectsRunFromAnotherTicketsSession(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketA, testStatePlanning)
	setTicketState(t, s, ticketB, testStatePlanning)

	ownerA, expiresA := claimForCommit(t, s, ticketA)
	reserved, err := s.Reserve(ctx, ticketA, ownerA, expiresA,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-x")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	ownerB, expiresB := claimForCommit(t, s, ticketB)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: ownerB, Expires: expiresB,
		Runs: []Run{{ID: reserved.RunID, Outcome: new(testOutcomeBug)}},
	})
	wantErr := fmt.Sprintf("run %d not owned by ticket %d", reserved.RunID, ticketB)
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want containing %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}

	var outcome sql.NullString
	if scanErr := s.db.QueryRowContext(ctx, `SELECT outcome FROM runs WHERE id = ?`, reserved.RunID).Scan(&outcome); scanErr != nil {
		t.Fatalf("read run: %v", scanErr)
	}
	if outcome.Valid {
		t.Errorf("run outcome = %v, want unchanged NULL (the commit must not touch another ticket's run)", outcome)
	}
}

// --- CommitHandlerResult: AttachRunToMsgs -----------------------------------

// TestCommitHandlerResult_AttachRunToMsgsAttachesUpdatedRunID proves the
// AttachRunToMsgs extension (design section 4.5): the attach id comes from a
// Run.ID > 0 update just as it would from a Run.ID == 0 insert.
func TestCommitHandlerResult_AttachRunToMsgsAttachesUpdatedRunID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-x")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Runs:    []Run{{ID: reserved.RunID, Outcome: new(testTypeQuestion)}},
		Messages: []Message{{
			TicketID: ticketID, Type: testTypeQuestion, Author: testAuthorZing,
			State: new(questionStateOpen), Body: "Q1", Payload: questionPayload("Q1"),
		}},
		AttachRunToMsgs: true,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	open, qErr := s.QuestionsByState(ctx, ticketID, questionStateOpen)
	if qErr != nil {
		t.Fatalf("QuestionsByState: %v", qErr)
	}
	if len(open) != 1 {
		t.Fatalf("open questions = %d, want 1", len(open))
	}
	if open[0].RunID == nil || *open[0].RunID != reserved.RunID {
		t.Errorf("question.RunID = %v, want the updated run id %d", open[0].RunID, reserved.RunID)
	}
}

// TestCommitHandlerResult_AttachRunToMsgsRejectsTwoRuns is
// AttachRunToMsgsRejectsNonSingleRun's sibling for the other non-single
// count: two Runs entries is rejected the same way zero is.
func TestCommitHandlerResult_AttachRunToMsgsRejectsTwoRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)
	externalID := testExternalID1

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session:         &SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake, ExternalID: &externalID},
		Runs:            []Run{{Turn: 0, Outcome: new(testOutcomeBug)}, {Turn: 1, Outcome: new(testOutcomeBug)}},
		AttachRunToMsgs: true,
	})
	if err == nil || !strings.Contains(err.Error(), "attach needs exactly one run") {
		t.Fatalf("err = %v, want containing %q", err, "attach needs exactly one run")
	}
	if applied {
		t.Error("applied = true, want false")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM runs`); n != 0 {
		t.Errorf("runs after a rejected commit = %d, want 0 (the whole commit rolled back)", n)
	}
}

// TestCommitHandlerResult_AttachRunToMsgsRejectsMessageRunIDZero proves a
// message whose RunID points at 0 is rejected rather than silently attached
// (design section 4.5): 0 is never a real run id, so a handler that sends
// one made a mistake worth failing loudly on.
func TestCommitHandlerResult_AttachRunToMsgsRejectsMessageRunIDZero(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-x")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	zero := int64(0)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []Run{{ID: reserved.RunID, Outcome: new(testTypeQuestion)}},
		Messages: []Message{{
			TicketID: ticketID, RunID: &zero, Type: testTypeQuestion, Author: testAuthorZing,
			State: new(questionStateOpen), Body: "Q1", Payload: questionPayload("Q1"),
		}},
		AttachRunToMsgs: true,
	})
	if err == nil || !strings.Contains(err.Error(), "message run id 0") {
		t.Fatalf("err = %v, want containing %q", err, "message run id 0")
	}
	if applied {
		t.Error("applied = true, want false")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("messages after a rejected commit = %d, want 0", n)
	}
}

// TestCommitHandlerResult_AttachRunToMsgsKeepsExplicitRunID proves a message
// that already carries a non-zero RunID is left alone even when
// AttachRunToMsgs is set: only a nil RunID is filled in.
func TestCommitHandlerResult_AttachRunToMsgsKeepsExplicitRunID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	priorSess := insertSession(t, s, ticketID, testStatePlanning)
	priorRunID := insertQuestionRun(t, s, priorSess)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-x")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []Run{{ID: reserved.RunID, Outcome: new(testTypeQuestion)}},
		Messages: []Message{
			{
				TicketID: ticketID, Type: testTypeQuestion, Author: testAuthorZing,
				State: new(questionStateOpen), Body: "Q1", Payload: questionPayload("Q1"),
			},
			{
				TicketID: ticketID, RunID: &priorRunID, Type: testTypeQuestion, Author: testAuthorZing,
				State: new(questionStateOpen), Body: "Q2", Payload: questionPayload("Q2"),
			},
		},
		AttachRunToMsgs: true,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	open, qErr := s.QuestionsByState(ctx, ticketID, questionStateOpen)
	if qErr != nil {
		t.Fatalf("QuestionsByState: %v", qErr)
	}
	if len(open) != 2 {
		t.Fatalf("open questions = %d, want 2", len(open))
	}
	var gotAttached, gotExplicit bool
	for _, m := range open {
		switch m.Body {
		case "Q1":
			gotAttached = true
			if m.RunID == nil || *m.RunID != reserved.RunID {
				t.Errorf("Q1.RunID = %v, want the attached run %d", m.RunID, reserved.RunID)
			}
		case "Q2":
			gotExplicit = true
			if m.RunID == nil || *m.RunID != priorRunID {
				t.Errorf("Q2.RunID = %v, want the explicit prior run %d, unchanged", m.RunID, priorRunID)
			}
		}
	}
	if !gotAttached || !gotExplicit {
		t.Fatalf("did not find both Q1 and Q2 among open questions: %+v", open)
	}
}

// --- CommitHandlerResult: SetKind -------------------------------------------

func TestCommitHandlerResult_SetKindNullToBugSucceeds(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		SetKind: new(testOutcomeBug),
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
	if got.Kind == nil || *got.Kind != testOutcomeBug {
		t.Errorf("ticket.Kind = %v, want bug", got.Kind)
	}
}

// TestCommitHandlerResult_SetKindSameToSameSucceeds proves "same-to-same
// succeeds" (design section 4.5): setting kind to the value it already holds
// is not a conflict, whether or not the driver reports it as zero rows
// affected.
func TestCommitHandlerResult_SetKindSameToSameSucceeds(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner1, expires1 := claimForCommit(t, s, ticketID)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner1, Expires: expires1, SetKind: new(testOutcomeBug),
	}); err != nil {
		t.Fatalf("seed SetKind(bug): %v", err)
	}

	owner2, expires2 := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner2, Expires: expires2, SetKind: new(testOutcomeBug),
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult(bug -> bug): %v, want nil", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.Kind == nil || *got.Kind != testOutcomeBug {
		t.Errorf("ticket.Kind = %v, want unchanged bug", got.Kind)
	}
}

func TestCommitHandlerResult_SetKindConflictErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner1, expires1 := claimForCommit(t, s, ticketID)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner1, Expires: expires1, SetKind: new(testOutcomeBug),
	}); err != nil {
		t.Fatalf("seed SetKind(bug): %v", err)
	}

	owner2, expires2 := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner2, Expires: expires2, SetKind: new("feature"),
	})
	wantErr := "kind conflict: have bug, want feature"
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want containing %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.Kind == nil || *got.Kind != testOutcomeBug {
		t.Errorf("ticket.Kind after a rejected conflicting SetKind = %v, want unchanged bug", got.Kind)
	}
}

// TestCommitHandlerResult_SetKindRejectsUnknownValueBeforeSQL proves an
// out-of-set kind value is rejected before it writes anything: paired with a
// message in the same commit, the whole commit rolls back rather than
// partially applying.
func TestCommitHandlerResult_SetKindRejectsUnknownValueBeforeSQL(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		SetKind: new("thing"),
		Messages: []Message{{
			TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress,
		}},
	})
	if err == nil {
		t.Error("CommitHandlerResult(SetKind=thing): want error, got nil")
	}
	if applied {
		t.Error("applied = true, want false")
	}

	got, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.Kind != nil {
		t.Errorf("ticket.Kind = %v, want unchanged nil", got.Kind)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("messages after a rejected commit = %d, want 0 (rolled back)", n)
	}
}

// --- CommitHandlerResult: Artifacts -----------------------------------------

// TestCommitHandlerResult_ArtifactTicketIDForced proves the same forcing
// rule commit.go already applies to Messages (design section 4.5): every
// inserted artifact lands under c.TicketID regardless of what the commit's
// Artifacts carried.
func TestCommitHandlerResult_ArtifactTicketIDForced(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketB, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketB)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: owner, Expires: expires,
		Artifacts: []Artifact{{TicketID: ticketA, Type: testTypeScenario, Payload: scenarioPayload("s1")}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	var gotTicketID int64
	if scanErr := s.db.QueryRowContext(ctx, `SELECT ticket_id FROM artifacts WHERE type = ?`, testTypeScenario).
		Scan(&gotTicketID); scanErr != nil {
		t.Fatalf("read artifact: %v", scanErr)
	}
	if gotTicketID != ticketB {
		t.Errorf("artifact.ticket_id = %d, want forced %d (not the handler's %d)", gotTicketID, ticketB, ticketA)
	}
}

// TestCommitHandlerResult_ArtifactRejectsRunFromAnotherTicket proves the
// same ownership subquery Runs updates use (design section 4.5): a non-nil
// Artifact.RunID that belongs to another ticket is rejected, and nothing is
// inserted.
func TestCommitHandlerResult_ArtifactRejectsRunFromAnotherTicket(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketA, testStatePlanning)
	setTicketState(t, s, ticketB, testStatePlanning)

	sessA := insertSession(t, s, ticketA, testStatePlanning)
	runA := insertQuestionRun(t, s, sessA)

	owner, expires := claimForCommit(t, s, ticketB)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: owner, Expires: expires,
		Artifacts: []Artifact{{RunID: &runA, Type: testTypeScenario, Payload: scenarioPayload("s1")}},
	})
	wantErr := fmt.Sprintf("artifact run %d not owned by ticket %d", runA, ticketB)
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want containing %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM artifacts`); n != 0 {
		t.Errorf("artifacts after a rejected commit = %d, want 0", n)
	}
}

// TestCommitHandlerResult_ArtifactZeroVersionAutoIncrements proves Version
// == 0 becomes one past (ticket, type)'s current maximum, computed inside
// the transaction, for a whole-document type ("plan").
func TestCommitHandlerResult_ArtifactZeroVersionAutoIncrements(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner1, expires1 := claimForCommit(t, s, ticketID)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner1, Expires: expires1,
		Artifacts: []Artifact{{Type: testTypePlan, Payload: planPayload()}},
	}); err != nil {
		t.Fatalf("first version-0 plan artifact: %v", err)
	}

	owner2, expires2 := claimForCommit(t, s, ticketID)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner2, Expires: expires2,
		Artifacts: []Artifact{{Type: testTypePlan, Payload: planPayload()}},
	}); err != nil {
		t.Fatalf("second version-0 plan artifact: %v", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT version FROM artifacts WHERE ticket_id = ? AND type = ? ORDER BY version`, ticketID, testTypePlan)
	if err != nil {
		t.Fatalf("query versions: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var v int
		if scanErr := rows.Scan(&v); scanErr != nil {
			t.Fatalf("scan version: %v", scanErr)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate versions: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("versions = %v, want [1 2]", got)
	}
}

// TestCommitHandlerResult_ArtifactExplicitVersionCollisionErrors proves
// Version > 0 is stored exactly, and a second insert at the same (ticket,
// type, version) collides against artifacts_whole_doc_uk with the exact
// text.
func TestCommitHandlerResult_ArtifactExplicitVersionCollisionErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner1, expires1 := claimForCommit(t, s, ticketID)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner1, Expires: expires1,
		Artifacts: []Artifact{{Type: testTypePlan, Version: 3, Payload: planPayload()}},
	}); err != nil {
		t.Fatalf("first version-3 plan artifact: %v", err)
	}

	var gotVersion int
	if scanErr := s.db.QueryRowContext(ctx, `SELECT version FROM artifacts WHERE ticket_id = ? AND type = ?`,
		ticketID, testTypePlan).Scan(&gotVersion); scanErr != nil {
		t.Fatalf("read version: %v", scanErr)
	}
	if gotVersion != 3 {
		t.Errorf("version = %d, want 3", gotVersion)
	}

	owner2, expires2 := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner2, Expires: expires2,
		Artifacts: []Artifact{{Type: testTypePlan, Version: 3, Payload: planPayload()}},
	})
	wantErr := "artifact plan version 3 exists"
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want containing %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM artifacts WHERE ticket_id = ? AND type = ?`, ticketID, testTypePlan); n != 1 {
		t.Errorf("plan artifacts = %d, want 1 (the collision must not insert a second row)", n)
	}
}

// TestCommitHandlerResult_ArtifactInvalidPayloadRollsBackWholeCommit proves
// insertArtifactTx still validates against the artifacts/<type> schema
// inside the transaction (design section 4.5: "do not bypass validation"),
// and a validation failure rolls the whole commit back, including an
// otherwise-valid message in the same commit.
func TestCommitHandlerResult_ArtifactInvalidPayloadRollsBackWholeCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Artifacts: []Artifact{{Type: testTypePlan, Payload: []byte(`{}`)}}, // missing every required field
		Messages: []Message{{
			TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress,
		}},
	})
	if err == nil {
		t.Error("CommitHandlerResult with an invalid artifact payload: want error, got nil")
	}
	if applied {
		t.Error("applied = true, want false")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM artifacts WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("artifacts after a rejected commit = %d, want 0", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("messages after a rejected commit = %d, want 0 (whole commit rolled back)", n)
	}
}

// --- CommitHandlerResult: ResolveAll -----------------------------------------

// TestCommitHandlerResult_ResolveAllResolvesOpenAndAnsweredQuestionsOnly
// proves ResolveAll's scope (design section 4.5, the abandon case): every
// "open" or "answered" question of the ticket becomes "resolved", an
// already-"resolved" question is untouched, and a non-question message is
// untouched.
func TestCommitHandlerResult_ResolveAllResolvesOpenAndAnsweredQuestionsOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qOpen := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	qAnswered := insertOpenQuestion(t, s, ticketID, runID, "Q2")
	markAnswered(t, s, qAnswered)
	qResolved := insertOpenQuestion(t, s, ticketID, runID, "Q3")
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET state = ? WHERE id = ?`, questionStateResolved, qResolved); err != nil {
		t.Fatalf("seed resolved question: %v", err)
	}
	otherMsgID, insErr := s.InsertMessage(ctx, Message{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress})
	if insErr != nil {
		t.Fatalf("insert non-question message: %v", insErr)
	}

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: "abandoned", Reason: "owner abandoned",
		ResolveAll: true,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	for _, id := range []int64{qOpen, qAnswered} {
		msg, getErr := s.GetMessage(ctx, id)
		if getErr != nil {
			t.Fatalf("GetMessage(%d): %v", id, getErr)
		}
		if msg.State == nil || *msg.State != questionStateResolved {
			t.Errorf("question %d state = %v, want resolved", id, msg.State)
		}
	}

	untouched, getErr := s.GetMessage(ctx, qResolved)
	if getErr != nil {
		t.Fatalf("GetMessage(qResolved): %v", getErr)
	}
	if untouched.State == nil || *untouched.State != questionStateResolved {
		t.Errorf("already-resolved question state = %v, want unchanged resolved", untouched.State)
	}

	other, getErr := s.GetMessage(ctx, otherMsgID)
	if getErr != nil {
		t.Fatalf("GetMessage(other): %v", getErr)
	}
	if other.State != nil {
		t.Errorf("non-question message state = %v, want unchanged nil", other.State)
	}
}

// --- CommitHandlerResult: Seal (design D16, section 4.5) --------------------

// insertRun inserts a bare runs row (outcome, exit_code, and agent_seconds
// all NULL) directly under sessionID, the run id a Seal fixture's plan and
// scenario artifacts attach to.
func insertRun(t *testing.T, s *Store, sessionID int64) int64 {
	t.Helper()
	res, err := s.db.ExecContext(t.Context(), `INSERT INTO runs (session_id, turn) VALUES (?, 0)`, sessionID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return id
}

// seedSealableCohort inserts one "plan" artifact at planVersion owned by
// runID, plus n "scenario" artifacts also owned by runID: the cohort a Seal
// test's SealRequest targets.
func seedSealableCohort(t *testing.T, s *Store, ticketID, runID int64, planVersion, n int) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.InsertArtifact(ctx, Artifact{
		TicketID: ticketID, RunID: &runID, Type: testTypePlan, Version: planVersion, Payload: planPayload(),
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	for i := range n {
		if _, err := s.InsertArtifact(ctx, Artifact{
			TicketID: ticketID, RunID: &runID, Type: testTypeScenario, Payload: scenarioPayload(fmt.Sprintf("s%d", i+1)),
		}); err != nil {
			t.Fatalf("seed scenario artifact %d: %v", i, err)
		}
	}
}

// assertSealMismatch asserts err is a *SealMismatchError with the given
// stage, expected, and affected, and that it also satisfies
// errors.Is(err, ErrSealMismatch) (design D16), so both the sentinel and the
// typed-error styles of check work against the same failure.
func assertSealMismatch(t *testing.T, err error, stage string, expected, affected int) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a *SealMismatchError")
	}
	if !errors.Is(err, ErrSealMismatch) {
		t.Errorf("errors.Is(err, ErrSealMismatch) = false, want true (err: %v)", err)
	}
	var mismatch *SealMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("errors.As(err, *SealMismatchError) = false, want true (err: %v)", err)
	}
	if mismatch.Stage != stage {
		t.Errorf("Stage = %q, want %q", mismatch.Stage, stage)
	}
	if mismatch.Expected != expected {
		t.Errorf("Expected = %d, want %d", mismatch.Expected, expected)
	}
	if mismatch.Affected != affected {
		t.Errorf("Affected = %d, want %d", mismatch.Affected, affected)
	}
}

// TestCommitHandlerResult_SealSealsExactlyTheCohortAndLeavesOthersUntouched
// proves the happy path (design D16): sealing one run's cohort stamps every
// one of its scenario rows with sealed_at and never touches another run's
// cohort on the same ticket.
func TestCommitHandlerResult_SealSealsExactlyTheCohortAndLeavesOthersUntouched(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sess := insertSession(t, s, ticketID, testStatePlanning)
	runA := insertRun(t, s, sess)
	runB := insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runA, 1, 3)
	seedSealableCohort(t, s, ticketID, runB, 2, 4)

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Seal: &SealRequest{RunID: runB, PlanVersion: 2, ExpectedCount: 4, At: time.Now()},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	sealedB := countRows(t, s,
		`SELECT COUNT(*) FROM artifacts WHERE ticket_id = ? AND type = ? AND run_id = ? AND sealed_at IS NOT NULL`,
		ticketID, testTypeScenario, runB)
	if sealedB != 4 {
		t.Errorf("sealed rows for the target cohort = %d, want 4", sealedB)
	}
	sealedA := countRows(t, s,
		`SELECT COUNT(*) FROM artifacts WHERE ticket_id = ? AND type = ? AND run_id = ? AND sealed_at IS NOT NULL`,
		ticketID, testTypeScenario, runA)
	if sealedA != 0 {
		t.Errorf("sealed rows for the other cohort = %d, want 0 (untouched)", sealedA)
	}
}

// TestCommitHandlerResult_SealPlanVersionMismatch proves a SealRequest whose
// PlanVersion does not match the ticket's max-version plan artifact fails at
// stage "plan" (design D16, section 4.5 check 1).
func TestCommitHandlerResult_SealPlanVersionMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runID, 1, 3)

	owner, expires := claimForCommit(t, s, ticketID)
	_, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Seal: &SealRequest{RunID: runID, PlanVersion: 2, ExpectedCount: 3, At: time.Now()},
	})
	assertSealMismatch(t, err, "plan", 3, 0)
}

// TestCommitHandlerResult_SealCohortCountOutOfRange proves a scenario cohort
// of 1 or 31 rows fails at stage "count" (design D16, section 4.5 check 2:
// the count must lie in [2,30]).
func TestCommitHandlerResult_SealCohortCountOutOfRange(t *testing.T) {
	tests := []struct {
		name string
		n    int
	}{
		{"one scenario is below the floor", 1},
		{"thirty-one scenarios is above the ceiling", 31},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			setTicketState(t, s, ticketID, testStatePlanning)
			sess := insertSession(t, s, ticketID, testStatePlanning)
			runID := insertRun(t, s, sess)
			seedSealableCohort(t, s, ticketID, runID, 1, tt.n)

			owner, expires := claimForCommit(t, s, ticketID)
			_, err := s.CommitHandlerResult(ctx, HandlerCommit{
				TicketID: ticketID, Owner: owner, Expires: expires,
				Seal: &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: tt.n, At: time.Now()},
			})
			assertSealMismatch(t, err, "count", tt.n, 0)
		})
	}
}

// TestCommitHandlerResult_SealPartiallySealedCohortStageUpdate proves a
// cohort with one row already sealed fails at stage "update" with Affected
// == ExpectedCount-1 (design D16, section 4.5 check 3): the UPDATE only
// touches rows still sealed_at IS NULL, so it affects one row fewer than
// ExpectedCount.
func TestCommitHandlerResult_SealPartiallySealedCohortStageUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runID, 1, 3)

	var oneID int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM artifacts WHERE ticket_id = ? AND type = ? AND run_id = ? LIMIT 1`,
		ticketID, testTypeScenario, runID).Scan(&oneID); err != nil {
		t.Fatalf("find one scenario artifact: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE artifacts SET sealed_at = ? WHERE id = ?`, formatTime(time.Now()), oneID); err != nil {
		t.Fatalf("pre-seal one scenario artifact: %v", err)
	}

	owner, expires := claimForCommit(t, s, ticketID)
	_, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Seal: &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 3, At: time.Now()},
	})
	assertSealMismatch(t, err, "update", 3, 2)
}

// TestCommitHandlerResult_SealMismatchRollsBackWholeCommit proves a seal
// mismatch rolls the whole commit back: the ticket's state is unchanged, its
// claim is still held (the fenced ticket UPDATE that would release it never
// ran), and an otherwise-valid message in the same commit was never inserted
// (design D16).
func TestCommitHandlerResult_SealMismatchRollsBackWholeCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runID, 1, 1) // one scenario: out of range

	before, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket before: %v", err)
	}

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Messages: []Message{{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress}},
		Seal:     &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 1, At: time.Now()},
	})
	assertSealMismatch(t, err, "count", 1, 0)
	if applied {
		t.Error("applied = true, want false")
	}

	after, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket after: %v", getErr)
	}
	if after.State != before.State {
		t.Errorf("ticket state = %q, want unchanged %q", after.State, before.State)
	}
	if after.ClaimOwner == nil || *after.ClaimOwner != owner {
		t.Error("the claim was released by a rolled-back commit, want it still held")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketID); n != 0 {
		t.Errorf("messages after a rolled-back seal mismatch = %d, want 0", n)
	}
}

// --- CommitHandlerResult: Escalation (design D10, section 6.7) -------------

// escalationTestPayload is a minimal, schema-valid EscalationPayload with
// the given code and origin.
func escalationTestPayload(code response.EscalationCode, origin response.EscalationOrigin) response.EscalationPayload {
	return response.EscalationPayload{
		Code: string(code), What: "what happened", Why: "why it happened", Tried: "what was tried",
		Options: []string{"retry", "planning", "abandon"}, Origin: string(origin),
	}
}

// wantEscalationOptions is the fixed retry/back-to-planning/abandon choice
// every escalation's linked question offers (design section 6.7).
var wantEscalationOptions = []response.Option{
	{Key: "a", Text: "Retry"},
	{Key: "b", Text: "Back to planning"},
	{Key: "c", Text: "Abandon"},
}

// TestCommitHandlerResult_EscalationCapHasNilRunID proves a cap escalation
// (no run caused it) inserts an escalation message and its linked question
// both with RunID nil, the question parented to the escalation's own id,
// recommended "b", offering the fixed three options, and both bodies exactly
// as design section 6.7 specifies.
func TestCommitHandlerResult_EscalationCapHasNilRunID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner, expires := claimForCommit(t, s, ticketID)
	body := "wall_clock: over budget"
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Escalation: &EscalationCommit{
			RunID: nil, Body: body,
			Payload: escalationTestPayload(response.EscalationCodeWallClock, response.EscalationOriginCapBudget),
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	msgs, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (escalation + question)", len(msgs))
	}
	esc, q := msgs[0], msgs[1]
	if esc.Type != msgTypeEscalation {
		t.Fatalf("msgs[0].Type = %q, want %q", esc.Type, msgTypeEscalation)
	}
	if q.Type != msgTypeQuestion {
		t.Fatalf("msgs[1].Type = %q, want %q", q.Type, msgTypeQuestion)
	}
	if esc.RunID != nil {
		t.Errorf("escalation.RunID = %v, want nil", esc.RunID)
	}
	if q.RunID != nil {
		t.Errorf("question.RunID = %v, want nil", q.RunID)
	}
	if q.ParentID == nil || *q.ParentID != esc.ID {
		t.Errorf("question.ParentID = %v, want %d (the escalation's own id)", q.ParentID, esc.ID)
	}
	if esc.Body != body {
		t.Errorf("escalation.Body = %q, want %q", esc.Body, body)
	}
	wantQBody := body + "\n\nHow should Zing proceed?"
	if q.Body != wantQBody {
		t.Errorf("question.Body = %q, want %q", q.Body, wantQBody)
	}

	var qp response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &qp); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if qp.Key != "Q1" {
		t.Errorf("question.Key = %q, want Q1", qp.Key)
	}
	if qp.Recommended != "b" {
		t.Errorf("question.Recommended = %q, want b", qp.Recommended)
	}
	if !reflect.DeepEqual(qp.Options, wantEscalationOptions) {
		t.Errorf("question.Options = %+v, want %+v", qp.Options, wantEscalationOptions)
	}

	var ep response.EscalationPayload
	if err := json.Unmarshal(esc.Payload, &ep); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if ep.Origin != string(response.EscalationOriginCapBudget) {
		t.Errorf("escalation.Origin = %q, want %q", ep.Origin, response.EscalationOriginCapBudget)
	}
	if ep.Code != string(response.EscalationCodeWallClock) {
		t.Errorf("escalation.Code = %q, want %q", ep.Code, response.EscalationCodeWallClock)
	}
}

// TestCommitHandlerResult_EscalationWithRunIDTiesBothMessagesToIt proves an
// owned escalation (a run caused it) ties both the escalation message and
// its linked question to that same run id.
func TestCommitHandlerResult_EscalationWithRunIDTiesBothMessagesToIt(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)

	owner, expires := claimForCommit(t, s, ticketID)
	_, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Escalation: &EscalationCommit{
			RunID: &runID, Body: "response_invalid: invalid output twice",
			Payload: escalationTestPayload(response.EscalationCodeResponseInvalid, response.EscalationOriginPlanningResume),
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}

	msgs, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (escalation + question)", len(msgs))
	}
	for _, m := range msgs {
		if m.RunID == nil || *m.RunID != runID {
			t.Errorf("message %d (%s) RunID = %v, want %d", m.ID, m.Type, m.RunID, runID)
		}
	}
}

// TestCommitHandlerResult_EscalationRejectsForeignRunID proves escalateTx
// refuses a RunID that belongs to another ticket: the foreign key alone only
// proves the run exists, so without this check the escalation and its linked
// question would route through the other ticket's session and job (design
// section 4.5, 6.7; PR #23 review).
func TestCommitHandlerResult_EscalationRejectsForeignRunID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, otherTicketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, otherTicketID, testStatePlanning)
	otherSess := insertSession(t, s, otherTicketID, testStatePlanning)
	foreignRunID := insertRun(t, s, otherSess)

	_, ticketID := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner, expires := claimForCommit(t, s, ticketID)
	_, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Escalation: &EscalationCommit{
			RunID: &foreignRunID, Body: "response_invalid: run from another ticket",
			Payload: escalationTestPayload(response.EscalationCodeResponseInvalid, response.EscalationOriginPlanningResume),
		},
	})
	if err == nil {
		t.Fatal("CommitHandlerResult with a foreign RunID: err = nil, want an ownership error")
	}
	if !strings.Contains(err.Error(), "does not belong to ticket") {
		t.Errorf("err = %v, want it to name the ownership violation", err)
	}

	// The whole transaction rolled back: no escalation or question landed on
	// the target ticket.
	msgs, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages on the target ticket = %d, want 0 (the commit rolled back)", len(msgs))
	}
}

// TestCommitHandlerResult_EscalationAllocatesSequentialQuestionKeys proves
// two escalations committed one after another on the same ticket allocate
// Q1 then Q2 (design section 6.7): the allocation counts every "question"
// message the ticket already carries, escalation or otherwise.
func TestCommitHandlerResult_EscalationAllocatesSequentialQuestionKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	for i := range 2 {
		owner, expires := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{
				Body:    "other: repeated escalation",
				Payload: escalationTestPayload(response.EscalationCodeOther, response.EscalationOriginSeal),
			},
		}); err != nil {
			t.Fatalf("CommitHandlerResult %d: %v", i, err)
		}
	}

	msgs, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var keys []string
	for _, m := range msgs {
		if m.Type != msgTypeQuestion {
			continue
		}
		var qp response.QuestionPayload
		if err := json.Unmarshal(m.Payload, &qp); err != nil {
			t.Fatalf("unmarshal question payload: %v", err)
		}
		keys = append(keys, qp.Key)
	}
	if !reflect.DeepEqual(keys, []string{"Q1", "Q2"}) {
		t.Errorf("question keys in commit order = %v, want [Q1 Q2]", keys)
	}
}

// TestCommitHandlerResult_TrackerEffectDoesNotChangeTheTransactionsWrites
// proves TrackerEffect is carried, never applied, by CommitHandlerResult
// (design D12): a commit with and without an identical TrackerEffect writes
// the same rows either way.
func TestCommitHandlerResult_TrackerEffectDoesNotChangeTheTransactionsWrites(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketA, testStatePlanning)
	setTicketState(t, s, ticketB, testStatePlanning)

	ownerA, expiresA := claimForCommit(t, s, ticketA)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketA, Owner: ownerA, Expires: expiresA,
		Next: testStateDone, Reason: "nothing to do",
	}); err != nil {
		t.Fatalf("commit without TrackerEffect: %v", err)
	}

	ownerB, expiresB := claimForCommit(t, s, ticketB)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketB, Owner: ownerB, Expires: expiresB,
		Next: testStateDone, Reason: "nothing to do",
		TrackerEffect: &TrackerEffect{Ref: "owner/repo#9", Notes: "nothing left to do"},
	}); err != nil {
		t.Fatalf("commit with TrackerEffect: %v", err)
	}

	ta, err := s.GetTicket(ctx, ticketA)
	if err != nil {
		t.Fatalf("GetTicket(A): %v", err)
	}
	tb, err := s.GetTicket(ctx, ticketB)
	if err != nil {
		t.Fatalf("GetTicket(B): %v", err)
	}
	if ta.State != tb.State {
		t.Errorf("state with TrackerEffect = %q, without = %q, want equal", tb.State, ta.State)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketA); n != 1 {
		t.Errorf("messages for ticket without TrackerEffect = %d, want 1 (the state message only)", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketB); n != 1 {
		t.Errorf("messages for ticket with TrackerEffect = %d, want 1 (TrackerEffect writes nothing)", n)
	}
}

// TestCommitHandlerResult_MessageQuestionAllocatesKeyWhenPayloadKeyIsEmpty
// proves a "question" message posted directly through c.Messages (a gate or
// a planning batch question, not an escalation's own linked question) gets
// a Q<n> key allocated at commit when its payload arrives with Key still
// empty (design section 4.5, 6.7: "gate and planning questions use the same
// Q<n> allocation" escalateTx's own linked question already uses): two such
// messages in one commit get Q1 then Q2, a message that already carries a
// key is left exactly as the handler set it, and every stored payload still
// validates against the messages/question schema (insertMessageTx's own
// check, run after the key is filled in).
func TestCommitHandlerResult_MessageQuestionAllocatesKeyWhenPayloadKeyIsEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Messages: []Message{
			{TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing, State: new(questionStateOpen), Body: "A", Payload: questionPayload("")},
			{TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing, State: new(questionStateOpen), Body: "B", Payload: questionPayload("")},
			{TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing, State: new(questionStateOpen), Body: "C", Payload: questionPayload("Q7")},
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	open, err := s.QuestionsByState(ctx, ticketID, questionStateOpen)
	if err != nil {
		t.Fatalf("QuestionsByState: %v", err)
	}
	if len(open) != 3 {
		t.Fatalf("open questions = %d, want 3", len(open))
	}
	keyByBody := make(map[string]string, 3)
	for _, m := range open {
		var qp response.QuestionPayload
		if err := json.Unmarshal(m.Payload, &qp); err != nil {
			t.Fatalf("unmarshal payload for %q: %v", m.Body, err)
		}
		keyByBody[m.Body] = qp.Key
	}
	if keyByBody["A"] != "Q1" {
		t.Errorf("A's allocated key = %q, want Q1", keyByBody["A"])
	}
	if keyByBody["B"] != "Q2" {
		t.Errorf("B's allocated key = %q, want Q2", keyByBody["B"])
	}
	if keyByBody["C"] != "Q7" {
		t.Errorf("C's key (already set) = %q, want unchanged Q7", keyByBody["C"])
	}
}
