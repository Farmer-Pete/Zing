package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"zing/internal/response"
)

// TestAbandonTicket_WaitingOnBuilderQuestion proves AbandonTicket's main
// path (#65): a ticket waiting on a builder question (not an escalation) is
// abandoned, its question resolved, and exactly one new message -- the
// state message carrying the owner's reason -- is written.
func TestAbandonTicket_WaitingOnBuilderQuestion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStateBuilding)
	owner, expires := claimForCommit(t, s, ticketID)

	before, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages before: %v", err)
	}

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Session: &SessionUpsert{Job: testStateBuilding, Runtime: testRuntimeFake},
		Runs:    []Run{{Turn: 0, Outcome: new(testTypeQuestion)}},
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

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket after commit: %v", err)
	}
	if got.ClaimOwner != nil {
		t.Fatalf("ticket still claimed after CommitHandlerResult, want released: %v", got.ClaimOwner)
	}

	const reason = "owner abandoned from the console"
	if abandonErr := s.AbandonTicket(ctx, ticketID, reason); abandonErr != nil {
		t.Fatalf("AbandonTicket: %v", abandonErr)
	}

	got, err = s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.State != testStateAbandoned {
		t.Errorf("state = %q, want %s", got.State, testStateAbandoned)
	}
	if got.WaitingOn != nil {
		t.Errorf("waiting_on = %v, want nil", got.WaitingOn)
	}

	after, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages after: %v", err)
	}
	if len(after) != len(before)+2 {
		// before had 0 messages; the commit added the question (+1), abandon
		// resolves it in place (no new row) and adds the state message (+1).
		t.Fatalf("message count = %d, want %d (the commit's question plus the abandon state message)", len(after), len(before)+2)
	}

	var question MessageRow
	for _, m := range after {
		if m.Type == msgTypeQuestion {
			question = m
		}
	}
	if question.ID == 0 {
		t.Fatal("no question message found")
	}
	if question.State == nil || *question.State != questionStateResolved {
		t.Errorf("question state = %v, want resolved", question.State)
	}

	last := after[len(after)-1]
	if last.Type != msgTypeState {
		t.Fatalf("last message type = %q, want state", last.Type)
	}
	var payload response.StatePayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal state payload: %v", err)
	}
	if payload.From != response.TicketState(testStateBuilding) || payload.To != response.TicketStateAbandoned || payload.Reason != reason {
		t.Errorf("state payload = %+v, want from %s to %s reason %q", payload, testStateBuilding, response.TicketStateAbandoned, reason)
	}
}

// TestAbandonTicket_RefusesClaimedAndTerminal proves AbandonTicket's three
// refusals (#65): a claimed ticket, a terminal (done) ticket, and an unknown
// id, each changing nothing. It also proves CanAbandon's own boundary.
func TestAbandonTicket_RefusesClaimedAndTerminal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	_, claimedTicket := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, claimedTicket, testStatePlanning)
	claimForCommit(t, s, claimedTicket)

	before, err := s.ListMessages(ctx, claimedTicket)
	if err != nil {
		t.Fatalf("ListMessages before: %v", err)
	}

	err = s.AbandonTicket(ctx, claimedTicket, "owner abandoned from the console")
	var abandonErr *AbandonError
	if !errors.As(err, &abandonErr) || abandonErr.Code != AbandonCodeClaimed || abandonErr.Reason != AbandonClaimedReason {
		t.Fatalf("AbandonTicket(claimed) = %v, want *AbandonError{Code: claimed, Reason: %q}", err, AbandonClaimedReason)
	}

	got, err := s.GetTicket(ctx, claimedTicket)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.State != testStatePlanning {
		t.Errorf("claimed ticket state = %q, want unchanged %q", got.State, testStatePlanning)
	}
	after, err := s.ListMessages(ctx, claimedTicket)
	if err != nil {
		t.Fatalf("ListMessages after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("message count after a claimed refusal = %d, want unchanged %d", len(after), len(before))
	}

	_, doneTicket := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, doneTicket, testStateDone)

	err = s.AbandonTicket(ctx, doneTicket, "owner abandoned from the console")
	if !errors.As(err, &abandonErr) || abandonErr.Code != AbandonCodeTerminal {
		t.Fatalf("AbandonTicket(done) = %v, want *AbandonError{Code: terminal}", err)
	}
	wantReason := fmt.Sprintf("ticket %d is already done", doneTicket)
	if abandonErr.Reason != wantReason {
		t.Errorf("AbandonTicket(done).Reason = %q, want %q", abandonErr.Reason, wantReason)
	}

	got, err = s.GetTicket(ctx, doneTicket)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.State != testStateDone {
		t.Errorf("done ticket state = %q, want unchanged %q", got.State, testStateDone)
	}

	err = s.AbandonTicket(ctx, 999999, "owner abandoned from the console")
	if !errors.As(err, &abandonErr) || abandonErr.Code != AbandonCodeNotFound {
		t.Fatalf("AbandonTicket(unknown) = %v, want *AbandonError{Code: not_found}", err)
	}

	wantAbandonable := []string{ticketStateQueued, ticketStatePlanning, testStateBuilding, testStateReviewing, testStateJudging, testStateShipping}
	for _, state := range wantAbandonable {
		if !CanAbandon(state) {
			t.Errorf("CanAbandon(%q) = false, want true", state)
		}
	}
	for _, state := range []string{testStateDone, testStateEscalated, testStateAbandoned} {
		if CanAbandon(state) {
			t.Errorf("CanAbandon(%q) = true, want false", state)
		}
	}
}
