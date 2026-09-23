package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"zing/internal/response"
)

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
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	items := []response.Item{{Ref: testRefAGo, Text: "a"}, {Ref: testRefBGo, Text: "b"}}
	qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindPerimeter, nil, items)

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
		TicketID: ticketID, QuestionID: &qID, Item: &ItemDecision{Ref: testRefBGo, Decision: response.DecisionReject},
	})
	if err != nil {
		t.Fatalf("SaveDraft (second ref): %v", err)
	}
	if res2.MessageID != res1.MessageID {
		t.Errorf("second ref MessageID = %d, want the same row %d", res2.MessageID, res1.MessageID)
	}
	got = draftPayloadOf(t, s, res1.MessageID)
	if len(got.Items) != 2 || got.Items[testRefAGo] != response.DecisionAccept || got.Items[testRefBGo] != response.DecisionReject {
		t.Fatalf("items after second ref = %v, want {a.go: accept, b.go: reject}", got.Items)
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
	if len(got.Items) != 2 || got.Items[testRefAGo] != response.DecisionDrop || got.Items[testRefBGo] != response.DecisionReject {
		t.Fatalf("items after replace one = %v, want {a.go: drop, b.go: reject}", got.Items)
	}
}

// ---- SaveDraft: replies ----------------------------------------------------

func TestSaveDraft_QuestionReplyAndThreadReply(t *testing.T) {
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
		{"closed question", DraftInput{TicketID: ticketA, QuestionID: &resolvedQID, Option: &opt}, "question closed"},
		{"wrong ticket", DraftInput{TicketID: ticketB, QuestionID: &qID, Option: &opt}, "wrong ticket"},
		{"bad option", DraftInput{TicketID: ticketA, QuestionID: &qID, Option: &badOpt}, "missing option"},
		{
			"bad item ref",
			DraftInput{TicketID: ticketA, QuestionID: &itemQID, Item: &ItemDecision{Ref: "nope.go", Decision: response.DecisionAccept}},
			"missing item",
		},
		{"ambiguous mode", DraftInput{TicketID: ticketA, QuestionID: &qID, Option: &opt, Text: "also this"}, "ambiguous draft mode"},
		{"empty text", DraftInput{TicketID: ticketA, QuestionID: &qID}, "empty text"},
		{"question not found", DraftInput{TicketID: ticketA, QuestionID: new(int64(999999)), Option: &opt}, "question not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

// ---- SendBatch --------------------------------------------------------------

func TestSendBatch_EmptyIsSafe(t *testing.T) {
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

func TestSendBatch_LocksAllocatesOneBatchIDAndClearsAQuestionsWait(t *testing.T) {
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

func TestSendBatch_ClearsAGateWaitButNeverErrorOrChildren(t *testing.T) {
	s := newTestStore(t)

	t.Run("gate wait cleared by a gate answer", func(t *testing.T) {
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
		_, ticketID := seedQueuedTicket(t, s, "err")
		setTicketWaiting(t, s, ticketID, "error")
		items := []response.Item{{Ref: "f1", Text: "finding"}}
		qID := insertQuestionOfKind(t, s, ticketID, "Q1", response.QuestionKindReview, nil, items)
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

// ---- SaveDraft: reply dedupe on repeated Enter ------------------------------

// TestSaveDraft_QuestionReplyIsIdempotentOnRepeatedEnter proves the
// review-fix contract: two Enters on the same question-targeted free reply
// update one draft row in place rather than accumulating a second row that
// would send twice.
func TestSaveDraft_QuestionReplyIsIdempotentOnRepeatedEnter(t *testing.T) {
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
	s := newTestStore(t)
	if err := s.MarkRead(t.Context(), 999999); err == nil {
		t.Error("MarkRead on a missing message: err = nil, want an error")
	}
}

// TestSetSettings_UpdatesExistingAndInsertsNew proves SetSettings (design
// section 6.12, 6.13, 7.2) writes every key/value pair in one call: it
// updates an existing row (log_level, seeded by migrations/0001_init.sql)
// and inserts a brand-new key (the shape Task 11's VAPID pair needs) in the
// same transaction.
func TestSetSettings_UpdatesExistingAndInsertsNew(t *testing.T) {
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
