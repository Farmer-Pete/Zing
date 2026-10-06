// escalation_detail_test.go is Task 1's test-first proof for #70's first
// change: a plan review loops_exhausted question lists the remaining
// findings from the planreview artifact at the version its own pending
// marker names (design section's withEscalationDetails, findingsDetail).
// Every test here drives the real store and the live GET /stream, the same
// boundary gate_test.go and question_kinds_test.go already exercise.
package console_test

import (
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// seedPlanReviewArtifactAt inserts one "planreview" artifact at version,
// carrying runID and findings, wrapped exactly as job/planning.go's own
// planReviewOkCommit stores them. Unlike gate_test.go's own
// seedPlanReviewArtifact (fixed at version 1, one cohort per fixture), this
// lets a test seed more than one plan version's own review on one ticket.
func seedPlanReviewArtifactAt(t *testing.T, s *store.Store, ticketID, runID int64, version int, findings []response.Finding) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Findings []response.Finding `json:"findings"`
	}{Findings: findings})
	if err != nil {
		t.Fatalf("marshal planreview findings: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, Type: testArtifactTypePlanreview, Version: version, RunID: &runID, Payload: payload,
	}); err != nil {
		t.Fatalf("InsertArtifact(planreview v%d): %v", version, err)
	}
}

// seedPlanreviewMarker inserts one "update" message on ticketID with body,
// the same shape job/planning.go's planreviewPendingMarker and
// planreviewDeliveredMarker write (design section 5.3).
func seedPlanreviewMarker(t *testing.T, s *store.Store, ticketID int64, body string) {
	t.Helper()
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeUpdate, Author: "system", Body: body,
	}); err != nil {
		t.Fatalf("InsertMessage(update %q): %v", body, err)
	}
}

// seedLoopsExhaustedEscalation inserts one cap_loops loops_exhausted
// escalation message (no run id) with body, mirroring
// job/planning.go's maybeResumeFloorFindings own escalationCommit call, then
// its linked open question child, mirroring store/commit.go's escalateTx. It
// returns the question message's id.
func seedLoopsExhaustedEscalation(t *testing.T, s *store.Store, ticketID int64, body string) int64 {
	t.Helper()

	escPayload, err := json.Marshal(response.EscalationPayload{
		Code: string(response.EscalationCodeLoopsExhausted), What: "raise machine.toml's planreview max_loops, or abandon",
		Why: "the plan review has delivered the maximum number of floor-finding cycles machine.toml allows", Tried: "",
		Options: []string{"retry", "planning", "abandon"}, Origin: string(response.EscalationOriginCapLoops),
	})
	if err != nil {
		t.Fatalf("marshal escalation payload: %v", err)
	}
	escID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: "escalation", Author: testAuthorZing, Body: body, Payload: escPayload,
	})
	if err != nil {
		t.Fatalf("InsertMessage(escalation): %v", err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: "Retry"}, {Key: "b", Text: "Return to planning"}, {Key: "c", Text: "Abandon"}},
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	openState := testQuestionStateOpen
	qID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, ParentID: &escID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State:   &openState,
		Body:    body + "\n\nHow should Zing proceed?",
		Payload: qPayload,
	})
	if err != nil {
		t.Fatalf("InsertMessage(question): %v", err)
	}
	return qID
}

// qBody returns the text found inside group's own q-body div, the one
// thread.templ writes around the question's rendered body
// (templates/thread.templ:220), so an assertion made against it never
// matches text a divider or a marker's own row renders elsewhere in the same
// question group (design review risk: "the marker row is inserted after the
// question, and its divider already prints the errors").
func qBody(t *testing.T, group string) string {
	t.Helper()
	const openTag = `<div class="q-body">`
	start := strings.Index(group, openTag)
	if start < 0 {
		t.Fatalf("qBody: no q-body div found in:\n%s", group)
	}
	start += len(openTag)
	end := strings.Index(group[start:], "</div>")
	if end < 0 {
		t.Fatalf("qBody: q-body div never closes in:\n%s", group)
	}
	return group[start : start+end]
}

// loopsExhaustedTitle is the fixed heading the loops_exhausted escalation's
// own body carries (job/planning.go's loopsExhaustedWhat), named once since
// seedLoopsExhaustedEscalation and TestEscalationQuestion_LoopsExhaustedListsFindings
// both need it.
const loopsExhaustedTitle = "loops_exhausted: raise machine.toml's planreview max_loops, or abandon"

// findingFixPlaceholder is the Fix text every finding this file seeds
// carries: no test here asserts on it, only on Text, Severity, and Location,
// so one fixed placeholder suffices for all of them.
const findingFixPlaceholder = "fix it"

// TestEscalationQuestion_LoopsExhaustedListsFindings proves #70's first
// change: the loops_exhausted question's own q-body lists the remaining
// findings from the planreview artifact at the version named by the newest
// "planreview vN pending" marker stored before the escalation -- v3 here,
// not the older v2 cohort's own artifact and marker, which exist only to
// prove the right version is read.
func TestEscalationQuestion_LoopsExhaustedListsFindings(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := seedRun(t, s, ticketID)

	seedPlanReviewArtifactAt(t, s, ticketID, runID, 2, []response.Finding{
		{Lens: response.LensQuality, Severity: response.SeverityMinor, Location: findingLocation, Text: "v2 minor finding text", Fix: findingFixPlaceholder},
	})
	seedPlanreviewMarker(t, s, ticketID, "planreview v2 pending")
	seedPlanreviewMarker(t, s, ticketID, "planreview v2 delivered")

	seedPlanReviewArtifactAt(t, s, ticketID, runID, 3, []response.Finding{
		{Lens: response.LensCorrectness, Severity: response.SeverityMajor, Location: findingLocation, Text: "v3 major finding text", Fix: findingFixPlaceholder},
		{Lens: response.LensQuality, Severity: response.SeverityMinor, Location: findingLocation, Text: "v3 minor finding text", Fix: findingFixPlaceholder},
	})
	seedPlanreviewMarker(t, s, ticketID, "planreview v3 pending")

	seedLoopsExhaustedEscalation(t, s, ticketID, loopsExhaustedTitle)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	// findGroup matches against the rendered, HTML-escaped title, so the
	// search text stops short of the apostrophe in "machine.toml's" (which
	// renders as "machine.toml&#39;s"); the group found is still the one and
	// only question this test seeds.
	group := findGroup(t, splitQuestionGroups(t, main), "loops_exhausted: raise machine.toml")
	body := qBody(t, group)

	for _, want := range []string{"plan review of v3", "major", findingLocation, "v3 major finding text", "v3 minor finding text"} {
		if !strings.Contains(body, want) {
			t.Errorf("q-body missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "v2 minor finding text") {
		t.Errorf("q-body shows the older v2 cohort's finding; got:\n%s", body)
	}
}
