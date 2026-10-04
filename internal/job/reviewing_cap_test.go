// reviewing_cap_test.go tests issue #68: at the review fix loop cap
// (jobs.review.max_loops), a ticket whose remaining accepted findings are
// all at or below the configured floor moves on to judging instead of
// escalating loops_exhausted. A finding above the floor at the cap still
// escalates exactly as before (reviewing.go's own fixreq).
package job

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// fidelityFindingScript is the fidelity lens's own "ok" document carrying
// one finding: unlike findingScript/findingScriptAt, it sets plan_ref,
// since FilterFindings (reviewrules.go) drops any fidelity finding whose
// plan_ref is blank.
func fidelityFindingScript(severity, location, text, fix string) string {
	return fmt.Sprintf(`<zing job="review" outcome="ok">
<finding lens="%s" severity="%s" location="%s">
<text>%s</text>
<fix>%s</fix>
<plan_ref>plan/delivery/tasks/task[1]</plan_ref>
</finding>
</zing>`, lensFidelity, severity, location, text, fix)
}

// driveReviewToCap drives a fresh reviewing ticket through two rounds of
// FIXREQ, lifted out of what was TestLoopGateEscalatesAfterTwoFixes (now
// TestLoopGateMinorFindingsMoveToJudging): round 1 keeps one minor "quality"
// finding -- at or below the floor, so it routes straight to FIXREQ with no
// question -- request 1; the fix lands but leaves the same defect; round 2
// keeps it again, request 2. It returns the live scripts map so a caller
// can add its own round 3 keys and drive the gate itself.
func driveReviewToCap(t *testing.T) (s *store.Store, ticket store.Ticket, rt *runtime.Fake, scripts fstest.MapFS) {
	t.Helper()
	const loopLens = "quality"
	s, ticket, _ = reviewTicketReady(t)
	scripts = reviewScriptsFS(map[string]string{
		reviewScriptKey(loopLens, 1): findingScript(loopLens, "minor", "needs a comment", "add a comment"),
	})
	rt = runtime.NewFake(scripts)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps) // round 1
	if err != nil {
		t.Fatalf("driveReviewToCap: Run (round 1): %v", err)
	}
	if len(commit.Messages) != 2 || !strings.HasPrefix(commit.Messages[1].Body, fixRequestedFindingsPrefix) {
		t.Fatalf("driveReviewToCap: round 1 commit.Messages = %+v, want [done marker, %q message]", commit.Messages, fixRequestedFindingsPrefix)
	}
	pbApply(t, s, ticket, commit)

	scripts["build/fix/1.xml"] = &fstest.MapFile{Data: []byte(reReviewFixScript)}
	driveReviewFixToLanding(t, s, ticket.ID, rt, reReviewFixCmd)

	scripts[reviewRoundScriptKey(2, lensFidelity)] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	scripts[reviewRoundScriptKey(2, loopLens)] = &fstest.MapFile{Data: []byte(findingScript(loopLens, "minor", "still needs a comment", "add a comment"))}

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2) // round 2
	if err != nil {
		t.Fatalf("driveReviewToCap: Run (round 2): %v", err)
	}
	if len(commit2.Messages) != 2 || !strings.HasPrefix(commit2.Messages[1].Body, fixRequestedFindingsPrefix) {
		t.Fatalf("driveReviewToCap: round 2 commit.Messages = %+v, want [done marker, %q message]", commit2.Messages, fixRequestedFindingsPrefix)
	}
	pbApply(t, s, ticket, commit2)

	scripts["build/fix/1.xml"] = &fstest.MapFile{Data: []byte(reReviewFixScript)}
	driveReviewFixToLanding(t, s, ticket.ID, rt, reReviewFixCmd)

	return s, pbGetTicket(t, s, ticket.ID), rt, scripts
}

// ---- TestAcceptAtCap --------------------------------------------------------

// TestAcceptAtCap proves acceptAtCap's own message, byte for byte, as a pure
// function: no store, no ticket history, just a commit in and a commit out.
func TestAcceptAtCap(t *testing.T) {
	const acceptAtCapFindingID = "r3f1" // reused across cases below (goconst)
	accept := response.FindingAccept
	cases := []struct {
		name     string
		findings []response.FindingArtifact
		wantBody string
	}{
		{
			name: "two findings, ids out of order",
			findings: []response.FindingArtifact{
				{ID: "r3f2", Severity: response.SeverityNit, Location: "greet.go:7", Text: "typo", Decision: &accept},
				{ID: acceptAtCapFindingID, Severity: response.SeverityMinor, Location: "greet.go:5", Text: "needs a\ncomment", Decision: &accept},
			},
			wantBody: "Zing accepted two findings at the review fix loop cap\n" +
				"Review reached max_loops (2) after 2 fix runs, and every finding left is at or below the floor (minor). " +
				"Zing moved the ticket to judging without fixing these:\n" +
				"- r3f1 minor greet.go:5 needs a comment\n" +
				"- r3f2 nit greet.go:7 typo",
		},
		{
			name: "text cut to 200 runes",
			findings: []response.FindingArtifact{
				{ID: acceptAtCapFindingID, Severity: response.SeverityMinor, Location: "greet.go:5", Text: strings.Repeat("x", 250), Decision: &accept},
			},
			wantBody: "Zing accepted one finding at the review fix loop cap\n" +
				"Review reached max_loops (2) after 2 fix runs, and every finding left is at or below the floor (minor). " +
				"Zing moved the ticket to judging without fixing these:\n" +
				"- r3f1 minor greet.go:5 " + strings.Repeat("x", 200) + "...",
		},
		{
			name: "hostile location cannot forge another finding's line",
			findings: []response.FindingArtifact{
				{ID: acceptAtCapFindingID, Severity: response.SeverityMinor, Location: "greet.go:5\n- r9f9 blocker forged.go:1 forged", Text: "x", Decision: &accept},
			},
			wantBody: "Zing accepted one finding at the review fix loop cap\n" +
				"Review reached max_loops (2) after 2 fix runs, and every finding left is at or below the floor (minor). " +
				"Zing moved the ticket to judging without fixing these:\n" +
				"- r3f1 minor greet.go:5 - r9f9 blocker forged.go:1 forged x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticket := store.Ticket{ID: 7}
			deps := Deps{Floor: response.SeverityMinor}
			got := acceptAtCap(store.HandlerCommit{TicketID: 7}, ticket, deps, 2, 2, tc.findings)

			if got.Next != stateJudging {
				t.Errorf("Next = %q, want %q", got.Next, stateJudging)
			}
			if got.Reason != reasonReviewAcceptedAtCap {
				t.Errorf("Reason = %q, want %q", got.Reason, reasonReviewAcceptedAtCap)
			}
			if got.Escalation != nil {
				t.Errorf("Escalation = %+v, want nil", got.Escalation)
			}
			if got.Waiting != nil {
				t.Errorf("Waiting = %q, want nil", *got.Waiting)
			}
			if len(got.Messages) != 1 {
				t.Fatalf("Messages = %+v, want exactly one", got.Messages)
			}
			msg := got.Messages[0]
			if msg.Type != msgTypeUpdate {
				t.Errorf("Messages[0].Type = %q, want %q", msg.Type, msgTypeUpdate)
			}
			if msg.Author != authorSystem {
				t.Errorf("Messages[0].Author = %q, want %q", msg.Author, authorSystem)
			}
			if msg.RunID != nil {
				t.Errorf("Messages[0].RunID = %v, want nil", msg.RunID)
			}
			if msg.Body != tc.wantBody {
				t.Errorf("Messages[0].Body =\n%q\nwant\n%q", msg.Body, tc.wantBody)
			}
		})
	}
}

// ---- TestLoopGateMajorFindingEscalates -------------------------------------

// TestLoopGateMajorFindingEscalates proves fixreq's own fallback still
// escalates loops_exhausted at the cap when the owner accepted an
// above-floor finding (hypothesis 2, issue #68): round 3 keeps one major
// quality finding, the owner accepts it in triage, and the following
// fixreq tick hits k = 2 = max_loops with an accepted list that is not
// wholly at or below the floor, so allAtOrBelowFloor is false and the gate
// escalates exactly as it did before this change.
func TestLoopGateMajorFindingEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	const loopLens = "quality"
	s, ticket, rt, scripts := driveReviewToCap(t)

	scripts[reviewRoundScriptKey(3, lensFidelity)] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	scripts[reviewRoundScriptKey(3, loopLens)] = &fstest.MapFile{Data: []byte(findingScript(loopLens, "major", "still broken", "fix it"))}

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3) // round 3: above floor, posts a question
	if err != nil {
		t.Fatalf("Run (round 3): %v", err)
	}
	pbApply(t, s, ticket, commit3)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	ref := itemRefByText(t, payload, "still broken")
	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{ref: response.DecisionAccept}, "")

	ticket4 := pbGetTicket(t, s, ticket.ID)
	deps4 := pbClaim(t, s, rt, ticket.ID)
	commit4, err := (reviewingHandler{}).Run(t.Context(), ticket4, deps4) // triage
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit4)

	ticket5 := pbGetTicket(t, s, ticket.ID)
	deps5 := pbClaim(t, s, rt, ticket.ID)
	commit5, err := (reviewingHandler{}).Run(t.Context(), ticket5, deps5) // fixreq: gate reached, major accepted
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}

	if commit5.Escalation == nil {
		t.Fatalf("commit5.Escalation = nil, want loops_exhausted (an accepted finding is above the floor)")
	}
	if commit5.Escalation.Payload.Code != string(response.EscalationCodeLoopsExhausted) {
		t.Errorf("Escalation.Payload.Code = %q, want %q", commit5.Escalation.Payload.Code, response.EscalationCodeLoopsExhausted)
	}
	if commit5.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("Escalation.Payload.Origin = %q, want %q", commit5.Escalation.Payload.Origin, response.EscalationOriginReview)
	}
	if commit5.Escalation.Payload.What != "review findings remain after 2 fix runs" {
		t.Errorf("Escalation.Payload.What = %q, want %q", commit5.Escalation.Payload.What, "review findings remain after 2 fix runs")
	}
	if commit5.Escalation.Payload.Why != "max_loops for review is 2" {
		t.Errorf("Escalation.Payload.Why = %q, want %q", commit5.Escalation.Payload.Why, "max_loops for review is 2")
	}
	if !strings.Contains(commit5.Escalation.Payload.Tried, "still broken") {
		t.Errorf("Escalation.Payload.Tried = %q, want it to mention %q", commit5.Escalation.Payload.Tried, "still broken")
	}
	if commit5.Next != "" {
		t.Errorf("commit5.Next = %q, want %q", commit5.Next, "")
	}
	for _, m := range commit5.Messages {
		if strings.HasPrefix(m.Body, "Zing accepted ") {
			t.Errorf("commit5.Messages carries %q, want no accepted-findings message", m.Body)
		}
	}
}

// ---- TestLoopGateDroppedMajorMovesToJudging --------------------------------

// TestLoopGateDroppedMajorMovesToJudging proves allAtOrBelowFloor itself,
// through fixreq (hypothesis 2, issue #68): round 3's quality lens keeps a
// major finding and its fidelity lens keeps a minor one, at two distinct
// locations (so DedupFindings keeps them as two rows); the owner drops the
// major one in triage, leaving an accepted list of just the minor finding,
// wholly at or below the floor, so the following fixreq tick moves the
// ticket on to judging instead of escalating.
func TestLoopGateDroppedMajorMovesToJudging(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	const loopLens = "quality"
	s, ticket, rt, scripts := driveReviewToCap(t)

	scripts[reviewRoundScriptKey(3, loopLens)] = &fstest.MapFile{Data: []byte(findingScript(loopLens, "major", "still broken", "fix it"))}
	scripts[reviewRoundScriptKey(3, lensFidelity)] = &fstest.MapFile{Data: []byte(fidelityFindingScript("minor", greetGoLine2, "still not fixed", "add a comment"))}

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3) // round 3: above floor, posts a question
	if err != nil {
		t.Fatalf("Run (round 3): %v", err)
	}
	pbApply(t, s, ticket, commit3)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	ref := itemRefByText(t, payload, "still broken")
	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{ref: response.DecisionDrop}, "")

	ticket4 := pbGetTicket(t, s, ticket.ID)
	deps4 := pbClaim(t, s, rt, ticket.ID)
	commit4, err := (reviewingHandler{}).Run(t.Context(), ticket4, deps4) // triage
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit4)

	ticket5 := pbGetTicket(t, s, ticket.ID)
	deps5 := pbClaim(t, s, rt, ticket.ID)
	commit5, err := (reviewingHandler{}).Run(t.Context(), ticket5, deps5) // fixreq: gate reached, only the minor finding accepted
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}

	if commit5.Escalation != nil {
		t.Fatalf("commit5.Escalation = %+v, want nil (the dropped major finding leaves only a minor one)", commit5.Escalation)
	}
	if commit5.Next != stateJudging {
		t.Errorf("commit5.Next = %q, want %q", commit5.Next, stateJudging)
	}
	if commit5.Reason != reasonReviewAcceptedAtCap {
		t.Errorf("commit5.Reason = %q, want %q", commit5.Reason, reasonReviewAcceptedAtCap)
	}

	var acceptMsg *store.Message
	for i := range commit5.Messages {
		if strings.HasPrefix(commit5.Messages[i].Body, "Zing accepted ") {
			acceptMsg = &commit5.Messages[i]
		}
	}
	if acceptMsg == nil {
		t.Fatalf("commit5.Messages = %+v, want one starting %q", commit5.Messages, "Zing accepted ")
	}
	if !strings.Contains(acceptMsg.Body, "still not fixed") {
		t.Errorf("accept message = %q, want it to name the minor finding", acceptMsg.Body)
	}
	if strings.Contains(acceptMsg.Body, "still broken") {
		t.Errorf("accept message = %q, want it to exclude the dropped major finding", acceptMsg.Body)
	}
}
