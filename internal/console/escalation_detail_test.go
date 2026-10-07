// escalation_detail_test.go is the test-first proof for #70's first two
// changes: a plan review loops_exhausted question lists the remaining
// findings from the planreview artifact at the version its own pending
// marker names, and a response_invalid question shows the run's job, lens
// and validator errors read from that run's own invalid marker (design
// section's withEscalationDetails, findingsDetail, responseInvalidDetail).
// It also proves the fallback: a question whose escalation points at a
// missing or unparseable source keeps its stored body, unchanged. Every
// test here drives the real store and the live GET /stream, the same
// boundary gate_test.go and question_kinds_test.go already exercise.
package console_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// seedSystemUpdate inserts one "update" message on ticketID with body,
// authored by "system", the same shape any of job/planning.go's or
// job/reviewing.go's own bookkeeping markers (a "planreview vN pending"
// marker, a "response invalid run <id>" marker, and so on) take when
// written.
func seedSystemUpdate(t *testing.T, s *store.Store, ticketID int64, body string) {
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
		Options: []string{"retry", testPlanningLiteral, "abandon"}, Origin: string(response.EscalationOriginCapLoops),
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

	seedPlanReviewArtifact(t, s, ticketID, runID, 2, []response.Finding{
		{Lens: response.LensQuality, Severity: response.SeverityMinor, Location: findingLocation, Text: "v2 minor finding text", Fix: findingFixPlaceholder},
	})
	seedSystemUpdate(t, s, ticketID, "planreview v2 pending")
	seedSystemUpdate(t, s, ticketID, "planreview v2 delivered")

	seedPlanReviewArtifact(t, s, ticketID, runID, 3, []response.Finding{
		{ID: "p3-f1", Lens: response.LensCorrectness, Severity: response.SeverityMajor, Location: findingLocation, Text: "v3 major finding text", Fix: findingFixPlaceholder},
		{ID: "p3-f2", Lens: response.LensQuality, Severity: response.SeverityMinor, Location: findingLocation, Text: "v3 minor finding text", Fix: findingFixPlaceholder},
	})
	seedSystemUpdate(t, s, ticketID, "planreview v3 pending")

	seedLoopsExhaustedEscalation(t, s, ticketID, loopsExhaustedTitle)

	// A v4 cohort seeded only after the escalation: latestPlanreviewPendingVersion
	// only looks at marker rows below the escalation's own id, so a later
	// retry's own v4 pending marker must never leak into this escalation's
	// findings (the #87 failure the design guards against). Without that
	// bound, v4 would be read and the unwanted v4 assertions below would fail.
	seedPlanReviewArtifact(t, s, ticketID, runID, 4, []response.Finding{
		{Lens: response.LensQuality, Severity: response.SeverityMinor, Location: findingLocation, Text: "v4 later finding text", Fix: findingFixPlaceholder},
	})
	seedSystemUpdate(t, s, ticketID, "planreview v4 pending")

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

	for _, want := range []string{
		"plan review of v3",
		"p3-f1 major at <code>" + findingLocation + "</code>: v3 major finding text",
		"p3-f2 minor at <code>" + findingLocation + "</code>: v3 minor finding text",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("q-body missing %q; got:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"v2 minor finding text", "v4 later finding text", "plan review of v4"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("q-body shows %q from the wrong cohort; got:\n%s", unwanted, body)
		}
	}
}

// seedReviewRun claims ticketID just long enough to reserve and terminalize
// one "review" session and run carrying lens, mirroring seedRun
// (console_test.go), so TestEscalationQuestion_ResponseInvalidShowsValidatorErrors
// can give its response_invalid escalation a real run whose own session
// names the job and whose own run names the lens (design's
// responseInvalidDetail reads both).
func seedReviewRun(t *testing.T, s *store.Store, ticketID int64, lens string) int64 {
	t.Helper()

	const owner = "test-review-run-owner"
	expires := time.Now().Add(10 * time.Minute)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	reserved, err := s.Reserve(t.Context(), ticketID, owner, expires,
		store.SessionUpsert{Job: "review", Runtime: testRuntimeFake}, store.RunSeed{Model: "test-model", Lens: &lens})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	outcome, exitCode, agentSeconds := "ready", 0, 1
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []store.Run{{ID: reserved.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
	return reserved.RunID
}

// seedResponseInvalidEscalation inserts one review-origin response_invalid
// escalation message whose own row RunID is runID (nil for none, the same
// shape a cap escalation's own nil RunID takes), mirroring
// job/reviewing.go's tableCommit own escalationCommit call for a lens's
// second consecutive invalid turn, then its linked open question child. It
// returns the question message's id.
func seedResponseInvalidEscalation(t *testing.T, s *store.Store, ticketID int64, runID *int64, body string) int64 {
	t.Helper()

	escPayload, err := json.Marshal(response.EscalationPayload{
		Code: string(response.EscalationCodeResponseInvalid), What: "lens correctness returned an invalid document twice in a row",
		Why:     "zing document failed validation; validator errors in response invalid run 1",
		Options: []string{"retry", testPlanningLiteral, "abandon"}, Origin: string(response.EscalationOriginReview),
	})
	if err != nil {
		t.Fatalf("marshal escalation payload: %v", err)
	}
	escID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, RunID: runID, Type: "escalation", Author: testAuthorZing, Body: body, Payload: escPayload,
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

// responseInvalidTitle is the fixed heading a review-origin response_invalid
// escalation's own body carries (job/reviewing.go's lensInvalidTwiceWhat),
// named once since seedResponseInvalidEscalation's callers and
// TestEscalationQuestion_ResponseInvalidShowsValidatorErrors both need it.
const responseInvalidTitle = "response_invalid: lens correctness returned an invalid document twice in a row"

// TestEscalationQuestion_ResponseInvalidShowsValidatorErrors proves #70's
// second change: a response_invalid escalation's own question shows the
// run's job and lens and the validator's errors read from that run's own
// "response invalid run <id>" marker (design H2) -- never the stored body
// alone, which names only the reason and the run id. The marker's third
// third error line is exactly a run of three backticks, on its own line,
// with a raw "<b>bold</b> tail after fence" line right after it: a fixed
// three-backtick fence would read that bare "```" line as its own closing
// delimiter (CommonMark lets a closing fence line hold only backticks) and
// let "tail after fence" leak out of the code block, so this proves the
// rendered fence is actually longer than the longest backtick run the
// errors contain, and that Render's own no-raw-HTML rule (render.go) still
// applies inside it.
func TestEscalationQuestion_ResponseInvalidShowsValidatorErrors(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#2", "Add a hello endpoint")
	runID := seedReviewRun(t, s, ticketID, "correctness")

	seedResponseInvalidEscalation(t, s, ticketID, &runID, responseInvalidTitle)
	seedSystemUpdate(t, s, ticketID, fmt.Sprintf(
		"response invalid run %d\nthe final message failed validation\nplan/overview/objective: required\nplan/design/shape: required\n```\n<b>bold</b> tail after fence",
		runID))

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	group := findGroup(t, splitQuestionGroups(t, main), "response_invalid: lens correctness")
	got := qBody(t, group)

	for _, want := range []string{
		"Job: review, lens correctness",
		fmt.Sprintf("Run %d", runID),
		"plan/overview/objective: required",
		"plan/design/shape: required",
		"&lt;b&gt;bold",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("q-body missing %q; got:\n%s", want, got)
		}
	}

	const openCodeTag = "<code"
	start := strings.Index(got, openCodeTag)
	if start < 0 {
		t.Fatalf("q-body has no code block; got:\n%s", got)
	}
	end := strings.Index(got[start:], "</code>")
	if end < 0 {
		t.Fatalf("q-body's code block never closes; got:\n%s", got)
	}
	if !strings.Contains(got[start:start+end], "tail after fence") {
		t.Errorf("q-body's fence closed before reaching \"tail after fence\"; got:\n%s", got)
	}
}

// TestEscalationQuestion_NoDetailWithoutSource proves withEscalationDetails
// leaves a question's stored body unchanged, with no error, for every
// escalation whose own detail source is missing or unparseable (design
// section's "one bad stored row never fails the thread render" goal): a
// cap_loops loops_exhausted escalation with no pending marker at all, one
// whose newest pending marker's version does not parse, and a
// response_invalid escalation with no row RunID.
func TestEscalationQuestion_NoDetailWithoutSource(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#3", "Add a hello endpoint")

	seedLoopsExhaustedEscalation(t, s, ticketID, loopsExhaustedTitle)

	runID := seedRun(t, s, ticketID)
	seedPlanReviewArtifact(t, s, ticketID, runID, 1, []response.Finding{
		{Lens: response.LensQuality, Severity: response.SeverityMinor, Location: findingLocation, Text: "ignored finding text", Fix: findingFixPlaceholder},
	})
	seedSystemUpdate(t, s, ticketID, "planreview vX pending")
	seedLoopsExhaustedEscalation(t, s, ticketID, loopsExhaustedTitle)

	seedResponseInvalidEscalation(t, s, ticketID, nil, responseInvalidTitle)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	groups := splitQuestionGroups(t, main)
	if len(groups) != 3 {
		t.Fatalf("splitQuestionGroups: got %d groups, want 3", len(groups))
	}
	for i, g := range groups {
		got := qBody(t, g)
		if !strings.Contains(got, "How should Zing proceed?") {
			t.Errorf("group %d q-body = %q, want it to contain %q", i, got, "How should Zing proceed?")
		}
		for _, unwanted := range []string{"Remaining findings", "Job:", "Validator errors"} {
			if strings.Contains(got, unwanted) {
				t.Errorf("group %d q-body = %q, want no detail (no %q)", i, got, unwanted)
			}
		}
	}
}
