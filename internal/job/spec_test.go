// spec_test.go tests task 2: the plan review tick carries the owner's
// settled planning decisions in its ticket input (design "Decisions and
// additions"), through the real planning handler (job.Registry()
// ["planning"].Run), reusing planning_test.go's own fixtures
// (seedFeatureTicketInPlanning, seedAnsweredPlanningRound, seedCohort,
// readyStep, findingsResponse, recordingRuntime, scriptedRuntime).
package job_test

import (
	"strings"
	"testing"
	"time"

	"zing/internal/response"
	"zing/internal/store"
)

// TestPlanReviewPromptCarriesOwnerDecisions proves that specFor's settled-
// planning-thread source reaches the plan review prompt (design goal 1, 3):
// a cohort reviewed after the owner's planning answer settles Q1 carries
// that answer and the planner's decision inside the ticket input, ahead of
// the scenarios input.
func TestPlanReviewPromptCarriesOwnerDecisions(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	_, qID := seedAnsweredPlanningRound(t, s, ticketID)

	owner := "settle-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	const decision = "The ticket note is out of scope."
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Conversation: &store.ConversationCommit{
			ThroughBatch: 1, // the owner's answer (seedAnsweredPlanningRound) is batch 1; below it the settle defers as "late".
			Settle:       []store.SettleQuestion{{QuestionID: qID, Decision: decision}},
		},
	})
	if err != nil || !applied {
		t.Fatalf("CommitHandlerResult (settle): applied=%v err=%v", applied, err)
	}

	seedCohort(t, s, ticketID, validPlan("Review the plan once the owner's planning answer has settled."), validScenarios(2, "owner-decision"))

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(), "owner-decision-review-sess")}}
	rec := &recordingRuntime{rt: rt}

	if _, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID); err != nil {
		t.Fatalf("planning run: %v", err)
	}

	prompt := rec.lastReq.Prompt
	found := strings.Contains(prompt, "ticket:\n")
	if !found {
		t.Fatalf("prompt has no ticket input:\n%s", prompt)
	}
	scenariosIdx := strings.Index(prompt, "scenarios:")
	if scenariosIdx == -1 {
		t.Fatalf("prompt has no scenarios input:\n%s", prompt)
	}

	for _, want := range []string{
		"Owner decisions, oldest first. They amend the ticket text above.",
		"picked option a: " + testOptionAText,
		"Decision: " + decision,
	} {
		idx := strings.Index(prompt, want)
		if idx == -1 {
			t.Errorf("prompt does not contain %q:\n%s", want, prompt)
			continue
		}
		if idx > scenariosIdx {
			t.Errorf("%q appears after the scenarios input, want it inside the ticket input before it", want)
		}
	}
	if rec.lastReq.Job != response.JobPlanreview {
		t.Errorf("last runtime request job = %s, want planreview", rec.lastReq.Job)
	}
}
