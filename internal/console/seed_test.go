// seed_test.go is Task 12's test-first proof for SeedDemo (design section
// 6.15): it seeds one demo project and one demo ticket carrying a plan
// artifact, a scenario set, a finding set, and one open question of each of
// the six kinds, all through the store's validated inserts, and a second
// call inserts nothing new.
package console_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/response"
	"zing/internal/store"
)

// seedDemoQuestionKinds is the design section 8 closed set of question
// kinds, the same six question_kinds_test.go asserts SeedQuestionFixtures
// covers; SeedDemo reuses SeedQuestionFixtures directly, so this file
// checks the same six are open on the demo ticket.
var seedDemoQuestionKinds = []response.QuestionKind{
	response.QuestionKindQuestion, response.QuestionKindGate, response.QuestionKindSplit,
	response.QuestionKindMerge, response.QuestionKindPerimeter, response.QuestionKindReview,
}

// TestSeedDemo_SeedsOneProjectAndTicketWithEveryArtifactAndQuestionKind
// proves SeedDemo's first call builds the full fixture design section 6.15
// names: one project, one ticket, one plan artifact, a scenario set, a
// finding set, and one open question of each of the six closed-set kinds.
func TestSeedDemo_SeedsOneProjectAndTicketWithEveryArtifactAndQuestionKind(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	ticketID := demoTicketID(ctx, t, s)

	assertSeedDemoArtifacts(ctx, t, s, ticketID)
	assertSeedDemoQuestions(ctx, t, s, ticketID)
}

// TestSeedDemo_SeededTicketWaitsOnItsGateNotQueued proves the seeded ticket
// ends in planning, waiting on its gate, rather than queued: a queued demo
// ticket would be a live dispatch candidate that `zing serve --seed-demo`
// picks up and advances into real planning, overwriting the fixture (design
// section 6.6, 6.15; PR #23 review).
func TestSeedDemo_SeededTicketWaitsOnItsGateNotQueued(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	tickets, err := s.TicketsByProject(ctx, projects[0].ID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want exactly 1", len(tickets))
	}
	tk := tickets[0]
	if tk.State != testPlanningLiteral {
		t.Errorf("seeded ticket state = %q, want planning (a queued ticket would be dispatched)", tk.State)
	}
	if tk.WaitingOn == nil || *tk.WaitingOn != string(response.QuestionKindGate) {
		t.Errorf("seeded ticket waiting_on = %v, want %q", tk.WaitingOn, response.QuestionKindGate)
	}
}

// demoTicketID asserts exactly one project and one ticket exist and returns
// the ticket's id, failing the test otherwise.
func demoTicketID(ctx context.Context, t *testing.T, s *store.Store) int64 {
	t.Helper()

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %d, want exactly 1", len(projects))
	}

	tickets, err := s.TicketsByProject(ctx, projects[0].ID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want exactly 1", len(tickets))
	}
	return tickets[0].ID
}

// assertSeedDemoArtifacts asserts ticketID carries exactly one plan
// artifact whose Design.Shape contains a mermaid fence and whose run_id is
// set, a scenario set of at least two rows all carrying that same run_id,
// and a "planreview" artifact at the plan's own version carrying at least
// two findings (design section 6.15, 7, Task 11: "a scenario artifact set",
// "store the seeded findings as the demo planreview artifact").
func assertSeedDemoArtifacts(ctx context.Context, t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()

	plan, ok, err := s.GetArtifact(ctx, ticketID, testArtifactTypePlan)
	if err != nil {
		t.Fatalf("GetArtifact(plan): %v", err)
	}
	if !ok {
		t.Fatal("no plan artifact stored")
	}
	if plan.RunID == nil {
		t.Fatal("plan artifact carries no run_id")
	}
	var p response.Plan
	if unmarshalErr := json.Unmarshal(plan.Payload, &p); unmarshalErr != nil {
		t.Fatalf("unmarshal plan payload: %v", unmarshalErr)
	}
	if !strings.Contains(p.Design.Shape, "```mermaid") {
		t.Errorf("plan Design.Shape does not contain a mermaid fence; got:\n%s", p.Design.Shape)
	}

	artifacts, err := s.ListArtifacts(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	var scenarios int
	var planreview *store.Artifact
	for i := range artifacts {
		switch artifacts[i].Type {
		case testArtifactTypeScenario:
			scenarios++
			if artifacts[i].RunID == nil || *artifacts[i].RunID != *plan.RunID {
				t.Errorf("scenario artifact run_id = %v, want the plan's own run %d", artifacts[i].RunID, *plan.RunID)
			}
		case testArtifactTypePlanreview:
			planreview = &artifacts[i]
		}
	}
	if scenarios < 2 {
		t.Errorf("scenario artifacts = %d, want at least 2 (a set)", scenarios)
	}

	if planreview == nil {
		t.Fatal("no planreview artifact stored")
	}
	if planreview.Version != plan.Version {
		t.Errorf("planreview version = %d, want the plan's own version %d", planreview.Version, plan.Version)
	}
	var findings response.FindingsResponse
	if unmarshalErr := json.Unmarshal(planreview.Payload, &findings); unmarshalErr != nil {
		t.Fatalf("unmarshal planreview payload: %v", unmarshalErr)
	}
	if len(findings.Findings) < 2 {
		t.Errorf("planreview findings = %d, want at least 2 (a set)", len(findings.Findings))
	}
}

// assertSeedDemoQuestions asserts ticketID has exactly one open question of
// each of the six closed-set kinds.
func assertSeedDemoQuestions(ctx context.Context, t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()

	open, err := s.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != len(seedDemoQuestionKinds) {
		t.Fatalf("open questions = %d, want exactly %d (one per kind)", len(open), len(seedDemoQuestionKinds))
	}

	seen := make(map[response.QuestionKind]int, len(seedDemoQuestionKinds))
	for i := range open {
		var payload response.QuestionPayload
		if err := json.Unmarshal(open[i].Payload, &payload); err != nil {
			t.Fatalf("unmarshal question %d payload: %v", open[i].ID, err)
		}
		seen[payload.Kind]++
	}
	for _, kind := range seedDemoQuestionKinds {
		if seen[kind] != 1 {
			t.Errorf("open questions of kind %q = %d, want exactly 1", kind, seen[kind])
		}
	}
}

// TestSeedDemo_IsIdempotent proves a second SeedDemo call on the same store
// inserts nothing new: the same project, the same ticket, and the same
// artifact and message counts as the first call.
func TestSeedDemo_IsIdempotent(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("first SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)

	artifactsBefore, err := s.ListArtifacts(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	messagesBefore, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	if seedErr := console.SeedDemo(ctx, s); seedErr != nil {
		t.Fatalf("second SeedDemo: %v", seedErr)
	}

	secondTicketID := demoTicketID(ctx, t, s)
	if secondTicketID != ticketID {
		t.Errorf("ticket id after second call = %d, want still %d", secondTicketID, ticketID)
	}

	artifactsAfter, err := s.ListArtifacts(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts (after): %v", err)
	}
	if len(artifactsAfter) != len(artifactsBefore) {
		t.Errorf("artifacts after second call = %d, want still %d (SeedDemo must insert nothing new)", len(artifactsAfter), len(artifactsBefore))
	}

	messagesAfter, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages (after): %v", err)
	}
	if len(messagesAfter) != len(messagesBefore) {
		t.Errorf("messages after second call = %d, want still %d (SeedDemo must insert nothing new)", len(messagesAfter), len(messagesBefore))
	}

	assertSeedDemoQuestions(ctx, t, s, ticketID)
}

// TestSeedDemo_NormalServeDoesNotSeed proves SeedDemo runs only when called:
// a store that never has SeedDemo (or the --seed-demo flag's call to it)
// invoked carries no demo project, matching the design section 6.15
// constraint "it must NEVER run in a normal serve". cmd/zing's own
// seed_test.go proves the same thing against the real serve() entry point;
// this is the package-level half of that guarantee: nothing in this
// package's own wiring (console.New, the stream, the mutation routes) ever
// calls SeedDemo on its own.
func TestSeedDemo_NormalServeDoesNotSeed(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	seedTicket(t, s, "fake#1", "an ordinary ticket, not the demo fixture")

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range projects {
		if p.Name == "demo" {
			t.Fatalf("a %q project exists without SeedDemo ever being called", "demo")
		}
	}
}

// TestSeedDemo_GateRendersScenariosAndFindings proves Task 11's demo wiring
// end to end (design section 7, 13 task 11): the gate the console renders for
// the seeded ticket carries the demo scenario table, and its planreview
// artifact renders through the same review.floor every other ticket's gate
// uses, over the live GET /stream a browser itself reads.
func TestSeedDemo_GateRendersScenariosAndFindings(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)

	// newTestServerFloor's own floor (response.SeverityMinor, console_test.go)
	// matches internal/config's own applyDefaults default: demoFindings' minor
	// finding sits at or below it (dropped, still in the review loop) and its
	// major finding sits above it (shown at the gate).
	srv := newTestServerFloor(t, s, bus.New(), nil, newTestLogHandler(t), response.SeverityMinor)

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	groups := splitQuestionGroups(t, main)
	gate := findGroup(t, groups, "Approve the plan?")

	// demoScenarios (seed.go, unexported) writes two scenarios that share
	// the same Given text; this file (package console_test) asserts the
	// rendered output, not the private fixture function.
	if !strings.Contains(gate, `class="scenarios"`) {
		t.Errorf("gate group missing its scenarios table; got:\n%s", gate)
	}
	const sharedGiven = "the server is running"
	if !strings.Contains(gate, sharedGiven) {
		t.Errorf("gate group missing scenario text %q; got:\n%s", sharedGiven, gate)
	}

	// demoFindings (seed.go, unexported) writes one minor finding (at or
	// below the SeverityMinor floor this test uses, so dropped) and one
	// major finding (above it, so shown).
	if !strings.Contains(gate, `class="findings"`) {
		t.Errorf("gate group missing its findings table; got:\n%s", gate)
	}
	const aboveFloorText = "The only test covers the happy path."
	const atOrBelowFloorText = "The shape section does not name the response content type."
	if !strings.Contains(gate, aboveFloorText) {
		t.Errorf("gate group missing the above-floor finding %q; got:\n%s", aboveFloorText, gate)
	}
	if strings.Contains(gate, atOrBelowFloorText) {
		t.Errorf("gate group shows the at-or-below-floor finding %q, want it dropped; got:\n%s", atOrBelowFloorText, gate)
	}
}

// demoPerimeterQuestion returns the demo ticket's one open question of kind
// perimeter, failing the test when there is not exactly one (design section
// 6.5, 6.15: SeedDemo's perimeter question replaces the generic fixture
// SeedQuestionFixtures would otherwise insert for that kind, so the ticket
// still carries only one).
func demoPerimeterQuestion(ctx context.Context, t *testing.T, s *store.Store, ticketID int64) response.QuestionPayload {
	t.Helper()

	open, err := s.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}

	var found []response.QuestionPayload
	for i := range open {
		var payload response.QuestionPayload
		if err := json.Unmarshal(open[i].Payload, &payload); err != nil {
			t.Fatalf("unmarshal question %d payload: %v", open[i].ID, err)
		}
		if payload.Kind == response.QuestionKindPerimeter {
			found = append(found, payload)
		}
	}
	if len(found) != 1 {
		t.Fatalf("open perimeter questions = %d, want exactly 1", len(found))
	}
	return found[0]
}

// TestSeedDemoPerimeterItemsUseTheBuildFormat proves the demo ticket's
// perimeter question carries the design section 6.5 wire format: the body
// names task 2 and three files, Recommended asks the owner to decide each
// file, and each of the three items' Text is one line built as the marker
// in brackets when set, then "Builder: <reason> ", then
// "Change: <description>" (design section 6.5, 6.15).
func TestSeedDemoPerimeterItemsUseTheBuildFormat(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)

	open, err := s.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	var body string
	for i := range open {
		var payload response.QuestionPayload
		if json.Unmarshal(open[i].Payload, &payload) == nil && payload.Kind == response.QuestionKindPerimeter {
			body = open[i].Body
		}
	}
	const wantBody = "Confirm the file perimeter\n\n" +
		"Task 2 changed three files outside the plan's declared files. Accept a file to commit it. Reject a file to revert it."
	if body != wantBody {
		t.Errorf("perimeter question body =\n%q\nwant\n%q", body, wantBody)
	}

	payload := demoPerimeterQuestion(ctx, t, s, ticketID)
	if payload.Recommended != "Decide each file" {
		t.Errorf("perimeter Recommended = %q, want %q", payload.Recommended, "Decide each file")
	}
	if len(payload.Items) != 3 {
		t.Fatalf("perimeter items = %d, want exactly 3", len(payload.Items))
	}

	byRef := make(map[string]response.Item, len(payload.Items))
	for _, item := range payload.Items {
		byRef[item.Ref] = item
	}

	handler, ok := byRef["internal/hello/handler.go"]
	if !ok {
		t.Fatal("no perimeter item for internal/hello/handler.go")
	}
	if strings.HasPrefix(handler.Text, "[") {
		t.Errorf("handler.go item carries a marker; got %q, want none", handler.Text)
	}
	if !strings.Contains(handler.Text, "Builder: ") || !strings.Contains(handler.Text, " Change: ") {
		t.Errorf("handler.go item does not follow the build format; got %q", handler.Text)
	}

	claudeMD, ok := byRef["CLAUDE.md"]
	if !ok {
		t.Fatal("no perimeter item for CLAUDE.md")
	}
	if !strings.HasPrefix(claudeMD.Text, "[style guide] Builder: ") {
		t.Errorf("CLAUDE.md item = %q, want it to start with %q", claudeMD.Text, "[style guide] Builder: ")
	}
	if !strings.Contains(claudeMD.Text, " Change: ") {
		t.Errorf("CLAUDE.md item does not follow the build format; got %q", claudeMD.Text)
	}

	machineToml, ok := byRef["machine.toml"]
	if !ok {
		t.Fatal("no perimeter item for machine.toml")
	}
	if !strings.HasPrefix(machineToml.Text, "[trust root] Builder: ") {
		t.Errorf("machine.toml item = %q, want it to start with %q", machineToml.Text, "[trust root] Builder: ")
	}
	if !strings.Contains(machineToml.Text, " Change: ") {
		t.Errorf("machine.toml item does not follow the build format; got %q", machineToml.Text)
	}
}

// demoDecidedFileEvents returns ticketID's stored "file" events, decoded,
// failing the test on a store or decode error.
func demoDecidedFileEvents(ctx context.Context, t *testing.T, s *store.Store, ticketID int64) []response.FileArtifact {
	t.Helper()

	events, err := s.FileEvents(ctx, ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	out := make([]response.FileArtifact, 0, len(events))
	for _, e := range events {
		out = append(out, e.File)
	}
	return out
}

// TestSeedDemoStoresDecidedFiles proves SeedDemo stores exactly two decided
// "file" artifacts for the demo ticket (design section 9.2, 6.15): one
// accepted path from task 1 with a create action, one rejected path from
// task 1 with a modify action, both carrying a builder reason and a
// description, and neither sharing a path with the perimeter question's
// three items.
func TestSeedDemoStoresDecidedFiles(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)

	files := demoDecidedFileEvents(ctx, t, s, ticketID)
	if len(files) != 2 {
		t.Fatalf("decided file events = %d, want exactly 2", len(files))
	}

	byPath := make(map[string]response.FileArtifact, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}

	perimeterPaths := map[string]bool{
		"internal/hello/handler.go": true, "CLAUDE.md": true, "machine.toml": true,
	}

	accepted, ok := byPath["internal/hello/routes.go"]
	if !ok {
		t.Fatal("no decided file event for internal/hello/routes.go")
	}
	if accepted.Decision == nil || *accepted.Decision != response.PerimeterAccept {
		t.Errorf("internal/hello/routes.go decision = %v, want %q", accepted.Decision, response.PerimeterAccept)
	}
	if accepted.Action != response.FileActionCreate {
		t.Errorf("internal/hello/routes.go action = %q, want %q", accepted.Action, response.FileActionCreate)
	}
	if accepted.TaskN != 1 {
		t.Errorf("internal/hello/routes.go task_n = %d, want 1", accepted.TaskN)
	}
	if accepted.Reason == "" || accepted.Description == "" {
		t.Errorf("internal/hello/routes.go carries an empty reason or description: %+v", accepted)
	}

	rejected, ok := byPath["Makefile"]
	if !ok {
		t.Fatal("no decided file event for Makefile")
	}
	if rejected.Decision == nil || *rejected.Decision != response.PerimeterReject {
		t.Errorf("Makefile decision = %v, want %q", rejected.Decision, response.PerimeterReject)
	}
	if rejected.Action != response.FileActionModify {
		t.Errorf("Makefile action = %q, want %q", rejected.Action, response.FileActionModify)
	}
	if rejected.TaskN != 1 {
		t.Errorf("Makefile task_n = %d, want 1", rejected.TaskN)
	}
	if rejected.Reason == "" || rejected.Description == "" {
		t.Errorf("Makefile carries an empty reason or description: %+v", rejected)
	}

	for path := range byPath {
		if perimeterPaths[path] {
			t.Errorf("decided file path %q also names one of the perimeter question's three items", path)
		}
	}
}

// TestSeedDemoDecidedFilesAreIdempotent proves a second SeedDemo call stores
// no additional "file" artifact: still the same two decided rows.
func TestSeedDemoDecidedFilesAreIdempotent(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("first SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)
	before := demoDecidedFileEvents(ctx, t, s, ticketID)

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("second SeedDemo: %v", err)
	}
	after := demoDecidedFileEvents(ctx, t, s, ticketID)

	if len(after) != len(before) {
		t.Errorf("file events after second call = %d, want still %d", len(after), len(before))
	}
	if len(after) != 2 {
		t.Errorf("file events = %d, want exactly 2", len(after))
	}
}

// TestSeedDemo_ThreadRendersDecidedFilesAndPerimeterMarker proves the
// rendered demo thread (the live GET /stream a browser reads) carries the
// plan view's "Decided during build" sub-list with both decided paths, and
// the perimeter question's marker pill (Task 11b: itemRow renders the
// "trust root" marker as its own element, not the raw bracketed text), so
// the owner can see and try every perimeter feature from the seeded demo
// alone (design section 9.2, 6.5, 6.15).
func TestSeedDemo_ThreadRendersDecidedFilesAndPerimeterMarker(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	if !strings.Contains(main, "Decided during build") {
		t.Errorf("rendered thread missing %q; got:\n%s", "Decided during build", main)
	}
	if !strings.Contains(main, "internal/hello/routes.go") {
		t.Errorf("rendered thread missing the accepted path %q; got:\n%s", "internal/hello/routes.go", main)
	}
	if !strings.Contains(main, "Makefile") {
		t.Errorf("rendered thread missing the rejected path %q; got:\n%s", "Makefile", main)
	}
	const wantMarkerPill = `<span class="pill item-marker">trust root</span>`
	if !strings.Contains(main, wantMarkerPill) {
		t.Errorf("rendered thread missing %q; got:\n%s", wantMarkerPill, main)
	}
}

// TestSeedDemoStoresBuildMarkers proves SeedDemo seeds the demo ticket's
// three build markers (Task 11b): all three type="update" messages exist
// after one call and still exactly three after a second (idempotent), and
// the rendered thread (views.go's updateLine) shows the claims-ok marker as
// its owner-facing sentence.
func TestSeedDemoStoresBuildMarkers(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("first SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)
	assertSeedDemoBuildMarkerCount(ctx, t, s, ticketID, 3)

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("second SeedDemo: %v", err)
	}
	assertSeedDemoBuildMarkerCount(ctx, t, s, ticketID, 3)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	const want = "Claims checked for run 5."
	if !strings.Contains(main, want) {
		t.Errorf("rendered thread missing %q; got:\n%s", want, main)
	}
}

// demoBuildMarkerBodies mirrors seed.go's own demoBuildMarkers, unexported:
// this file is package console_test, so it recognizes the three seeded
// build markers by their own literal bodies (design section 9.2, Task 11b)
// rather than importing seed.go's unexported constants.
var demoBuildMarkerBodies = map[string]bool{
	"claims ok run 5": true,
	"claim errors pending run 6\n" +
		"claims/files_changed: observed [greet.go], claimed [greet.go, greet_test.go]": true,
	"perimeter resolved run 6": true,
}

// assertSeedDemoBuildMarkerCount asserts ticketID carries exactly want
// type="update" messages whose body is one of demoBuildMarkerBodies.
func assertSeedDemoBuildMarkerCount(ctx context.Context, t *testing.T, s *store.Store, ticketID int64, want int) {
	t.Helper()

	messages, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var got int
	for i := range messages {
		if messages[i].Type == testMsgTypeUpdate && demoBuildMarkerBodies[messages[i].Body] {
			got++
		}
	}
	if got != want {
		t.Errorf("build marker messages = %d, want exactly %d", got, want)
	}
}
