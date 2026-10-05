// gate_test.go is Task 11's test-first proof for the gate's two new context
// regions (design section 7, D8, 13 task 11): the current scenario cohort's
// table and the above-floor plan-review findings' table, both rendered
// before the stored plan inside gateContext (thread.templ). Every test here
// drives the real store and the live GET /stream, the same boundary
// question_kinds_test.go and plan_test.go already exercise.
package console_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// gateQuestionTitle is the fixed heading every gate_test.go fixture's own
// seeded gate question carries, distinct from SeedQuestionFixtures' own
// "Approve the plan?" fixture (seed.go) used elsewhere in this package, so
// findGroup never matches the wrong ticket's group.
const gateQuestionTitle = "Approve the gate fixture plan?"

// findingLocation and the four severities' Text fixtures are named once,
// per goconst's own repeated-literal guard, since fourSeverityFindings and
// both floor tests below each read them.
const (
	findingLocation    = "plan/design/shape"
	blockerFindingText = "blocker finding text"
	majorFindingText   = "major finding text"
	minorFindingText   = "minor finding text"
	nitFindingText     = "nit finding text"
)

// seedGateQuestion inserts one open gate-kind "question" message on
// ticketID, through the store's own validated insert (design section 6.6):
// the same shape a real gate producer's commit writes (job/planning.go's
// gateQuestionMessage), minus the run attachment this file's fixtures have
// no reason to assert on.
func seedGateQuestion(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindGate, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
	})
	if err != nil {
		t.Fatalf("marshal gate question payload: %v", err)
	}
	openState := testQuestionStateOpen
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State:   &openState,
		Body:    gateQuestionTitle + "\n\nReview the plan, scenarios, and findings.",
		Payload: payload,
	}); err != nil {
		t.Fatalf("InsertMessage(gate question): %v", err)
	}
}

// seedPlanArtifact inserts one minimal, fully valid plan artifact (plan_test.go's
// own fixturePlan) at version, carrying runID -- or no run_id at all when
// runID is nil, the legacy shape a plan artifact stored before every
// artifact carried one.
func seedPlanArtifact(t *testing.T, s *store.Store, ticketID int64, runID *int64, version int) {
	t.Helper()
	payload, err := json.Marshal(fixturePlan())
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, Type: testArtifactTypePlan, Version: version, RunID: runID, Payload: payload,
	}); err != nil {
		t.Fatalf("InsertArtifact(plan): %v", err)
	}
}

// seedScenarioArtifact inserts one scenario artifact at version, carrying
// runID -- or no run_id at all when runID is nil.
func seedScenarioArtifact(t *testing.T, s *store.Store, ticketID int64, runID *int64, version int, sc response.Scenario) {
	t.Helper()
	payload, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal scenario %s: %v", sc.ID, err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, Type: testArtifactTypeScenario, Version: version, RunID: runID, Payload: payload,
	}); err != nil {
		t.Fatalf("InsertArtifact(scenario %s): %v", sc.ID, err)
	}
}

// seedPlanReviewArtifact inserts one "planreview" artifact at version 1 (every
// caller in this file seeds a single-cohort fixture), carrying runID and
// findings, wrapped exactly as job/planning.go's own planReviewOkCommit
// stores them (internal/store/schemas/artifacts/planreview.json:
// {"findings": [...]}).
func seedPlanReviewArtifact(t *testing.T, s *store.Store, ticketID, runID int64, findings []response.Finding) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Findings []response.Finding `json:"findings"`
	}{Findings: findings})
	if err != nil {
		t.Fatalf("marshal planreview findings: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, Type: "planreview", Version: 1, RunID: &runID, Payload: payload,
	}); err != nil {
		t.Fatalf("InsertArtifact(planreview): %v", err)
	}
}

func TestGateScenariosTable_CurrentCohortOnly(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	oldRun := seedRun(t, s, ticketID)
	seedPlanArtifact(t, s, ticketID, &oldRun, 1)
	seedScenarioArtifact(t, s, ticketID, &oldRun, 1, response.Scenario{
		ID: "s9", Kind: response.ScenarioKindBehavior,
		Given: "the older cohort's given", When: "the older cohort's when", Then: "the older cohort's then",
	})

	newRun := seedRun(t, s, ticketID)
	seedPlanArtifact(t, s, ticketID, &newRun, 2)
	wantScenarios := []response.Scenario{
		{ID: "s1", Kind: response.ScenarioKindBehavior, Given: "cohort given one", When: "cohort when one", Then: "cohort then one"},
		{ID: "s2", Kind: response.ScenarioKindNegative, Given: "cohort given two", When: "cohort when two", Then: "cohort then two"},
		{ID: "s3", Kind: response.ScenarioKindBehavior, Given: "cohort given three", When: "cohort when three", Then: "cohort then three"},
	}
	for i, sc := range wantScenarios {
		seedScenarioArtifact(t, s, ticketID, &newRun, i+1, sc)
	}
	seedGateQuestion(t, s, ticketID)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

	if !strings.Contains(gate, `class="scenarios"`) {
		t.Fatalf("gate group missing its scenarios table; got:\n%s", gate)
	}

	lastIdx := -1
	for _, sc := range wantScenarios {
		idx := strings.Index(gate, sc.Given)
		if idx < 0 {
			t.Errorf("scenarios table missing %q", sc.Given)
			continue
		}
		if idx < lastIdx {
			t.Errorf("scenario %q rendered out of insertion order", sc.Given)
		}
		lastIdx = idx
	}
	if strings.Contains(gate, "the older cohort's given") {
		t.Errorf("gate group shows the older cohort's scenario; got:\n%s", gate)
	}
}

// TestGateOwnerEditControls_SealedCohortOnly proves the wiring views.go's
// buildScenarioRows and loadPlan do for #41's owner-edit controls, driven
// against a real store and the live GET /stream rather than a hand-built
// view model: a cohort with at least one sealed scenario renders exactly
// one data-target="scenario" box (for the sealed scenario, not the
// unsealed one) and an owner-edit-drop control on every plan task, while a
// cohort with no sealed scenario at all renders neither.
func TestGateOwnerEditControls_SealedCohortOnly(t *testing.T) {
	t.Parallel()

	t.Run("sealed cohort shows the controls", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

		runID := seedRun(t, s, ticketID)
		seedPlanArtifact(t, s, ticketID, &runID, 1)
		seedSealedScenarioArtifact(t, s, ticketID, &runID, response.Scenario{
			ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g1", When: "w1", Then: "t1",
		})
		seedScenarioArtifact(t, s, ticketID, &runID, 2, response.Scenario{
			ID: "s2", Kind: response.ScenarioKindBehavior, Given: "g2", When: "w2", Then: "t2",
		}) // unsealed
		seedGateQuestion(t, s, ticketID)

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)
		assertExactSSEFraming(t, main)

		gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

		if n := strings.Count(gate, `data-target="scenario"`); n != 1 {
			t.Errorf(`want exactly one data-target="scenario" box; got %d in:\n%s`, n, gate)
		}
		if n := strings.Count(gate, "owner-edit-drop"); n != 1 {
			t.Errorf("want one owner-edit-drop control (fixturePlan's one task); got %d in:\n%s", n, gate)
		}
	})

	t.Run("no sealed scenario shows neither", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		ticketID := seedTicket(t, s, "fake#2", "Add another endpoint")

		runID := seedRun(t, s, ticketID)
		seedPlanArtifact(t, s, ticketID, &runID, 1)
		seedScenarioArtifact(t, s, ticketID, &runID, 1, response.Scenario{
			ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g1", When: "w1", Then: "t1",
		}) // unsealed
		seedGateQuestion(t, s, ticketID)

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)
		assertExactSSEFraming(t, main)

		gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

		if strings.Contains(gate, `data-target="scenario"`) {
			t.Errorf(`want no data-target="scenario" box; got:\n%s`, gate)
		}
		if strings.Contains(gate, "owner-edit-drop") {
			t.Errorf("want no owner-edit-drop control; got:\n%s", gate)
		}
	})
}

func TestGateFindingsTable_FloorMinorShowsBlockerAndMajorOnly(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := seedRun(t, s, ticketID)
	seedPlanArtifact(t, s, ticketID, &runID, 1)
	seedPlanReviewArtifact(t, s, ticketID, runID, fourSeverityFindings())
	seedGateQuestion(t, s, ticketID)

	srv := newTestServerFloor(t, s, bus.New(), nil, newTestLogHandler(t), response.SeverityMinor)
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

	for _, want := range []string{blockerFindingText, majorFindingText} {
		if !strings.Contains(gate, want) {
			t.Errorf("floor minor: missing %q; got:\n%s", want, gate)
		}
	}
	for _, notWant := range []string{minorFindingText, nitFindingText} {
		if strings.Contains(gate, notWant) {
			t.Errorf("floor minor: unwanted %q shown; got:\n%s", notWant, gate)
		}
	}
}

func TestGateFindingsTable_FloorNitShowsBlockerMajorAndMinor(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := seedRun(t, s, ticketID)
	seedPlanArtifact(t, s, ticketID, &runID, 1)
	seedPlanReviewArtifact(t, s, ticketID, runID, fourSeverityFindings())
	seedGateQuestion(t, s, ticketID)

	srv := newTestServerFloor(t, s, bus.New(), nil, newTestLogHandler(t), response.SeverityNit)
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

	for _, want := range []string{blockerFindingText, majorFindingText, minorFindingText} {
		if !strings.Contains(gate, want) {
			t.Errorf("floor nit: missing %q; got:\n%s", want, gate)
		}
	}
	if strings.Contains(gate, nitFindingText) {
		t.Errorf("floor nit: unwanted nit finding shown; got:\n%s", gate)
	}
}

// TestGateFindingsTable_LoopExhaustedShowsFloorFindings proves issue #48's
// loosened filter (views.go's loadFindings): once job/planning.go's own
// gateCapMarker is stored for the current cohort's plan version, the
// floor-conditional skip stops firing, since those at-or-below-floor
// findings are exactly what the owner must now decide on at the gate --
// unlike TestGateFindingsTable_FloorMinorShowsBlockerAndMajorOnly, which
// must keep excluding them with no marker present, proving the loosened
// filter is marker-conditional, not a blanket change. No machine is passed
// (nil, like the unmodified floor tests): the marker alone drives this, not
// config.
func TestGateFindingsTable_LoopExhaustedShowsFloorFindings(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := seedRun(t, s, ticketID)
	seedPlanArtifact(t, s, ticketID, &runID, 1)
	seedPlanReviewArtifact(t, s, ticketID, runID, fourSeverityFindings())
	seedGateQuestion(t, s, ticketID)
	seedUnreadUpdate(t, s, ticketID, "gate cap reached plan v1")

	srv := newTestServerFloor(t, s, bus.New(), nil, newTestLogHandler(t), response.SeverityMinor)
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

	for _, want := range []string{blockerFindingText, majorFindingText, minorFindingText, nitFindingText} {
		if !strings.Contains(gate, want) {
			t.Errorf("loop exhausted: missing %q; got:\n%s", want, gate)
		}
	}
}

// TestGateFindingsTable_CapMarkerDrivesCappedRegardlessOfCurrentConfig
// proves issue #48 review P2 directly: loadFindings decides "cap gate" from
// job/planning.go's own gateCapMarker, not by recomputing
// CountDeliveredReviews against current config. A real machine (testMachine)
// whose planreview.max_loops (2) this ticket's zero "delivered" review
// markers come nowhere near -- CountDeliveredReviews would read 0, well
// under the cap -- still shows every floor finding once the fixed marker is
// present, proving the marker, not a live config recomputation, is what the
// capped branch depends on; raising max_loops after a gate posts can never
// retroactively hide what it already showed.
func TestGateFindingsTable_CapMarkerDrivesCappedRegardlessOfCurrentConfig(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := seedRun(t, s, ticketID)
	seedPlanArtifact(t, s, ticketID, &runID, 1)
	seedPlanReviewArtifact(t, s, ticketID, runID, fourSeverityFindings())
	seedGateQuestion(t, s, ticketID)
	seedUnreadUpdate(t, s, ticketID, "gate cap reached plan v1")

	srv := newTestServerFloor(t, s, bus.New(), testMachine(t), newTestLogHandler(t), response.SeverityMinor)
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

	for _, want := range []string{blockerFindingText, majorFindingText, minorFindingText, nitFindingText} {
		if !strings.Contains(gate, want) {
			t.Errorf("cap marker present, delivered-review count not capped: missing %q; got:\n%s", want, gate)
		}
	}
}

// fourSeverityFindings returns one finding of each of the four closed
// severities, each with a distinct, greppable Text, for the floor tests
// above.
func fourSeverityFindings() []response.Finding {
	return []response.Finding{
		{Lens: response.LensCorrectness, Severity: response.SeverityBlocker, Location: findingLocation, Text: blockerFindingText, Fix: "fix the blocker"},
		{Lens: response.LensCorrectness, Severity: response.SeverityMajor, Location: findingLocation, Text: majorFindingText, Fix: "fix the major"},
		{Lens: response.LensQuality, Severity: response.SeverityMinor, Location: findingLocation, Text: minorFindingText, Fix: "fix the minor"},
		{Lens: response.LensQuality, Severity: response.SeverityNit, Location: findingLocation, Text: nitFindingText, Fix: "fix the nit"},
	}
}

// Not parallel: it calls slog.SetDefault below to capture a log line, which
// swaps the process-wide default logger.
func TestGateScenariosTable_LegacyNullCohortRendersAllAndLogsDebug(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	seedPlanArtifact(t, s, ticketID, nil, 1)
	legacyScenarios := []response.Scenario{
		{ID: "s1", Kind: response.ScenarioKindBehavior, Given: "legacy given one", When: "legacy when one", Then: "legacy then one"},
		{ID: "s2", Kind: response.ScenarioKindNegative, Given: "legacy given two", When: "legacy when two", Then: "legacy then two"},
	}
	for i, sc := range legacyScenarios {
		seedScenarioArtifact(t, s, ticketID, nil, i+1, sc)
	}
	seedGateQuestion(t, s, ticketID)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

	for _, sc := range legacyScenarios {
		if !strings.Contains(gate, sc.Given) {
			t.Errorf("missing legacy scenario %q; got:\n%s", sc.Given, gate)
		}
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, "legacy uncohorted scenarios rendered") {
		t.Errorf("missing the legacy debug log line; got:\n%s", logOut)
	}
	if !strings.Contains(logOut, fmt.Sprintf("ticket_id=%d", ticketID)) {
		t.Errorf("legacy debug log line missing ticket_id=%d; got:\n%s", ticketID, logOut)
	}
}

func TestGateContext_NoCohortRendersNeitherTable(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	seedGateQuestion(t, s, ticketID)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)

	if strings.Contains(gate, `class="scenarios"`) {
		t.Errorf("gate group has a scenarios table with no cohort; got:\n%s", gate)
	}
	if strings.Contains(gate, `class="findings"`) {
		t.Errorf("gate group has a findings table with no cohort; got:\n%s", gate)
	}
	if !strings.Contains(gate, "No plan stored for this ticket yet.") {
		t.Errorf("gate group missing its plan placeholder; got:\n%s", gate)
	}
}
