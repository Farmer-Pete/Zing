package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
)

// seedSealedScenario seeds ticketID (seedQueuedTicket's ticketID) with one
// sealed scenario artifact named id, through insertScenarioArtifact
// (planning_reads_test.go).
func seedSealedScenario(t *testing.T, s *Store, ticketID int64, id string) { //nolint:unparam // every test in this file today seeds "s1"; kept as a parameter for a future test that seeds a second scenario
	t.Helper()
	at := time.Now().UTC().Truncate(time.Second)
	insertScenarioArtifact(t, s, ticketID, nil, id, &at)
}

// ownerEditScenarioEvents returns every owner_edit event on ticketID.
func ownerEditScenarioEvents(t *testing.T, s *Store, ticketID int64) []MessageRow {
	t.Helper()
	rows, err := s.Events(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{})
	if err != nil {
		t.Fatalf("Events(owner_edit): %v", err)
	}
	return rows
}

// scenarioCheck reads back ticketID's scenario id's check_cmd field.
func scenarioCheck(t *testing.T, s *Store, ticketID int64, id string) string {
	t.Helper()
	var payload []byte
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT payload FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND json_extract(payload, '$.id') = ?`,
		ticketID, id,
	).Scan(&payload); err != nil {
		t.Fatalf("read back scenario %s: %v", id, err)
	}
	var sc response.Scenario
	if err := json.Unmarshal(payload, &sc); err != nil {
		t.Fatalf("unmarshal scenario %s: %v", id, err)
	}
	return sc.Check
}

// TestOwnerEdit_ScenarioCheckRewritesPayloadAndWritesEvent proves the
// store seam of the ticket's "done when" scenario: the artifact's check_cmd
// rewrites, exactly one owner_edit event is written, and its body equals
// OwnerEditLine's sentence.
func TestOwnerEdit_ScenarioCheckRewritesPayloadAndWritesEvent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID, "s1")

	if err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit,
		Check: new("go test ./new"),
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}

	if got := scenarioCheck(t, s, ticketID, "s1"); got != "go test ./new" {
		t.Errorf("check_cmd = %q, want %q", got, "go test ./new")
	}

	events := ownerEditScenarioEvents(t, s, ticketID)
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
	if refusal.Code != ownerEditCodeNotSealed {
		t.Errorf("code = %q, want %q", refusal.Code, ownerEditCodeNotSealed)
	}

	err = s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s9", Action: OwnerEditActionEdit, Check: new("x"),
	})
	refusal, ok = errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(missing s9) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != ownerEditCodeNotFound {
		t.Errorf("code = %q, want %q", refusal.Code, ownerEditCodeNotFound)
	}
}

// TestOwnerEdit_RefusesFieldNotAllowedForTarget proves checkOwnerEditShape's
// three bad_request shapes: a field the target does not allow, drop on a
// target that is not plan_task, and an edit with no fields set.
func TestOwnerEdit_RefusesFieldNotAllowedForTarget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID, "s1")

	tests := []struct {
		name string
		req  OwnerEditRequest
	}{
		{"text field on scenario", OwnerEditRequest{TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit, Text: new("x")}},
		{"drop on scenario", OwnerEditRequest{TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionDrop}},
		{"edit with no fields", OwnerEditRequest{TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := s.OwnerEdit(t.Context(), tc.req)
			refusal, ok := errors.AsType[*OwnerEditError](err)
			if !ok {
				t.Fatalf("OwnerEdit error = %v (%T), want *OwnerEditError", err, err)
			}
			if refusal.Code != ownerEditCodeBadRequest {
				t.Errorf("code = %q, want %q", refusal.Code, ownerEditCodeBadRequest)
			}
		})
	}
}

// TestOwnerEdit_RefusesWhileClaimed proves a scenario edit is refused
// claimed while a run holds the ticket's claim, and that the payload does
// not change.
func TestOwnerEdit_RefusesWhileClaimed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID, "s1")
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
	if refusal.Code != ownerEditCodeClaimed {
		t.Errorf("code = %q, want %q", refusal.Code, ownerEditCodeClaimed)
	}
	if got := scenarioCheck(t, s, ticketID, "s1"); got != before {
		t.Errorf("check_cmd changed to %q, want unchanged %q", got, before)
	}
	if n, err := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); err != nil || n != 0 {
		t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
	}
}

// TestOwnerEdit_RefusesSchemaBreakingEdit proves an edit that would blank a
// required scenario field is refused invalid, and the payload is unchanged.
func TestOwnerEdit_RefusesSchemaBreakingEdit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")
	seedSealedScenario(t, s, ticketID, "s1")

	err := s.OwnerEdit(t.Context(), OwnerEditRequest{
		TicketID: ticketID, Target: OwnerEditScenario, Ref: "s1", Action: OwnerEditActionEdit, Given: new(""),
	})
	refusal, ok := errors.AsType[*OwnerEditError](err)
	if !ok {
		t.Fatalf("OwnerEdit(blank given) error = %v (%T), want *OwnerEditError", err, err)
	}
	if refusal.Code != ownerEditCodeInvalid {
		t.Errorf("code = %q, want %q", refusal.Code, ownerEditCodeInvalid)
	}
	if n, err := s.CountEvents(t.Context(), ticketID, EventKindOwnerEdit, EventFilter{}); err != nil || n != 0 {
		t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
	}
}
