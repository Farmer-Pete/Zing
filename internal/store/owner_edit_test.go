package store

import (
	"bytes"
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
func scenarioCheck(t *testing.T, s *Store, ticketID int64, id string) string {
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
