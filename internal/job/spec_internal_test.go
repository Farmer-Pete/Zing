package job

// spec_internal_test.go tests spec.go's own pure renderer, renderSpec
// (design shape, "Spec text format"), with store.Ticket, store.
// PlanningConversation, []store.OwnerAnswer, and approval notes built in
// memory -- the same questionRow, ownerAnswerRow, and ownerReplyRow
// fixtures conversation_internal_test.go's own render tests use. It also
// gives seedOwnerDecision, the one resolved-escalation fixture
// reviewing_test.go's, judging_test.go's, and merge_test.go's own tests
// seed through the real store. export_test.go re-exports it as
// SeedOwnerDecision for package job_test's own fix_test.go and
// planning_test.go, so every one of those tests seeds the identical
// fixture and proves it round-trips through specFor end to end.

import (
	"encoding/json"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// specTestMsgTypeEscalation mirrors store's own unexported msgTypeEscalation
// (internal/store/commit.go): this package cannot reach that one, and the
// value is part of the closed set migrations/0001_init.sql fixes, the same
// reasoning conversation.go's own mirrors (authorYou, msgTypeAnswer, ...)
// already give.
const specTestMsgTypeEscalation = "escalation"

// specTestBase is store.Ticket{Title: "T", Body: "B"}'s own rendered base
// text, repeated across several TestRenderSpec subtests whose block is
// dropped (goconst).
const specTestBase = "T\n\nB"

// seedOwnerDecision inserts one resolved escalation question through the
// real store -- an escalation message, a question parented to it (key Q90,
// state resolved), a sent owner answer picking "a", and a sent owner reply
// whose body is reply -- the fixture task 3 and 5's own
// TestReviewPromptCarriesOwnerDecisions, TestJudgePromptCarriesOwnerDecisions,
// and TestMergeFirstPromptCarriesOwnerDecisions seed before driving their
// handler, so specFor's OwnerAnswers source has something to carry.
func seedOwnerDecision(t *testing.T, s *store.Store, ticketID int64, reply string) {
	t.Helper()
	ctx := t.Context()

	escPayload, err := json.Marshal(response.EscalationPayload{
		Code: string(response.EscalationCodePlanGap), What: "H1 is false", Why: "the red test passed",
		Tried: "ran it", Options: []string{"retry", "abandon"}, Origin: string(response.EscalationOriginBuild),
	})
	if err != nil {
		t.Fatalf("seedOwnerDecision: marshal escalation payload: %v", err)
	}
	escID, err := s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, Type: specTestMsgTypeEscalation, Author: authorZing,
		Body: "plan_gap: H1 is false", Payload: escPayload,
	})
	if err != nil {
		t.Fatalf("seedOwnerDecision: insert escalation: %v", err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q90", Kind: response.QuestionKindQuestion, State: response.QuestionStateResolved,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: pbEscalationTextRetry}, {Key: "c", Text: pbEscalationTextAbandon}},
	})
	if err != nil {
		t.Fatalf("seedOwnerDecision: marshal question payload: %v", err)
	}
	qID, err := s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &escID, Type: msgTypeQuestion, Author: authorZing,
		State: new("resolved"), Body: "plan_gap: H1 is false\n\nHow should Zing proceed?", Payload: qPayload,
	})
	if err != nil {
		t.Fatalf("seedOwnerDecision: insert question: %v", err)
	}

	answerPayload, err := json.Marshal(response.AnswerPayload{Option: new("a")})
	if err != nil {
		t.Fatalf("seedOwnerDecision: marshal answer payload: %v", err)
	}
	if _, err := s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &qID, Type: msgTypeAnswer, Author: authorYou,
		State: new(answerStateSent), Payload: answerPayload,
	}); err != nil {
		t.Fatalf("seedOwnerDecision: insert answer: %v", err)
	}

	if _, err := s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &qID, Type: msgTypeReply, Author: authorYou,
		State: new(answerStateSent), Body: reply,
	}); err != nil {
		t.Fatalf("seedOwnerDecision: insert reply: %v", err)
	}
}

func TestRenderSpec(t *testing.T) {
	t.Parallel()

	t.Run("no_decisions", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "Fetch retries", Body: "Retry a failed fetch."}
		got := renderSpec(ticket, store.PlanningConversation{}, nil, "")
		want := "Fetch retries\n\nRetry a failed fetch."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("worked_example", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "Fetch retries", Body: "Retry a failed fetch and add a ticket note."}

		q2 := questionRow(2, "Q2", "Is the ticket note in scope?", "",
			[]response.Option{{Key: "b", Text: "No, defer to #84"}}, "b")
		conv := store.PlanningConversation{Threads: []store.Thread{{
			Question: q2,
			Turns:    []store.MessageRow{ownerAnswerRow(20, 2, 1, "b")},
			Settled:  true,
			Decision: "The fetch-failure ticket note is out of scope and deferred to #84.",
		}}}

		q5 := questionRow(5, "Q5", "plan_gap: H1 is false", "How should Zing proceed?",
			[]response.Option{{Key: "a", Text: pbEscalationTextRetry}}, "a")
		answers := []store.OwnerAnswer{{
			Question:   q5,
			Escalation: true,
			Owner: []store.MessageRow{
				ownerAnswerRow(50, 5, 1, "a"),
				ownerReplyRow(51, 5, 1, "Keep the test as a guard only."),
			},
		}}

		got := renderSpec(ticket, conv, answers, "Keep the JSON output stable.")
		want := "Fetch retries\n\n" +
			"Retry a failed fetch and add a ticket note.\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"Q2: Is the ticket note in scope?\n" +
			"The owner, oldest first:\n" +
			"- picked option b: No, defer to #84\n" +
			"Decision: The fetch-failure ticket note is out of scope and deferred to #84.\n\n" +
			"Q5: plan_gap: H1 is false\n" +
			"The owner, oldest first:\n" +
			"- wrote: Keep the test as a guard only.\n\n" +
			"At the gate, the owner approved the plan and wrote:\n" +
			"- Keep the JSON output stable."
		if got != want {
			t.Errorf("renderSpec =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("unsettled_thread_omitted", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q1", "Still open?", "", nil, "a")
		conv := store.PlanningConversation{Threads: []store.Thread{{Question: q1, Settled: false}}}
		got := renderSpec(ticket, conv, nil, "")
		if got != specTestBase {
			t.Errorf("renderSpec = %q, want %q", got, specTestBase)
		}
	})

	t.Run("settled_no_owner_rows", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q1", "Settled with no owner rows", "", nil, "a")
		conv := store.PlanningConversation{Threads: []store.Thread{{
			Question: q1, Settled: true, Decision: "Settled by the planner alone.",
		}}}
		got := renderSpec(ticket, conv, nil, "")
		want := specTestBase + "\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"Q1: Settled with no owner rows\n" +
			"Decision: Settled by the planner alone."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("escalation_pick_only", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q1", "An escalation question", "", []response.Option{{Key: "a", Text: pbEscalationTextRetry}}, "a")
		answers := []store.OwnerAnswer{{
			Question: q1, Escalation: true,
			Owner: []store.MessageRow{ownerAnswerRow(10, 1, 1, "a")},
		}}
		got := renderSpec(ticket, store.PlanningConversation{}, answers, "")
		if got != specTestBase {
			t.Errorf("renderSpec = %q, want %q", got, specTestBase)
		}
	})

	t.Run("escalation_pick_and_reply", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q1", "An escalation question", "", []response.Option{{Key: "a", Text: pbEscalationTextRetry}}, "a")
		answers := []store.OwnerAnswer{{
			Question: q1, Escalation: true,
			Owner: []store.MessageRow{
				ownerAnswerRow(10, 1, 1, "a"),
				ownerReplyRow(11, 1, 1, "Keep the guard only."),
			},
		}}
		got := renderSpec(ticket, store.PlanningConversation{}, answers, "")
		want := specTestBase + "\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"Q1: An escalation question\n" +
			"The owner, oldest first:\n" +
			"- wrote: Keep the guard only."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("job_question_pick", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q3", "A classify-style question", "",
			[]response.Option{{Key: "b", Text: "Keep it this way"}}, "a")
		answers := []store.OwnerAnswer{{
			Question: q1, Escalation: false,
			Owner: []store.MessageRow{ownerAnswerRow(10, 1, 1, "b")},
		}}
		got := renderSpec(ticket, store.PlanningConversation{}, answers, "")
		want := specTestBase + "\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"Q3: A classify-style question\n" +
			"The owner, oldest first:\n" +
			"- picked option b: Keep it this way"
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("items_only_answer", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q1", "A perimeter question", "", nil, "a")
		payload, err := json.Marshal(response.AnswerPayload{Items: map[string]response.Decision{"f1": response.DecisionAccept}})
		if err != nil {
			t.Fatalf("marshal items-only answer payload: %v", err)
		}
		parentID := int64(1)
		answers := []store.OwnerAnswer{{
			Question: q1, Escalation: false,
			Owner: []store.MessageRow{{ID: 10, Type: msgTypeAnswer, Author: authorYou, ParentID: &parentID, Payload: payload}},
		}}
		got := renderSpec(ticket, store.PlanningConversation{}, answers, "")
		if got != specTestBase {
			t.Errorf("renderSpec = %q, want %q", got, specTestBase)
		}
	})

	t.Run("empty_key", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "", "A question with no key", "", []response.Option{{Key: "a", Text: "Yes"}}, "a")
		conv := store.PlanningConversation{Threads: []store.Thread{{
			Question: q1, Settled: true, Decision: "Decided.",
		}}}
		got := renderSpec(ticket, conv, nil, "")
		want := specTestBase + "\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"A question with no key\n" +
			"Decision: Decided."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("approval_notes", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q1", "A settled thread", "", nil, "a")
		conv := store.PlanningConversation{Threads: []store.Thread{{
			Question: q1, Settled: true, Decision: "Decided.",
		}}}
		got := renderSpec(ticket, conv, nil, "Keep the JSON output stable.\nDo not touch the CLI flags.")
		want := specTestBase + "\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"Q1: A settled thread\n" +
			"Decision: Decided.\n\n" +
			"At the gate, the owner approved the plan and wrote:\n" +
			"- Keep the JSON output stable.\n" +
			"  Do not touch the CLI flags."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("ordering", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q9 := questionRow(9, "Q9", "A later settled thread", "", nil, "a")
		conv := store.PlanningConversation{Threads: []store.Thread{{
			Question: q9, Settled: true, Decision: "Decided late.",
		}}}
		q3 := questionRow(3, "Q3", "An earlier answered question", "", []response.Option{{Key: "b", Text: "B text"}}, "a")
		answers := []store.OwnerAnswer{{
			Question: q3, Escalation: false,
			Owner: []store.MessageRow{ownerAnswerRow(30, 3, 1, "b")},
		}}
		got := renderSpec(ticket, conv, answers, "")
		want := specTestBase + "\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"Q3: An earlier answered question\n" +
			"The owner, oldest first:\n" +
			"- picked option b: B text\n\n" +
			"Q9: A later settled thread\n" +
			"Decision: Decided late."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("agent_turns_skipped", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B"}
		q1 := questionRow(1, "Q1", "A settled thread", "", []response.Option{{Key: "a", Text: "Keep it"}}, "a")
		conv := store.PlanningConversation{Threads: []store.Thread{{
			Question: q1,
			Turns: []store.MessageRow{
				ownerAnswerRow(10, 1, 1, "a"),
				agentReplyRow(11, 1, "the planner's own aside, never the owner's words"),
				ownerReplyRow(12, 1, 1, "   "),
			},
			Settled:  true,
			Decision: "Settled by the planner and the owner together.",
		}}}
		got := renderSpec(ticket, conv, nil, "")
		want := specTestBase + "\n\n" +
			"Owner decisions, oldest first. They amend the ticket text above.\n\n" +
			"Q1: A settled thread\n" +
			"The owner, oldest first:\n" +
			"- picked option a: Keep it\n" +
			"Decision: Settled by the planner and the owner together."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})
}

// TestRenderSpecOwnerComments proves renderSpec's owner-comments block
// (#98, design "comments appear inside the ticket input"): the comments sit
// right after the body, under specCommentsHeader, and ahead of
// specDecisionsHeader when there is one.
func TestRenderSpecOwnerComments(t *testing.T) {
	t.Parallel()

	t.Run("comments_no_decisions", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B", OwnerComments: "Comment by owner-login:\nUse serve."}
		got := renderSpec(ticket, store.PlanningConversation{}, nil, "")
		want := specTestBase + "\n\n" +
			specCommentsHeader + "\n\n" +
			"Comment by owner-login:\nUse serve."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})

	t.Run("comments_and_decisions", func(t *testing.T) {
		t.Parallel()
		ticket := store.Ticket{Title: "T", Body: "B", OwnerComments: "Comment by owner-login:\nUse serve."}
		q1 := questionRow(1, "Q1", "Settled with no owner rows", "", nil, "a")
		conv := store.PlanningConversation{Threads: []store.Thread{{
			Question: q1, Settled: true, Decision: "Settled by the planner alone.",
		}}}
		got := renderSpec(ticket, conv, nil, "")
		want := specTestBase + "\n\n" +
			specCommentsHeader + "\n\n" +
			"Comment by owner-login:\nUse serve.\n\n" +
			specDecisionsHeader + "\n\n" +
			"Q1: Settled with no owner rows\n" +
			"Decision: Settled by the planner alone."
		if got != want {
			t.Errorf("renderSpec = %q, want %q", got, want)
		}
	})
}
