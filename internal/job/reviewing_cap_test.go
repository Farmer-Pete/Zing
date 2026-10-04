// reviewing_cap_test.go tests issue #68: at the review fix loop cap
// (jobs.review.max_loops), a ticket whose remaining accepted findings are
// all at or below the configured floor moves on to judging instead of
// escalating loops_exhausted. A finding above the floor at the cap still
// escalates exactly as before (reviewing.go's own fixreq).
package job

import (
	"strings"
	"testing"
	"testing/fstest"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

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
			if tc.name == "hostile location cannot forge another finding's line" {
				lines := strings.Split(msg.Body, "\n")
				if len(lines) != 3 {
					t.Fatalf("len(lines) = %d, want 3", len(lines))
				}
				for _, line := range lines {
					if strings.HasPrefix(line, "- r9f9") {
						t.Errorf("line %q forged a second finding", line)
					}
				}
			}
		})
	}
}
