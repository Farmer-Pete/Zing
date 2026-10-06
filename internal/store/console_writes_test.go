package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"zing/internal/response"
)

// testConflictQuestionClosed is the "question closed" conflict reason,
// named once so goconst has nothing to flag across this file's several
// closed-question tests (D30 and D31 alike).
const testConflictQuestionClosed = "question closed"

// testReopenText is the owner's reopening reply body, shared by this
// file's several D32 reopen tests (goconst).
const testReopenText = "Print JSON too."

// draftQuestionPayload builds a QuestionPayload for kind, with options for
// an option kind or items for an item kind, marshaled the way a real
// planning (or gate, split, perimeter, review, merge) commit would write it
// (messages/question.json requires "options" even when empty).
func draftQuestionPayload(t *testing.T, key string, kind response.QuestionKind, options []response.Option, items []response.Item) []byte {
	t.Helper()
	if options == nil {
		options = []response.Option{}
	}
	b, err := json.Marshal(response.QuestionPayload{
		Key: key, Kind: kind, State: response.QuestionState(questionStateOpen),
		Recommended: "a", Options: options, Items: items,
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	return b
}

// insertQuestionOfKind inserts an "open" question message of kind directly
// (fixture setup, not the code under test), with no run attached, matching
// the run-less shape a first-entry commit writes (design section 6.3).
func insertQuestionOfKind(t *testing.T, s *Store, ticketID int64, key string, kind response.QuestionKind, options []response.Option, items []response.Item) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, Type: msgTypeQuestion, Author: testAuthorZing,
		State: new(questionStateOpen), Body: key + "\n\nbody",
		Payload: draftQuestionPayload(t, key, kind, options, items),
	})
	if err != nil {
		t.Fatalf("insert %s question %s: %v", kind, key, err)
	}
	return id
}

var optionsAB = []response.Option{{Key: "a", Text: "Plain hello"}, {Key: "b", Text: "hello, world"}}

// testReplyWhyThough is a free-reply body repeated across this file's
// question-reply tests, named once so goconst has nothing to flag.
const testReplyWhyThough = "why though"

// insertQuestionOption is insertQuestionOfKind for the "question" kind with
// the standard two-option payload every SaveDraft option test drives.
func insertQuestionOption(t *testing.T, s *Store, ticketID int64, key string) int64 {
	t.Helper()
	return insertQuestionOfKind(t, s, ticketID, key, response.QuestionKindQuestion, optionsAB, nil)
}

// closeQuestion sets id's lifecycle state directly, past SaveDraft and
// SendBatch, to arrange the "closed question" conflict fixture.
func closeQuestion(t *testing.T, s *Store, id int64, state string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE messages SET state = ? WHERE id = ?`, state, id); err != nil {
		t.Fatalf("close question %d: %v", id, err)
	}
}

// draftPayloadOf reads back id's stored payload, decoded into an
// AnswerPayload, for a SaveDraft/SendBatch test to assert against.
func draftPayloadOf(t *testing.T, s *Store, id int64) response.AnswerPayload {
	t.Helper()
	m, err := s.GetMessage(t.Context(), id)
	if err != nil {
		t.Fatalf("GetMessage(%d): %v", id, err)
	}
	var ap response.AnswerPayload
	if err := json.Unmarshal(m.Payload, &ap); err != nil {
		t.Fatalf("unmarshal draft payload: %v", err)
	}
	return ap
}

func conflictReason(t *testing.T, err error) string {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v (%T), want a *ConflictError", err, err)
	}
	return ce.Reason
}

// ---- SaveDraft: option answers -------------------------------------------

func TestSaveDraft_OptionUpsertsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	opt := "a"
	res1, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt})
	if err != nil {
		t.Fatalf("SaveDraft (fresh): %v", err)
	}
	if res1.Replaced {
		t.Error("fresh draft: Replaced = true, want false")
	}
	if got := draftPayloadOf(t, s, res1.MessageID); got.Option == nil || *got.Option != "a" {
		t.Errorf("stored option = %v, want \"a\"", got.Option)
	}

	// A repeat with the same value is idempotent: same row, Replaced=false.
	res2, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt})
	if err != nil {
		t.Fatalf("SaveDraft (repeat): %v", err)
	}
	if res2.MessageID != res1.MessageID {
		t.Errorf("repeat MessageID = %d, want the same row %d", res2.MessageID, res1.MessageID)
	}
	if res2.Replaced {
		t.Error("repeat with the same option: Replaced = true, want false")
	}

	// A different value upserts the same row and reports Replaced=true.
	optB := "b"
	res3, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &optB})
	if err != nil {
		t.Fatalf("SaveDraft (replace): %v", err)
	}
	if res3.MessageID != res1.MessageID {
		t.Errorf("replace MessageID = %d, want the same row %d", res3.MessageID, res1.MessageID)
	}
	if !res3.Replaced {
		t.Error("replace with a different option: Replaced = false, want true")
	}
	if got := draftPayloadOf(t, s, res1.MessageID); got.Option == nil || *got.Option != "b" {
		t.Errorf("stored option after replace = %v, want \"b\"", got.Option)
	}
}

// ---- SaveDraft: item answers ----------------------------------------------

func TestSaveDraft_ItemMergesFirstPickThenReplacesOne(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}, {Ref: testRefBGo, Text: "b"}}
	// review, not perimeter: this test exercises drop and discuss, which a
	// perimeter item no longer accepts (design section 4.2).
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindReview, nil, items)

	res1, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionAccept},
	})
	if err != nil {
		t.Fatalf("SaveDraft (first pick): %v", err)
	}
	if res1.Replaced {
		t.Error("first pick: Replaced = true, want false")
	}
	got := draftPayloadOf(t, s, res1.MessageID)
	if len(got.Items) != 1 || got.Items[testRefAGo] != response.DecisionAccept {
		t.Fatalf("items after first pick = %v, want {a.go: accept}", got.Items)
	}

	// A second ref merges in alongside the first.
	res2, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefBGo, Decision: response.DecisionDiscuss},
	})
	if err != nil {
		t.Fatalf("SaveDraft (second ref): %v", err)
	}
	if res2.MessageID != res1.MessageID {
		t.Errorf("second ref MessageID = %d, want the same row %d", res2.MessageID, res1.MessageID)
	}
	got = draftPayloadOf(t, s, res1.MessageID)
	if len(got.Items) != 2 || got.Items[testRefAGo] != response.DecisionAccept || got.Items[testRefBGo] != response.DecisionDiscuss {
		t.Fatalf("items after second ref = %v, want {a.go: accept, b.go: discuss}", got.Items)
	}

	// Replacing the first ref's decision keeps the row, changes only that entry.
	res3, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionDrop},
	})
	if err != nil {
		t.Fatalf("SaveDraft (replace one): %v", err)
	}
	if !res3.Replaced {
		t.Error("replacing a.go's decision: Replaced = false, want true")
	}
	got = draftPayloadOf(t, s, res1.MessageID)
	if len(got.Items) != 2 || got.Items[testRefAGo] != response.DecisionDrop || got.Items[testRefBGo] != response.DecisionDiscuss {
		t.Fatalf("items after replace one = %v, want {a.go: drop, b.go: discuss}", got.Items)
	}
}

// TestSaveDraftReviewDecision proves the section 4.2 rule: a review item
// takes accept, drop, or discuss; reject is refused with the conflict "a
// review item takes accept, drop, or discuss".
func TestSaveDraftReviewDecision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}}

	tests := []struct {
		name     string
		key      string
		decision response.Decision
		wantErr  string
	}{
		{"accept saves", "Q1", response.DecisionAccept, ""},
		{"drop saves", "Q2", response.DecisionDrop, ""},
		{"discuss saves", "Q3", response.DecisionDiscuss, ""},
		{"reject is refused", "Q4", response.DecisionReject, "a review item takes accept, drop, or discuss"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qID := insertQuestionOfKind(t, s, ticketID, tc.key, response.QuestionKindReview, nil, items)
			_, err := s.SaveDraft(t.Context(), DraftInput{
				TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: tc.decision},
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("SaveDraft(decision=%s): %v, want nil", tc.decision, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("SaveDraft(decision=%s): err = nil, want a ConflictError", tc.decision)
			}
			if got := conflictReason(t, err); got != tc.wantErr {
				t.Errorf("conflict reason = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

// TestSaveDraftPerimeterDecision proves the section 4.2 rule: a perimeter
// item takes accept or reject; accept and reject save, drop and discuss
// return the conflict "a perimeter item takes accept or reject".
func TestSaveDraftPerimeterDecision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}}

	tests := []struct {
		name     string
		key      string
		decision response.Decision
		wantErr  string
	}{
		{"accept saves", "Q1", response.DecisionAccept, ""},
		{"reject saves", "Q2", response.DecisionReject, ""},
		{"drop is refused", "Q3", response.DecisionDrop, "a perimeter item takes accept or reject"},
		{"discuss is refused", "Q4", response.DecisionDiscuss, "a perimeter item takes accept or reject"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qID := insertQuestionOfKind(t, s, ticketID, tc.key, response.QuestionKindPerimeter, nil, items)
			_, err := s.SaveDraft(t.Context(), DraftInput{
				TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: tc.decision},
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("SaveDraft(decision=%s): %v, want nil", tc.decision, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("SaveDraft(decision=%s): err = nil, want a ConflictError", tc.decision)
			}
			if got := conflictReason(t, err); got != tc.wantErr {
				t.Errorf("conflict reason = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

// ---- SaveDraft: replies ----------------------------------------------------

func TestSaveDraft_QuestionReplyAndThreadReply(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	qReply, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough})
	if err != nil {
		t.Fatalf("SaveDraft (question reply): %v", err)
	}
	m, err := s.GetMessage(t.Context(), qReply.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(question reply): %v", err)
	}
	if m.Type != msgTypeReply || m.ParentID == nil || *m.ParentID != qID || m.Body != testReplyWhyThough {
		t.Errorf("question reply row = %+v, want type=reply parent_id=%d body=%q", m, qID, testReplyWhyThough)
	}
	if m.State == nil || *m.State != draftState {
		t.Errorf("question reply state = %v, want %q", m.State, draftState)
	}

	threadReply, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, Text: "unrelated note"})
	if err != nil {
		t.Fatalf("SaveDraft (thread reply): %v", err)
	}
	m2, err := s.GetMessage(t.Context(), threadReply.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(thread reply): %v", err)
	}
	if m2.Type != msgTypeReply || m2.ParentID != nil || m2.Body != "unrelated note" {
		t.Errorf("thread reply row = %+v, want type=reply parent_id=nil body=%q", m2, "unrelated note")
	}
}

// ---- SaveDraft: conflicts ---------------------------------------------------

func TestSaveDraft_Conflicts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketA := seedQueuedTicket(t, s, "a")
	_, ticketB := seedQueuedTicket(t, s, "b")
	qID := insertQuestionOption(t, s, ticketA, "Q1")

	resolvedQID := insertQuestionOption(t, s, ticketA, "Q2")
	closeQuestion(t, s, resolvedQID, questionStateResolved)

	opt := "a"
	badOpt := "z"
	items := []response.Item{{Ref: testRefAGo, Text: "a"}}
	itemQID := insertQuestionOfKind(t, s, ticketA, "Q3", response.QuestionKindPerimeter, nil, items)

	tests := []struct {
		name string
		in   DraftInput
		want string
	}{
		{"closed question", DraftInput{TicketID: ticketA, QuestionID: &resolvedQID, Option: &opt}, testConflictQuestionClosed},
		{"wrong ticket", DraftInput{TicketID: ticketB, QuestionID: &qID, Option: &opt}, "wrong ticket"},
		{"bad option", DraftInput{TicketID: ticketA, QuestionID: &qID, Option: &badOpt}, "missing option"},
		{
			"bad item ref",
			DraftInput{TicketID: ticketA, QuestionID: &itemQID, Item: &ItemDecision{Ref: "nope.go", Decision: response.DecisionAccept}},
			"missing item",
		},
		{"ambiguous mode", DraftInput{TicketID: ticketA, QuestionID: &qID, Option: &opt, Text: "also this"}, "ambiguous draft mode"},
		// Empty text against a question clears its reply draft instead of
		// conflicting (TestSaveDraft_EmptyTextClearsQuestionReplyDraft); only a
		// thread reply (no question) keeps the "empty text" conflict.
		{"empty text", DraftInput{TicketID: ticketA}, "empty text"},
		{"question not found", DraftInput{TicketID: ticketA, QuestionID: new(int64(999999)), Option: &opt}, "question not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.SaveDraft(t.Context(), tc.in)
			if err == nil {
				t.Fatal("SaveDraft: err = nil, want a ConflictError")
			}
			if got := conflictReason(t, err); got != tc.want {
				t.Errorf("conflict reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSaveDraft_AnsweredQuestionDraftableWhileWaitingOnQuestions proves D30:
// a draft against a question already state=answered is allowed, not
// "question closed", while its ticket's waiting_on is still "questions" --
// the agent has not yet resumed with that round, so a revised pick still
// lands where the resume prompt will read it.
func TestSaveDraft_AnsweredQuestionDraftableWhileWaitingOnQuestions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	qID := insertQuestionOption(t, s, ticketID, "Q1")
	closeQuestion(t, s, qID, questionStateAnswered)

	opt := "b"
	result, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt})
	if err != nil {
		t.Fatalf("SaveDraft against an answered question while still waiting: %v", err)
	}
	if ap := draftPayloadOf(t, s, result.MessageID); ap.Option == nil || *ap.Option != "b" {
		t.Errorf("draft option = %v, want \"b\"", ap.Option)
	}
}

// TestSaveDraft_AnsweredQuestionConflictsOnceWaitCleared proves D30's other
// half: the same draft, once the ticket's wait has cleared (the agent was
// already resumed with this round), is an ordinary "question closed"
// conflict, same as any other closed question.
func TestSaveDraft_AnsweredQuestionConflictsOnceWaitCleared(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")
	closeQuestion(t, s, qID, questionStateAnswered)
	// ticketID's waiting_on is left nil (seedQueuedTicket's own default):
	// the wait has already cleared, as it would once SendBatch resumes the
	// agent with every question of the round answered.

	opt := "b"
	_, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt})
	if err == nil {
		t.Fatal("SaveDraft: err = nil, want a ConflictError")
	}
	if got := conflictReason(t, err); got != testConflictQuestionClosed {
		t.Errorf("conflict reason = %q, want %q", got, testConflictQuestionClosed)
	}
}

// TestSaveDraft_EmptyTextClearsQuestionReplyDraft proves the autosave
// contract (design section 6.7): an empty Text against a question deletes
// that question's existing draft reply and reports Cleared=true, rather
// than the "empty text" conflict a thread reply still gets. A second empty
// save, with no draft left to clear, reports Cleared=false with no error.
func TestSaveDraft_EmptyTextClearsQuestionReplyDraft(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	saved, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough})
	if err != nil {
		t.Fatalf("SaveDraft (text): %v", err)
	}

	cleared, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: ""})
	if err != nil {
		t.Fatalf("SaveDraft (empty text): %v", err)
	}
	if !cleared.Cleared {
		t.Error("SaveDraft (empty text): Cleared = false, want true")
	}
	if cleared.MessageID != saved.MessageID {
		t.Errorf("SaveDraft (empty text): MessageID = %d, want the deleted row's id %d", cleared.MessageID, saved.MessageID)
	}

	messages, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := range messages {
		m := &messages[i]
		if m.ID == saved.MessageID {
			t.Fatalf("draft reply %d still exists after an empty-text save", saved.MessageID)
		}
	}

	// A second empty save finds no draft left to clear: no error, Cleared=false.
	again, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: ""})
	if err != nil {
		t.Fatalf("SaveDraft (second empty text): %v", err)
	}
	if again.Cleared {
		t.Error("SaveDraft (second empty text): Cleared = true, want false (nothing left to clear)")
	}
	if again.MessageID != 0 {
		t.Errorf("SaveDraft (second empty text): MessageID = %d, want 0 (nothing was deleted)", again.MessageID)
	}
}

// TestSaveDraft_EmptyTextAgainstClosedQuestionConflicts proves the relaxed
// n==0 guard still runs openQuestionForTicketTx before it ever reaches
// clearReplyDraftTx: an empty-text autosave against a question that has
// already closed is still the ordinary "question closed" conflict, not a
// silent delete of whatever draft reply that question still has. The
// fixture saves a text draft before closing the question, and asserts that
// draft row survives the conflict, proving the conflict path returned
// before ever reaching clearReplyDraftTx.
func TestSaveDraft_EmptyTextAgainstClosedQuestionConflicts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	saved, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough})
	if err != nil {
		t.Fatalf("SaveDraft (text): %v", err)
	}

	closeQuestion(t, s, qID, questionStateResolved)

	_, err = s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: ""})
	if err == nil {
		t.Fatal("SaveDraft (empty text, closed question): err = nil, want a ConflictError")
	}
	if got := conflictReason(t, err); got != testConflictQuestionClosed {
		t.Errorf("conflict reason = %q, want %q", got, testConflictQuestionClosed)
	}

	messages, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	found := false
	for i := range messages {
		if messages[i].ID == saved.MessageID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("draft reply %d no longer exists after the closed-question conflict: clearReplyDraftTx ran despite the conflict", saved.MessageID)
	}
}

// testReplyFromTabA is the stored body TestSaveDraft_StaleBaseConflicts
// drives its whole fixture around, named once so goconst has nothing to
// flag across its several assertions.
const testReplyFromTabA = "from tab A"

// TestSaveDraft_StaleBaseConflicts proves the base check (ticket #43): a
// save that carries the text this tab last saw stored (Base) is refused,
// with the stored text returned as ConflictError.Current, when another
// tab's save has since moved the stored body away from both Base and the
// text this call is about to write. Base is ignored, as always, when it is
// nil, matching every SaveDraft call before this ticket.
func TestSaveDraft_StaleBaseConflicts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	tabA, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: testReplyFromTabA})
	if err != nil {
		t.Fatalf("SaveDraft(tab A): %v", err)
	}

	// Tab B never saw tab A's save: its base is still the page-load value,
	// empty. The stored body ("from tab A") differs from both that base and
	// tab B's own text, so the save conflicts and the row is untouched.
	_, err = s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "from tab B", Base: new("")})
	if err == nil {
		t.Fatal("SaveDraft(tab B) err = nil, want a ConflictError")
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("SaveDraft(tab B) error = %v (%T), want a *ConflictError", err, err)
	}
	if ce.Reason != changedInAnotherTabReason {
		t.Errorf("SaveDraft(tab B) conflict reason = %q, want %q", ce.Reason, changedInAnotherTabReason)
	}
	if ce.Current == nil || *ce.Current != testReplyFromTabA {
		t.Errorf("SaveDraft(tab B) conflict Current = %v, want %q", ce.Current, testReplyFromTabA)
	}
	m, getErr := s.GetMessage(t.Context(), tabA.MessageID)
	if getErr != nil {
		t.Fatalf("GetMessage: %v", getErr)
	}
	if got := m.Body; got != testReplyFromTabA {
		t.Errorf("stored body = %q, want %q", got, testReplyFromTabA)
	}

	// Clearing (empty text) against the same stale base conflicts too.
	_, err = s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "", Base: new("")})
	if err == nil {
		t.Fatal("SaveDraft(tab B clear) err = nil, want a ConflictError")
	}
	if got := conflictReason(t, err); got != changedInAnotherTabReason {
		t.Errorf("SaveDraft(tab B clear) conflict reason = %q, want %q", got, changedInAnotherTabReason)
	}
	if _, getErr := s.GetMessage(t.Context(), tabA.MessageID); getErr != nil {
		t.Errorf("draft reply %d gone after a clear that should have conflicted: %v", tabA.MessageID, getErr)
	}

	// Saving the exact stored text, even with a stale base, is not a
	// conflict: there is nothing tab B would overwrite that it did not
	// already intend to write.
	same, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: testReplyFromTabA, Base: new("something stale")})
	if err != nil {
		t.Fatalf("SaveDraft(same text, stale base): %v", err)
	}
	if same.MessageID != tabA.MessageID {
		t.Errorf("SaveDraft(same text, stale base) MessageID = %d, want %d", same.MessageID, tabA.MessageID)
	}
	if m, getErr := s.GetMessage(t.Context(), tabA.MessageID); getErr != nil {
		t.Fatalf("GetMessage: %v", getErr)
	} else if m.Body != testReplyFromTabA {
		t.Errorf("SaveDraft(same text, stale base) stored body = %q, want %q", m.Body, testReplyFromTabA)
	}

	// A nil base keeps overwriting unconditionally, as before this ticket.
	overwritten, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "no base at all"})
	if err != nil {
		t.Fatalf("SaveDraft(nil base): %v", err)
	}
	if overwritten.MessageID != tabA.MessageID || !overwritten.Replaced {
		t.Errorf("SaveDraft(nil base) = %+v, want MessageID=%d Replaced=true", overwritten, tabA.MessageID)
	}
	if m, getErr := s.GetMessage(t.Context(), tabA.MessageID); getErr != nil {
		t.Fatalf("GetMessage: %v", getErr)
	} else if m.Body != "no base at all" {
		t.Errorf("SaveDraft(nil base) stored body = %q, want %q", m.Body, "no base at all")
	}

	// A base equal to the stored body updates cleanly.
	updated, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "caught up", Base: new("no base at all")})
	if err != nil {
		t.Fatalf("SaveDraft(base matches stored): %v", err)
	}
	if updated.MessageID != tabA.MessageID {
		t.Errorf("SaveDraft(base matches stored) MessageID = %d, want %d", updated.MessageID, tabA.MessageID)
	}
	if m, getErr := s.GetMessage(t.Context(), tabA.MessageID); getErr != nil {
		t.Fatalf("GetMessage: %v", getErr)
	} else if m.Body != "caught up" {
		t.Errorf("SaveDraft(base matches stored) stored body = %q, want %q", m.Body, "caught up")
	}

	// With no row at all, a non-empty base that matches neither the (empty)
	// current body nor the new text still conflicts, with Current empty.
	q2 := insertQuestionOption(t, s, ticketID, "Q2")
	_, err = s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q2, Text: "first text ever", Base: new("x")})
	if err == nil {
		t.Fatal("SaveDraft(no row, stale base) err = nil, want a ConflictError")
	}
	if got := conflictReason(t, err); got != changedInAnotherTabReason {
		t.Errorf("SaveDraft(no row, stale base) conflict reason = %q, want %q", got, changedInAnotherTabReason)
	}
	var ce2 *ConflictError
	if !errors.As(err, &ce2) {
		t.Fatalf("SaveDraft(no row, stale base) error = %v (%T), want a *ConflictError", err, err)
	}
	if ce2.Current == nil || *ce2.Current != "" {
		t.Errorf("SaveDraft(no row, stale base) conflict Current = %v, want empty", ce2.Current)
	}
}

// TestDraftFingerprint proves draftFingerprint (ticket #43's own log-safe
// stand-in for a draft's text or base) is a stable, 16-character hex digest
// that never contains the text it fingerprints -- a reader of the log must
// not be able to recover a short draft such as "ok" by hashing guesses.
func TestDraftFingerprint(t *testing.T) {
	t.Parallel()
	ok1 := draftFingerprint("ok")
	ok2 := draftFingerprint("ok")
	if ok1 != ok2 {
		t.Errorf("draftFingerprint(%q) = %q then %q, want the same value both times", "ok", ok1, ok2)
	}
	if len(ok1) != 16 {
		t.Errorf("len(draftFingerprint(%q)) = %d, want 16", "ok", len(ok1))
	}
	for _, c := range ok1 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("draftFingerprint(%q) = %q, want lowercase hex only", "ok", ok1)
			break
		}
	}
	no := draftFingerprint("no")
	if no == ok1 {
		t.Errorf("draftFingerprint(%q) = draftFingerprint(%q) = %q, want different values", "no", "ok", ok1)
	}
	// An unkeyed hex(sha256(s))[:16] would pass every check above, but the
	// key is the whole reason draftFingerprint exists instead of a plain
	// hash (a reader of the log could otherwise recover "ok" by guessing).
	// Comparing against that unkeyed digest catches a draftFingerprint that
	// dropped the HMAC key.
	unkeyed := sha256.Sum256([]byte("ok"))
	if unkeyedHex := hex.EncodeToString(unkeyed[:])[:16]; ok1 == unkeyedHex {
		t.Errorf("draftFingerprint(%q) = %q, matches the unkeyed sha256 digest: it is not keyed", "ok", ok1)
	}

	empty := draftFingerprint("")
	if len(empty) != 16 {
		t.Errorf("len(draftFingerprint(%q)) = %d, want 16", "", len(empty))
	}
}

// ---- SendBatch --------------------------------------------------------------

func TestSendBatch_EmptyIsSafe(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")

	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if !res.Empty || res.Sent != 0 || res.BatchID != 0 || res.WaitCleared {
		t.Errorf("SendBatch on an empty ticket = %+v, want Empty=true and everything else zero", res)
	}
}

// TestSendBatchOnly_SendsOnlyListedQuestion proves a scoped send leaves an
// unlisted question's draft untouched (ticket #43: a forgotten draft on
// another question must not go out with a later Cmd+Enter). The ticket
// stays waiting on "questions" throughout, since q2 is never answered, so a
// fresh draft on q1 is still draftable after its first answer round.
func TestSendBatchOnly_SendsOnlyListedQuestion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	q1 := insertQuestionOption(t, s, ticketID, "Q1")
	q2 := insertQuestionOption(t, s, ticketID, "Q2")

	r1, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1, Text: testReplyWhyThough})
	if err != nil {
		t.Fatalf("SaveDraft(q1): %v", err)
	}
	r2, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q2, Text: testReplyWhyThough})
	if err != nil {
		t.Fatalf("SaveDraft(q2): %v", err)
	}

	res, err := s.SendBatchOnly(t.Context(), ticketID, []int64{q1}, SendFromLoopback)
	if err != nil {
		t.Fatalf("SendBatchOnly(q1): %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("SendBatchOnly(q1) Sent = %d, want 1", res.Sent)
	}

	m1, err := s.GetMessage(t.Context(), r1.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(q1 reply): %v", err)
	}
	if m1.State == nil || *m1.State != answerStateSent {
		t.Errorf("q1 reply state = %v, want %q", m1.State, answerStateSent)
	}

	m2, err := s.GetMessage(t.Context(), r2.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(q2 reply): %v", err)
	}
	if m2.State == nil || *m2.State != draftState {
		t.Errorf("q2 reply state = %v, want %q", m2.State, draftState)
	}
	if m2.BatchID != nil {
		t.Errorf("q2 reply batch id = %v, want nil", *m2.BatchID)
	}

	// A fresh draft on q1, listed twice, still sends once.
	r1b, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1, Text: "one more time"})
	if err != nil {
		t.Fatalf("SaveDraft(q1, second round): %v", err)
	}
	res, err = s.SendBatchOnly(t.Context(), ticketID, []int64{q1, q1}, SendFromLoopback)
	if err != nil {
		t.Fatalf("SendBatchOnly([q1, q1]): %v", err)
	}
	if res.Sent != 1 {
		t.Errorf("SendBatchOnly([q1, q1]) Sent = %d, want 1", res.Sent)
	}
	m1b, err := s.GetMessage(t.Context(), r1b.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(q1 reply, second round): %v", err)
	}
	if m1b.State == nil || *m1b.State != answerStateSent {
		t.Errorf("q1 reply (second round) state = %v, want %q", m1b.State, answerStateSent)
	}

	// A question id with no draft at all sends nothing.
	res, err = s.SendBatchOnly(t.Context(), ticketID, []int64{999999999}, SendFromLoopback)
	if err != nil {
		t.Fatalf("SendBatchOnly(no draft): %v", err)
	}
	if !res.Empty {
		t.Errorf("SendBatchOnly(no draft).Empty = %v, want true", res.Empty)
	}
}

func TestSendBatch_LocksAllocatesOneBatchIDAndClearsAQuestionsWait(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Empty || res.Sent != 1 || res.BatchID != 1 {
		t.Errorf("SendBatch = %+v, want Sent=1 BatchID=1 Empty=false", res)
	}
	if !res.WaitCleared {
		t.Error("WaitCleared = false, want true (the only open question kind was answered)")
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil", *ticket.WaitingOn)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage(question): %v", err)
	}
	if q.State == nil || *q.State != questionStateAnswered {
		t.Errorf("question state = %v, want %q", q.State, questionStateAnswered)
	}
}

// TestSendBatch_MarksRepliedQuestionRead proves answering a question sets
// its read_at: the owner reading the answer draft implies they read the
// question it replies to (design section 6.7, console_writes.go SendBatch).
func TestSendBatch_MarksRepliedQuestionRead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage(question): %v", err)
	}
	if q.ReadAt == nil {
		t.Error("question ReadAt = nil, want set after answering")
	}
}

// TestSendBatch_ClearsAGateWaitButNeverErrorOrChildren's three subtests each
// open their own store rather than share one across t.Parallel() siblings:
// each seeds a ticket under the same fixed testProject name, and
// EnsureProject's own check-then-insert is not safe for two goroutines
// racing on the same project row.
func TestSendBatch_ClearsAGateWaitButNeverErrorOrChildren(t *testing.T) {
	t.Parallel()

	t.Run("gate wait cleared by a gate answer", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "gate")
		setTicketWaiting(t, s, ticketID, "gate")
		qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindGate, optionsAB, nil)
		opt := "a"
		if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		res, err := s.SendBatch(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		if !res.WaitCleared {
			t.Error("WaitCleared = false, want true for a fully answered gate wait")
		}
		ticket, err := s.GetTicket(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("GetTicket: %v", err)
		}
		if ticket.WaitingOn != nil {
			t.Errorf("ticket.WaitingOn = %q, want nil", *ticket.WaitingOn)
		}
	})

	t.Run("a reply-only batch never clears an error wait", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "err")
		setTicketWaiting(t, s, ticketID, "error")
		items := []response.Item{{Ref: "f1", Text: "finding"}}
		qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindPerimeter, nil, items)
		if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "looking into it"}); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		res, err := s.SendBatch(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		if res.WaitCleared {
			t.Error("WaitCleared = true, want false: error is not question-backed")
		}
		ticket, err := s.GetTicket(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("GetTicket: %v", err)
		}
		if ticket.WaitingOn == nil || *ticket.WaitingOn != "error" {
			t.Errorf("ticket.WaitingOn = %v, want unchanged \"error\"", ticket.WaitingOn)
		}
		// The reply still marks the question it targeted answered.
		q, err := s.GetMessage(t.Context(), qID)
		if err != nil {
			t.Fatalf("GetMessage(question): %v", err)
		}
		if q.State == nil || *q.State != questionStateAnswered {
			t.Errorf("question state = %v, want %q (a sent reply answers its question)", q.State, questionStateAnswered)
		}
	})

	t.Run("children wait is never cleared by SendBatch", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "children")
		setTicketWaiting(t, s, ticketID, "children")
		qID := insertQuestionOption(t, s, ticketID, "Q1")
		opt := "a"
		if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		res, err := s.SendBatch(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		if res.WaitCleared {
			t.Error("WaitCleared = true, want false: children is not question-backed")
		}
	})
}

func TestSendBatch_IncompleteItemAnswerLeavesQuestionOpen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}, {Ref: testRefBGo, Text: "b"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindPerimeter, nil, items)

	if _, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionAccept},
	}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Sent != 1 {
		t.Errorf("Sent = %d, want 1 (the draft still sends)", res.Sent)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state = %v, want unchanged %q (only one of two items decided)", q.State, questionStateOpen)
	}
}

// TestSendBatch_DiscardsStaleDraftAndSendsTheRest proves the PR review fix:
// a draft saved against a question that closes before it sends (a race with
// another writer, or simply time passing between the draft and the send) no
// longer wedges the whole batch behind a permanent 409, the way the old
// all-or-nothing revalidateBatchTx did. SendBatch instead discards the one
// stale draft outright and sends every other draft in the batch.
func TestSendBatch_DiscardsStaleDraftAndSendsTheRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)

	q1 := insertQuestionOption(t, s, ticketID, "Q1")
	q2 := insertQuestionOption(t, s, ticketID, "Q2")

	opt := "a"
	staleDraft, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1, Option: &opt})
	if err != nil {
		t.Fatalf("SaveDraft(q1): %v", err)
	}

	// q1 closes out from under its own draft (simulating a race with
	// another writer) between the draft and the send.
	closeQuestion(t, s, q1, questionStateResolved)

	if _, saveErr := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q2, Option: &opt}); saveErr != nil {
		t.Fatalf("SaveDraft(q2): %v", saveErr)
	}

	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Sent != 1 || res.Discarded != 1 || res.Empty {
		t.Errorf("SendBatch = %+v, want Sent=1 Discarded=1 Empty=false", res)
	}
	if !res.WaitCleared {
		t.Error("WaitCleared = false, want true: q1 was already closed and q2's send answers the only other open question of that kind")
	}

	// The stale q1 draft is gone outright, not left behind in any state.
	if _, getErr := s.GetMessage(t.Context(), staleDraft.MessageID); !errors.Is(getErr, sql.ErrNoRows) {
		t.Errorf("GetMessage(stale draft) err = %v, want sql.ErrNoRows (discarded)", getErr)
	}

	// q2's draft sent normally, and its question is answered.
	q2Msg, err := s.GetMessage(t.Context(), q2)
	if err != nil {
		t.Fatalf("GetMessage(q2 question): %v", err)
	}
	if q2Msg.State == nil || *q2Msg.State != questionStateAnswered {
		t.Errorf("q2 question state = %v, want %q", q2Msg.State, questionStateAnswered)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil", *ticket.WaitingOn)
	}

	// A retry after the fact stays safe: nothing left to send, and no
	// lingering stale row to keep tripping over.
	res2, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch (retry): %v", err)
	}
	if !res2.Empty || res2.Sent != 0 || res2.Discarded != 0 {
		t.Errorf("SendBatch (retry) = %+v, want Empty=true and everything else zero", res2)
	}
}

// TestSendBatch_AllStaleDiscardsAndClearsWait covers the all-stale path: the
// batch's only draft is for a question that has since closed, so SendBatch
// discards it and finds nothing to send, yet must still clear a now-obsolete
// wait so the ticket is not left blocked with no run to resume it.
func TestSendBatch_AllStaleDiscardsAndClearsWait(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)

	q1 := insertQuestionOption(t, s, ticketID, "Q1")
	opt := "a"
	staleDraft, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1, Option: &opt})
	if err != nil {
		t.Fatalf("SaveDraft(q1): %v", err)
	}

	// q1 closes under its own draft; it was the only open question.
	closeQuestion(t, s, q1, questionStateResolved)

	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if !res.Empty || res.Sent != 0 || res.Discarded != 1 {
		t.Errorf("SendBatch = %+v, want Empty=true Sent=0 Discarded=1", res)
	}
	if !res.WaitCleared {
		t.Error("WaitCleared = false, want true: no open question of the wait's kind remains after the discard")
	}

	if _, getErr := s.GetMessage(t.Context(), staleDraft.MessageID); !errors.Is(getErr, sql.ErrNoRows) {
		t.Errorf("GetMessage(stale draft) err = %v, want sql.ErrNoRows (discarded)", getErr)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil (the obsolete wait must clear)", *ticket.WaitingOn)
	}
}

func TestSendBatch_CommitsOnceUnderAConcurrentSend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")
	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]BatchResult, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.SendBatch(t.Context(), ticketID)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("SendBatch[%d]: %v", i, err)
		}
	}

	sentCount, emptyCount := 0, 0
	for i, res := range results {
		switch {
		case res.Sent == 1:
			sentCount++
			if res.BatchID != 1 {
				t.Errorf("result[%d].BatchID = %d, want 1", i, res.BatchID)
			}
		case res.Empty:
			emptyCount++
		default:
			t.Errorf("result[%d] = %+v, want either Sent=1 or Empty=true", i, res)
		}
	}
	if sentCount != 1 || emptyCount != 1 {
		t.Errorf("sentCount=%d emptyCount=%d, want exactly one of each (one draft, sent exactly once)", sentCount, emptyCount)
	}
}

// TestSendBatch_ItemCompletenessAccumulatesAcrossSends proves the review-fix
// contract: an item-kind question decided across two separate SendBatch
// calls is marked answered, and its wait clears, once every item has a
// decision from any send, not only the batch just sent.
func TestSendBatch_ItemCompletenessAccumulatesAcrossSends(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, string(response.QuestionKindPerimeter))
	items := []response.Item{{Ref: testRefAGo, Text: "a"}, {Ref: testRefBGo, Text: "b"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindPerimeter, nil, items)

	// Batch 1 decides only item A, and sends.
	if _, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionAccept},
	}); err != nil {
		t.Fatalf("SaveDraft(a.go): %v", err)
	}
	res1, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch (batch 1): %v", err)
	}
	if res1.Sent != 1 {
		t.Errorf("batch 1 Sent = %d, want 1", res1.Sent)
	}
	if res1.WaitCleared {
		t.Error("batch 1 WaitCleared = true, want false: only one of two items decided")
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage after batch 1: %v", err)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state after batch 1 = %v, want unchanged %q", q.State, questionStateOpen)
	}

	// Batch 2, a wholly separate SendBatch call, decides the remaining item B.
	if _, err = s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefBGo, Decision: response.DecisionReject},
	}); err != nil {
		t.Fatalf("SaveDraft(b.go): %v", err)
	}
	res2, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch (batch 2): %v", err)
	}
	if res2.Sent != 1 {
		t.Errorf("batch 2 Sent = %d, want 1", res2.Sent)
	}
	if !res2.WaitCleared {
		t.Error("batch 2 WaitCleared = false, want true: every item now has a decision across both sends")
	}

	q, err = s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage after batch 2: %v", err)
	}
	if q.State == nil || *q.State != questionStateAnswered {
		t.Errorf("question state after batch 2 = %v, want %q (both items decided across two sends)", q.State, questionStateAnswered)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil", *ticket.WaitingOn)
	}
}

// TestSendBatch_ReviewReplyLeavesQuestionOpen proves the ticket's fix 2: a
// free reply on a review question no longer answers it on its own. a.go is
// dropped, b.go is left undecided, and a question-level reply is sent in the
// same batch. The question must stay open, since one finding still lacks a
// decision; only the item-completeness check may close a review question.
func TestSendBatch_ReviewReplyLeavesQuestionOpen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}, {Ref: testRefBGo, Text: "b"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindReview, nil, items)

	itemDraft, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionDrop},
	})
	if err != nil {
		t.Fatalf("SaveDraft(item): %v", err)
	}
	replyDraft, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough,
	})
	if err != nil {
		t.Fatalf("SaveDraft(reply): %v", err)
	}

	if _, err = s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage(question): %v", err)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state = %v, want unchanged %q (a review reply does not answer, and b.go is still undecided)", q.State, questionStateOpen)
	}

	item, err := s.GetMessage(t.Context(), itemDraft.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(item answer): %v", err)
	}
	if item.State == nil || *item.State != answerStateSent {
		t.Errorf("item answer state = %v, want %q", item.State, answerStateSent)
	}

	reply, err := s.GetMessage(t.Context(), replyDraft.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(reply): %v", err)
	}
	if reply.State == nil || *reply.State != answerStateSent {
		t.Errorf("reply state = %v, want %q", reply.State, answerStateSent)
	}
}

// TestSendBatch_FullyDecidedReviewWithReplyAnswers guards the fix in
// TestSendBatch_ReviewReplyLeavesQuestionOpen against over-reach: once every
// finding on a review question has a decision, the question is still marked
// answered by the item-completeness check, whether or not the same batch
// also carries a free reply.
func TestSendBatch_FullyDecidedReviewWithReplyAnswers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}, {Ref: testRefBGo, Text: "b"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindReview, nil, items)

	if _, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionDrop},
	}); err != nil {
		t.Fatalf("SaveDraft(a.go): %v", err)
	}
	if _, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefBGo, Decision: response.DecisionAccept},
	}); err != nil {
		t.Fatalf("SaveDraft(b.go): %v", err)
	}
	if _, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough,
	}); err != nil {
		t.Fatalf("SaveDraft(reply): %v", err)
	}

	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage(question): %v", err)
	}
	if q.State == nil || *q.State != questionStateAnswered {
		t.Errorf("question state = %v, want %q (every finding is decided)", q.State, questionStateAnswered)
	}
}

// TestSendBatch_ReviewItemNoteStaysWithItsFinding proves a note typed on one
// finding's row is stored under that finding's own ref, not the question's
// joined reply, and a finding left undecided gets no note at all.
func TestSendBatch_ReviewItemNoteStaysWithItsFinding(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}, {Ref: testRefBGo, Text: "b"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindReview, nil, items)

	itemDraft, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID,
		Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionDrop, Note: "out of scope"},
	})
	if err != nil {
		t.Fatalf("SaveDraft(a.go): %v", err)
	}

	if _, err = s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage(question): %v", err)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state = %v, want unchanged %q (b.go is still undecided)", q.State, questionStateOpen)
	}

	ap := draftPayloadOf(t, s, itemDraft.MessageID)
	if ap.Items[testRefAGo] != response.DecisionDrop {
		t.Errorf("Items[a.go] = %v, want drop", ap.Items[testRefAGo])
	}
	if ap.Notes[testRefAGo] != "out of scope" {
		t.Errorf("Notes[a.go] = %q, want %q", ap.Notes[testRefAGo], "out of scope")
	}
	if _, ok := ap.Notes[testRefBGo]; ok {
		t.Errorf("Notes[b.go] = %q, want no entry", ap.Notes[testRefBGo])
	}
}

// TestSendBatch_ReplyStillAnswersNonReviewQuestion proves the fix is scoped
// to review questions only: a free reply on a kind=question question still
// answers it, exactly as before.
func TestSendBatch_ReplyStillAnswersNonReviewQuestion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	if _, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough,
	}); err != nil {
		t.Fatalf("SaveDraft(reply): %v", err)
	}

	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage(question): %v", err)
	}
	if q.State == nil || *q.State != questionStateAnswered {
		t.Errorf("question state = %v, want %q (a reply still answers a non-review question)", q.State, questionStateAnswered)
	}
}

// TestSaveDraft_NoteOnNonReviewItemRefused proves SaveDraft refuses a note
// on an item-kind question other than review (perimeter here), and writes
// nothing: the owner's note on an accept/reject pick belongs to a review
// finding only.
func TestSaveDraft_NoteOnNonReviewItemRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindPerimeter, nil, items)

	_, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID,
		Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionAccept, Note: "looks fine"},
	})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Reason != "a note is for a review item" {
		t.Fatalf("SaveDraft err = %v, want conflict %q", err, "a note is for a review item")
	}

	var count int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM messages WHERE parent_id = ? AND type = 'answer'`, qID,
	).Scan(&count); err != nil {
		t.Fatalf("count answers: %v", err)
	}
	if count != 0 {
		t.Errorf("answer row count = %d, want 0 (nothing written on a refused note)", count)
	}
}

// TestSaveDraft_ItemNoteReplacesAndClears proves a later pick on the same
// ref replaces its note, and a pick with an empty note clears it: a note
// never outlives the pick the owner made it against.
func TestSaveDraft_ItemNoteReplacesAndClears(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindReview, nil, items)

	first, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID,
		Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionDrop, Note: "first"},
	})
	if err != nil {
		t.Fatalf("SaveDraft(first): %v", err)
	}
	if first.Replaced {
		t.Error("first.Replaced = true, want false (fresh draft)")
	}

	second, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID,
		Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionDiscuss, Note: "second"},
	})
	if err != nil {
		t.Fatalf("SaveDraft(second): %v", err)
	}
	if !second.Replaced {
		t.Error("second.Replaced = false, want true (decision and note both changed)")
	}
	ap := draftPayloadOf(t, s, second.MessageID)
	if ap.Notes[testRefAGo] != "second" {
		t.Errorf("Notes[a.go] = %q, want %q", ap.Notes[testRefAGo], "second")
	}

	third, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID,
		Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionAccept, Note: ""},
	})
	if err != nil {
		t.Fatalf("SaveDraft(third): %v", err)
	}
	ap = draftPayloadOf(t, s, third.MessageID)
	if ap.Notes != nil {
		t.Errorf("Notes = %v, want nil (cleared)", ap.Notes)
	}

	// A note-only edit (same decision, different note) must still report
	// Replaced true: this is the save installItemNoteSave's own 'change'
	// listener makes most often, keeping today's pick and only touching the
	// note (review fix, tests).
	fourth, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID,
		Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionAccept, Note: "fourth"},
	})
	if err != nil {
		t.Fatalf("SaveDraft(fourth): %v", err)
	}
	if !fourth.Replaced {
		t.Error("fourth.Replaced = false, want true (note changed, decision unchanged)")
	}

	// Saving the exact same decision and note again changes nothing, so
	// Replaced must be false.
	fifth, err := s.SaveDraft(t.Context(), DraftInput{
		TicketID: ticketID, QuestionID: &qID,
		Item: &ItemDecision{Ref: testRefAGo, Decision: response.DecisionAccept, Note: "fourth"},
	})
	if err != nil {
		t.Fatalf("SaveDraft(fifth): %v", err)
	}
	if fifth.Replaced {
		t.Error("fifth.Replaced = true, want false (decision and note both unchanged)")
	}
}

// TestSendBatch_RevisedAnswerStaysAnsweredAndDoesNotClearAnEarlyWait proves
// D30: sending a revised draft against a question already state=answered
// (SaveDraft allowed it while the ticket still waits, the test above) keeps
// it answered -- markAnsweredQuestionsTx's own UPDATE only ever matches
// state=open, so re-marking an already-answered question is a no-op, not a
// second transition -- and, with a second question of the same round still
// open, does not clear the ticket's wait early.
func TestSendBatch_RevisedAnswerStaysAnsweredAndDoesNotClearAnEarlyWait(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	q1ID := insertQuestionOption(t, s, ticketID, "Q1")
	q2ID := insertQuestionOption(t, s, ticketID, "Q2")

	// Answer and send Q1 alone: Q2 is still open, so the wait must not clear
	// yet (ordinary behavior, not D30's own concern).
	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1ID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft(Q1, first answer): %v", err)
	}
	first, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch (first): %v", err)
	}
	if first.Sent != 1 || first.WaitCleared {
		t.Fatalf("first SendBatch = %+v, want Sent=1 WaitCleared=false (Q2 still open)", first)
	}

	// Revise Q1's answer while the ticket still waits (D30: allowed since
	// Q1 is answered, not resolved, and waiting_on is still "questions"),
	// then send again.
	revised := "b"
	if _, saveErr := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1ID, Option: &revised}); saveErr != nil {
		t.Fatalf("SaveDraft(Q1, revised answer): %v", saveErr)
	}
	second, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch (second, revised): %v", err)
	}
	if second.Sent != 1 {
		t.Errorf("second SendBatch.Sent = %d, want 1", second.Sent)
	}
	if second.WaitCleared {
		t.Error("second SendBatch.WaitCleared = true, want false: Q2 is still open")
	}

	q1, err := s.GetMessage(t.Context(), q1ID)
	if err != nil {
		t.Fatalf("GetMessage(Q1): %v", err)
	}
	if q1.State == nil || *q1.State != questionStateAnswered {
		t.Errorf("Q1 state = %v, want unchanged %q", q1.State, questionStateAnswered)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn == nil || *ticket.WaitingOn != testWaitingQuestions {
		t.Errorf("ticket.WaitingOn = %v, want still %q (Q2 open)", ticket.WaitingOn, testWaitingQuestions)
	}

	q2, err := s.GetMessage(t.Context(), q2ID)
	if err != nil {
		t.Fatalf("GetMessage(Q2): %v", err)
	}
	if q2.State == nil || *q2.State != questionStateOpen {
		t.Errorf("Q2 state = %v, want unchanged %q", q2.State, questionStateOpen)
	}

	// The revised answer is what the round reader now sees for Q1.
	if ap := draftPayloadOf(t, s, mustLatestAnswerID(t, s, q1ID)); ap.Option == nil || *ap.Option != "b" {
		t.Errorf("Q1's sent answer option = %v, want \"b\" (the revision)", ap.Option)
	}
}

// mustLatestAnswerID returns the highest-id "answer" message whose parent is
// questionID -- the most recently sent one, since SendBatch only ever
// inserts higher ids -- for a D30 test to read the round's latest pick back.
func mustLatestAnswerID(t *testing.T, s *Store, questionID int64) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT id FROM messages WHERE parent_id = ? AND type = ? ORDER BY id DESC LIMIT 1`,
		questionID, msgTypeAnswer,
	).Scan(&id); err != nil {
		t.Fatalf("mustLatestAnswerID(%d): %v", questionID, err)
	}
	return id
}

// ---- D31: planning questions through SendBatch and SaveDraft ---------------

// insertPlanningQuestion inserts an open planning question keyed "Q1"
// (D31, design section 22.1): a "question" message, kind "question", no
// parent, asked by a run on a fresh "planning" job session -- the one
// shape planningQuestionsSQL (conversation_reads.go) recognizes.
func insertPlanningQuestion(t *testing.T, s *Store, ticketID int64) int64 {
	t.Helper()
	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	return insertQuestionOfKindWithRun(t, s, ticketID, runID, "Q1", response.QuestionKindQuestion)
}

// TestSendBatchWakesPlanningOnOneMessage proves wakePlanningTx (design
// section 22.3): sending one message to an open planning question clears
// the "questions" wait at once, with no escalation in the way.
func TestSendBatchWakesPlanningOnOneMessage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	qID := insertPlanningQuestion(t, s, ticketID)

	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "Also print the commit hash."}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Empty || res.Sent != 1 {
		t.Fatalf("SendBatch = %+v, want Sent=1", res)
	}
	if !res.WaitCleared {
		t.Error("WaitCleared = false, want true (no escalation open)")
	}
	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil", *ticket.WaitingOn)
	}
}

// TestSendBatchNeverMarksPlanningQuestionAnswered proves markAnsweredQuestionsTx
// skips planning questions (design section 22.3): the question stays "open"
// after SendBatch sends an option draft against it, unlike every other
// question kind.
func TestSendBatchNeverMarksPlanningQuestionAnswered(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	qID := insertPlanningQuestion(t, s, ticketID)

	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("SendBatch = %+v, want Sent=1", res)
	}

	q, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if q.State == nil || *q.State != questionStateOpen {
		t.Errorf("question state = %v, want %q (planning questions never become answered)", q.State, questionStateOpen)
	}
}

// TestSendBatchQueuesUnderOpenEscalation proves wakePlanningTx's own
// escalation guard (design section 22.3): with an open escalation-linked
// question on the ticket, sending a message to a planning question leaves
// waiting_on untouched -- the messages queue behind the escalation.
func TestSendBatchQueuesUnderOpenEscalation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	qID := insertPlanningQuestion(t, s, ticketID)

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	parentID := insertUpdateMarker(t, s, ticketID, "escalation placeholder")
	insertQuestionWithParent(t, s, ticketID, runID, parentID, "Q9") // open escalation-linked question

	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("SendBatch = %+v, want Sent=1", res)
	}
	if res.WaitCleared {
		t.Error("WaitCleared = true, want false (an escalation question is still open)")
	}
	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn == nil || *ticket.WaitingOn != testWaitingQuestions {
		t.Errorf("ticket.WaitingOn = %v, want still %q", ticket.WaitingOn, testWaitingQuestions)
	}
}

// TestSaveDraftPlanningQuestionLocksOnlyAtSeal proves questionDraftableTx's
// own planning rule (design section 22.3, widened by D32, design section
// 22.12.1, 22.12.2): a planning question drafts fine in "answered" state
// with waiting_on already cleared (nil) -- every other question kind would
// refuse this as "question closed" -- and a "resolved" one still drafts
// fine (reopenable) while its ticket is in "planning". Only once the ticket
// has left "planning" (the seal, building here) does the same resolved
// question finally refuse.
func TestSaveDraftPlanningQuestionLocksOnlyAtSeal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	qID := insertPlanningQuestion(t, s, ticketID)
	closeQuestion(t, s, qID, questionStateAnswered)
	// waiting_on stays nil: under D30's own rule this would refuse the
	// draft outright, but a planning question ignores waiting_on entirely.

	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft on an answered planning question: %v, want it to succeed", err)
	}

	closeQuestion(t, s, qID, questionStateResolved)
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "reopen it"}); err != nil {
		t.Fatalf("SaveDraft on a resolved, reopenable planning question: %v, want it to succeed", err)
	}

	setTicketState(t, s, ticketID, testStateBuilding) // the seal: locks every planning thread for good
	_, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "too late"})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Reason != testConflictQuestionClosed {
		t.Errorf("SaveDraft on a resolved planning question after the seal: err = %v, want the \"question closed\" conflict", err)
	}
}

// TestClearMatchingWaitIgnoresPlanningQuestions proves openQuestionOfKindExistsTx's
// own planning exclusion (design section 22.3): an open planning question
// never holds a classify round's own "questions" wait open -- answering
// that round's own question clears the wait even while the planning
// thread stays open.
func TestClearMatchingWaitIgnoresPlanningQuestions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketWaiting(t, s, ticketID, testWaitingQuestions)
	insertPlanningQuestion(t, s, ticketID) // stays open throughout

	classifyQID := insertQuestionOption(t, s, ticketID, "Q2") // a classify-round-shaped question, no run
	opt := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &classifyQID, Option: &opt}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("SendBatch = %+v, want Sent=1", res)
	}
	if !res.WaitCleared {
		t.Error("WaitCleared = false, want true (the open planning question must not hold this wait)")
	}
}

// ---- D32: reopen until the gate (design section 22.12.2) ------------------

// insertSettledPlanningQuestion inserts a planning question already
// resolved, with a zing-authored decision row: the fixture D32's reopen
// tests build on (design section 22.12.1's "reopenable").
func insertSettledPlanningQuestion(t *testing.T, s *Store, ticketID, runID int64, key, decision string) int64 {
	t.Helper()
	qID := insertQuestionOfKindWithRun(t, s, ticketID, runID, key, response.QuestionKindQuestion)
	closeQuestion(t, s, qID, questionStateResolved)
	if _, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: &qID, RunID: &runID, Type: msgTypeResolved, Author: authorZing, Body: decision,
	}); err != nil {
		t.Fatalf("insert decision row for %s: %v", key, err)
	}
	return qID
}

// TestSendBatchReopensSettledThreadAndWithdrawsGate proves reopenStepTx's
// own steps 2 through 4 (design section 22.12.2): a reply to a settled
// planning thread, while the ticket sits at an open gate, reopens the
// thread with its own "followup" turn, withdraws the gate question (a
// resolved/system row, design section 4.2's withdrawQuestionTx), and clears
// the "gate" wait -- all in the one SendBatch transaction.
func TestSendBatchReopensSettledThreadAndWithdrawsGate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	setTicketWaiting(t, s, ticketID, string(response.QuestionKindGate))

	planningSess := insertSession(t, s, ticketID, testStatePlanning)
	planningRun := insertQuestionRun(t, s, planningSess)
	q1ID := insertSettledPlanningQuestion(t, s, ticketID, planningRun, "Q1", "Plain text only.")
	gateQID := insertQuestionOfKind(t, s, ticketID, "Q2", response.QuestionKindGate, optionsAB, nil)

	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1ID, Text: testReopenText}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("SendBatch = %+v, want Sent=1", res)
	}
	if !res.WaitCleared {
		t.Error("WaitCleared = false, want true (the gate question was withdrawn)")
	}

	q1, err := s.GetMessage(t.Context(), q1ID)
	if err != nil {
		t.Fatalf("GetMessage(q1): %v", err)
	}
	if q1.State == nil || *q1.State != questionStateOpen {
		t.Errorf("Q1 state = %v, want %q (reopened)", q1.State, questionStateOpen)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND parent_id = ? AND type = ? AND author = ? AND body = ?`,
		ticketID, q1ID, msgTypeFollowup, authorYou, "reopened"); n != 1 {
		t.Errorf("followup rows for Q1 = %d, want 1", n)
	}

	gate, err := s.GetMessage(t.Context(), gateQID)
	if err != nil {
		t.Fatalf("GetMessage(gate): %v", err)
	}
	if gate.State == nil || *gate.State != questionStateResolved {
		t.Errorf("gate state = %v, want %q (withdrawn)", gate.State, questionStateResolved)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND parent_id = ? AND type = ? AND author = ?`,
		ticketID, gateQID, msgTypeResolved, authorSystem); n != 1 {
		t.Errorf("resolved/system rows for the gate = %d, want 1", n)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil", *ticket.WaitingOn)
	}
}

// TestSendBatchReopenBeatsApproveInOneBatch proves step 1 (design section
// 22.12.2, 22.12.6's "Approve and a reopen in one batch"): a batch that
// carries both a reply on a reopenable thread and an approve pick on the
// still-open gate discards the gate draft as stale, and the gate ends
// withdrawn rather than answered.
func TestSendBatchReopenBeatsApproveInOneBatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	setTicketWaiting(t, s, ticketID, string(response.QuestionKindGate))

	planningSess := insertSession(t, s, ticketID, testStatePlanning)
	planningRun := insertQuestionRun(t, s, planningSess)
	q1ID := insertSettledPlanningQuestion(t, s, ticketID, planningRun, "Q1", "Plain text only.")
	gateQID := insertQuestionOfKind(t, s, ticketID, "Q2", response.QuestionKindGate, optionsAB, nil)

	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1ID, Text: testReopenText}); err != nil {
		t.Fatalf("SaveDraft Q1: %v", err)
	}
	approve := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &gateQID, Option: &approve}); err != nil {
		t.Fatalf("SaveDraft gate: %v", err)
	}

	res, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if res.Sent != 1 {
		t.Errorf("Sent = %d, want 1 (only Q1's reply)", res.Sent)
	}
	if res.Discarded != 1 {
		t.Errorf("Discarded = %d, want 1 (the losing gate draft)", res.Discarded)
	}

	gate, err := s.GetMessage(t.Context(), gateQID)
	if err != nil {
		t.Fatalf("GetMessage(gate): %v", err)
	}
	if gate.State == nil || *gate.State != questionStateResolved {
		t.Errorf("gate state = %v, want %q (withdrawn, never answered)", gate.State, questionStateResolved)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE parent_id = ? AND type = ?`, gateQID, msgTypeAnswer); n != 0 {
		t.Errorf("answer rows on the gate = %d, want 0 (the draft was discarded, not sent)", n)
	}
}

// TestSendDuringApprovalCancelsIt proves step 3's own "answered with an
// approval in progress" branch (design section 22.12.2, 22.12.6's "the
// owner writes on a settled thread during an approval"): the owner sends
// Approve first (the gate goes "answered"), then reopens a settled thread
// in a later batch; the gate is withdrawn with a cancellation marker naming
// that later batch, not the run that would have confirmed it.
func TestSendDuringApprovalCancelsIt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	planningSess := insertSession(t, s, ticketID, testStatePlanning)
	planningRun := insertQuestionRun(t, s, planningSess)
	q1ID := insertSettledPlanningQuestion(t, s, ticketID, planningRun, "Q1", "Plain text only.")
	gateQID := insertQuestionOfKind(t, s, ticketID, "Q2", response.QuestionKindGate, optionsAB, nil)

	approve := "a"
	if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &gateQID, Option: &approve}); err != nil {
		t.Fatalf("SaveDraft approve: %v", err)
	}
	approveRes, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch (approve): %v", err)
	}
	if approveRes.Sent != 1 {
		t.Fatalf("approve SendBatch = %+v, want Sent=1", approveRes)
	}
	gateAfterApprove, err := s.GetMessage(t.Context(), gateQID)
	if err != nil {
		t.Fatalf("GetMessage(gate) after approve: %v", err)
	}
	if gateAfterApprove.State == nil || *gateAfterApprove.State != questionStateAnswered {
		t.Fatalf("gate state after approve = %v, want %q", gateAfterApprove.State, questionStateAnswered)
	}

	if _, saveErr := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &q1ID, Text: testReopenText}); saveErr != nil {
		t.Fatalf("SaveDraft reopen: %v", saveErr)
	}
	reopenRes, err := s.SendBatch(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SendBatch (reopen): %v", err)
	}
	if reopenRes.Sent != 1 {
		t.Fatalf("reopen SendBatch = %+v, want Sent=1", reopenRes)
	}

	gate, err := s.GetMessage(t.Context(), gateQID)
	if err != nil {
		t.Fatalf("GetMessage(gate) after reopen: %v", err)
	}
	if gate.State == nil || *gate.State != questionStateResolved {
		t.Errorf("gate state after reopen = %v, want %q (withdrawn)", gate.State, questionStateResolved)
	}
	wantCancel := fmt.Sprintf("gate approval cancelled gate %d batch %d", gateQID, reopenRes.BatchID)
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND parent_id = ? AND type = ? AND author = ? AND body = ?`,
		ticketID, gateQID, msgTypeUpdate, authorSystem, wantCancel); n != 1 {
		t.Errorf("cancellation marker %q not found", wantCancel)
	}

	q1, err := s.GetMessage(t.Context(), q1ID)
	if err != nil {
		t.Fatalf("GetMessage(q1): %v", err)
	}
	if q1.State == nil || *q1.State != questionStateOpen {
		t.Errorf("Q1 state = %v, want %q (reopened)", q1.State, questionStateOpen)
	}
}

// ---- SaveDraft: reply dedupe on repeated Enter ------------------------------

// TestSaveDraft_QuestionReplyIsIdempotentOnRepeatedEnter proves the
// review-fix contract: two Enters on the same question-targeted free reply
// update one draft row in place rather than accumulating a second row that
// would send twice.
func TestSaveDraft_QuestionReplyIsIdempotentOnRepeatedEnter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	qID := insertQuestionOption(t, s, ticketID, "Q1")

	first, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough})
	if err != nil {
		t.Fatalf("SaveDraft (first Enter): %v", err)
	}
	if first.Replaced {
		t.Error("first Enter: Replaced = true, want false")
	}

	second, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: testReplyWhyThough})
	if err != nil {
		t.Fatalf("SaveDraft (second Enter, same text): %v", err)
	}
	if second.MessageID != first.MessageID {
		t.Errorf("second Enter MessageID = %d, want the same row %d", second.MessageID, first.MessageID)
	}
	if second.Replaced {
		t.Error("second Enter with the same text: Replaced = true, want false")
	}

	var count int
	if scanErr := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND parent_id = ? AND type = ?`,
		ticketID, qID, msgTypeReply,
	).Scan(&count); scanErr != nil {
		t.Fatalf("count reply rows: %v", scanErr)
	}
	if count != 1 {
		t.Errorf("reply rows for (ticket, question) = %d, want 1 (repeated Enter must not duplicate)", count)
	}

	// A third Enter with edited text updates the same row and reports it.
	third, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Text: "actually, why not"})
	if err != nil {
		t.Fatalf("SaveDraft (edited reply): %v", err)
	}
	if third.MessageID != first.MessageID || !third.Replaced {
		t.Errorf("edited reply = %+v, want MessageID=%d Replaced=true", third, first.MessageID)
	}
	m, err := s.GetMessage(t.Context(), first.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.Body != "actually, why not" {
		t.Errorf("reply body after edit = %q, want %q", m.Body, "actually, why not")
	}
}

// TestSaveDraft_ThreadReplyIsIdempotentOnRepeatedEnter is
// TestSaveDraft_QuestionReplyIsIdempotentOnRepeatedEnter for an unparented
// thread reply (QuestionID nil), which dedupes by (ticket, thread) instead
// of (ticket, question).
func TestSaveDraft_ThreadReplyIsIdempotentOnRepeatedEnter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")

	first, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, Text: "note one"})
	if err != nil {
		t.Fatalf("SaveDraft (first Enter): %v", err)
	}

	second, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, Text: "note one"})
	if err != nil {
		t.Fatalf("SaveDraft (second Enter, same text): %v", err)
	}
	if second.MessageID != first.MessageID || second.Replaced {
		t.Errorf("second Enter on a thread reply = %+v, want MessageID=%d Replaced=false", second, first.MessageID)
	}

	var count int
	if scanErr := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND parent_id IS NULL AND type = ?`,
		ticketID, msgTypeReply,
	).Scan(&count); scanErr != nil {
		t.Fatalf("count thread reply rows: %v", scanErr)
	}
	if count != 1 {
		t.Errorf("thread reply rows for ticket = %d, want 1 (repeated Enter must not duplicate)", count)
	}
}

// ---- MarkRead ---------------------------------------------------------------

func TestMarkRead_SetsReadAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	msgID := insertZingUpdate(t, s, ticketID)

	before, err := s.GetMessage(t.Context(), msgID)
	if err != nil {
		t.Fatalf("GetMessage (before): %v", err)
	}
	if before.ReadAt != nil {
		t.Fatal("ReadAt before MarkRead is already set")
	}

	if markErr := s.MarkRead(t.Context(), msgID); markErr != nil {
		t.Fatalf("MarkRead: %v", markErr)
	}

	after, err := s.GetMessage(t.Context(), msgID)
	if err != nil {
		t.Fatalf("GetMessage (after): %v", err)
	}
	if after.ReadAt == nil {
		t.Error("ReadAt after MarkRead is still nil")
	}
}

func TestMarkRead_MissingMessageErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.MarkRead(t.Context(), 999999); err == nil {
		t.Error("MarkRead on a missing message: err = nil, want an error")
	}
}

// TestMarkThreadRead_MarksOnlyUnreadZingMessages proves MarkThreadRead marks
// every unread, zing-authored, thread-visible message on one ticket, leaves
// a "you"-authored message and a state message alone, never touches another
// ticket's messages, and returns 0 (not an error) on a second call or on an
// id that names no ticket.
func TestMarkThreadRead_MarksOnlyUnreadZingMessages(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	zing1 := insertZingUpdate(t, s, ticketID)
	zing2 := insertZingUpdate(t, s, ticketID)
	you, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, Type: testTypeUpdate, Author: "you", Body: "a note",
	})
	if err != nil {
		t.Fatalf("InsertMessage (you): %v", err)
	}
	stateMsg, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, Type: msgTypeState, Author: authorSystem,
		Payload: []byte(`{"from":"queued","to":"queued","reason":"test"}`),
	})
	if err != nil {
		t.Fatalf("InsertMessage (state): %v", err)
	}

	_, otherTicketID := seedQueuedTicket(t, s, "2")
	otherZing := insertZingUpdate(t, s, otherTicketID)

	n, err := s.MarkThreadRead(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("MarkThreadRead: %v", err)
	}
	if n != 2 {
		t.Errorf("MarkThreadRead marked = %d, want 2", n)
	}

	for _, id := range []int64{zing1, zing2} {
		msg, getErr := s.GetMessage(t.Context(), id)
		if getErr != nil {
			t.Fatalf("GetMessage(%d): %v", id, getErr)
		}
		if msg.ReadAt == nil {
			t.Errorf("message %d: ReadAt is nil, want set", id)
		}
	}
	for _, id := range []int64{you, stateMsg, otherZing} {
		msg, getErr := s.GetMessage(t.Context(), id)
		if getErr != nil {
			t.Fatalf("GetMessage(%d): %v", id, getErr)
		}
		if msg.ReadAt != nil {
			t.Errorf("message %d: ReadAt is set, want nil", id)
		}
	}

	second, err := s.MarkThreadRead(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("MarkThreadRead (second call): %v", err)
	}
	if second != 0 {
		t.Errorf("MarkThreadRead (already read) marked = %d, want 0", second)
	}

	missing, err := s.MarkThreadRead(t.Context(), 999999)
	if err != nil {
		t.Fatalf("MarkThreadRead (missing ticket): %v", err)
	}
	if missing != 0 {
		t.Errorf("MarkThreadRead (missing ticket) marked = %d, want 0", missing)
	}
}

// TestSetSettings_UpdatesExistingAndInsertsNew proves SetSettings (design
// section 6.12, 6.13, 7.2) writes every key/value pair in one call: it
// updates an existing row (log_level, seeded by migrations/0001_init.sql)
// and inserts a brand-new key (the shape Task 11's VAPID pair needs) in the
// same transaction.
func TestSetSettings_UpdatesExistingAndInsertsNew(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	if err := s.SetSettings(t.Context(), "log_level", "debug", "vapid_public", "pub-key"); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	level, ok, err := s.GetSetting(t.Context(), "log_level")
	if err != nil {
		t.Fatalf("GetSetting(log_level): %v", err)
	}
	if !ok || level != "debug" {
		t.Errorf("GetSetting(log_level) = (%q, %v), want (debug, true)", level, ok)
	}

	pub, ok, err := s.GetSetting(t.Context(), "vapid_public")
	if err != nil {
		t.Fatalf("GetSetting(vapid_public): %v", err)
	}
	if !ok || pub != "pub-key" {
		t.Errorf("GetSetting(vapid_public) = (%q, %v), want (pub-key, true)", pub, ok)
	}
}

// TestSetSettings_RejectsOddArgumentCount proves a mismatched key without a
// value is rejected before any write happens, rather than silently dropping
// the dangling key.
func TestSetSettings_RejectsOddArgumentCount(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SetSettings(t.Context(), "log_level"); err == nil {
		t.Error("SetSettings with an odd argument count: err = nil, want an error")
	}
}

// pushKeysJSON builds a valid {p256dh, auth} keys_json payload for
// UpsertPushSubscription's tests.
func pushKeysJSON(p256dh, auth string) []byte {
	return []byte(`{"p256dh":` + strconv.Quote(p256dh) + `,"auth":` + strconv.Quote(auth) + `}`)
}

// TestUpsertPushSubscription_InsertsThenReplacesByEndpoint proves the design
// section 6.13 upsert: a first call inserts, and a second call for the same
// endpoint replaces its keys_json in place, so a re-subscribe is idempotent
// rather than leaving two rows.
func TestUpsertPushSubscription_InsertsThenReplacesByEndpoint(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const endpoint = "https://push.example/abc"

	if err := s.UpsertPushSubscription(t.Context(), PushSubscription{
		Endpoint: endpoint, KeysJSON: pushKeysJSON("p256dh-one", "auth-one"),
	}); err != nil {
		t.Fatalf("UpsertPushSubscription (insert): %v", err)
	}
	if err := s.UpsertPushSubscription(t.Context(), PushSubscription{
		Endpoint: endpoint, KeysJSON: pushKeysJSON("p256dh-two", "auth-two"),
	}); err != nil {
		t.Fatalf("UpsertPushSubscription (replace): %v", err)
	}

	var count int
	var keysJSON string
	row := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*), MAX(keys_json) FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	if err := row.Scan(&count, &keysJSON); err != nil {
		t.Fatalf("query push_subscriptions: %v", err)
	}
	if count != 1 {
		t.Errorf("push_subscriptions rows for %s = %d, want 1 (replaced, not duplicated)", endpoint, count)
	}
	if !strings.Contains(keysJSON, "p256dh-two") {
		t.Errorf("keys_json = %s, want the replaced value", keysJSON)
	}
}

// TestUpsertPushSubscription_RejectsKeysMissingRequiredFields proves
// keys_json is validated against the push_subscriptions/keys schema: a
// payload missing p256dh or auth is rejected and writes nothing.
func TestUpsertPushSubscription_RejectsKeysMissingRequiredFields(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	tests := []struct {
		name string
		keys []byte
	}{
		{name: "missing p256dh", keys: []byte(`{"auth":"auth-one"}`)},
		{name: "missing auth", keys: []byte(`{"p256dh":"p256dh-one"}`)},
		{name: "unknown key", keys: []byte(`{"p256dh":"a","auth":"b","extra":"c"}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := s.UpsertPushSubscription(t.Context(), PushSubscription{
				Endpoint: "https://push.example/" + tc.name, KeysJSON: tc.keys,
			})
			if err == nil {
				t.Fatal("UpsertPushSubscription: err = nil, want a schema validation error")
			}
		})
	}

	var count int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM push_subscriptions`).Scan(&count); err != nil {
		t.Fatalf("count push_subscriptions: %v", err)
	}
	if count != 0 {
		t.Errorf("push_subscriptions row count = %d, want 0 (nothing written on a rejected payload)", count)
	}
}
