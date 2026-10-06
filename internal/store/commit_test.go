package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
	testOutcomeError     = "error"
	testTypePlan         = "plan"

	// testStateReviewing and testStateJudging name the two post-seal ticket
	// states the escalation-options tests (#47 item 2) table over, alongside
	// testStateBuilding and reads_test.go's own testStateShipping.
	testStateReviewing = "reviewing"
	testStateJudging   = "judging"

	// testStateGeneric is an arbitrary Next value for a fence or claim test
	// that cares only about ownership and expiry, not about any real
	// transition's own side effects: deliberately not "building", which
	// checkGateApprovalTx's own seal invariant (D32) now gates on a
	// HandlerCommit carrying GateApproval when the ticket is in "planning".
	testStateGeneric = "reviewing"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		Next: testStateGeneric, Reason: testReasonPlanReady,
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
	if got.State != testStateGeneric {
		t.Errorf("ticket state = %q, want %s", got.State, testStateGeneric)
	}
}

func TestCommitHandlerResult_StaleOwnerAppliesNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	_, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: testOwnerOther, Expires: expires,
		Next: testStateGeneric, Reason: testReasonPlanReady,
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
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)
	differentExpires := expires.Add(time.Minute)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: differentExpires,
		Next: testStateGeneric, Reason: testReasonPlanReady,
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
	t.Parallel()
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
	t.Parallel()
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
		Next: testStateGeneric, Reason: testReasonPlanReady,
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
	if got.State != testStateGeneric {
		t.Errorf("ticket state = %q, want %s", got.State, testStateGeneric)
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
	if from != testStatePlanning || to != testStateGeneric || reason != testReasonPlanReady {
		t.Errorf("state message = (%s, %s, %s), want (%s, %s, %s)", from, to, reason, testStatePlanning, testStateGeneric, testReasonPlanReady)
	}
}

// TestCommitHandlerResult_SessionUpsertFillsNullExternalID proves the
// SessionUpsert extension (design section 4.5): a resume commit whose
// ExternalID is non-nil fills a session's still-null external_id -- the
// D13 case where Reserve created the session before the runtime call ran,
// and this terminalizing commit is the first to learn the runtime's session
// id -- and BumpResumes still increments alongside it.
func TestCommitHandlerResult_SessionUpsertFillsNullExternalID(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStateReviewing)
	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateJudging, Reason: "review clean",
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelOpus48})
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
	if model != testModelOpus48 {
		t.Errorf("run.model = %q, want unchanged claude-opus-4-8 (Reserve set it; an update must never touch it)", model)
	}
}

// TestCommitHandlerResult_RunUpdateRejectsRunFromAnotherTicketsSession proves
// the ownership subquery (design section 4.5): a Run.ID that belongs to a
// session on a different ticket is rejected with the exact text, and the
// run's own row is left untouched.
func TestCommitHandlerResult_RunUpdateRejectsRunFromAnotherTicketsSession(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketA, testStatePlanning)
	setTicketState(t, s, ticketB, testStatePlanning)

	ownerA, expiresA := claimForCommit(t, s, ticketA)
	reserved, err := s.Reserve(ctx, ticketA, ownerA, expiresA,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
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
	t.Parallel()
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

// TestCommitHandlerResult_MessageRejectsRunFromAnotherTicket proves the
// same ownership subquery the Artifacts loop already uses (design section
// 4.5): a non-nil Message.RunID that belongs to another ticket's run is
// rejected, and nothing is inserted.
func TestCommitHandlerResult_MessageRejectsRunFromAnotherTicket(t *testing.T) {
	t.Parallel()
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
		Messages: []Message{{RunID: &runA, Type: msgTypeUpdate, Author: authorSystem, Body: "hello"}},
	})
	wantErr := fmt.Sprintf("message run %d not owned by ticket %d", runA, ticketB)
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want containing %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketB); n != 0 {
		t.Errorf("messages after a rejected commit = %d, want 0", n)
	}
}

// TestCommitHandlerResult_AttachRunToMsgsKeepsExplicitRunID proves a message
// that already carries a non-zero RunID is left alone even when
// AttachRunToMsgs is set: only a nil RunID is filled in.
func TestCommitHandlerResult_AttachRunToMsgsKeepsExplicitRunID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	priorSess := insertSession(t, s, ticketID, testStatePlanning)
	priorRunID := insertQuestionRun(t, s, priorSess)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// --- CommitHandlerResult: SetBranch -----------------------------------------

// TestCommitSetBranch proves SetBranch mirrors SetKind exactly (design
// section 4.2): NULL to a branch succeeds, that same branch again succeeds
// (same-to-same, whether or not the driver reports it as zero rows
// affected), and a different branch is a conflict naming both values.
func TestCommitSetBranch(t *testing.T) {
	t.Parallel()
	const zingBranch1, zingBranch2 = "zing/1", "zing/2"

	t.Run("null to a branch succeeds", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			SetBranch: new(zingBranch1),
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
		if got.Branch == nil || *got.Branch != zingBranch1 {
			t.Errorf("ticket.Branch = %v, want zing/1", got.Branch)
		}
	})

	t.Run("same value succeeds", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStatePlanning)

		owner1, expires1 := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner1, Expires: expires1, SetBranch: new(zingBranch1),
		}); err != nil {
			t.Fatalf("seed SetBranch(zing/1): %v", err)
		}

		owner2, expires2 := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner2, Expires: expires2, SetBranch: new(zingBranch1),
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult(zing/1 -> zing/1): %v, want nil", err)
		}
		if !applied {
			t.Fatal("CommitHandlerResult: applied = false, want true")
		}

		got, getErr := s.GetTicket(ctx, ticketID)
		if getErr != nil {
			t.Fatalf("GetTicket: %v", getErr)
		}
		if got.Branch == nil || *got.Branch != zingBranch1 {
			t.Errorf("ticket.Branch = %v, want unchanged zing/1", got.Branch)
		}
	})

	t.Run("a different value conflicts", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStatePlanning)

		owner1, expires1 := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner1, Expires: expires1, SetBranch: new(zingBranch1),
		}); err != nil {
			t.Fatalf("seed SetBranch(zing/1): %v", err)
		}

		owner2, expires2 := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner2, Expires: expires2, SetBranch: new(zingBranch2),
		})
		wantErr := "branch conflict: have zing/1, want zing/2"
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
		if got.Branch == nil || *got.Branch != zingBranch1 {
			t.Errorf("ticket.Branch after a rejected conflicting SetBranch = %v, want unchanged zing/1", got.Branch)
		}
	})
}

// --- CommitHandlerResult: SetPRURL ------------------------------------------

func TestCommitSetPRURL(t *testing.T) {
	t.Parallel()
	const prURL1, prURL2 = "https://github.com/x/zing/pull/1", "https://github.com/x/zing/pull/2"

	t.Run("null to a url succeeds", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		owner, expires := claimForCommit(t, s, ticketID)

		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires, SetPRURL: new(prURL1),
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
		if got.PRURL == nil || *got.PRURL != prURL1 {
			t.Errorf("ticket.PRURL = %v, want %s", got.PRURL, prURL1)
		}
	})

	t.Run("same value succeeds", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")

		owner1, expires1 := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner1, Expires: expires1, SetPRURL: new(prURL1),
		}); err != nil {
			t.Fatalf("seed SetPRURL(%s): %v", prURL1, err)
		}

		owner2, expires2 := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner2, Expires: expires2, SetPRURL: new(prURL1),
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult(same url): %v, want nil", err)
		}
		if !applied {
			t.Fatal("CommitHandlerResult: applied = false, want true")
		}

		got, getErr := s.GetTicket(ctx, ticketID)
		if getErr != nil {
			t.Fatalf("GetTicket: %v", getErr)
		}
		if got.PRURL == nil || *got.PRURL != prURL1 {
			t.Errorf("ticket.PRURL = %v, want unchanged %s", got.PRURL, prURL1)
		}
	})

	t.Run("a different value conflicts", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")

		owner1, expires1 := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner1, Expires: expires1, SetPRURL: new(prURL1),
		}); err != nil {
			t.Fatalf("seed SetPRURL(%s): %v", prURL1, err)
		}

		owner2, expires2 := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner2, Expires: expires2, SetPRURL: new(prURL2),
		})
		wantErr := "pr url conflict: have " + prURL1 + ", want " + prURL2
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
		if got.PRURL == nil || *got.PRURL != prURL1 {
			t.Errorf("ticket.PRURL after a rejected conflicting SetPRURL = %v, want unchanged %s", got.PRURL, prURL1)
		}
	})
}

// --- CommitHandlerResult: Poll, PollSchedule, ClearPoll ---------------------

// testFingerprint is the one 64-lowercase-hex fingerprint value this
// section's commit tests share (goconst).
const testFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestCommitPoll(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	owner, expires := claimForCommit(t, s, ticketID)

	nextAt := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Poll: &PollUpdate{NextAt: nextAt, IntervalS: 30, Fingerprint: testFingerprint},
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
	if got.NextPollAt == nil || !got.NextPollAt.Equal(nextAt) {
		t.Errorf("ticket.NextPollAt = %v, want %v", got.NextPollAt, nextAt)
	}
	if got.PollIntervalS == nil || *got.PollIntervalS != 30 {
		t.Errorf("ticket.PollIntervalS = %v, want 30", got.PollIntervalS)
	}
	if got.PollFingerprint == nil || *got.PollFingerprint != testFingerprint {
		t.Errorf("ticket.PollFingerprint = %v, want %q", got.PollFingerprint, testFingerprint)
	}
}

// TestCommitPollSchedule proves PollSchedule moves next_poll_at and
// poll_interval_s but leaves poll_fingerprint exactly as it is (design
// section 4.2, 8.3): still NULL when nothing set it yet, and still whatever
// a previous Poll commit wrote when one did.
func TestCommitPollSchedule(t *testing.T) {
	t.Parallel()

	t.Run("fingerprint NULL stays NULL", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		owner, expires := claimForCommit(t, s, ticketID)

		nextAt := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			PollSchedule: &PollSchedule{NextAt: nextAt, IntervalS: 30},
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
		if got.NextPollAt == nil || !got.NextPollAt.Equal(nextAt) {
			t.Errorf("ticket.NextPollAt = %v, want %v", got.NextPollAt, nextAt)
		}
		if got.PollIntervalS == nil || *got.PollIntervalS != 30 {
			t.Errorf("ticket.PollIntervalS = %v, want 30", got.PollIntervalS)
		}
		if got.PollFingerprint != nil {
			t.Errorf("ticket.PollFingerprint = %v, want nil", *got.PollFingerprint)
		}
	})

	t.Run("fingerprint stays untouched", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")

		owner1, expires1 := claimForCommit(t, s, ticketID)
		firstAt := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)
		if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner1, Expires: expires1,
			Poll: &PollUpdate{NextAt: firstAt, IntervalS: 30, Fingerprint: testFingerprint},
		}); err != nil {
			t.Fatalf("seed Poll: %v", err)
		}

		owner2, expires2 := claimForCommit(t, s, ticketID)
		secondAt := time.Now().Add(60 * time.Second).UTC().Truncate(time.Second)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner2, Expires: expires2,
			PollSchedule: &PollSchedule{NextAt: secondAt, IntervalS: 60},
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
		if got.NextPollAt == nil || !got.NextPollAt.Equal(secondAt) {
			t.Errorf("ticket.NextPollAt = %v, want %v", got.NextPollAt, secondAt)
		}
		if got.PollIntervalS == nil || *got.PollIntervalS != 60 {
			t.Errorf("ticket.PollIntervalS = %v, want 60", got.PollIntervalS)
		}
		if got.PollFingerprint == nil || *got.PollFingerprint != testFingerprint {
			t.Errorf("ticket.PollFingerprint = %v, want unchanged %q", got.PollFingerprint, testFingerprint)
		}
	})
}

// TestCommitClearPoll proves ClearPoll sets all three poll columns to NULL
// (design section 4.2, D8).
func TestCommitClearPoll(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	owner1, expires1 := claimForCommit(t, s, ticketID)
	nextAt := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner1, Expires: expires1,
		Poll: &PollUpdate{NextAt: nextAt, IntervalS: 30, Fingerprint: testFingerprint},
	}); err != nil {
		t.Fatalf("seed Poll: %v", err)
	}

	owner2, expires2 := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner2, Expires: expires2, ClearPoll: true,
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
	if got.NextPollAt != nil {
		t.Errorf("ticket.NextPollAt = %v, want nil", *got.NextPollAt)
	}
	if got.PollIntervalS != nil {
		t.Errorf("ticket.PollIntervalS = %v, want nil", *got.PollIntervalS)
	}
	if got.PollFingerprint != nil {
		t.Errorf("ticket.PollFingerprint = %v, want nil", *got.PollFingerprint)
	}
}

// TestCommitOnePollUpdateOnly proves CommitHandlerResult refuses a commit
// that sets more than one of Poll, PollSchedule, and ClearPoll (design
// section 4.2), rejecting it before the transaction opens (no ticket row
// changes).
func TestCommitOnePollUpdateOnly(t *testing.T) {
	t.Parallel()
	wantErr := "commit handler result: at most one poll update per commit"
	nextAt := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)

	cases := map[string]HandlerCommit{
		"Poll and PollSchedule": {
			Poll:         &PollUpdate{NextAt: nextAt, IntervalS: 30, Fingerprint: testFingerprint},
			PollSchedule: &PollSchedule{NextAt: nextAt, IntervalS: 30},
		},
		"Poll and ClearPoll": {
			Poll:      &PollUpdate{NextAt: nextAt, IntervalS: 30, Fingerprint: testFingerprint},
			ClearPoll: true,
		},
		"PollSchedule and ClearPoll": {
			PollSchedule: &PollSchedule{NextAt: nextAt, IntervalS: 30},
			ClearPoll:    true,
		},
	}
	for name, partial := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			owner, expires := claimForCommit(t, s, ticketID)

			commit := partial
			commit.TicketID, commit.Owner, commit.Expires = ticketID, owner, expires
			applied, err := s.CommitHandlerResult(ctx, commit)
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("err = %v, want containing %q", err, wantErr)
			}
			if applied {
				t.Error("applied = true, want false")
			}
		})
	}
}

// TestCommitFingerprintUppercaseRefused proves Poll.Fingerprint's validation
// (design section 4.2) refuses an uppercase character, even at the right
// length.
func TestCommitFingerprintUppercaseRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	owner, expires := claimForCommit(t, s, ticketID)

	uppercase := "A" + testFingerprint[1:]
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Poll: &PollUpdate{NextAt: time.Now(), IntervalS: 30, Fingerprint: uppercase},
	})
	wantErr := "commit handler result: poll fingerprint must be 64 lowercase hex characters"
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want containing %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}
}

// TestCommitFingerprintNonHexRefused proves Poll.Fingerprint's validation
// (design section 4.2) refuses a 64-character value that is not hex.
func TestCommitFingerprintNonHexRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	owner, expires := claimForCommit(t, s, ticketID)

	nonHex := "g" + testFingerprint[1:]
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Poll: &PollUpdate{NextAt: time.Now(), IntervalS: 30, Fingerprint: nonHex},
	})
	wantErr := "commit handler result: poll fingerprint must be 64 lowercase hex characters"
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want containing %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}
}

// TestCommitPollIntervalBounds proves IntervalS's 30-to-300 validation
// (design section 4.2) on both Poll and PollSchedule: 29 and 301 are
// refused, 30 and 300 are accepted.
func TestCommitPollIntervalBounds(t *testing.T) {
	t.Parallel()
	wantErr := "commit handler result: poll interval must be 30 to 300"
	nextAt := time.Now().Add(30 * time.Second)

	for _, iv := range []int{29, 301} {
		t.Run(fmt.Sprintf("Poll refuses %d", iv), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			owner, expires := claimForCommit(t, s, ticketID)

			applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
				TicketID: ticketID, Owner: owner, Expires: expires,
				Poll: &PollUpdate{NextAt: nextAt, IntervalS: iv, Fingerprint: testFingerprint},
			})
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("err = %v, want containing %q", err, wantErr)
			}
			if applied {
				t.Error("applied = true, want false")
			}
		})

		t.Run(fmt.Sprintf("PollSchedule refuses %d", iv), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			owner, expires := claimForCommit(t, s, ticketID)

			applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
				TicketID: ticketID, Owner: owner, Expires: expires,
				PollSchedule: &PollSchedule{NextAt: nextAt, IntervalS: iv},
			})
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("err = %v, want containing %q", err, wantErr)
			}
			if applied {
				t.Error("applied = true, want false")
			}
		})
	}

	for _, iv := range []int{30, 300} {
		t.Run(fmt.Sprintf("Poll accepts %d", iv), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			owner, expires := claimForCommit(t, s, ticketID)

			applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
				TicketID: ticketID, Owner: owner, Expires: expires,
				Poll: &PollUpdate{NextAt: nextAt, IntervalS: iv, Fingerprint: testFingerprint},
			})
			if err != nil {
				t.Fatalf("CommitHandlerResult: %v, want nil", err)
			}
			if !applied {
				t.Error("applied = false, want true")
			}
		})
	}
}

// --- CommitHandlerResult: Sessions ------------------------------------------

// TestCommitExtraSessions proves HandlerCommit.Sessions updates further
// existing sessions beyond the single Session field, each entry keyed by its
// own ID -- the review round's own shape, terminalizing seven lens sessions
// in one commit (design section 4.2) -- and that an entry with a nil ID is
// refused before anything commits.
func TestCommitExtraSessions(t *testing.T) {
	t.Parallel()
	t.Run("seven sessions updated", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		const n = 7
		ids := make([]int64, n)
		sessions := make([]SessionUpsert, n)
		externals := make([]string, n)
		for i := range n {
			ids[i] = insertSession(t, s, ticketID, testJobBuild)
			externals[i] = fmt.Sprintf("ext-lens-%d", i)
			sessions[i] = SessionUpsert{ID: &ids[i], ExternalID: &externals[i]}
		}

		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Sessions: sessions,
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}
		if !applied {
			t.Fatal("CommitHandlerResult: applied = false, want true")
		}

		for i, id := range ids {
			var externalID sql.NullString
			if err := s.db.QueryRowContext(ctx, `SELECT external_id FROM sessions WHERE id = ?`, id).Scan(&externalID); err != nil {
				t.Fatalf("read session %d: %v", id, err)
			}
			if !externalID.Valid || externalID.String != externals[i] {
				t.Errorf("session %d external_id = %v, want %q", id, externalID, externals[i])
			}
		}
	})

	t.Run("a nil id is refused, and nothing commits", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		sessionID := insertSession(t, s, ticketID, testJobBuild)
		ext := testExternalID1

		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Sessions: []SessionUpsert{{ID: &sessionID, ExternalID: &ext}, {ExternalID: &ext}},
		})
		wantErr := "commit handler result: extra session needs an id"
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("err = %v, want containing %q", err, wantErr)
		}
		if applied {
			t.Error("applied = true, want false")
		}

		var externalID sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT external_id FROM sessions WHERE id = ?`, sessionID).Scan(&externalID); err != nil {
			t.Fatalf("read session %d: %v", sessionID, err)
		}
		if externalID.Valid {
			t.Errorf("session external_id = %q after a rejected commit, want NULL (whole commit rolled back)", externalID.String)
		}
	})
}

// --- CommitHandlerResult: Artifacts -----------------------------------------

// TestCommitHandlerResult_ArtifactTicketIDForced proves the same forcing
// rule commit.go already applies to Messages (design section 4.5): every
// inserted artifact lands under c.TicketID regardless of what the commit's
// Artifacts carried.
func TestCommitHandlerResult_ArtifactTicketIDForced(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// --- CommitHandlerResult: WithdrawQuestions (M4 task 8) ---------------------

// TestCommitWithdrawQuestions proves WithdrawQuestions' own scope (design
// section 4.2, M4, D13's merge question): an "open" question resolves, an
// "answered" question resolves, an already-"resolved" question is a no-op
// (no second "resolved" message on a retried withdraw), and an id that
// belongs to another ticket is refused.
func TestCommitWithdrawQuestions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qOpen := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	qAnswered := insertOpenQuestion(t, s, ticketID, runID, "Q2")
	markAnswered(t, s, qAnswered)

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		WithdrawQuestions: []int64{qOpen, qAnswered},
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
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE type = ? AND parent_id = ?`, msgTypeResolved, qOpen); n != 1 {
		t.Errorf("resolved messages for %d = %d, want 1", qOpen, n)
	}

	// Already resolved: re-withdrawing qOpen is a no-op, no second
	// "resolved" message (an idempotent retry, design section 11).
	owner2, expires2 := claimForCommit(t, s, ticketID)
	applied, err = s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner2, Expires: expires2,
		WithdrawQuestions: []int64{qOpen},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult (re-withdraw): %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult (re-withdraw): applied = false, want true")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE type = ? AND parent_id = ?`, msgTypeResolved, qOpen); n != 1 {
		t.Errorf("resolved messages for %d after a second withdraw = %d, want 1 (idempotent)", qOpen, n)
	}

	// Another ticket's id is refused.
	_, otherTicketID := seedQueuedTicket(t, s, "2")
	owner3, expires3 := claimForCommit(t, s, otherTicketID)
	_, err = s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: otherTicketID, Owner: owner3, Expires: expires3,
		WithdrawQuestions: []int64{qAnswered},
	})
	wantErr := fmt.Sprintf("commit handler result: withdraw question %d: question %d is not a question of ticket %d", qAnswered, qAnswered, otherTicketID)
	if err == nil || err.Error() != wantErr {
		t.Errorf("error = %v, want %q", err, wantErr)
	}
}

// TestWithdrawRacesOwnerAnswer proves WithdrawQuestions' own idempotent
// race handling (design section 8.5 rows 3 to 5, 11; M4 task 8's merge
// question): a poll that read the merge question "open" builds its own
// commit with WithdrawQuestions before the owner's answer lands; by the
// time the commit actually runs, the owner's own answer has already moved
// the question to "answered" underneath it. The commit still applies, the
// question still ends "resolved" (not left "answered"), and
// CommitHandlerResult returns no error -- the dispatcher does not fail
// closed over the race.
func TestWithdrawRacesOwnerAnswer(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStateBuilding)

	sessID := insertSession(t, s, ticketID, testStateBuilding)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")

	// The poll's own read saw "open" and built a commit with
	// WithdrawQuestions; before that commit runs, the owner's answer lands
	// and moves the question to "answered" underneath it.
	markAnswered(t, s, qID)

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		WithdrawQuestions: []int64{qID},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	msg, getErr := s.GetMessage(ctx, qID)
	if getErr != nil {
		t.Fatalf("GetMessage: %v", getErr)
	}
	if msg.State == nil || *msg.State != questionStateResolved {
		t.Errorf("question state = %v, want resolved", msg.State)
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

// seedConfirmedGateApproval seeds a complete, confirmed gate approval for
// ticketID (D32, design section 22.12.1, 22.12.3a): an "answered" gate
// question, its newest sent answer (option "a", batch), and the confirming
// marker binding both to planVersion under confirmRunID. It returns the
// GateApproval a Seal test passes on HandlerCommit, matching exactly what
// the real flow would have produced by the time gateApprove seals.
func seedConfirmedGateApproval(t *testing.T, s *Store, ticketID, confirmRunID int64, planVersion int, batch int64) GateApproval {
	t.Helper()
	ctx := t.Context()
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindGate, optionsAB, nil)
	markAnswered(t, s, qID)
	aID, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, ParentID: &qID, Type: msgTypeAnswer, Author: authorYou,
		State: new(answerStateSent), Payload: []byte(`{"option":"a"}`), BatchID: &batch,
	})
	if err != nil {
		t.Fatalf("seed gate approve answer: %v", err)
	}
	marker := fmt.Sprintf("gate confirmed run %d plan v%d gate %d answer %d", confirmRunID, planVersion, qID, aID)
	if _, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, ParentID: &qID, Type: msgTypeUpdate, Author: authorSystem, Body: marker,
	}); err != nil {
		t.Fatalf("seed confirming marker: %v", err)
	}
	return GateApproval{QuestionID: qID, AnswerID: aID, ApproveBatch: batch, PlanVersion: planVersion}
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
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sess := insertSession(t, s, ticketID, testStatePlanning)
	runA := insertRun(t, s, sess)
	runB := insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runA, 1, 3)
	seedSealableCohort(t, s, ticketID, runB, 2, 4)
	approval := seedConfirmedGateApproval(t, s, ticketID, runB, 2, 1)

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Seal:         &SealRequest{RunID: runB, PlanVersion: 2, ExpectedCount: 4, At: time.Now()},
		GateApproval: &approval,
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
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runID, 1, 3)
	approval := seedConfirmedGateApproval(t, s, ticketID, runID, 2, 1)

	owner, expires := claimForCommit(t, s, ticketID)
	_, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Seal:         &SealRequest{RunID: runID, PlanVersion: 2, ExpectedCount: 3, At: time.Now()},
		GateApproval: &approval,
	})
	assertSealMismatch(t, err, "plan", 3, 0)
}

// TestCommitHandlerResult_SealCohortCountOutOfRange proves a scenario cohort
// of 1 or 31 rows fails at stage "count" (design D16, section 4.5 check 2:
// the count must lie in [2,30]).
func TestCommitHandlerResult_SealCohortCountOutOfRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		n    int
	}{
		{"one scenario is below the floor", 1},
		{"thirty-one scenarios is above the ceiling", 31},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			setTicketState(t, s, ticketID, testStatePlanning)
			sess := insertSession(t, s, ticketID, testStatePlanning)
			runID := insertRun(t, s, sess)
			seedSealableCohort(t, s, ticketID, runID, 1, tt.n)
			approval := seedConfirmedGateApproval(t, s, ticketID, runID, 1, 1)

			owner, expires := claimForCommit(t, s, ticketID)
			_, err := s.CommitHandlerResult(ctx, HandlerCommit{
				TicketID: ticketID, Owner: owner, Expires: expires,
				Seal:         &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: tt.n, At: time.Now()},
				GateApproval: &approval,
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
	t.Parallel()
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
	approval := seedConfirmedGateApproval(t, s, ticketID, runID, 1, 1)

	owner, expires := claimForCommit(t, s, ticketID)
	_, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Seal:         &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 3, At: time.Now()},
		GateApproval: &approval,
	})
	assertSealMismatch(t, err, "update", 3, 2)
}

// TestCommitHandlerResult_SealMismatchRollsBackWholeCommit proves a seal
// mismatch rolls the whole commit back: the ticket's state is unchanged, its
// claim is still held (the fenced ticket UPDATE that would release it never
// ran), and an otherwise-valid message in the same commit was never inserted
// (design D16).
func TestCommitHandlerResult_SealMismatchRollsBackWholeCommit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runID, 1, 1) // one scenario: out of range
	approval := seedConfirmedGateApproval(t, s, ticketID, runID, 1, 1)

	before, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket before: %v", err)
	}

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Messages:     []Message{{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress}},
		Seal:         &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 1, At: time.Now()},
		GateApproval: &approval,
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
	// 3, not 0: seedConfirmedGateApproval's own fixture (the gate question,
	// its approving answer, and the confirming marker), committed before this
	// test's own CommitHandlerResult call -- the rolled-back commit's
	// Messages entry is what must still be absent.
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, ticketID); n != 3 {
		t.Errorf("messages after a rolled-back seal mismatch = %d, want 3 (only the gate approval fixture)", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ? AND body = ?`,
		ticketID, testTypeUpdate, testBodyProgress); n != 0 {
		t.Errorf("progress messages after a rolled-back seal mismatch = %d, want 0", n)
	}
}

// --- CommitHandlerResult: the seal invariant (D32, design section 22.12.3a) -

// assertSealRefused asserts err is a *SealRefusedError with the exact
// reason wantReason, that it also satisfies errors.Is(err, ErrSealRefused),
// and that its Error() text is exactly "seal refused: " + wantReason
// (design section 22.12.3a, the dispatcher's own marker text).
func assertSealRefused(t *testing.T, err error, wantReason string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a *SealRefusedError")
	}
	if !errors.Is(err, ErrSealRefused) {
		t.Errorf("errors.Is(err, ErrSealRefused) = false, want true (err: %v)", err)
	}
	var refused *SealRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("errors.As(err, *SealRefusedError) = false, want true (err: %v)", err)
	}
	if refused.Reason != wantReason {
		t.Errorf("Reason = %q, want %q", refused.Reason, wantReason)
	}
	if got := refused.Error(); got != "seal refused: "+wantReason {
		t.Errorf("Error() = %q, want %q", got, "seal refused: "+wantReason)
	}
}

// sealableFixture seeds a ticket in planning with a sealable 3-scenario
// cohort at plan version 1 under runID, and claims it, the shared setup
// every seal-invariant test below starts from.
func sealableFixture(t *testing.T, s *Store) (ticketID, runID int64, owner string, expires time.Time) {
	t.Helper()
	_, ticketID = seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID = insertRun(t, s, sess)
	seedSealableCohort(t, s, ticketID, runID, 1, 3)
	owner, expires = claimForCommit(t, s, ticketID)
	return ticketID, runID, owner, expires
}

// TestSealRefusedWithoutGateApproval proves check 1 (design section
// 22.12.3a): a Seal with no GateApproval at all is refused before
// sealCohortTx ever runs, and nothing is written.
func TestSealRefusedWithoutGateApproval(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ticketID, runID, owner, expires := sealableFixture(t, s)

	_, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Seal: &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 3, At: time.Now()},
	})
	assertSealRefused(t, err, "no gate approval check")

	got, getErr := s.GetTicket(t.Context(), ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if got.State != testStatePlanning {
		t.Errorf("ticket state = %q, want unchanged planning", got.State)
	}
}

// TestSealRefusedWithoutMatchingConfirm proves check 3 (design section
// 22.12.3a): a GateApproval naming a plan version no confirming marker ever
// named is refused with its exact reason text.
func TestSealRefusedWithoutMatchingConfirm(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ticketID, runID, owner, expires := sealableFixture(t, s)
	// A gate question and an approving answer exist, but no confirming
	// marker was ever written for them.
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindGate, optionsAB, nil)
	markAnswered(t, s, qID)
	batch := int64(1)
	aID, insErr := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: &qID, Type: msgTypeAnswer, Author: authorYou,
		State: new(answerStateSent), Payload: []byte(`{"option":"a"}`), BatchID: &batch,
	})
	if insErr != nil {
		t.Fatalf("insert approve answer: %v", insErr)
	}

	approval := GateApproval{QuestionID: qID, AnswerID: aID, ApproveBatch: batch, PlanVersion: 1}
	_, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Seal:         &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 3, At: time.Now()},
		GateApproval: &approval,
	})
	assertSealRefused(t, err, fmt.Sprintf("no confirmation for gate question %d answer %d plan v1", qID, aID))
}

// TestSealRefusedAfterCancellation proves check 4 (design section
// 22.12.3a): a cancellation marker with a greater id than the confirming
// marker refuses the seal, even though the confirming marker itself is
// otherwise a perfect match.
func TestSealRefusedAfterCancellation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ticketID, runID, owner, expires := sealableFixture(t, s)
	approval := seedConfirmedGateApproval(t, s, ticketID, runID, 1, 1)
	if _, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: &approval.QuestionID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("gate approval cancelled gate %d batch 2", approval.QuestionID),
	}); err != nil {
		t.Fatalf("insert cancellation marker: %v", err)
	}

	_, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Seal:         &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 3, At: time.Now()},
		GateApproval: &approval,
	})
	assertSealRefused(t, err, fmt.Sprintf("approval of gate question %d was cancelled", approval.QuestionID))
}

// TestSealRefusedOnOwnerRowAfterApproval proves check 5, the owner fence
// (design section 22.12.3a): any sent owner row on a planning question with
// a batch above the approval's own batch refuses the seal -- nothing the
// owner sent after Approve may be sealed past.
func TestSealRefusedOnOwnerRowAfterApproval(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ticketID, runID, owner, expires := sealableFixture(t, s)
	approval := seedConfirmedGateApproval(t, s, ticketID, runID, 1, 1)

	planningSess := insertSession(t, s, ticketID, testStatePlanning)
	planningRun := insertQuestionRun(t, s, planningSess)
	planningQID := insertOpenQuestion(t, s, ticketID, planningRun, "Q2")
	markAnswered(t, s, planningQID) // resolved by CommitHandlerResult's own Conversation path in real use; state irrelevant here
	laterBatch := int64(2)
	if _, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: &planningQID, Type: msgTypeReply, Author: authorYou,
		State: new(answerStateSent), Body: "print JSON too", BatchID: &laterBatch,
	}); err != nil {
		t.Fatalf("insert late owner reply: %v", err)
	}

	_, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Seal:         &SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: 3, At: time.Now()},
		GateApproval: &approval,
	})
	assertSealRefused(t, err, "owner wrote after approval (batch 2 > 1)")
}

// TestSealRefusedWithPlanVersionMismatch proves check 6 (design section
// 22.12.3a): a Seal whose PlanVersion does not match the approval's own is
// refused before sealCohortTx ever runs its own, different plan check.
func TestSealRefusedWithPlanVersionMismatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ticketID, runID, owner, expires := sealableFixture(t, s)
	approval := seedConfirmedGateApproval(t, s, ticketID, runID, 1, 1)

	_, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: testReasonPlanReady,
		Seal:         &SealRequest{RunID: runID, PlanVersion: 2, ExpectedCount: 3, At: time.Now()},
		GateApproval: &approval,
	})
	assertSealRefused(t, err, "seal is for plan v2, approval is for v1")
}

// TestSealRefusedReleasesClaimAndMarks is covered by internal/dispatch's own
// TestSealRefusedReleasesClaimAndMarks (dispatch_test.go): the dispatcher,
// not the store, owns releasing the claim and writing the "seal refused"
// marker after CommitHandlerResult returns this error.

// --- CommitHandlerResult: Escalation (design D10, section 6.7) -------------

// testEscalationBodyPlanGap and testExtraOptionAcceptXText are escalation
// test fixtures repeated often enough across this section's own tests that
// goconst asks for a constant.
const (
	testEscalationBodyPlanGap  = "plan_gap: x.go belongs to another task"
	testExtraOptionAcceptXText = "Accept X"
)

// escalationTestPayload is a minimal, schema-valid EscalationPayload with
// the given code and origin.
func escalationTestPayload(code response.EscalationCode, origin response.EscalationOrigin) response.EscalationPayload {
	return response.EscalationPayload{
		Code: string(code), What: "what happened", Why: "why it happened", Tried: "what was tried",
		Options: []string{"retry", "planning", "abandon"}, Origin: string(origin),
	}
}

// wantEscalationOptionsPlanning is the three-option retry/back-to-planning/
// abandon choice a planning-stage escalation's linked question offers
// (design section 6.7; #47 item 2: back to planning can run in planning, so
// all three are offered there).
var wantEscalationOptionsPlanning = []response.Option{
	{Key: "a", Text: escalationOptionRetry},
	{Key: "b", Text: escalationOptionBackToPlanning},
	{Key: "c", Text: escalationOptionAbandon},
}

// wantEscalationOptionsPostSeal is the two-option retry/abandon choice a
// post-seal escalation's linked question offers (#47 item 2): back to
// planning cannot run once the plan is sealed (replanUnsupportedEscalation
// is the only thing choosing it does there), so it is dropped rather than
// offered and left to loop. Abandon keeps its "c" key; it is never
// renumbered to "b".
var wantEscalationOptionsPostSeal = []response.Option{
	{Key: "a", Text: escalationOptionRetry},
	{Key: "c", Text: escalationOptionAbandon},
}

// TestCommitHandlerResult_EscalationCapHasNilRunID proves a cap escalation
// (no run caused it) inserts an escalation message and its linked question
// both with RunID nil, the question parented to the escalation's own id,
// recommended "a" (wall_clock is not a back-to-planning code), offering the
// three planning-stage options, and both bodies exactly as design section
// 6.7 specifies.
func TestCommitHandlerResult_EscalationCapHasNilRunID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner, expires := claimForCommit(t, s, ticketID)
	body := testEscalationBodyWallClock
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
	if qp.Recommended != "a" {
		t.Errorf("question.Recommended = %q, want a", qp.Recommended)
	}
	if !reflect.DeepEqual(qp.Options, wantEscalationOptionsPlanning) {
		t.Errorf("question.Options = %+v, want %+v", qp.Options, wantEscalationOptionsPlanning)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// latestQuestionPayload returns the newest "question" message's own payload
// for ticketID, unmarshaled (#47 item 2's escalation-options tests share
// this instead of each repeating the ListMessages/unmarshal pair).
func latestQuestionPayload(t *testing.T, s *Store, ticketID int64) response.QuestionPayload {
	t.Helper()
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := range slices.Backward(msgs) {
		if msgs[i].Type != msgTypeQuestion {
			continue
		}
		var qp response.QuestionPayload
		if err := json.Unmarshal(msgs[i].Payload, &qp); err != nil {
			t.Fatalf("unmarshal question payload: %v", err)
		}
		return qp
	}
	t.Fatal("latestQuestionPayload: no question message found")
	return response.QuestionPayload{}
}

// TestCommitHandlerResult_EscalationPostSealOmitsBackToPlanning proves a
// post-seal escalation (#47 item 2) offers only Retry and Abandon,
// recommending Retry, for every post-seal state and whatever code raised
// it: back to planning cannot run once the plan is sealed
// (replanUnsupportedEscalation is the only thing choosing it does there),
// so it is dropped rather than offered and left to loop. Abandon keeps its
// "c" key in every row -- it is never renumbered to "b".
func TestCommitHandlerResult_EscalationPostSealOmitsBackToPlanning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		state  string
		origin response.EscalationOrigin
	}{
		{"building", testStateBuilding, response.EscalationOriginBuild},
		{"reviewing", testStateReviewing, response.EscalationOriginReview},
		{"judging", testStateJudging, response.EscalationOriginJudge},
		{"shipping", testStateShipping, response.EscalationOriginShipping},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			setTicketState(t, s, ticketID, tt.state)

			owner, expires := claimForCommit(t, s, ticketID)
			applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
				TicketID: ticketID, Owner: owner, Expires: expires,
				Escalation: &EscalationCommit{
					Body:    "response_invalid: post-seal, invalid output twice",
					Payload: escalationTestPayload(response.EscalationCodeResponseInvalid, tt.origin),
				},
			})
			if err != nil {
				t.Fatalf("CommitHandlerResult: %v", err)
			}
			if !applied {
				t.Fatal("applied = false, want true")
			}

			qp := latestQuestionPayload(t, s, ticketID)
			if !reflect.DeepEqual(qp.Options, wantEscalationOptionsPostSeal) {
				t.Errorf("question.Options = %+v, want %+v", qp.Options, wantEscalationOptionsPostSeal)
			}
			if qp.Recommended != "a" {
				t.Errorf("question.Recommended = %q, want a", qp.Recommended)
			}
		})
	}
}

// TestCommitHandlerResult_EscalationPlanningLoopsExhaustedRecommendsRetry
// proves a planning-stage loops_exhausted escalation recommends Retry
// (#47 item 2's table: "retry with findings," matching the owner's own
// example) while still offering all three options.
func TestCommitHandlerResult_EscalationPlanningLoopsExhaustedRecommendsRetry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Escalation: &EscalationCommit{
			Body:    "loops_exhausted: floor findings remain open",
			Payload: escalationTestPayload(response.EscalationCodeLoopsExhausted, response.EscalationOriginCapLoops),
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	qp := latestQuestionPayload(t, s, ticketID)
	if qp.Recommended != "a" {
		t.Errorf("question.Recommended = %q, want a", qp.Recommended)
	}
	if !reflect.DeepEqual(qp.Options, wantEscalationOptionsPlanning) {
		t.Errorf("question.Options = %+v, want %+v", qp.Options, wantEscalationOptionsPlanning)
	}
}

// TestCommitHandlerResult_EscalationPlanningSplitUnsupportedRecommendsBack
// proves a planning-stage split_unsupported escalation recommends back to
// planning (#47 item 2's table: the split can't proceed, so a new plan is
// the actual fix) while still offering all three options.
func TestCommitHandlerResult_EscalationPlanningSplitUnsupportedRecommendsBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Escalation: &EscalationCommit{
			Body:    "split_unsupported: the split cannot proceed",
			Payload: escalationTestPayload(response.EscalationCodeSplitUnsupported, response.EscalationOriginSplit),
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	qp := latestQuestionPayload(t, s, ticketID)
	if qp.Recommended != "b" {
		t.Errorf("question.Recommended = %q, want b", qp.Recommended)
	}
	if !reflect.DeepEqual(qp.Options, wantEscalationOptionsPlanning) {
		t.Errorf("question.Options = %+v, want %+v", qp.Options, wantEscalationOptionsPlanning)
	}
}

// TestCommitHandlerResult_EscalationPlanningDefaultsToRetry proves every
// planning-stage code but the two back-to-planning codes recommends Retry
// (#47 item 2's table: transient, or both choices already run the identical
// commit, so Retry is the non-misleading label).
func TestCommitHandlerResult_EscalationPlanningDefaultsToRetry(t *testing.T) {
	t.Parallel()
	codes := []response.EscalationCode{
		response.EscalationCodeResponseInvalid, response.EscalationCodeEnvironment,
		response.EscalationCodeWallClock, response.EscalationCodeResumesExhausted,
		response.EscalationCodeSandboxUnavailable, response.EscalationCodeOther,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			_, ticketID := seedQueuedTicket(t, s, "1")
			setTicketState(t, s, ticketID, testStatePlanning)

			owner, expires := claimForCommit(t, s, ticketID)
			applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
				TicketID: ticketID, Owner: owner, Expires: expires,
				Escalation: &EscalationCommit{
					Body:    string(code) + ": test",
					Payload: escalationTestPayload(code, response.EscalationOriginPlanningResume),
				},
			})
			if err != nil {
				t.Fatalf("CommitHandlerResult: %v", err)
			}
			if !applied {
				t.Fatal("applied = false, want true")
			}

			qp := latestQuestionPayload(t, s, ticketID)
			if qp.Recommended != "a" {
				t.Errorf("question.Recommended = %q, want a", qp.Recommended)
			}
		})
	}
}

// TestCommitHandlerResult_EscalationOffersFileGrant proves an escalation
// whose payload carries a Grant adds option d, "Let task N also change
// PATH", after Retry and Abandon, without disturbing the recommendation;
// an escalation with no Grant offers only Retry and Abandon (plan #51,
// design rule 2).
func TestCommitHandlerResult_EscalationOffersFileGrant(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStateBuilding)

	owner, expires := claimForCommit(t, s, ticketID)
	payload := escalationTestPayload(response.EscalationCodePlanGap, response.EscalationOriginBuild)
	payload.Grant = &response.FileGrant{Task: 2, Paths: []string{"x.go"}}
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Escalation: &EscalationCommit{Body: "plan_gap: x.go belongs to another task", Payload: payload},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	qp := latestQuestionPayload(t, s, ticketID)
	wantWithGrant := []response.Option{
		{Key: "a", Text: escalationOptionRetry},
		{Key: "c", Text: escalationOptionAbandon},
		{Key: "d", Text: "Let task 2 also change x.go"},
	}
	if !reflect.DeepEqual(qp.Options, wantWithGrant) {
		t.Errorf("question.Options = %+v, want %+v", qp.Options, wantWithGrant)
	}
	if qp.Recommended != "a" {
		t.Errorf("question.Recommended = %q, want a", qp.Recommended)
	}

	_, ticketID2 := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, ticketID2, testStateBuilding)
	owner2, expires2 := claimForCommit(t, s, ticketID2)
	applied, err = s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID2, Owner: owner2, Expires: expires2,
		Escalation: &EscalationCommit{
			Body:    "plan_gap: no grant here",
			Payload: escalationTestPayload(response.EscalationCodePlanGap, response.EscalationOriginBuild),
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}
	qp2 := latestQuestionPayload(t, s, ticketID2)
	if !reflect.DeepEqual(qp2.Options, wantEscalationOptionsPostSeal) {
		t.Errorf("question.Options = %+v, want %+v", qp2.Options, wantEscalationOptionsPostSeal)
	}
}

// TestCommitHandlerResult_EscalationExtraOptions proves EscalationCommit's
// ExtraOptions and Recommended fields (ticket 60): ExtraOptions insert
// before Abandon, in slice order, on both the post-seal two-option table and
// the planning three-option table; Recommended overrides
// escalationOptionsFor's own pick; a duplicate option key (ExtraOptions
// colliding with the file grant's own "d") rolls the whole commit back; and
// a Recommended naming no option key does too.
func TestCommitHandlerResult_EscalationExtraOptions(t *testing.T) {
	t.Parallel()

	t.Run("reviewing inserts before abandon and recommends", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStateReviewing)

		owner, expires := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{
				Body:         "loops_exhausted: review findings remain",
				Payload:      escalationTestPayload(response.EscalationCodeLoopsExhausted, response.EscalationOriginReview),
				ExtraOptions: []response.Option{{Key: "d", Text: testExtraOptionAcceptXText}},
				Recommended:  "d",
			},
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}
		if !applied {
			t.Fatal("applied = false, want true")
		}

		qp := latestQuestionPayload(t, s, ticketID)
		want := []response.Option{
			{Key: "a", Text: escalationOptionRetry},
			{Key: "d", Text: testExtraOptionAcceptXText},
			{Key: "c", Text: escalationOptionAbandon},
		}
		if !reflect.DeepEqual(qp.Options, want) {
			t.Errorf("question.Options = %+v, want %+v", qp.Options, want)
		}
		if qp.Recommended != "d" {
			t.Errorf("question.Recommended = %q, want d", qp.Recommended)
		}
	})

	t.Run("planning inserts before abandon and recommends", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStatePlanning)

		owner, expires := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{
				Body:         "loops_exhausted: review findings remain",
				Payload:      escalationTestPayload(response.EscalationCodeLoopsExhausted, response.EscalationOriginReview),
				ExtraOptions: []response.Option{{Key: "d", Text: testExtraOptionAcceptXText}},
				Recommended:  "d",
			},
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}
		if !applied {
			t.Fatal("applied = false, want true")
		}

		qp := latestQuestionPayload(t, s, ticketID)
		want := []response.Option{
			{Key: "a", Text: escalationOptionRetry},
			{Key: "b", Text: escalationOptionBackToPlanning},
			{Key: "d", Text: testExtraOptionAcceptXText},
			{Key: "c", Text: escalationOptionAbandon},
		}
		if !reflect.DeepEqual(qp.Options, want) {
			t.Errorf("question.Options = %+v, want %+v", qp.Options, want)
		}
		if qp.Recommended != "d" {
			t.Errorf("question.Recommended = %q, want d", qp.Recommended)
		}
	})

	t.Run("extra option colliding with grant key rolls back", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStateBuilding)

		owner, expires := claimForCommit(t, s, ticketID)
		payload := escalationTestPayload(response.EscalationCodePlanGap, response.EscalationOriginBuild)
		payload.Grant = &response.FileGrant{Task: 2, Paths: []string{"x.go"}}
		_, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{
				Body:         testEscalationBodyPlanGap,
				Payload:      payload,
				ExtraOptions: []response.Option{{Key: "d", Text: testExtraOptionAcceptXText}},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "duplicate option key") {
			t.Fatalf("CommitHandlerResult error = %v, want duplicate option key", err)
		}

		open, err := s.QuestionsByState(ctx, ticketID, string(response.QuestionStateOpen))
		if err != nil {
			t.Fatalf("QuestionsByState: %v", err)
		}
		if len(open) != 0 {
			t.Errorf("QuestionsByState(open) = %d rows, want 0", len(open))
		}
	})

	t.Run("bad recommendation rolls back", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStateBuilding)

		owner, expires := claimForCommit(t, s, ticketID)
		_, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{
				Body:        testEscalationBodyPlanGap,
				Payload:     escalationTestPayload(response.EscalationCodePlanGap, response.EscalationOriginBuild),
				Recommended: "z",
			},
		})
		if err == nil || !strings.Contains(err.Error(), "is not an option key") {
			t.Fatalf("CommitHandlerResult error = %v, want is not an option key", err)
		}

		open, err := s.QuestionsByState(ctx, ticketID, string(response.QuestionStateOpen))
		if err != nil {
			t.Fatalf("QuestionsByState: %v", err)
		}
		if len(open) != 0 {
			t.Errorf("QuestionsByState(open) = %d rows, want 0", len(open))
		}
	})
}

// grantTestPlan returns a 6-task plan whose a.go names task "6" and whose
// b.go names task "2 6", the fixture TestCommitHandlerResult_
// GrantFilesUpdatesPlanAndAudits grants task 2 against.
func grantTestPlan(t *testing.T) response.Plan {
	t.Helper()
	tasks := make([]response.Task, 6)
	for i := range tasks {
		tasks[i] = response.Task{N: i + 1, Test: "TestX", Text: "do it"}
	}
	files := []response.FileChange{
		{Path: testRefAGo, Action: response.FileActionModify, Task: "6", Reason: "task 6 touches a.go"},
		{Path: testRefBGo, Action: response.FileActionModify, Task: testTasks2And6, Reason: "task 2 and 6 touch b.go"},
	}
	return planWithTasks(t, tasks, files)
}

// testTasks2And6 is grantTestPlan's and TestCommitHandlerResult_
// GrantFilesUpdatesPlanAndAudits' own "2 6" result literal (goconst).
const testTasks2And6 = "2 6"

// readPlanPayload reads back ticketID's newest plan artifact's raw payload
// bytes, for a byte-for-byte unchanged check.
func readPlanPayload(t *testing.T, s *Store, ticketID int64) []byte {
	t.Helper()
	var payload []byte
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT payload FROM artifacts WHERE ticket_id = ? AND type = 'plan' ORDER BY version DESC LIMIT 1`,
		ticketID,
	).Scan(&payload); err != nil {
		t.Fatalf("read back plan: %v", err)
	}
	return payload
}

// TestCommitHandlerResult_GrantFilesUpdatesPlanAndAudits proves
// CommitHandlerResult's GrantFiles step (plan #51, design rule 6): it
// rewrites the stored plan in place (no new plan artifact row), grants
// every named path whose task list is non-empty and lacks the grantee,
// ignores a path the plan does not declare, and writes exactly one
// owner_edit event per file that actually changed. A grant that fails
// checkPlanStructure (task 9 does not exist on a 6-task plan) errors and
// leaves the stored plan's bytes untouched.
func TestCommitHandlerResult_GrantFilesUpdatesPlanAndAudits(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStateBuilding)
	insertPlanArtifactPayload(t, s, ticketID, nil, grantTestPlan(t))

	owner, expires := claimForCommit(t, s, ticketID)
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		GrantFiles: &response.FileGrant{Task: 2, Paths: []string{testRefAGo, testRefBGo, "missing.go"}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	if n := countPlanArtifacts(t, s, ticketID); n != 1 {
		t.Errorf("plan artifacts = %d, want 1 (rewritten in place)", n)
	}
	plan, _, ok, err := s.StoredPlan(ctx, ticketID)
	if err != nil {
		t.Fatalf("StoredPlan: %v", err)
	}
	if !ok {
		t.Fatal("StoredPlan: ok = false, want true")
	}
	if got := fileTaskByPath(plan.Delivery.Files, testRefAGo); got != testTasks2And6 {
		t.Errorf("a.go task = %q, want %q", got, testTasks2And6)
	}
	if got := fileTaskByPath(plan.Delivery.Files, testRefBGo); got != testTasks2And6 {
		t.Errorf("b.go task = %q, want %q", got, testTasks2And6)
	}

	events := ownerEditEvents(t, s, ticketID)
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err = json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if ev.Target != OwnerEditPlanFile || ev.Ref != testRefAGo || ev.Old != "6" || ev.New != testTasks2And6 {
		t.Errorf("event = %+v, want target plan_file ref a.go old 6 new 2 6", ev)
	}

	beforeSecond := readPlanPayload(t, s, ticketID)
	owner2, expires2 := claimForCommit(t, s, ticketID)
	_, err = s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner2, Expires: expires2,
		GrantFiles: &response.FileGrant{Task: 9, Paths: []string{testRefAGo}},
	})
	if err == nil {
		t.Fatal("CommitHandlerResult(grant task 9): err = nil, want error")
	}
	if !strings.Contains(err.Error(), "grant plan files") {
		t.Errorf("err = %q, want it to name grant plan files (checkPlanStructure's own fault)", err.Error())
	}
	if got := readPlanPayload(t, s, ticketID); !bytes.Equal(got, beforeSecond) {
		t.Errorf("plan bytes changed after the refused grant")
	}
	if got := ownerEditEvents(t, s, ticketID); len(got) != 1 {
		t.Errorf("owner_edit events after the refused grant = %d, want still 1 (the rollback wrote none)", len(got))
	}
}

// TestCommitHandlerResult_GrantFilesNoPlanRefusesCommit proves
// grantPlanFilesTx's own load failure branch: a commit whose GrantFiles
// names a ticket with no plan artifact at all errors (sql.ErrNoRows is
// not special-cased), and the whole commit rolls back, writing neither an
// artifact nor an owner_edit event.
func TestCommitHandlerResult_GrantFilesNoPlanRefusesCommit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStateBuilding)

	owner, expires := claimForCommit(t, s, ticketID)
	_, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		GrantFiles: &response.FileGrant{Task: 2, Paths: []string{testRefAGo}},
	})
	if err == nil {
		t.Fatal("CommitHandlerResult: err = nil, want error (no plan artifact)")
	}
	if !strings.Contains(err.Error(), "grant plan files") {
		t.Errorf("err = %q, want it to name grant plan files", err.Error())
	}
	if n := countPlanArtifacts(t, s, ticketID); n != 0 {
		t.Errorf("plan artifacts = %d, want 0", n)
	}
	if got := ownerEditEvents(t, s, ticketID); len(got) != 0 {
		t.Errorf("owner_edit events = %d, want 0 (the rollback wrote none)", len(got))
	}
}

// TestCommitHandlerResult_TrackerEffectDoesNotChangeTheTransactionsWrites
// proves TrackerEffect is carried, never applied, by CommitHandlerResult
// (design D12): a commit with and without an identical TrackerEffect writes
// the same rows either way.
func TestCommitHandlerResult_TrackerEffectDoesNotChangeTheTransactionsWrites(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// --- CommitHandlerResult: Conversation (design section 22.3, D31) ---------

// TestCommitConversationSettlesWithDecisionRow proves a Settle entry with
// no late owner message moves the question to "resolved" and inserts one
// zing-authored "resolved" row, parented to the question, carrying the
// commit's own run id and the decision text.
func TestCommitConversationSettlesWithDecisionRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")

	owner, expires := claimForCommit(t, s, ticketID)
	decision := "Agreed, no ldflags."

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &sessID},
		Runs:    []Run{{Turn: 1, Outcome: new(testTypeQuestion)}},
		Conversation: &ConversationCommit{
			ThroughBatch: 0,
			Settle:       []SettleQuestion{{QuestionID: qID, Decision: decision}},
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM messages WHERE id = ?`, qID).Scan(&state); err != nil {
		t.Fatalf("read question state: %v", err)
	}
	if state != questionStateResolved {
		t.Errorf("question state = %q, want %q", state, questionStateResolved)
	}

	var author string
	var runID int64
	var body string
	row := s.db.QueryRowContext(ctx,
		`SELECT author, run_id, body FROM messages WHERE type = ? AND parent_id = ?`, msgTypeResolved, qID)
	if err := row.Scan(&author, &runID, &body); err != nil {
		t.Fatalf("read decision row: %v", err)
	}
	if author != authorZing {
		t.Errorf("decision row author = %q, want %q", author, authorZing)
	}
	if body != decision {
		t.Errorf("decision row body = %q, want %q", body, decision)
	}
	if runID == 0 {
		t.Error("decision row run_id = 0, want the commit's own run")
	}
}

// TestCommitConversationSettleMarksQuestionRead proves settling a question
// sets its read_at: the agent closing it implies the owner has seen it
// (commit.go applyConversationTx).
func TestCommitConversationSettleMarksQuestionRead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")

	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &sessID},
		Runs:    []Run{{Turn: 1, Outcome: new(testTypeQuestion)}},
		Conversation: &ConversationCommit{
			ThroughBatch: 0,
			Settle:       []SettleQuestion{{QuestionID: qID, Decision: "Agreed, no ldflags."}},
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	var readAt *string
	if err := s.db.QueryRowContext(ctx, `SELECT read_at FROM messages WHERE id = ?`, qID).Scan(&readAt); err != nil {
		t.Fatalf("read question read_at: %v", err)
	}
	if readAt == nil {
		t.Error("question read_at = nil, want set after settle")
	}
}

// TestCommitConversationDefersSettleOnLateMessage proves a Settle entry
// whose question carries a sent owner message above ThroughBatch is
// skipped: the question stays open, and no decision row is inserted
// (design section 22.3 step 2).
func TestCommitConversationDefersSettleOnLateMessage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")
	insertSentOwnerBatch(t, s, ticketID, qID, 5, "actually, wait")

	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &sessID},
		Runs:    []Run{{Turn: 1, Outcome: new(testTypeQuestion)}},
		Conversation: &ConversationCommit{
			ThroughBatch: 3, // below the late message's batch of 5
			Settle:       []SettleQuestion{{QuestionID: qID, Decision: "too late"}},
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM messages WHERE id = ?`, qID).Scan(&state); err != nil {
		t.Fatalf("read question state: %v", err)
	}
	if state != questionStateOpen {
		t.Errorf("question state = %q, want unchanged %q (settle deferred)", state, questionStateOpen)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE type = ? AND parent_id = ?`, msgTypeResolved, qID); n != 0 {
		t.Errorf("resolved rows for a deferred settle = %d, want 0", n)
	}
}

// TestCommitConversationClearsQuestionsWaitOnLateMessage proves the fence
// of design section 22.3 step 3: a commit that would wait on "questions",
// with no Escalation, writes waiting_on NULL instead when an unsettled
// planning question carries a late owner message.
func TestCommitConversationClearsQuestionsWaitOnLateMessage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")
	insertSentOwnerBatch(t, s, ticketID, qID, 5, "one more thing")

	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Conversation: &ConversationCommit{
			ThroughBatch: 3,
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.WaitingOn != nil {
		t.Errorf("ticket waiting_on = %q, want nil (fenced by the late message)", *got.WaitingOn)
	}
}

// TestCommitFenceWithdrawsGateOnLateReopen proves the D32 widening of the
// same fence to "gate" (design section 22.12.2): a plan-review run can be
// in flight when the owner reopens a thread elsewhere, and its own clean
// commit posts a fresh gate question while that thread is still open. The
// fence clears waiting_on to nil exactly as it does for "questions", and
// also withdraws the gate question this same commit just inserted, in the
// same transaction.
func TestCommitFenceWithdrawsGateOnLateReopen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")
	insertSentOwnerBatch(t, s, ticketID, qID, 5, "one more thing")

	gatePayload, err := json.Marshal(response.QuestionPayload{
		Kind: response.QuestionKindGate, State: response.QuestionStateOpen,
		Recommended: "a", Options: []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
	})
	if err != nil {
		t.Fatalf("marshal gate payload: %v", err)
	}

	owner, expires := claimForCommit(t, s, ticketID)
	gateWaiting := waitingFlagGate
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: &gateWaiting,
		Messages: []Message{{
			TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing,
			State: new(questionStateOpen), Body: "the plan objective", Payload: gatePayload,
		}},
		Conversation: &ConversationCommit{ThroughBatch: 3},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.WaitingOn != nil {
		t.Errorf("ticket waiting_on = %q, want nil (fenced by the late reopen)", *got.WaitingOn)
	}

	var gateState string
	if err := s.db.QueryRowContext(ctx,
		`SELECT state FROM messages WHERE ticket_id = ? AND type = ? AND json_extract(payload, '$.kind') = 'gate'`,
		ticketID, msgTypeQuestion).Scan(&gateState); err != nil {
		t.Fatalf("read gate question state: %v", err)
	}
	if gateState != questionStateResolved {
		t.Errorf("gate question state = %q, want resolved (withdrawn)", gateState)
	}
}

// TestCommitConversationKeepsEscalationWait proves the fence above never
// fires when the commit also carries an Escalation: the same late-message
// fixture, but waiting_on stays "questions".
func TestCommitConversationKeepsEscalationWait(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, run0ID, "Q1")
	insertSentOwnerBatch(t, s, ticketID, qID, 5, "one more thing")

	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Escalation: &EscalationCommit{
			RunID: nil, Body: testEscalationBodyWallClock,
			Payload: escalationTestPayload(response.EscalationCodeWallClock, response.EscalationOriginCapBudget),
		},
		Conversation: &ConversationCommit{
			ThroughBatch: 3,
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.WaitingOn == nil || *got.WaitingOn != testWaitingQuestions {
		t.Errorf("ticket waiting_on = %v, want %q (escalation open: the fence must not fire)", got.WaitingOn, testWaitingQuestions)
	}
}

// TestCommitConversationRejectsNonPlanningQuestion proves a Settle entry
// naming a question that is not a planning question (a gate question
// here) fails with the exact error text design section 22.3 specifies,
// rather than settling it.
func TestCommitConversationRejectsNonPlanningQuestion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run0ID := insertQuestionRun(t, s, sessID)
	gateID := insertQuestionOfKindWithRun(t, s, ticketID, run0ID, "Q1", response.QuestionKindGate)

	owner, expires := claimForCommit(t, s, ticketID)

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Conversation: &ConversationCommit{
			Settle: []SettleQuestion{{QuestionID: gateID, Decision: "not actually planning"}},
		},
	})
	wantErr := fmt.Sprintf("commit handler result: settle question %d: not an open planning question of ticket %d", gateID, ticketID)
	if err == nil || err.Error() != wantErr {
		t.Fatalf("CommitHandlerResult error = %v, want %q", err, wantErr)
	}
	if applied {
		t.Error("applied = true, want false")
	}
}
