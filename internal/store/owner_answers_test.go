package store

import (
	"testing"

	"zing/internal/response"
)

// TestOwnerAnswers_SkipsPlanningGateAndUnsentRows proves OwnerAnswers'
// selection rules (design shape, OwnerAnswers selection): a planning
// question and a gate question are both excluded regardless of their
// answers, a question with no sent owner row is dropped, and the two
// qualifying questions come back in question id order, one carrying the
// Escalation flag and both owner rows it accrued.
func TestOwnerAnswers_SkipsPlanningGateAndUnsentRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	// 1. A planning question on a planning session run, with a sent answer:
	// excluded by planningQuestionsSQL.
	planSess := insertSession(t, s, ticketID, testStatePlanning)
	planRun := insertQuestionRun(t, s, planSess)
	planQID := insertOpenQuestion(t, s, ticketID, planRun, "Q1")
	insertSentAnswer(t, s, ticketID, planQID, "a")

	// 2. A gate question with a sent answer: excluded by its payload kind.
	gateQID := insertQuestionOfKind(t, s, ticketID, "Q2", response.QuestionKindGate, optionsAB, nil)
	insertSentAnswer(t, s, ticketID, gateQID, "a")

	// 3. A classify-style question (kind question, no parent, no run) with a
	// sent answer picking "b": qualifies, not an escalation.
	classifyQID := insertQuestionOfKind(t, s, ticketID, "Q3", response.QuestionKindQuestion, optionsAB, nil)
	insertSentAnswer(t, s, ticketID, classifyQID, "b")

	// 4. An escalation message and a question parented to it, resolved, with
	// a sent answer picking "a" and a sent reply: qualifies, an escalation.
	escID, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: msgTypeEscalation, Author: authorZing,
		Body:    "plan_gap: H1 is false",
		Payload: mustMarshalEscalation(t, escalationTestPayload(response.EscalationCodePlanGap, response.EscalationOriginBuild)),
	})
	if err != nil {
		t.Fatalf("insert escalation: %v", err)
	}
	escQID, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, ParentID: &escID, Type: msgTypeQuestion, Author: authorZing,
		State: new(questionStateResolved), Body: "Q4\n\nHow should Zing proceed?",
		Payload: draftQuestionPayload(t, "Q4", response.QuestionKindQuestion, optionsAB, nil),
	})
	if err != nil {
		t.Fatalf("insert escalation-linked question: %v", err)
	}
	insertSentAnswer(t, s, ticketID, escQID, "a")
	insertSentReply(t, s, ticketID, &escQID, "keep the guard")

	// 5. A question whose only row is a reply in state draft: excluded, no
	// sent owner row.
	draftQID := insertQuestionOfKind(t, s, ticketID, "Q5", response.QuestionKindQuestion, optionsAB, nil)
	if _, insertErr := s.InsertMessage(ctx, Message{
		TicketID: ticketID, ParentID: &draftQID, Type: msgTypeReply, Author: authorYou,
		State: new(draftState), Body: "half-written",
	}); insertErr != nil {
		t.Fatalf("insert draft reply: %v", insertErr)
	}

	got, err := s.OwnerAnswers(ctx, ticketID)
	if err != nil {
		t.Fatalf("OwnerAnswers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("OwnerAnswers returned %d entries, want 2: %+v", len(got), got)
	}

	if got[0].Question.ID != classifyQID {
		t.Errorf("entry 0 question id = %d, want %d (Q3)", got[0].Question.ID, classifyQID)
	}
	if got[0].Escalation {
		t.Error("entry 0 (Q3) Escalation = true, want false")
	}
	if len(got[0].Owner) != 1 {
		t.Errorf("entry 0 (Q3) Owner rows = %d, want 1", len(got[0].Owner))
	}

	if got[1].Question.ID != escQID {
		t.Errorf("entry 1 question id = %d, want %d (Q4)", got[1].Question.ID, escQID)
	}
	if !got[1].Escalation {
		t.Error("entry 1 (Q4) Escalation = false, want true")
	}
	if len(got[1].Owner) != 2 {
		t.Fatalf("entry 1 (Q4) Owner rows = %d, want 2", len(got[1].Owner))
	}
	if got[1].Owner[0].ID >= got[1].Owner[1].ID {
		t.Errorf("entry 1 (Q4) Owner rows not in id order: %+v", got[1].Owner)
	}
}
