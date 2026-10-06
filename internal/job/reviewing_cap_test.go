// reviewing_cap_test.go tests issue #68: at the review fix loop cap
// (jobs.review.max_loops), a ticket whose remaining accepted findings are
// all at or below the configured floor moves on to judging instead of
// escalating loops_exhausted. It also tests ticket 55: an above-floor
// finding at the cap starts a fix run, with no escalation, when at least
// one accepted row carries an owner's own explicit pick (OwnerPicked); a
// defaulted accept, or a row stored before OwnerPicked existed, still
// escalates loops_exhausted exactly as before (reviewing.go's own fixreq).
package job

import (
	"encoding/json"
	"fmt"
	"reflect"
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
// FIXREQ with two real, file-changing fixes (reReviewFixScript and
// reReviewFixCmd for both), lifted out of what was
// TestLoopGateEscalatesAfterTwoFixes (now TestLoopGateMinorFindingsMoveToJudging).
func driveReviewToCap(t *testing.T) (s *store.Store, ticket store.Ticket, rt *runtime.Fake, scripts fstest.MapFS) {
	t.Helper()
	return driveReviewToCapWithFix(t, reReviewFixScript, reReviewFixCmd)
}

// noopFixScript is a fix driver turn (job "build") that changes no file:
// ticket 60's own "the newest fix run changed nothing" scenario, paired with
// noopFixCmd.
const noopFixScript = `<zing job="build" outcome="ok">
  <claims>
    <files_changed>
    </files_changed>
  </claims>
  <report>Nothing to change in greet.go.</report>
  <notes></notes>
</zing>`

// noopFixCmd is reReviewFixCmd's own no-op counterpart: it touches no file,
// matching noopFixScript's own empty files_changed, so CHECK's own
// cross-check of claimed against real changed paths still agrees.
const noopFixCmd = "test -f greet.go"

// driveReviewToCapWithFix is driveReviewToCap generalized over its own
// second fix (ticket 60): round 1 keeps one minor "quality" finding -- at or
// below the floor, so it routes straight to FIXREQ with no question --
// request 1, landed with reReviewFixScript/reReviewFixCmd; round 2 keeps it
// again, request 2, landed with secondFixScript/secondFixCmd. It returns the
// live scripts map so a caller can add its own round 3 keys and drive the
// gate itself.
func driveReviewToCapWithFix(t *testing.T, secondFixScript, secondFixCmd string) (s *store.Store, ticket store.Ticket, rt *runtime.Fake, scripts fstest.MapFS) {
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
		t.Fatalf("driveReviewToCapWithFix: Run (round 1): %v", err)
	}
	if len(commit.Messages) != 2 || !strings.HasPrefix(commit.Messages[1].Body, fixRequestedFindingsPrefix) {
		t.Fatalf("driveReviewToCapWithFix: round 1 commit.Messages = %+v, want [done marker, %q message]", commit.Messages, fixRequestedFindingsPrefix)
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
		t.Fatalf("driveReviewToCapWithFix: Run (round 2): %v", err)
	}
	if len(commit2.Messages) != 2 || !strings.HasPrefix(commit2.Messages[1].Body, fixRequestedFindingsPrefix) {
		t.Fatalf("driveReviewToCapWithFix: round 2 commit.Messages = %+v, want [done marker, %q message]", commit2.Messages, fixRequestedFindingsPrefix)
	}
	pbApply(t, s, ticket, commit2)

	scripts["build/fix/1.xml"] = &fstest.MapFile{Data: []byte(secondFixScript)}
	driveReviewFixToLanding(t, s, ticket.ID, rt, secondFixCmd)

	return s, pbGetTicket(t, s, ticket.ID), rt, scripts
}

// driveToReviewLoopsExhausted drives driveReviewToCapWithFix's own ticket
// one further round (round 3, an above-floor "quality" major finding, "still
// broken") through the review question, triaged with no item decision and a
// free reply ("going with the recommendation", so the accept is the safe
// default, same as TestLoopGateDefaultAcceptEscalates), then into fixreq,
// which escalates loops_exhausted (ticket 60). Returns the store, ticket,
// fake runtime, and fixreq's own escalating commit, not yet applied.
func driveToReviewLoopsExhausted(t *testing.T, secondFixScript, secondFixCmd string) (s *store.Store, ticket store.Ticket, rt *runtime.Fake, fixreqCommit store.HandlerCommit) {
	t.Helper()
	s, ticket, rt, scripts := driveReviewToCapWithFix(t, secondFixScript, secondFixCmd)

	// Round 3's own major finding comes from lensFidelity, not the "quality"
	// loop lens: lensesForRound/selectLenses (6.7) always re-selects fidelity
	// regardless of which files changed, but re-selects "quality" only when
	// the fix actually touched greet.go -- not true when secondFixScript is
	// noopFixScript, this helper's own reason for existing.
	scripts[reviewRoundScriptKey(3, lensFidelity)] = &fstest.MapFile{Data: []byte(fidelityFindingScript("major", greetGoLine2, "still broken", "fix it"))}

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3) // round 3: above floor, posts a question
	if err != nil {
		t.Fatalf("driveToReviewLoopsExhausted: Run (round 3): %v", err)
	}
	pbApply(t, s, ticket, commit3)

	q := newestOpenQuestion(t, s, ticket.ID)
	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{}, "going with the recommendation")

	ticket4 := pbGetTicket(t, s, ticket.ID)
	deps4 := pbClaim(t, s, rt, ticket.ID)
	commit4, err := (reviewingHandler{}).Run(t.Context(), ticket4, deps4) // triage: defaults to accept
	if err != nil {
		t.Fatalf("driveToReviewLoopsExhausted: Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit4)

	ticket5 := pbGetTicket(t, s, ticket.ID)
	deps5 := pbClaim(t, s, rt, ticket.ID)
	commit5, err := (reviewingHandler{}).Run(t.Context(), ticket5, deps5) // fixreq: gate reached, defaulted accept
	if err != nil {
		t.Fatalf("driveToReviewLoopsExhausted: Run (fixreq): %v", err)
	}
	if commit5.Escalation == nil {
		t.Fatalf("driveToReviewLoopsExhausted: commit5.Escalation = nil, want loops_exhausted")
	}

	return s, pbGetTicket(t, s, ticket.ID), rt, commit5
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

// ---- TestLoopGateOwnerAcceptStartsFix ---------------------------------------

// TestLoopGateOwnerAcceptStartsFix proves ticket 55: when the owner's own
// explicit accept on the review question leaves an above-floor finding in
// the round's accepted list, the following fixreq tick at the cap (k = 2 =
// max_loops) starts a fix run instead of escalating loops_exhausted, since
// the review question already asked the owner this once.
func TestLoopGateOwnerAcceptStartsFix(t *testing.T) {
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
	commit5, err := (reviewingHandler{}).Run(t.Context(), ticket5, deps5) // fixreq: gate reached, owner picked accept
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}

	if commit5.Escalation != nil {
		t.Fatalf("commit5.Escalation = %+v, want nil (the owner's own accept starts a fix run)", commit5.Escalation)
	}
	if commit5.Next != "" {
		t.Errorf("commit5.Next = %q, want %q", commit5.Next, "")
	}
	if commit5.Waiting != nil {
		t.Errorf("commit5.Waiting = %q, want nil", *commit5.Waiting)
	}
	if len(commit5.Messages) != 1 {
		t.Fatalf("commit5.Messages = %+v, want exactly one", commit5.Messages)
	}
	if !strings.HasPrefix(commit5.Messages[0].Body, fixRequestedFindingsPrefix) {
		t.Errorf("commit5.Messages[0].Body = %q, want prefix %q", commit5.Messages[0].Body, fixRequestedFindingsPrefix)
	}
	if !strings.Contains(commit5.Messages[0].Body, "still broken") {
		t.Errorf("commit5.Messages[0].Body = %q, want it to mention %q", commit5.Messages[0].Body, "still broken")
	}
	for _, m := range commit5.Messages {
		if strings.HasPrefix(m.Body, "Zing accepted ") {
			t.Errorf("commit5.Messages carries %q, want no accepted-at-cap message", m.Body)
		}
	}

	pbApply(t, s, ticket, commit5)
	ticket6 := pbGetTicket(t, s, ticket.ID)
	if ticket6.State != ticket5.State || ticket6.State != stateReviewing {
		t.Errorf("ticket.State = %q, want %q unchanged", ticket6.State, stateReviewing)
	}
}

// ---- TestLoopGateDefaultAcceptEscalates -------------------------------------

// TestLoopGateDefaultAcceptEscalates proves fixreq's own fallback still
// escalates loops_exhausted at the cap when the above-floor finding's own
// accept is the safe default (no OwnerPicked), not the owner's own pick:
// the owner answers the review question with a free reply and no item
// decision, so triage defaults the item to accept with OwnerPicked false,
// and the following fixreq tick at the cap escalates exactly as before.
func TestLoopGateDefaultAcceptEscalates(t *testing.T) {
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
	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{}, "going with the recommendation")

	ticket4 := pbGetTicket(t, s, ticket.ID)
	deps4 := pbClaim(t, s, rt, ticket.ID)
	commit4, err := (reviewingHandler{}).Run(t.Context(), ticket4, deps4) // triage: defaults to accept
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit4)

	ticket5 := pbGetTicket(t, s, ticket.ID)
	deps5 := pbClaim(t, s, rt, ticket.ID)
	commit5, err := (reviewingHandler{}).Run(t.Context(), ticket5, deps5) // fixreq: gate reached, defaulted accept
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}

	if commit5.Escalation == nil {
		t.Fatalf("commit5.Escalation = nil, want loops_exhausted (the accept is a default, not an owner pick)")
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
		if strings.HasPrefix(m.Body, fixRequestedFindingsPrefix) || strings.HasPrefix(m.Body, "Zing accepted ") {
			t.Errorf("commit5.Messages carries %q, want neither a fix request nor an accepted-at-cap message", m.Body)
		}
	}
	wantExtra := []response.Option{{Key: escalationChoiceAccept, Text: reviewAcceptRemainingOptionText}}
	if !reflect.DeepEqual(commit5.Escalation.ExtraOptions, wantExtra) {
		t.Errorf("Escalation.ExtraOptions = %+v, want %+v", commit5.Escalation.ExtraOptions, wantExtra)
	}
	if commit5.Escalation.Recommended != escalationChoiceRetry {
		t.Errorf("Escalation.Recommended = %q, want %q (driveReviewToCap's second fix changed greet.go)", commit5.Escalation.Recommended, escalationChoiceRetry)
	}
}

// ---- TestLoopsExhaustedRecommendation ---------------------------------------

// TestLoopsExhaustedRecommendation proves loopsExhaustedRecommendation and
// newestFixChangedFiles as pure functions (ticket 60, owner decisions Q2
// and Q3's companion rule section 5).
func TestLoopsExhaustedRecommendation(t *testing.T) {
	accept := response.FindingAccept
	minor := response.FindingArtifact{ID: "r3f1", Severity: response.SeverityMinor, Decision: &accept}
	nit := response.FindingArtifact{ID: "r3f2", Severity: response.SeverityNit, Decision: &accept}
	major := response.FindingArtifact{ID: "r3f3", Severity: response.SeverityMajor, Decision: &accept}
	blocker := response.FindingArtifact{ID: "r3f4", Severity: response.SeverityBlocker, Decision: &accept}

	t.Run("recommendation", func(t *testing.T) {
		cases := []struct {
			name     string
			accepted []response.FindingArtifact
			changed  bool
			found    bool
			want     string
		}{
			{"minor and nit, changed, found: accept", []response.FindingArtifact{minor, nit}, true, true, escalationChoiceAccept},
			{"major, changed, found: retry", []response.FindingArtifact{major}, true, true, escalationChoiceRetry},
			{"blocker, unchanged, found: accept", []response.FindingArtifact{blocker}, false, true, escalationChoiceAccept},
			{"major, not found: retry", []response.FindingArtifact{major}, false, false, escalationChoiceRetry},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := loopsExhaustedRecommendation(tc.accepted, tc.changed, tc.found)
				if got != tc.want {
					t.Errorf("loopsExhaustedRecommendation(...) = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("newestFixChangedFiles", func(t *testing.T) {
		withFiles := response.BuildReport{FilesChanged: []string{"fix.go"}}
		noFiles := response.BuildReport{}
		task1WithFiles := withFiles
		task1WithFiles.TaskN = 1
		task2WithFiles := withFiles
		task2WithFiles.TaskN = 2

		cases := []struct {
			name        string
			reports     []store.BuildReportRow
			wantChanged bool
			wantFound   bool
		}{
			{
				name: "task report, fix with files, fix with none: newest fix wins",
				reports: []store.BuildReportRow{
					{Report: task1WithFiles},
					{Report: withFiles},
					{Report: noFiles},
				},
				wantChanged: false,
				wantFound:   true,
			},
			{
				name: "fix with none, then task report: fix still newest",
				reports: []store.BuildReportRow{
					{Report: noFiles},
					{Report: task2WithFiles},
				},
				wantChanged: false,
				wantFound:   true,
			},
			{
				name:      "task report only: not found",
				reports:   []store.BuildReportRow{{Report: task1WithFiles}},
				wantFound: false,
			},
			{
				name:      "nil: not found",
				reports:   nil,
				wantFound: false,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				changed, found := newestFixChangedFiles(tc.reports)
				if changed != tc.wantChanged || found != tc.wantFound {
					t.Errorf("newestFixChangedFiles(...) = (%v, %v), want (%v, %v)", changed, found, tc.wantChanged, tc.wantFound)
				}
			})
		}
	})
}

// ---- TestLoopGateLegacyAcceptRowEscalates -----------------------------------

// TestLoopGateLegacyAcceptRowEscalates proves a finding row stored before
// OwnerPicked existed (no "owner_picked" key at all, decoding as false)
// still escalates loops_exhausted at the cap: this test writes round 3's
// triage result itself, the same shape the old binary wrote, instead of
// calling triage.
func TestLoopGateLegacyAcceptRowEscalates(t *testing.T) {
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

	findings, err := s.Findings(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	row, ok := newestFindingRowPerID(findings)[ref]
	if !ok {
		t.Fatalf("no stored finding row for %q", ref)
	}
	accept := response.FindingAccept
	finding := row.Finding
	finding.Decision = &accept
	fPayload, marshalErr := json.Marshal(finding)
	if marshalErr != nil {
		t.Fatalf("marshal finding: %v", marshalErr)
	}
	if strings.Contains(string(fPayload), "owner_picked") {
		t.Fatalf("legacy payload = %s, want no %q key", fPayload, "owner_picked")
	}

	deps4 := pbClaim(t, s, rt, ticket.ID)
	c := baseCommit(ticket, deps4)
	c.ResolveQuestions = []int64{q.ID}
	c.Artifacts = []store.Artifact{{Type: artifactTypeFinding, RunID: row.RunID, Payload: fPayload}}
	pbApply(t, s, ticket, c)

	ticket5 := pbGetTicket(t, s, ticket.ID)
	deps5 := pbClaim(t, s, rt, ticket.ID)
	commit5, err := (reviewingHandler{}).Run(t.Context(), ticket5, deps5) // fixreq: gate reached, legacy accept row
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}

	if commit5.Escalation == nil {
		t.Fatalf("commit5.Escalation = nil, want loops_exhausted (the accept row carries no owner_picked)")
	}
	if commit5.Escalation.Payload.What != "review findings remain after 2 fix runs" {
		t.Errorf("Escalation.Payload.What = %q, want %q", commit5.Escalation.Payload.What, "review findings remain after 2 fix runs")
	}
	if commit5.Escalation.Payload.Why != "max_loops for review is 2" {
		t.Errorf("Escalation.Payload.Why = %q, want %q", commit5.Escalation.Payload.Why, "max_loops for review is 2")
	}
	for _, m := range commit5.Messages {
		if strings.HasPrefix(m.Body, fixRequestedFindingsPrefix) {
			t.Errorf("commit5.Messages carries %q, want no fix request message", m.Body)
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

// ---- TestLoopGateOwnerAcceptsRemainingFindings ------------------------------

// TestLoopGateOwnerAcceptsRemainingFindings proves ticket 60's own accept
// path end to end: at the review loop cap, with a major finding left and
// the newest fix run (noopFixScript/noopFixCmd) changing no file, the
// escalation offers a, d, c and recommends d; the owner's own explicit pick
// of d moves the ticket to judging, writes the accepted-findings message,
// and leaves no open question, with no database edit.
func TestLoopGateOwnerAcceptsRemainingFindings(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, fixreqCommit := driveToReviewLoopsExhausted(t, noopFixScript, noopFixCmd)

	wantExtra := []response.Option{{Key: escalationChoiceAccept, Text: reviewAcceptRemainingOptionText}}
	if !reflect.DeepEqual(fixreqCommit.Escalation.ExtraOptions, wantExtra) {
		t.Errorf("Escalation.ExtraOptions = %+v, want %+v", fixreqCommit.Escalation.ExtraOptions, wantExtra)
	}
	if fixreqCommit.Escalation.Recommended != escalationChoiceAccept {
		t.Errorf("Escalation.Recommended = %q, want %q (noopFixScript changed no file)", fixreqCommit.Escalation.Recommended, escalationChoiceAccept)
	}
	pbApply(t, s, ticket, fixreqCommit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	wantKeys := []string{"a", "d", "c"}
	gotKeys := make([]string, len(payload.Options))
	for i, o := range payload.Options {
		gotKeys[i] = o.Key
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("question option keys = %v, want %v", gotKeys, wantKeys)
	}
	if payload.Recommended != escalationChoiceAccept {
		t.Errorf("question Recommended = %q, want %q", payload.Recommended, escalationChoiceAccept)
	}

	pbAnswerEscalation(t, s, ticket.ID, q.ID, escalationChoiceAccept)

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2) // resolve the owner's own d pick
	if err != nil {
		t.Fatalf("Run (accept): %v", err)
	}

	if commit.Next != stateJudging {
		t.Errorf("commit.Next = %q, want %q", commit.Next, stateJudging)
	}
	if commit.Reason != reasonReviewOwnerAcceptedAtCap {
		t.Errorf("commit.Reason = %q, want %q", commit.Reason, reasonReviewOwnerAcceptedAtCap)
	}
	if commit.Escalation != nil {
		t.Errorf("commit.Escalation = %+v, want nil", commit.Escalation)
	}
	if len(commit.ResolveQuestions) == 0 {
		t.Error("commit.ResolveQuestions = empty, want non-empty")
	}

	var acceptMsg *store.Message
	for i := range commit.Messages {
		if strings.HasPrefix(commit.Messages[i].Body, "The owner accepted ") {
			if acceptMsg != nil {
				t.Fatalf("commit.Messages carries more than one message starting %q", "The owner accepted ")
			}
			acceptMsg = &commit.Messages[i]
		}
	}
	if acceptMsg == nil {
		t.Fatalf("commit.Messages = %+v, want exactly one starting %q", commit.Messages, "The owner accepted ")
	}
	wantFirstLine := "The owner accepted one finding at the review fix loop cap"
	if firstLine, _, _ := strings.Cut(acceptMsg.Body, "\n"); firstLine != wantFirstLine {
		t.Errorf("accept message first line = %q, want %q", firstLine, wantFirstLine)
	}
	if !strings.Contains(acceptMsg.Body, "still broken") {
		t.Errorf("accept message = %q, want it to mention %q", acceptMsg.Body, "still broken")
	}

	pbApply(t, s, ticket, commit)
	ticket3 := pbGetTicket(t, s, ticket.ID)
	if ticket3.State != stateJudging {
		t.Errorf("ticket.State = %q, want %q", ticket3.State, stateJudging)
	}
	open, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 0 {
		t.Errorf("QuestionsByState(open) = %+v, want zero rows", open)
	}
	answered, err := s.QuestionsByState(t.Context(), ticket.ID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered): %v", err)
	}
	if len(answered) != 0 {
		t.Errorf("QuestionsByState(answered) = %+v, want zero rows", answered)
	}
}

// ---- TestLoopGateReplyOnlyRetriesAtCap --------------------------------------

// TestLoopGateReplyOnlyRetriesAtCap proves owner decision Q3: a reply with
// no picked option on a review loops_exhausted question resolves as Retry,
// opening fix request k+1 with the reply appended as notes, even though the
// question recommends d.
func TestLoopGateReplyOnlyRetriesAtCap(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, fixreqCommit := driveToReviewLoopsExhausted(t, noopFixScript, noopFixCmd)
	pbApply(t, s, ticket, fixreqCommit)

	q := newestOpenQuestion(t, s, ticket.ID)
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &q.ID, Text: "looking into it"}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticket.ID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2) // resolve the reply-only answer
	if err != nil {
		t.Fatalf("Run (reply only): %v", err)
	}

	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want %q", commit.Next, "")
	}
	if commit.Escalation != nil {
		t.Errorf("commit.Escalation = %+v, want nil", commit.Escalation)
	}
	if len(commit.ResolveQuestions) == 0 {
		t.Error("commit.ResolveQuestions = empty, want non-empty")
	}

	var fixMsg *store.Message
	for i := range commit.Messages {
		body := commit.Messages[i].Body
		if strings.HasPrefix(body, fixRequestedFindingsPrefix) {
			fixMsg = &commit.Messages[i]
		}
		if strings.HasPrefix(body, "The owner accepted ") {
			t.Errorf("commit.Messages carries %q, want no accepted-at-cap message", body)
		}
	}
	if fixMsg == nil {
		t.Fatalf("commit.Messages = %+v, want one starting %q", commit.Messages, fixRequestedFindingsPrefix)
	}
	if !strings.Contains(fixMsg.Body, "still broken") {
		t.Errorf("fix request message = %q, want it to mention %q", fixMsg.Body, "still broken")
	}
	if !strings.Contains(fixMsg.Body, "looking into it") {
		t.Errorf("fix request message = %q, want it to mention the owner's own reply %q", fixMsg.Body, "looking into it")
	}
}
