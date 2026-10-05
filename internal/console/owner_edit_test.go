// owner_edit_test.go: POST /tickets/{id}/edit route tests (#41), driven
// against a real store and a real httptest server, the same boundary
// pickup_test.go already exercises for POST /projects/{id}/pickup.
package console_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// testOldCheck and testNewCheck are the before/after check_cmd literals
// every test in this file shares (goconst).
const (
	testOldCheck = "go test ./old"
	testNewCheck = "go test ./new"
)

// ownerEditPath builds POST /tickets/{id}/edit's path.
func ownerEditPath(ticketID int64) string {
	return "/tickets/" + strconv.FormatInt(ticketID, 10) + "/edit"
}

// seedSealedScenarioArtifact inserts one sealed scenario artifact on
// ticketID, owned by runID -- or carrying no run_id at all when runID is
// nil, the legacy-but-still-sealed shape this route's refusal and
// acceptance paths both need (store.OwnerEdit's editScenarioTx looks up by
// ticket_id and the payload's own id, not by run_id). runID non-nil is the
// shape editPlanTaskTx's seal check needs, counting sealed scenarios by
// run_id.
func seedSealedScenarioArtifact(t *testing.T, s *store.Store, ticketID int64, runID *int64, sc response.Scenario) {
	t.Helper()
	payload, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal scenario %s: %v", sc.ID, err)
	}
	sealedAt := time.Now().UTC()
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: runID, Type: testArtifactTypeScenario, Payload: payload, SealedAt: &sealedAt,
	}); err != nil {
		t.Fatalf("InsertArtifact(sealed scenario %s): %v", sc.ID, err)
	}
}

// readScenarioCheck reads back ticketID's scenario "s1" own check_cmd
// field, through the exported AllScenarios read.
func readScenarioCheck(t *testing.T, s *store.Store, ticketID int64) string {
	t.Helper()
	artifacts, err := s.AllScenarios(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("AllScenarios: %v", err)
	}
	for _, a := range artifacts {
		var sc response.Scenario
		if err := json.Unmarshal(a.Payload, &sc); err != nil {
			t.Fatalf("unmarshal scenario: %v", err)
		}
		if sc.ID == "s1" {
			return sc.Check
		}
	}
	t.Fatalf("no scenario s1 on ticket %d", ticketID)
	return ""
}

// TestOwnerEditRoute_EditsSealedScenarioCheck proves the done-when test:
// posting a check edit for a sealed scenario rewrites its payload and
// writes exactly one owner_edit event holding the old and new check.
func TestOwnerEditRoute_EditsSealedScenarioCheck(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "1", "fix the bug")
	seedSealedScenarioArtifact(t, s, ticketID, nil, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Check: testOldCheck, Given: "g", When: "w", Then: "t",
	})
	seedSealedScenarioArtifact(t, s, ticketID, nil, response.Scenario{
		ID: "s2", Kind: response.ScenarioKindBehavior, Check: "go test ./other", Given: "g", When: "w", Then: "t",
	})

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID), `{"target":"scenario","ref":"s1","action":"edit","check":"`+testNewCheck+`"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %q", resp.StatusCode, readBody(t, resp))
	}

	if got := readScenarioCheck(t, s, ticketID); got != testNewCheck {
		t.Errorf("s1 check_cmd = %q, want %q", got, testNewCheck)
	}

	events, err := s.Events(t.Context(), ticketID, store.EventKindOwnerEdit, store.EventFilter{})
	if err != nil {
		t.Fatalf("Events(owner_edit): %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if !strings.Contains(ev.Old, testOldCheck) {
		t.Errorf("event old = %q, want it to contain the old check", ev.Old)
	}
	if !strings.Contains(ev.New, testNewCheck) {
		t.Errorf("event new = %q, want it to contain the new check", ev.New)
	}
}

// TestOwnerEditRoute_RefusesSchemaBreakingEdit proves an edit that would
// blank a required scenario field is refused 422, with the payload
// unchanged and no owner_edit event written.
func TestOwnerEditRoute_RefusesSchemaBreakingEdit(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "1", "fix the bug")
	seedSealedScenarioArtifact(t, s, ticketID, nil, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Check: testOldCheck, Given: "g", When: "w", Then: "t",
	})

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID), `{"target":"scenario","ref":"s1","action":"edit","given":""}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %q", resp.StatusCode, readBody(t, resp))
	}

	if got := readScenarioCheck(t, s, ticketID); got != testOldCheck {
		t.Errorf("s1 check_cmd = %q, want unchanged %q", got, testOldCheck)
	}
	if n, err := s.CountEvents(t.Context(), ticketID, store.EventKindOwnerEdit, store.EventFilter{}); err != nil || n != 0 {
		t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
	}
}

// planWithTasks returns fixturePlan (plan_test.go) with its Delivery.Tasks
// and Delivery.Files replaced by tasks and files: every other field, already
// schema-valid, rides along unchanged.
func planWithTasks(tasks []response.Task, files []response.FileChange) response.Plan {
	plan := fixturePlan()
	plan.Delivery.Tasks = tasks
	plan.Delivery.Files = files
	return plan
}

// fileTaskByPath returns the Task field of the file named path, so the drop
// test can check its post-drop task list by path rather than slice index.
func fileTaskByPath(files []response.FileChange, path string) string {
	for _, f := range files {
		if f.Path == path {
			return f.Task
		}
	}
	return ""
}

// TestOwnerEditRoute_DropsPlanTask proves the done-when test: dropping a
// sealed plan's task 2 of 3 through the console route renumbers task 3 down
// to 2, rewrites every file's task list, leaves the version unchanged, and
// answers 204.
func TestOwnerEditRoute_DropsPlanTask(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "1", "fix the bug")
	runID := seedRun(t, s, ticketID)

	plan := planWithTasks(
		[]response.Task{
			{N: 1, Test: "T1", Demo: true, Text: "task one"},
			{N: 2, Test: "T2", Demo: false, Text: "task two"},
			{N: 3, Test: "T3", Demo: false, Text: "task three"},
		},
		[]response.FileChange{
			{Path: "internal/one.go", Action: response.FileActionModify, Task: "1", Reason: "r1"},
			{Path: "internal/two.go", Action: response.FileActionModify, Task: "2 3", Reason: "r2"},
			{Path: "internal/three.go", Action: response.FileActionModify, Task: "3", Reason: "r3"},
		},
	)
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, insErr := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: &runID, Type: testArtifactTypePlan, Version: 1, Payload: payload,
	}); insErr != nil {
		t.Fatalf("InsertArtifact(plan): %v", insErr)
	}
	seedSealedScenarioArtifact(t, s, ticketID, &runID, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g1", When: "w1", Then: "t1",
	})
	seedSealedScenarioArtifact(t, s, ticketID, &runID, response.Scenario{
		ID: "s2", Kind: response.ScenarioKindBehavior, Given: "g2", When: "w2", Then: "t2",
	})

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID), `{"target":"plan_task","ref":"2","action":"drop"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %q", resp.StatusCode, readBody(t, resp))
	}

	got, version, ok, err := s.StoredPlan(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
	}
	if version != 1 {
		t.Errorf("version = %d, want unchanged 1", version)
	}
	if len(got.Delivery.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got.Delivery.Tasks))
	}
	if got.Delivery.Tasks[0].N != 1 || got.Delivery.Tasks[0].Text != "task one" {
		t.Errorf("task 1 = %+v, want N 1 text %q", got.Delivery.Tasks[0], "task one")
	}
	if got.Delivery.Tasks[1].N != 2 || got.Delivery.Tasks[1].Text != "task three" {
		t.Errorf("task 2 = %+v, want N 2 text %q (the old task 3)", got.Delivery.Tasks[1], "task three")
	}

	wantFileTasks := map[string]string{"internal/one.go": "1", "internal/two.go": "2", "internal/three.go": "2"}
	for path, want := range wantFileTasks {
		if got := fileTaskByPath(got.Delivery.Files, path); got != want {
			t.Errorf("file %s task = %q, want %q", path, got, want)
		}
	}
}

// seedTicketWithBody inserts one queued ticket under testProject and ref,
// with body as its stored body, returning its id: the fixture
// TestOwnerEditRoute_AmendsTicketBody needs an old body to assert against.
func seedTicketWithBody(t *testing.T, s *store.Store, ref, body string) int64 {
	t.Helper()
	projectID, err := s.EnsureProject(t.Context(), testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: ref, Title: "fix the bug", Body: body, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket(%s): %v", ref, err)
	}
	return id
}

// TestOwnerEditRoute_AmendsTicketBody proves the done-when test: posting a
// ticket_body edit through the console route rewrites tickets.body and
// writes exactly one owner_edit event holding the old and new body.
func TestOwnerEditRoute_AmendsTicketBody(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicketWithBody(t, s, "1", "old body text")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID), `{"target":"ticket_body","action":"edit","body":"new body text"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %q", resp.StatusCode, readBody(t, resp))
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.Body != "new body text" {
		t.Errorf("body = %q, want %q", ticket.Body, "new body text")
	}

	events, err := s.Events(t.Context(), ticketID, store.EventKindOwnerEdit, store.EventFilter{})
	if err != nil {
		t.Fatalf("Events(owner_edit): %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if ev.Old != "old body text" {
		t.Errorf("event old = %q, want %q", ev.Old, "old body text")
	}
	if ev.New != "new body text" {
		t.Errorf("event new = %q, want %q", ev.New, "new body text")
	}
}

// TestOwnerEditRoute_RefusesClaimedTicket proves an edit is refused 409
// with the exact claimed text while a run holds the ticket's claim, and
// that the payload is unchanged.
func TestOwnerEditRoute_RefusesClaimedTicket(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "1", "fix the bug")
	seedSealedScenarioArtifact(t, s, ticketID, nil, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Check: testOldCheck, Given: "g", When: "w", Then: "t",
	})
	claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID), `{"target":"scenario","ref":"s1","action":"edit","check":"`+testNewCheck+`"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %q", resp.StatusCode, readBody(t, resp))
	}
	if got, want := readBody(t, resp), "ticket is claimed; edits are refused while a run holds it"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	if got := readScenarioCheck(t, s, ticketID); got != testOldCheck {
		t.Errorf("s1 check_cmd = %q, want unchanged %q", got, testOldCheck)
	}
}

// TestOwnerEditRoute_RefusesNonLoopbackCheckEdit proves a request that sets
// check (or test) is refused 403 from a non-loopback caller, even with a
// valid Host and same-origin Origin, the same local-only boundary
// sandboxrun.go's requireLoopback draws around POST
// /tickets/{id}/sandbox-run: both become shell commands CHECK and the build
// later run. Driven straight against the handler (no real listener) so
// RemoteAddr can be set directly (sandboxrun_test.go's own
// TestSandboxRunRefusesNonLoopback precedent).
func TestOwnerEditRoute_RefusesNonLoopbackCheckEdit(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "1", "fix the bug")
	seedSealedScenarioArtifact(t, s, ticketID, nil, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Check: testOldCheck, Given: "g", When: "w", Then: "t",
	})

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	authority := strings.TrimPrefix(srv.URL, "http://")
	body := `{"target":"scenario","ref":"s1","action":"edit","check":"` + testNewCheck + `"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+ownerEditPath(ticketID), strings.NewReader(body))
	req.Host = authority
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", srv.URL)

	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %q", rec.Code, rec.Body.String())
	}
	if got, want := strings.TrimSpace(rec.Body.String()), "editing a check or test command is allowed from this machine only"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	if got := readScenarioCheck(t, s, ticketID); got != testOldCheck {
		t.Errorf("s1 check_cmd = %q, want unchanged %q", got, testOldCheck)
	}
}
