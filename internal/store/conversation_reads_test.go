package store

import (
	"fmt"
	"testing"

	"zing/internal/response"
)

// insertSentOwnerBatch inserts a sent "reply" message on questionID
// directly, carrying batch: the shape a later SendBatch (task D31-3) will
// write for a planning thread, built here so this file's reads can be
// tested before that wiring lands.
func insertSentOwnerBatch(t *testing.T, s *Store, ticketID, questionID, batch int64, body string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: &questionID, Type: msgTypeReply, Author: authorYou,
		State: new(answerStateSent), Body: body, BatchID: &batch,
	})
	if err != nil {
		t.Fatalf("insert sent owner batch %d: %v", batch, err)
	}
	return id
}

// insertAgentReply inserts a sent, zing-authored "reply" on questionID,
// carrying runID: the agent's own answer in a thread (design section
// 22.3's "agent reply" row).
func insertAgentReply(t *testing.T, s *Store, ticketID, questionID, runID int64, body string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: &questionID, RunID: &runID, Type: msgTypeReply, Author: authorZing,
		State: new(answerStateSent), Body: body,
	})
	if err != nil {
		t.Fatalf("insert agent reply: %v", err)
	}
	return id
}

// insertQuestionOfKindWithRun is insertQuestionOfKind (console_writes_test.go)
// with a run attached, so a question of any kind can be tested against the
// planningQuestionsSQL fragment's run-and-session join.
func insertQuestionOfKindWithRun(t *testing.T, s *Store, ticketID, runID int64, key string, kind response.QuestionKind) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, RunID: &runID, Type: msgTypeQuestion, Author: testAuthorZing,
		State: new(questionStateOpen), Body: key, Payload: draftQuestionPayload(t, key, kind, optionsAB, nil),
	})
	if err != nil {
		t.Fatalf("insert %s question %s: %v", kind, key, err)
	}
	return id
}

// insertQuestionWithParent inserts an open, run-attached "question" message
// parented to parentID: the shape an escalation-linked question takes
// (its parent is the escalation message's own id), so
// TestPlanningQuestionsExcludeOtherKinds can prove the exclusion.
func insertQuestionWithParent(t *testing.T, s *Store, ticketID, runID, parentID int64, key string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, RunID: &runID, ParentID: &parentID, Type: msgTypeQuestion, Author: testAuthorZing,
		State: new(questionStateOpen), Body: key, Payload: questionPayload(key),
	})
	if err != nil {
		t.Fatalf("insert escalation-linked question %s: %v", key, err)
	}
	return id
}

// insertDeliveredMarker and insertPendingMarker write the two D31 system
// markers (design section 22.3) directly, through insertUpdateMarker
// (planning_reads_test.go).
func insertDeliveredMarker(t *testing.T, s *Store, ticketID, runID, batch int64) {
	t.Helper()
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("%s%d batch %d", conversationDeliveredPrefix, runID, batch))
}

func insertPendingMarker(t *testing.T, s *Store, ticketID, runID, batch int64) {
	t.Helper()
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("%s%d batch %d", conversationPendingPrefix, runID, batch))
}

// TestPlanningQuestionsExcludeOtherKinds proves planningQuestionsSQL (and
// so PlanningConversation) carries only the real planning question: a
// classify question (session job "classify"), a gate question (kind
// "gate"), an escalation-linked question (non-nil parent_id), and a
// plan-review question (session job "planreview") are all left out (design
// section 22.1).
func TestPlanningQuestionsExcludeOtherKinds(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	planningSess := insertSession(t, s, ticketID, testStatePlanning)
	planningRun := insertQuestionRun(t, s, planningSess)
	wantID := insertOpenQuestion(t, s, ticketID, planningRun, "Q1")

	classifySess := insertSession(t, s, ticketID, "classify")
	classifyRun := insertQuestionRun(t, s, classifySess)
	insertOpenQuestion(t, s, ticketID, classifyRun, "Q1")

	gateRun := insertQuestionRun(t, s, planningSess)
	insertQuestionOfKindWithRun(t, s, ticketID, gateRun, "Q1", response.QuestionKindGate)

	escRun := insertQuestionRun(t, s, planningSess)
	parentID := insertUpdateMarker(t, s, ticketID, "escalation placeholder")
	insertQuestionWithParent(t, s, ticketID, escRun, parentID, "Q1")

	reviewSess := insertSession(t, s, ticketID, "planreview")
	reviewRun := insertQuestionRun(t, s, reviewSess)
	insertOpenQuestion(t, s, ticketID, reviewRun, "Q1")

	conv, err := s.PlanningConversation(ctx, ticketID)
	if err != nil {
		t.Fatalf("PlanningConversation: %v", err)
	}
	if len(conv.Threads) != 1 {
		t.Fatalf("Threads = %d, want 1 (only the real planning question)", len(conv.Threads))
	}
	if conv.Threads[0].Question.ID != wantID {
		t.Errorf("Threads[0].Question.ID = %d, want %d", conv.Threads[0].Question.ID, wantID)
	}
}

// TestUndeliveredUsesBatchNotID proves Undelivered keys off batch_id, not
// id: a draft saved before a delivered marker but sent after it would carry
// a low id but a batch above the watermark, and must still be undelivered
// (design section 22.3: "a message's id is allocated when its draft is
// saved ... so a draft saved before run R starts and sent after it would
// sit below an id watermark and never be delivered").
func TestUndeliveredUsesBatchNotID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")

	lateID := insertSentOwnerBatch(t, s, ticketID, qID, 7, "early id, late batch")
	insertDeliveredMarker(t, s, ticketID, runID, 5)

	conv, err := s.PlanningConversation(ctx, ticketID)
	if err != nil {
		t.Fatalf("PlanningConversation: %v", err)
	}
	if conv.Delivered != 5 {
		t.Errorf("Delivered = %d, want 5", conv.Delivered)
	}
	undelivered := conv.Undelivered()
	if len(undelivered) != 1 || undelivered[0].ID != lateID {
		t.Fatalf("Undelivered = %+v, want [message %d]", undelivered, lateID)
	}
}

// TestDeliveredWatermarkIsNewestMarker proves W is the newest "conversation
// delivered" marker's batch, not the largest batch by value alone, by
// writing an older marker with a smaller batch after a newer one would
// naturally have a larger one -- the two still agree here, so this proves
// the newest-by-id marker is the one read.
func TestDeliveredWatermarkIsNewestMarker(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run1 := insertQuestionRun(t, s, sessID)
	run2 := insertQuestionRun(t, s, sessID)

	insertDeliveredMarker(t, s, ticketID, run1, 4)
	insertDeliveredMarker(t, s, ticketID, run2, 9)

	conv, err := s.PlanningConversation(ctx, ticketID)
	if err != nil {
		t.Fatalf("PlanningConversation: %v", err)
	}
	if conv.Delivered != 9 {
		t.Errorf("Delivered = %d, want 9 (the newest marker)", conv.Delivered)
	}
}

// TestUndeliveredSkipsSettledThreads proves an undelivered owner message
// on a settled thread never comes back from Undelivered (design section
// 22.3: "whose parent is an unsettled planning question").
func TestUndeliveredSkipsSettledThreads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	insertSentOwnerBatch(t, s, ticketID, qID, 3, "late message on a settled thread")
	closeQuestion(t, s, qID, questionStateResolved)

	conv, err := s.PlanningConversation(ctx, ticketID)
	if err != nil {
		t.Fatalf("PlanningConversation: %v", err)
	}
	if got := conv.Undelivered(); len(got) != 0 {
		t.Errorf("Undelivered for a settled thread = %+v, want none", got)
	}
}

// TestTurnOrder proves a thread's Turns interleave by the run that answered
// them (design section 22.3): the owner row delivered to run 1 comes
// before run 1's own reply, and a later owner row sent after the last
// delivered marker (still queued) comes last.
func TestTurnOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	sessID := insertSession(t, s, ticketID, testStatePlanning)
	run1 := insertQuestionRun(t, s, sessID)
	qID := insertOpenQuestion(t, s, ticketID, run1, "Q1")

	ownerDelivered := insertSentOwnerBatch(t, s, ticketID, qID, 4, "picked option b")
	insertDeliveredMarker(t, s, ticketID, run1, 4)
	agentReply := insertAgentReply(t, s, ticketID, qID, run1, "Agreed, no ldflags.")
	ownerQueued := insertSentOwnerBatch(t, s, ticketID, qID, 6, "one more thing")

	conv, err := s.PlanningConversation(ctx, ticketID)
	if err != nil {
		t.Fatalf("PlanningConversation: %v", err)
	}
	if len(conv.Threads) != 1 {
		t.Fatalf("Threads = %d, want 1", len(conv.Threads))
	}

	turns := conv.Threads[0].Turns
	wantIDs := []int64{ownerDelivered, agentReply, ownerQueued}
	if len(turns) != len(wantIDs) {
		t.Fatalf("Turns = %d rows, want %d", len(turns), len(wantIDs))
	}
	for i, want := range wantIDs {
		if turns[i].ID != want {
			t.Errorf("Turns[%d].ID = %d, want %d", i, turns[i].ID, want)
		}
	}
}

// TestInFlightRunThroughBatch proves InFlight is the newest NULL-outcome
// planning run, with ThroughBatch from its own "conversation pending"
// marker when it has one, and the watermark W otherwise (design section
// 22.3).
func TestInFlightRunThroughBatch(t *testing.T) {
	t.Parallel()

	t.Run("without a pending marker", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")

		sessID := insertSession(t, s, ticketID, testStatePlanning)
		priorRun := insertQuestionRun(t, s, sessID)
		insertDeliveredMarker(t, s, ticketID, priorRun, 3)
		inFlightRun := insertRun(t, s, sessID) // outcome NULL: in flight, no owner message received

		conv, err := s.PlanningConversation(ctx, ticketID)
		if err != nil {
			t.Fatalf("PlanningConversation: %v", err)
		}
		if conv.InFlight == nil {
			t.Fatal("InFlight = nil, want the in-flight run")
		}
		if conv.InFlight.RunID != inFlightRun {
			t.Errorf("InFlight.RunID = %d, want %d", conv.InFlight.RunID, inFlightRun)
		}
		if conv.InFlight.ThroughBatch != conv.Delivered {
			t.Errorf("InFlight.ThroughBatch = %d, want the watermark %d", conv.InFlight.ThroughBatch, conv.Delivered)
		}
	})

	t.Run("with a pending marker", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")

		sessID := insertSession(t, s, ticketID, testStatePlanning)
		priorRun := insertQuestionRun(t, s, sessID)
		insertDeliveredMarker(t, s, ticketID, priorRun, 3)
		inFlightRun := insertRun(t, s, sessID)
		insertPendingMarker(t, s, ticketID, inFlightRun, 6)

		conv, err := s.PlanningConversation(ctx, ticketID)
		if err != nil {
			t.Fatalf("PlanningConversation: %v", err)
		}
		if conv.InFlight == nil {
			t.Fatal("InFlight = nil, want the in-flight run")
		}
		if conv.InFlight.ThroughBatch != 6 {
			t.Errorf("InFlight.ThroughBatch = %d, want 6 (the pending marker's batch)", conv.InFlight.ThroughBatch)
		}
	})
}
