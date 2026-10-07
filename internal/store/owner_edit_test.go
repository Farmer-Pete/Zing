package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
)

// planTaskText1 and planTaskText2 are the task-1 and task-2 Text literals
// several plan fixtures below share. fieldNameText is the field name
// TestOwnerEdit_RefusesFieldNotAllowedForTarget expects named in a
// refusal's reason, matching store_test.go's own "text" literal (goconst).
const (
	planTaskText1 = "text 1"
	planTaskText2 = "text 2"
	fieldNameText = "text"
)

// seedSealedScenario seeds ticketID (seedQueuedTicket's ticketID) with one
// sealed scenario artifact "s1", through insertScenarioArtifact
// (planning_reads_test.go).
func seedSealedScenario(t *testing.T, s *Store, ticketID int64) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Second)
	insertScenarioArtifact(t, s, ticketID, nil, "s1", &at)
}

// ownerEditEvents returns every owner_edit event on ticketID.
func ownerEditEvents(t *testing.T, s *Store, ticketID int64) []MessageRow {
	t.Helper()
	rows, err := s.Events(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{})
	if err != nil {
		t.Fatalf("Events(owner_edit): %v", err)
	}
	return rows
}

// readScenarioPayload reads back ticketID's scenario id's raw payload bytes,
// for a byte-for-byte unchanged check.
func readScenarioPayload(t *testing.T, s *Store, ticketID int64, id string) []byte {
	t.Helper()
	var payload []byte
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT payload FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND json_extract(payload, '$.id') = ?`,
		ticketID, id,
	).Scan(&payload); err != nil {
		t.Fatalf("read back scenario %s: %v", id, err)
	}
	return payload
}

// scenarioCheck reads back ticketID's scenario id's check_cmd field.
func scenarioCheck(t *testing.T, s *Store, ticketID int64, id string) string { //nolint:unparam // every call site below reads "s1", but the helper mirrors readScenarioPayload's own general id parameter
	t.Helper()
	var sc response.Scenario
	if err := json.Unmarshal(readScenarioPayload(t, s, ticketID, id), &sc); err != nil {
		t.Fatalf("unmarshal scenario %s: %v", id, err)
	}
	return sc.Check
}

// seedQueuedTicketWithBody is seedQueuedTicket, but with ticket.Body set to
// body rather than left at its default empty string, the fixture the
// ticket_body edit tests need an old value to assert against.
func seedQueuedTicketWithBody(t *testing.T, s *Store, ref, body string) (projectID, ticketID int64) {
	t.Helper()
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err = s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: ref, Title: "t", Body: body, State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return projectID, ticketID
}

// seedPlanRun inserts a bare session and run, returning the run's id: the
// fixture a plan and its cohort's scenarios share through run_id so a
// sealed scenario on the run marks the plan sealed (editPlanTaskTx's own
// seal check).
func seedPlanRun(t *testing.T, s *Store, ticketID int64) int64 {
	t.Helper()
	res, err := s.db.ExecContext(t.Context(),
		`INSERT INTO sessions (ticket_id, job, runtime) VALUES (?, 'planning', 'fake')`, ticketID)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	sessionID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	runRes, err := s.db.ExecContext(t.Context(), `INSERT INTO runs (session_id, turn) VALUES (?, 0)`, sessionID)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	runID, err := runRes.LastInsertId()
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// planWithTasks returns the checked-in example plan (planPayload) with its
// Delivery.Tasks and Delivery.Files replaced by tasks and files: every other
// field, already schema-valid, rides along unchanged.
func planWithTasks(t *testing.T, tasks []response.Task, files []response.FileChange) response.Plan {
	t.Helper()
	var plan response.Plan
	if err := json.Unmarshal(planPayload(), &plan); err != nil {
		t.Fatalf("unmarshal plan fixture: %v", err)
	}
	plan.Delivery.Tasks = tasks
	plan.Delivery.Files = files
	return plan
}

// insertPlanArtifactPayload inserts a "plan" artifact at version 1, owned by
// runID, with plan marshaled as its payload.
func insertPlanArtifactPayload(t *testing.T, s *Store, ticketID int64, runID *int64, plan response.Plan) {
	t.Helper()
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), Artifact{
		TicketID: ticketID, RunID: runID, Type: testTypePlan, Version: 1, Payload: payload,
	}); err != nil {
		t.Fatalf("insert plan artifact: %v", err)
	}
}

// buildReportPayloadLanded is a minimal, schema-valid "build_report" artifact
// payload for task n that landed: it carries a commit_sha, the field
// landedTaskNumbers filters on.
func buildReportPayloadLanded(n int, sha string) []byte {
	return []byte(fmt.Sprintf(
		`{"task_n":%d,"files_changed":["a.go"],"extras":[],"fences":[],"report":"did it","title":"Task %d","commit_sha":%q}`,
		n, n, sha))
}

// countPlanArtifacts returns the number of "plan" artifacts on ticketID, the
// fixture TestOwnerEdit_EditsPlanTaskInPlace uses to prove editPlanTaskTx
// updates the plan artifact in place rather than inserting a new one.
func countPlanArtifacts(t *testing.T, s *Store, ticketID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM artifacts WHERE ticket_id = ? AND type = 'plan'`, ticketID,
	).Scan(&n); err != nil {
		t.Fatalf("count plan artifacts: %v", err)
	}
	return n
}

// fileTaskByPath returns the Task field of the file named path, so a drop
// test can check its post-drop task list by path rather than by slice index.
func fileTaskByPath(files []response.FileChange, path string) string {
	for _, f := range files {
		if f.Path == path {
			return f.Task
		}
	}
	return ""
}

// TestOwnerEdit_ScenarioCheckRewritesPayloadAndWritesEvent proves the
// store seam of the ticket's "done when" scenario: the artifact's check_cmd
// rewrites, exactly one owner_edit event is written, and its body equals
// OwnerEditLine's sentence.
func TestOwnerEdit_ScenarioCheckRewritesPayloadAndWritesEvent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID)

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
		Check: new("go test ./new"),
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}

	if got := scenarioCheck(t, s, ticketID, "s1"); got != "go test ./new" {
		t.Errorf("check_cmd = %q, want %q", got, "go test ./new")
	}

	events := ownerEditEvents(t, s, ticketID)
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if !strings.Contains(ev.Old, "go test ./...") {
		t.Errorf("event old = %q, want it to contain the old check", ev.Old)
	}
	if !strings.Contains(ev.New, "go test ./new") {
		t.Errorf("event new = %q, want it to contain the new check", ev.New)
	}
	if events[0].Body != response.OwnerEditLine(ev) {
		t.Errorf("event body = %q, want %q", events[0].Body, response.OwnerEditLine(ev))
	}
}

// TestOwnerEdit_RefusesUnsealedScenario proves an unsealed scenario is
// refused not_sealed, and a scenario id this ticket does not carry is
// refused not_found.
func TestOwnerEdit_RefusesUnsealedScenario(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertScenarioArtifact(t, s, ticketID, nil, "s1", nil) // unsealed

	err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit, Check: new("x"),
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(unsealed s1) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeNotSealed {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeNotSealed)
	}

	err = s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s9", Action: OwnerEditActionEdit, Check: new("x"),
	})
	refusal, ok = errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(missing s9) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeNotFound {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeNotFound)
	}
}

// TestOwnerEdit_RefusesFieldNotAllowedForTarget proves checkOwnerEditShape's
// three bad_request shapes: a field the target does not allow, drop on a
// target that is not plan_task, and an edit with no fields set.
func TestOwnerEdit_RefusesFieldNotAllowedForTarget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID)

	tests := []struct {
		name       string
		req        OwnerEditRequest
		wantReason string
	}{
		{"text field on scenario", OwnerEditRequest{TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit, Text: new("x")}, fieldNameText},
		{"drop on scenario", OwnerEditRequest{TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionDrop}, ""},
		{"edit with no fields", OwnerEditRequest{TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := s.OwnerEdit(t.Context(), tc.req)
			refusal, ok := errors.AsType[*OwnerEditError](err)
			if !ok {
				t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
			}
			if refusal.Code != OwnerEditCodeBadRequest {
				t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeBadRequest)
			}
			if tc.wantReason != "" && !strings.Contains(refusal.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to name %q", refusal.Reason, tc.wantReason)
			}
		})
	}
}

// TestOwnerEdit_RefusesWhileClaimed proves every target is refused claimed
// while a run holds the ticket's claim, and that nothing changes: a
// scenario edit, a plan task edit, and a ticket body edit.
func TestOwnerEdit_RefusesWhileClaimed(t *testing.T) {
	t.Parallel()

	t.Run("scenario", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID)
		before := scenarioCheck(t, s, ticketID, "s1")

		claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
		if err != nil || !claimed {
			t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
		}

		err = s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit, Check: new("new"),
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit(claimed) error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeClaimed {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeClaimed)
		}
		if got := scenarioCheck(t, s, ticketID, "s1"); got != before {
			t.Errorf("check_cmd changed to %q, want unchanged %q", got, before)
		}
		if n, err := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); err != nil || n != 0 {
			t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
		}
	})

	t.Run("plan_task", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		runID := seedPlanRun(t, s, ticketID)
		plan := planWithTasks(t,
			[]response.Task{{N: 1, Test: "T1", Demo: true, Text: planTaskText1}},
			[]response.FileChange{{Path: testRefAGo, Action: response.FileActionModify, Task: "1", Reason: "r"}},
		)
		insertPlanArtifactPayload(t, s, ticketID, &runID, plan)
		sealedAt := time.Now().UTC()
		insertScenarioArtifact(t, s, ticketID, &runID, "s1", &sealedAt)

		claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
		if err != nil || !claimed {
			t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
		}

		err = s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditPlanTask, Ref: "1", Action: OwnerEditActionEdit, Text: new("new text"),
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit(claimed) error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeClaimed {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeClaimed)
		}
		got, _, ok, err := s.StoredPlan(t.Context(), ticketID)
		if err != nil || !ok {
			t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
		}
		if got.Delivery.Tasks[0].Text != planTaskText1 {
			t.Errorf("task 1 text = %q, want unchanged %q", got.Delivery.Tasks[0].Text, planTaskText1)
		}
		if n, err := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); err != nil || n != 0 {
			t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
		}
	})

	t.Run("ticket_body", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicketWithBody(t, s, "1", "old body")

		claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
		if err != nil || !claimed {
			t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
		}

		err = s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditTicketBody, Action: OwnerEditActionEdit, Body: new("new body"),
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit(claimed) error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeClaimed {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeClaimed)
		}
		ticket, err := s.GetTicket(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("GetTicket: %v", err)
		}
		if ticket.Body != "old body" {
			t.Errorf("body = %q, want unchanged %q", ticket.Body, "old body")
		}
		if n, err := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); err != nil || n != 0 {
			t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
		}
	})
}

// TestOwnerEdit_RefusesSchemaBreakingEdit proves an edit that would blank a
// required scenario field is refused invalid, and the payload is unchanged.
func TestOwnerEdit_RefusesSchemaBreakingEdit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID)
	before := readScenarioPayload(t, s, ticketID, "s1")

	err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit, Given: new(""),
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(blank given) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeInvalid {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
	}
	if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
		t.Errorf("payload = %s, want unchanged %s", after, before)
	}
	if n, err := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); err != nil || n != 0 {
		t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
	}
}

// TestOwnerEdit_HostScenarioEmptyCheckRefused proves a host scenario's
// check cannot be blanked: the edit is refused invalid with
// response.HostScenarioNeedsCheck, nothing changes, and a non-blank check
// still succeeds.
func TestOwnerEdit_HostScenarioEmptyCheckRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID)
	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE artifacts SET payload = json_set(payload, '$.kind', 'host')
		 WHERE ticket_id = ? AND type = 'scenario' AND json_extract(payload, '$.id') = ?`,
		ticketID, "s1",
	); err != nil {
		t.Fatalf("set scenario s1 kind to host: %v", err)
	}
	before := readScenarioPayload(t, s, ticketID, "s1")

	err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
		Check: new("  "),
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(blank check on host scenario) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeInvalid {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
	}
	if refusal.Reason != response.HostScenarioNeedsCheck {
		t.Errorf("reason = %q, want %q", refusal.Reason, response.HostScenarioNeedsCheck)
	}
	if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
		t.Errorf("payload = %s, want unchanged %s", after, before)
	}
	if events := ownerEditEvents(t, s, ticketID); len(events) != 0 {
		t.Errorf("owner_edit events = %d, want 0", len(events))
	}

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
		Check: new("go test ./x"),
	}); err != nil {
		t.Fatalf("OwnerEdit(non-blank check on host scenario): %v", err)
	}
	if got := scenarioCheck(t, s, ticketID, "s1"); got != "go test ./x" {
		t.Errorf("check_cmd = %q, want %q", got, "go test ./x")
	}
}

// TestOwnerEditKind proves OwnerEditRequest.Kind (#57): a host kind with a
// non-blank check is stored, and a bogus kind is refused invalid (the
// schema's enum) with nothing changed.
func TestOwnerEditKind(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID)

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
		Kind: new("host"), Check: new("go test ./host"),
	}); err != nil {
		t.Fatalf("OwnerEdit(kind host): %v", err)
	}
	var sc response.Scenario
	if err := json.Unmarshal(readScenarioPayload(t, s, ticketID, "s1"), &sc); err != nil {
		t.Fatalf("unmarshal scenario s1: %v", err)
	}
	if sc.Kind != response.ScenarioKindHost || sc.Check != "go test ./host" {
		t.Errorf("scenario s1 = %+v, want kind host, check %q", sc, "go test ./host")
	}

	before := readScenarioPayload(t, s, ticketID, "s1")
	err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
		Kind: new("bogus"),
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(kind bogus) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeInvalid {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
	}
	if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
		t.Errorf("payload = %s, want unchanged %s", after, before)
	}

	// A host check runs unsandboxed with only the owner's reading of its
	// rendered text as approval, so switching an existing check to kind
	// host, without changing the check text, still refuses a check that
	// hides its true meaning behind a bidi override (#57, r1f13).
	at := time.Now().UTC().Truncate(time.Second)
	insertScenarioArtifact(t, s, ticketID, nil, "s2", &at)
	unsafeCheck := "go test ./old\u202e"
	if editErr := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s2", Action: OwnerEditActionEdit,
		Check: &unsafeCheck,
	}); editErr != nil {
		t.Fatalf("OwnerEdit(check with bidi override, kind behavior): %v", editErr)
	}

	before = readScenarioPayload(t, s, ticketID, "s2")
	err = s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s2", Action: OwnerEditActionEdit,
		Kind: new("host"),
	})
	refusal, ok = errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(kind host, unsafe check) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeInvalid {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
	}
	if after := readScenarioPayload(t, s, ticketID, "s2"); !bytes.Equal(after, before) {
		t.Errorf("payload = %s, want unchanged %s", after, before)
	}

	// Q16 drops the owner-typed exemption entirely: an edit that sets kind
	// host and, in the same edit, types an unsafe check is refused too,
	// whoever wrote the check (#57, r4f8).
	ownedUnsafe := "go test ./typed\u202e"
	before = readScenarioPayload(t, s, ticketID, "s2")
	err = s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s2", Action: OwnerEditActionEdit,
		Kind: new("host"), Check: &ownedUnsafe,
	})
	refusal, ok = errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(kind host, owner-typed bidi check) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeInvalid {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
	}
	if after := readScenarioPayload(t, s, ticketID, "s2"); !bytes.Equal(after, before) {
		t.Errorf("payload = %s, want unchanged %s", after, before)
	}

	// Switch s2 to host with a safe check, then prove an edit to only
	// given, when or then on a scenario that is already host still runs
	// HostCheckUnsafe against the resulting (already-stored, safe) check
	// and so is unaffected (#57, Q16).
	safeCheck := "go test ./safe"
	if editErr := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s2", Action: OwnerEditActionEdit,
		Kind: new("host"), Check: &safeCheck,
	}); editErr != nil {
		t.Fatalf("OwnerEdit(kind host, safe check): %v", editErr)
	}
	if editErr := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s2", Action: OwnerEditActionEdit,
		Given: new(amendedGiven),
	}); editErr != nil {
		t.Fatalf("OwnerEdit(given only, already host): %v", editErr)
	}
	if err := json.Unmarshal(readScenarioPayload(t, s, ticketID, "s2"), &sc); err != nil {
		t.Fatalf("unmarshal scenario s2: %v", err)
	}
	if sc.Given != amendedGiven || sc.Check != safeCheck {
		t.Errorf("scenario s2 = %+v, want given %q, check unchanged %q", sc, amendedGiven, safeCheck)
	}
}

// --- plan task edits (#41, task 2) ------------------------------------------

// TestOwnerEdit_EditsPlanTaskInPlace proves a plan_task edit updates the one
// stored plan artifact's task in place: no new artifact row, the version
// unchanged, the edited fields changed, the untouched task left alone, and
// the owner_edit event holding the full old and new plan JSON.
func TestOwnerEdit_EditsPlanTaskInPlace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	runID := seedPlanRun(t, s, ticketID)

	plan := planWithTasks(t,
		[]response.Task{
			{N: 1, Test: "T1", Demo: true, Text: "old text 1"},
			{N: 2, Test: "T2", Demo: false, Text: "old text 2"},
		},
		[]response.FileChange{
			{Path: testRefAGo, Action: response.FileActionModify, Task: "1 2", Reason: "r"},
		},
	)
	insertPlanArtifactPayload(t, s, ticketID, &runID, plan)
	sealedAt := time.Now().UTC()
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", &sealedAt)

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditPlanTask, Ref: "2", Action: OwnerEditActionEdit,
		Text: new("new text 2"), Demo: new(true),
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}

	if n := countPlanArtifacts(t, s, ticketID); n != 1 {
		t.Errorf("plan artifacts = %d, want 1", n)
	}

	got, version, ok, err := s.StoredPlan(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if len(got.Delivery.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got.Delivery.Tasks))
	}
	if got.Delivery.Tasks[0].Text != "old text 1" {
		t.Errorf("task 1 text = %q, want unchanged %q", got.Delivery.Tasks[0].Text, "old text 1")
	}
	if got.Delivery.Tasks[1].Text != "new text 2" || !got.Delivery.Tasks[1].Demo {
		t.Errorf("task 2 = %+v, want text %q demo true", got.Delivery.Tasks[1], "new text 2")
	}

	events := ownerEditEvents(t, s, ticketID)
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if !strings.Contains(ev.Old, "old text 2") {
		t.Errorf("event old = %q, want it to contain the old task text", ev.Old)
	}
	if !strings.Contains(ev.New, "new text 2") {
		t.Errorf("event new = %q, want it to contain the new task text", ev.New)
	}
}

// TestOwnerEdit_RefusesUnsealedPlan proves a plan whose run has no sealed
// scenario is refused not_sealed.
func TestOwnerEdit_RefusesUnsealedPlan(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	runID := seedPlanRun(t, s, ticketID)

	plan := planWithTasks(t,
		[]response.Task{{N: 1, Test: "T1", Demo: true, Text: planTaskText1}},
		[]response.FileChange{{Path: testRefAGo, Action: response.FileActionModify, Task: "1", Reason: "r"}},
	)
	insertPlanArtifactPayload(t, s, ticketID, &runID, plan)
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", nil) // unsealed

	err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditPlanTask, Ref: "1", Action: OwnerEditActionEdit, Text: new("x"),
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(unsealed plan) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeNotSealed {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeNotSealed)
	}
}

// TestOwnerEdit_RefusesDropAtOrBeforeLandedTask proves a drop is refused
// landed when a landed build_report's task_n is at or after the task being
// dropped, and succeeds for a task strictly after the landed one.
func TestOwnerEdit_RefusesDropAtOrBeforeLandedTask(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	runID := seedPlanRun(t, s, ticketID)

	plan := planWithTasks(t,
		[]response.Task{
			{N: 1, Test: "T1", Demo: true, Text: planTaskText1},
			{N: 2, Test: "T2", Demo: false, Text: planTaskText2},
			{N: 3, Test: "T3", Demo: false, Text: "text 3"},
		},
		[]response.FileChange{{Path: testRefAGo, Action: response.FileActionModify, Task: "1 2 3", Reason: "r"}},
	)
	insertPlanArtifactPayload(t, s, ticketID, &runID, plan)
	sealedAt := time.Now().UTC()
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", &sealedAt)

	if _, err := s.InsertArtifact(t.Context(), Artifact{
		TicketID: ticketID, Type: testTypeBuildReport, Payload: buildReportPayloadLanded(2, strings.Repeat("a", 40)),
	}); err != nil {
		t.Fatalf("insert build_report: %v", err)
	}
	// A build_report with no commit_sha, for task 3, is not landed: it must
	// not itself block a drop of task 3 (landedTaskNumbers filters on
	// commit_sha IS NOT NULL).
	if _, err := s.InsertArtifact(t.Context(), Artifact{
		TicketID: ticketID, Type: testTypeBuildReport, Payload: buildReportPayload(3),
	}); err != nil {
		t.Fatalf("insert build_report (no commit_sha): %v", err)
	}

	for _, n := range []string{"1", "2"} {
		err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditPlanTask, Ref: n, Action: OwnerEditActionDrop,
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit(drop task %s) error = %v (%T), want *OwnerEditError", n, err, err)
		}
		if refusal.Code != OwnerEditCodeLanded {
			t.Errorf("drop task %s: code = %q, want %q", n, refusal.Code, OwnerEditCodeLanded)
		}
	}

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditPlanTask, Ref: "3", Action: OwnerEditActionDrop,
	}); err != nil {
		t.Fatalf("OwnerEdit(drop task 3): %v", err)
	}
	got, _, ok, err := s.StoredPlan(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
	}
	if len(got.Delivery.Tasks) != 2 {
		t.Errorf("tasks = %d, want 2", len(got.Delivery.Tasks))
	}

	events := ownerEditEvents(t, s, ticketID)
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if ev.Action != OwnerEditActionDrop || ev.Ref != "3" {
		t.Errorf("event action/ref = %q/%q, want %q/%q", ev.Action, ev.Ref, OwnerEditActionDrop, "3")
	}
	if !strings.Contains(ev.Old, "text 3") {
		t.Errorf("event old = %q, want it to contain the dropped task's text", ev.Old)
	}
	if strings.Contains(ev.New, "text 3") {
		t.Errorf("event new = %q, want it to no longer contain the dropped task's text", ev.New)
	}
	wantBody := "Owner dropped plan task 3; later tasks moved up one."
	if events[0].Body != wantBody {
		t.Errorf("event body = %q, want %q", events[0].Body, wantBody)
	}
}

// TestOwnerEdit_RefusesDropOfOnlyTask proves dropping a one-task plan's only
// task is refused invalid (the resulting empty tasks list fails the
// schema), and the payload is unchanged.
func TestOwnerEdit_RefusesDropOfOnlyTask(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	runID := seedPlanRun(t, s, ticketID)

	plan := planWithTasks(t,
		[]response.Task{{N: 1, Test: "T1", Demo: true, Text: planTaskText1}},
		[]response.FileChange{{Path: testRefAGo, Action: response.FileActionModify, Task: "1", Reason: "r"}},
	)
	insertPlanArtifactPayload(t, s, ticketID, &runID, plan)
	sealedAt := time.Now().UTC()
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", &sealedAt)

	err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditPlanTask, Ref: "1", Action: OwnerEditActionDrop,
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(drop only task) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeInvalid {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
	}

	got, _, ok, err := s.StoredPlan(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
	}
	if len(got.Delivery.Tasks) != 1 {
		t.Errorf("tasks = %d, want unchanged 1", len(got.Delivery.Tasks))
	}
}

// --- ticket body edits (#41, task 3) ----------------------------------------

// TestOwnerEdit_AmendsTicketBody proves ticket_body's edit updates
// tickets.body, writes an owner_edit event holding the old and new body,
// and that a whitespace-only body is refused bad_request before any
// transaction opens.
func TestOwnerEdit_AmendsTicketBody(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicketWithBody(t, s, "1", "old body text")

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditTicketBody, Action: OwnerEditActionEdit,
		Body: new("new body text"),
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.Body != "new body text" {
		t.Errorf("body = %q, want %q", ticket.Body, "new body text")
	}

	events := ownerEditEvents(t, s, ticketID)
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err = json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if ev.Old != "old body text" {
		t.Errorf("event old = %q, want %q", ev.Old, "old body text")
	}
	if ev.New != "new body text" {
		t.Errorf("event new = %q, want %q", ev.New, "new body text")
	}
	if events[0].Body != response.OwnerEditLine(ev) {
		t.Errorf("event body = %q, want %q", events[0].Body, response.OwnerEditLine(ev))
	}

	err = s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditTicketBody, Action: OwnerEditActionEdit,
		Body: new("   \n\t  "),
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(blank body) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != OwnerEditCodeBadRequest {
		t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeBadRequest)
	}
}

// TestOwnerEdit_TicketBodyKeepsTrackerBaseline proves editTicketBodyTx fills
// a missing tracker_body baseline from the pre-edit body before it
// overwrites body (#98), so a later planning refresh can tell a console
// edit apart from an edit on the tracker: a row whose tracker_body is NULL
// (standing in for one inserted before migration 0011) gets tracker_body
// "A" on its first edit and keeps it on a second edit, while a row whose
// tracker_body is already set keeps that value.
func TestOwnerEdit_TicketBodyKeepsTrackerBaseline(t *testing.T) {
	t.Parallel()

	t.Run("NULL tracker_body is filled from the pre-edit body", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicketWithBody(t, s, "2", "A")
		if _, err := s.db.ExecContext(ctx, `UPDATE tickets SET tracker_body = NULL WHERE id = ?`, ticketID); err != nil {
			t.Fatalf("set tracker_body NULL: %v", err)
		}

		if err := s.OwnerEdit(ctx, OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditTicketBody, Action: OwnerEditActionEdit,
			Body: new("A, edited"),
		}); err != nil {
			t.Fatalf("OwnerEdit: %v", err)
		}

		got, getErr := s.GetTicket(ctx, ticketID)
		if getErr != nil {
			t.Fatalf("GetTicket: %v", getErr)
		}
		if got.Body != "A, edited" {
			t.Errorf("body = %q, want %q", got.Body, "A, edited")
		}
		if got.TrackerBody == nil || *got.TrackerBody != "A" {
			t.Errorf("tracker_body = %v, want %q", got.TrackerBody, "A")
		}

		if err := s.OwnerEdit(ctx, OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditTicketBody, Action: OwnerEditActionEdit,
			Body: new("A, again"),
		}); err != nil {
			t.Fatalf("OwnerEdit (second edit): %v", err)
		}
		got2, getErr2 := s.GetTicket(ctx, ticketID)
		if getErr2 != nil {
			t.Fatalf("GetTicket: %v", getErr2)
		}
		if got2.TrackerBody == nil || *got2.TrackerBody != "A" {
			t.Errorf("tracker_body after a second edit = %v, want unchanged %q", got2.TrackerBody, "A")
		}
	})

	t.Run("an already-set tracker_body is left alone", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicketWithBody(t, s, "3", "A")
		if _, err := s.db.ExecContext(ctx, `UPDATE tickets SET tracker_body = 'X' WHERE id = ?`, ticketID); err != nil {
			t.Fatalf("set tracker_body X: %v", err)
		}

		if err := s.OwnerEdit(ctx, OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditTicketBody, Action: OwnerEditActionEdit,
			Body: new("A, edited"),
		}); err != nil {
			t.Fatalf("OwnerEdit: %v", err)
		}

		got, getErr := s.GetTicket(ctx, ticketID)
		if getErr != nil {
			t.Fatalf("GetTicket: %v", getErr)
		}
		if got.TrackerBody == nil || *got.TrackerBody != "X" {
			t.Errorf("tracker_body = %v, want unchanged %q", got.TrackerBody, "X")
		}
	})
}

// --- AnswerQuestion (#57, "Edit it") -----------------------------------------

// seedAmendedEscalation claims ticketID and commits one escalation whose
// linked question carries an amendment for scenario ref (no amendment at
// all when ref is ""), returning that question's id. CommitHandlerResult's
// own final ticket UPDATE always clears the claim it just took, so the
// ticket is unclaimed again by the time this returns -- OwnerEdit's own
// claim guard would otherwise refuse every edit below.
func seedAmendedEscalation(t *testing.T, s *Store, ticketID int64, ref string) int64 {
	t.Helper()
	payload := escalationTestPayload(response.EscalationCodeCannotRun, response.EscalationOriginJudge)
	if ref != "" {
		payload.Amendment = &response.Amendment{
			Scenario: ref, Kind: response.ScenarioKindNegative,
			Given: "g2", When: "w2", Then: "t2", Check: amendedCheck, Reason: amendedReason,
		}
	}
	owner, expires := claimForCommit(t, s, ticketID)
	if _, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Escalation: &EscalationCommit{Body: amendedEscalationBody, Payload: payload},
	}); err != nil {
		t.Fatalf("CommitHandlerResult(escalation): %v", err)
	}
	open, err := s.QuestionsByState(t.Context(), ticketID, questionStateOpen)
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open questions = %d, want 1", len(open))
	}
	return open[0].ID
}

// TestOwnerEditAnswersAmendedEscalation proves OwnerEditRequest.AnswerQuestion
// (#57 Q3, "Edit it"): saving the owner's own edit with AnswerQuestion set
// to the amended escalation's question lands the edit and answers the
// question "b" in one transaction. A question with no amendment, one that
// amends a different scenario, or one already answered is refused
// answer_refused, and every refusal leaves the scenario, the events, and the
// question's state all unchanged.
func TestOwnerEditAnswersAmendedEscalation(t *testing.T) {
	t.Parallel()

	t.Run("lands the edit and answers b", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID) // s1
		qID := seedAmendedEscalation(t, s, ticketID, "s1")

		const ownerCheck = "go test ./owner-written"
		if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Check: new(ownerCheck), AnswerQuestion: &qID,
		}); err != nil {
			t.Fatalf("OwnerEdit: %v", err)
		}

		if got := scenarioCheck(t, s, ticketID, "s1"); got != ownerCheck {
			t.Errorf("check_cmd = %q, want the owner's own %q", got, ownerCheck)
		}
		if events := ownerEditEvents(t, s, ticketID); len(events) != 1 {
			t.Errorf("owner_edit events = %d, want 1", len(events))
		}
		if open, err := s.QuestionsByState(t.Context(), ticketID, questionStateOpen); err != nil || len(open) != 0 {
			t.Errorf("open questions = %+v (err %v), want none", open, err)
		}
		answered, err := s.QuestionsByState(t.Context(), ticketID, questionStateAnswered)
		if err != nil || len(answered) != 1 || answered[0].ID != qID {
			t.Errorf("answered questions = %+v (err %v), want exactly [%d]", answered, err, qID)
		}
	})

	t.Run("a draft picked on the question before Save is cleared", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID) // s1
		qID := seedAmendedEscalation(t, s, ticketID, "s1")

		// The owner picked the Accept chip (draft option "a") before opening
		// the box and saving it, the same way a chip pick drafts against any
		// other question (#57, r1f12): Save's own answer must not leave that
		// draft stranded against a now-answered question.
		draftOption := "a"
		if _, err := s.SaveDraft(t.Context(), DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &draftOption}); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}

		const ownerCheck = "go test ./owner-written"
		if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Check: new(ownerCheck), AnswerQuestion: &qID,
		}); err != nil {
			t.Fatalf("OwnerEdit: %v", err)
		}

		var remaining int
		if err := s.db.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM messages WHERE parent_id = ? AND state = 'draft'`, qID,
		).Scan(&remaining); err != nil {
			t.Fatalf("count drafts on question %d: %v", qID, err)
		}
		if remaining != 0 {
			t.Errorf("drafts on question %d = %d, want 0", qID, remaining)
		}

		var readAt sql.NullString
		if err := s.db.QueryRowContext(t.Context(),
			`SELECT read_at FROM messages WHERE id = ?`, qID,
		).Scan(&readAt); err != nil {
			t.Fatalf("read read_at for question %d: %v", qID, err)
		}
		if !readAt.Valid {
			t.Errorf("question %d read_at is NULL, want set", qID)
		}
	})

	t.Run("a question with no amendment is refused", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID)
		qID := seedAmendedEscalation(t, s, ticketID, "")
		before := readScenarioPayload(t, s, ticketID, "s1")

		err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Check: new("x"), AnswerQuestion: &qID,
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeAnswerRefused {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeAnswerRefused)
		}
		if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
			t.Errorf("payload = %s, want unchanged %s (the refusal rolls the edit back too)", after, before)
		}
		if n, countErr := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); countErr != nil || n != 0 {
			t.Errorf("owner_edit events = %d (err %v), want 0", n, countErr)
		}
		if open, openErr := s.QuestionsByState(t.Context(), ticketID, questionStateOpen); openErr != nil || len(open) != 1 || open[0].ID != qID {
			t.Errorf("open questions = %+v (err %v), want still exactly [%d]", open, openErr, qID)
		}
	})

	t.Run("an amendment for another scenario is refused", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID) // s1
		at := time.Now().UTC().Truncate(time.Second)
		insertScenarioArtifact(t, s, ticketID, nil, "s2", &at)
		qID := seedAmendedEscalation(t, s, ticketID, "s2")
		before := readScenarioPayload(t, s, ticketID, "s1")

		err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Check: new("x"), AnswerQuestion: &qID,
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeAnswerRefused {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeAnswerRefused)
		}
		if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
			t.Errorf("payload = %s, want unchanged %s", after, before)
		}
	})

	t.Run("an already-answered question is refused", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID)
		qID := seedAmendedEscalation(t, s, ticketID, "s1")

		if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Check: new("first"), AnswerQuestion: &qID,
		}); err != nil {
			t.Fatalf("OwnerEdit(first): %v", err)
		}

		err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Check: new("second"), AnswerQuestion: &qID,
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit(second) error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeAnswerRefused {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeAnswerRefused)
		}
		if got := scenarioCheck(t, s, ticketID, "s1"); got != "first" {
			t.Errorf("check_cmd = %q, want unchanged %q (the second edit rolled back)", got, "first")
		}
		if events := ownerEditEvents(t, s, ticketID); len(events) != 1 {
			t.Errorf("owner_edit events = %d, want 1 (only the first edit)", len(events))
		}
	})

	t.Run("a check unchanged from the judge's own unsafe amendment is refused under kind host", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID) // s1, kind behavior

		unsafeCheck := "go test ./amended\u202e"
		payload := escalationTestPayload(response.EscalationCodeCannotRun, response.EscalationOriginJudge)
		payload.Amendment = &response.Amendment{
			Scenario: "s1", Kind: response.ScenarioKindBehavior,
			Given: "g2", When: "w2", Then: "t2", Check: unsafeCheck, Reason: amendedReason,
		}
		owner, expires := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{Body: amendedEscalationBody, Payload: payload},
		}); err != nil {
			t.Fatalf("CommitHandlerResult(escalation): %v", err)
		}
		open, err := s.QuestionsByState(t.Context(), ticketID, questionStateOpen)
		if err != nil || len(open) != 1 {
			t.Fatalf("QuestionsByState(open) = %+v (err %v), want exactly 1", open, err)
		}
		qID := open[0].ID
		before := readScenarioPayload(t, s, ticketID, "s1")

		// The owner only switched the kind select to host; the "Edit it"
		// box resends every field, so Check still carries the judge's own
		// unsafe text byte for byte -- never typed by the owner.
		err = s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Kind: new("host"), Check: new(unsafeCheck), AnswerQuestion: &qID,
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeInvalid {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
		}
		if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
			t.Errorf("payload = %s, want unchanged %s", after, before)
		}
		if n, countErr := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); countErr != nil || n != 0 {
			t.Errorf("owner_edit events = %d (err %v), want 0", n, countErr)
		}
		if openAfter, openErr := s.QuestionsByState(t.Context(), ticketID, questionStateOpen); openErr != nil || len(openAfter) != 1 || openAfter[0].ID != qID {
			t.Errorf("open questions = %+v (err %v), want still exactly [%d]", openAfter, openErr, qID)
		}
	})

	t.Run("saving an already-host scenario's edit with the judge's unsafe check unchanged is refused (#57, r4f8)", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID) // s1, kind behavior

		// The scenario is already host before the judge's amendment.
		if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Kind: new("host"), Check: new("go test ./host"),
		}); err != nil {
			t.Fatalf("OwnerEdit(kind host, safe check): %v", err)
		}

		// The judge's own amendment proposes kind behavior with an unsafe
		// check; checkScenarioRules runs HostCheckUnsafe only for kind
		// host, so an amendment resolving to behavior passes and is
		// offered even though its check is unsafe for host.
		unsafeCheck := "go test ./amended\u202e"
		payload := escalationTestPayload(response.EscalationCodeCannotRun, response.EscalationOriginJudge)
		payload.Amendment = &response.Amendment{
			Scenario: "s1", Kind: response.ScenarioKindBehavior,
			Given: "g2", When: "w2", Then: "t2", Check: unsafeCheck, Reason: amendedReason,
		}
		owner, expires := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(t.Context(), HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{Body: amendedEscalationBody, Payload: payload},
		}); err != nil {
			t.Fatalf("CommitHandlerResult(escalation): %v", err)
		}
		open, err := s.QuestionsByState(t.Context(), ticketID, questionStateOpen)
		if err != nil || len(open) != 1 {
			t.Fatalf("QuestionsByState(open) = %+v (err %v), want exactly 1", open, err)
		}
		qID := open[0].ID
		before := readScenarioPayload(t, s, ticketID, "s1")

		// The owner leaves the kind select at host (the scenario's own
		// current kind) and saves the box, which resends the judge's
		// check unchanged. The resulting kind is host, so HostCheckUnsafe
		// must run on it regardless of the kind having "switched".
		err = s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
			Kind: new("host"), Check: new(unsafeCheck), AnswerQuestion: &qID,
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeInvalid {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeInvalid)
		}
		if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
			t.Errorf("payload = %s, want unchanged %s", after, before)
		}
		if n, countErr := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); countErr != nil || n != 1 {
			t.Errorf("owner_edit events = %d (err %v), want 1 (only the earlier switch-to-host edit)", n, countErr)
		}
		if openAfter, openErr := s.QuestionsByState(t.Context(), ticketID, questionStateOpen); openErr != nil || len(openAfter) != 1 || openAfter[0].ID != qID {
			t.Errorf("open questions = %+v (err %v), want still exactly [%d]", openAfter, openErr, qID)
		}
	})

	t.Run("answer_question is allowed only for a scenario edit", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		_, ticketID := seedQueuedTicketWithBody(t, s, "1", "old body")
		qID := int64(1)

		err := s.OwnerEdit(t.Context(), OwnerEditRequest{
			TicketID: ticketID, Target: OwnerEditTicketBody, Action: OwnerEditActionEdit,
			Body: new("new body"), AnswerQuestion: &qID,
		})
		refusal, ok := errors.AsType[*OwnerEditError](err)
		if !ok {
			t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
		}
		if refusal.Code != OwnerEditCodeBadRequest {
			t.Errorf("code = %q, want %q", refusal.Code, OwnerEditCodeBadRequest)
		}
	})
}

// TestDropPlanTask_RenumbersTasksAndFileLists proves the worked example: a
// five-task plan with files "1 3", "3 4", "3", "5", and no task attribute,
// dropping task 3 renumbers tasks 4 and 5 down by one, rewrites each file's
// task list accordingly, drops the file left with none, and leaves the
// input plan unmodified.
func TestDropPlanTask_RenumbersTasksAndFileLists(t *testing.T) {
	t.Parallel()
	tasks := make([]response.Task, 5)
	for i := range tasks {
		tasks[i] = response.Task{N: i + 1, Test: fmt.Sprintf("T%d", i+1), Demo: i == 0, Text: fmt.Sprintf("text %d", i+1)}
	}
	files := []response.FileChange{
		{Path: "f1.go", Action: response.FileActionModify, Task: "1 3", Reason: "r1"},
		{Path: "f2.go", Action: response.FileActionModify, Task: "3 4", Reason: "r2"},
		{Path: "f3.go", Action: response.FileActionModify, Task: "3", Reason: "r3"},
		{Path: "f4.go", Action: response.FileActionModify, Task: "5", Reason: "r4"},
		{Path: "f5.go", Action: response.FileActionModify, Reason: "r5"},
	}
	plan := planWithTasks(t, tasks, files)
	before, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan snapshot: %v", err)
	}

	got := dropPlanTask(plan, 3)

	after, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan after drop: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("dropPlanTask modified its input plan")
	}

	if len(got.Delivery.Tasks) != 4 {
		t.Fatalf("tasks = %d, want 4", len(got.Delivery.Tasks))
	}
	for i, want := range []struct {
		n    int
		text string
	}{
		{1, planTaskText1}, {2, planTaskText2}, {3, "text 4"}, {4, "text 5"},
	} {
		if got.Delivery.Tasks[i].N != want.n || got.Delivery.Tasks[i].Text != want.text {
			t.Errorf("task %d = {N:%d Text:%q}, want {N:%d Text:%q}",
				i, got.Delivery.Tasks[i].N, got.Delivery.Tasks[i].Text, want.n, want.text)
		}
	}

	wantFileTasks := map[string]string{
		"f1.go": "1",
		"f2.go": "3",
		"f4.go": "4",
		"f5.go": "",
	}
	for path, want := range wantFileTasks {
		if got := fileTaskByPath(got.Delivery.Files, path); got != want {
			t.Errorf("file %s task = %q, want %q", path, got, want)
		}
	}
	for _, f := range got.Delivery.Files {
		if f.Path == "f3.go" {
			t.Error("f3.go should have been dropped, its only task was the one removed")
		}
	}
}

// --- plan file edits (#51, task 3) ------------------------------------------

// planFileTasksTestPath is the delivery file path TestOwnerEdit_
// SetsPlanFileTasks and TestOwnerEdit_PlanFileRefusals edit, matching
// plan #51's own acceptance example.
const planFileTasksTestPath = "internal/store/console_reads.go"

// sealedSixTaskPlanWithFile seeds ticketID with a sealed, six-task plan
// whose only delivery file is planFileTasksTestPath, with task list "6".
func sealedSixTaskPlanWithFile(t *testing.T, s *Store, ticketID int64) {
	t.Helper()
	runID := seedPlanRun(t, s, ticketID)
	tasks := make([]response.Task, 6)
	for i := range tasks {
		tasks[i] = response.Task{N: i + 1, Test: "TestX", Text: "do it"}
	}
	files := []response.FileChange{
		{Path: planFileTasksTestPath, Action: response.FileActionModify, Task: "6", Reason: "r"},
	}
	insertPlanArtifactPayload(t, s, ticketID, &runID, planWithTasks(t, tasks, files))
	sealedAt := time.Now().UTC()
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", &sealedAt)
}

// TestOwnerEdit_SetsPlanFileTasks proves a plan_file edit normalizes and
// stores the new task list in place, with no new plan artifact row, and
// writes exactly one owner_edit event whose body equals OwnerEditLine's
// sentence.
func TestOwnerEdit_SetsPlanFileTasks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	sealedSixTaskPlanWithFile(t, s, ticketID)

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionEdit,
		Tasks: new("6 2"),
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}

	if n := countPlanArtifacts(t, s, ticketID); n != 1 {
		t.Errorf("plan artifacts = %d, want 1", n)
	}

	got, _, ok, err := s.StoredPlan(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
	}
	if task := fileTaskByPath(got.Delivery.Files, planFileTasksTestPath); task != testTasks2And6 {
		t.Errorf("file task = %q, want %q", task, testTasks2And6)
	}

	events := ownerEditEvents(t, s, ticketID)
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if ev.Target != OwnerEditPlanFile || ev.Ref != planFileTasksTestPath || ev.Action != OwnerEditActionEdit {
		t.Errorf("event target/ref/action = %q/%q/%q, want %q/%q/%q",
			ev.Target, ev.Ref, ev.Action, OwnerEditPlanFile, planFileTasksTestPath, OwnerEditActionEdit)
	}
	if ev.Old != "6" || ev.New != testTasks2And6 {
		t.Errorf("event old/new = %q/%q, want %q/%q", ev.Old, ev.New, "6", testTasks2And6)
	}
	if events[0].Body != response.OwnerEditLine(ev) {
		t.Errorf("event body = %q, want %q", events[0].Body, response.OwnerEditLine(ev))
	}
}

// TestOwnerEdit_PlanFileRefusals is a table test over every plan_file
// refusal: each case asserts *OwnerEditError's Code, that the stored plan's
// bytes are unchanged, and that no owner_edit event is written.
func TestOwnerEdit_PlanFileRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantCode OwnerEditCode
		seed     func(t *testing.T, s *Store, ticketID int64)
		req      func(ticketID int64) OwnerEditRequest
	}{
		{
			name:     "task number outside plan",
			wantCode: OwnerEditCodeInvalid,
			seed:     sealedSixTaskPlanWithFile,
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionEdit, Tasks: new("2 9")}
			},
		},
		{
			name:     "blank tasks",
			wantCode: OwnerEditCodeBadRequest,
			seed:     sealedSixTaskPlanWithFile,
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionEdit, Tasks: new("")}
			},
		},
		{
			name:     "double space in tasks",
			wantCode: OwnerEditCodeBadRequest,
			seed:     sealedSixTaskPlanWithFile,
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionEdit, Tasks: new("2  6")}
			},
		},
		{
			name:     "unknown file",
			wantCode: OwnerEditCodeNotFound,
			seed:     sealedSixTaskPlanWithFile,
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: "nope.go", Action: OwnerEditActionEdit, Tasks: new("2")}
			},
		},
		{
			name:     "blank ref",
			wantCode: OwnerEditCodeBadRequest,
			seed:     sealedSixTaskPlanWithFile,
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: "", Action: OwnerEditActionEdit, Tasks: new("2")}
			},
		},
		{
			name:     "unsealed plan",
			wantCode: OwnerEditCodeNotSealed,
			seed: func(t *testing.T, s *Store, ticketID int64) {
				t.Helper()
				runID := seedPlanRun(t, s, ticketID)
				plan := planWithTasks(t,
					[]response.Task{{N: 1, Test: "T1", Demo: true, Text: planTaskText1}},
					[]response.FileChange{{Path: planFileTasksTestPath, Action: response.FileActionModify, Task: "1", Reason: "r"}},
				)
				insertPlanArtifactPayload(t, s, ticketID, &runID, plan)
				insertScenarioArtifact(t, s, ticketID, &runID, "s1", nil) // unsealed
			},
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionEdit, Tasks: new("1")}
			},
		},
		{
			name:     "plan not task-mapped",
			wantCode: OwnerEditCodeInvalid,
			seed: func(t *testing.T, s *Store, ticketID int64) {
				t.Helper()
				runID := seedPlanRun(t, s, ticketID)
				plan := planWithTasks(t,
					[]response.Task{{N: 1, Test: "T1", Demo: true, Text: planTaskText1}},
					[]response.FileChange{{Path: planFileTasksTestPath, Action: response.FileActionModify, Reason: "r"}},
				)
				insertPlanArtifactPayload(t, s, ticketID, &runID, plan)
				sealedAt := time.Now().UTC()
				insertScenarioArtifact(t, s, ticketID, &runID, "s1", &sealedAt)
			},
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionEdit, Tasks: new("1")}
			},
		},
		{
			name:     "claimed ticket",
			wantCode: OwnerEditCodeClaimed,
			seed: func(t *testing.T, s *Store, ticketID int64) {
				t.Helper()
				sealedSixTaskPlanWithFile(t, s, ticketID)
				claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
				if err != nil || !claimed {
					t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
				}
			},
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionEdit, Tasks: new("2")}
			},
		},
		{
			name:     "drop not allowed",
			wantCode: OwnerEditCodeBadRequest,
			seed:     sealedSixTaskPlanWithFile,
			req: func(ticketID int64) OwnerEditRequest {
				return OwnerEditRequest{TicketID: ticketID, Target: OwnerEditPlanFile, Ref: planFileTasksTestPath, Action: OwnerEditActionDrop}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			_, ticketID := seedQueuedTicket(t, s, "1")
			tc.seed(t, s, ticketID)
			before := readPlanPayload(t, s, ticketID)

			err := s.OwnerEdit(t.Context(), tc.req(ticketID))
			refusal, ok := errors.AsType[*OwnerEditError](err)
			if !ok {
				t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
			}
			if refusal.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", refusal.Code, tc.wantCode)
			}
			if after := readPlanPayload(t, s, ticketID); !bytes.Equal(after, before) {
				t.Errorf("plan payload changed, want unchanged")
			}
			if n, err := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); err != nil || n != 0 {
				t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
			}
		})
	}
}

// TestCheckPlanStructure proves checkPlanStructure's two faults -- tasks not
// numbered 1..len in order, and a file task list naming a number outside
// 1..len -- and that a well-formed plan reports no fault.
func TestCheckPlanStructure(t *testing.T) {
	t.Parallel()

	t.Run("tasks out of order", func(t *testing.T) {
		t.Parallel()
		plan := planWithTasks(t,
			[]response.Task{
				{N: 1, Test: "T1", Demo: true, Text: planTaskText1},
				{N: 3, Test: "T2", Demo: false, Text: planTaskText2},
			},
			[]response.FileChange{{Path: testRefAGo, Action: response.FileActionModify, Task: "1", Reason: "r"}},
		)
		if fault := checkPlanStructure(plan); fault == "" {
			t.Error("checkPlanStructure(tasks 1,3) = \"\", want a fault")
		}
	})

	t.Run("file task out of range", func(t *testing.T) {
		t.Parallel()
		plan := planWithTasks(t,
			[]response.Task{
				{N: 1, Test: "T1", Demo: true, Text: planTaskText1},
				{N: 2, Test: "T2", Demo: false, Text: planTaskText2},
				{N: 3, Test: "T3", Demo: false, Text: "text 3"},
			},
			[]response.FileChange{{Path: testRefAGo, Action: response.FileActionModify, Task: "4", Reason: "r"}},
		)
		if fault := checkPlanStructure(plan); fault == "" {
			t.Error("checkPlanStructure(file task 4 of 3) = \"\", want a fault")
		}
	})

	t.Run("well formed", func(t *testing.T) {
		t.Parallel()
		plan := planWithTasks(t,
			[]response.Task{
				{N: 1, Test: "T1", Demo: true, Text: planTaskText1},
				{N: 2, Test: "T2", Demo: false, Text: planTaskText2},
			},
			[]response.FileChange{{Path: testRefAGo, Action: response.FileActionModify, Task: "1 2", Reason: "r"}},
		)
		if fault := checkPlanStructure(plan); fault != "" {
			t.Errorf("checkPlanStructure(well formed) = %q, want \"\"", fault)
		}
	})
}
