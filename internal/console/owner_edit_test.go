// owner_edit_test.go: POST /tickets/{id}/edit route tests (#41), driven
// against a real store and a real httptest server, the same boundary
// pickup_test.go already exercises for POST /projects/{id}/pickup.
package console_test

import (
	"encoding/json"
	"net/http"
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
// ticketID, carrying no run_id, the legacy-but-still-sealed shape this
// route's refusal and acceptance paths both need (store.OwnerEdit's
// editScenarioTx looks up by ticket_id and the payload's own id, not by
// run_id).
func seedSealedScenarioArtifact(t *testing.T, s *store.Store, ticketID int64, sc response.Scenario) {
	t.Helper()
	payload, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal scenario %s: %v", sc.ID, err)
	}
	sealedAt := time.Now().UTC()
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, Type: testArtifactTypeScenario, Payload: payload, SealedAt: &sealedAt,
	}); err != nil {
		t.Fatalf("InsertArtifact(sealed scenario %s): %v", sc.ID, err)
	}
}

// readScenarioCheck reads back ticketID's scenario id's check_cmd field,
// through the exported AllScenarios read.
func readScenarioCheck(t *testing.T, s *store.Store, ticketID int64, id string) string {
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
		if sc.ID == id {
			return sc.Check
		}
	}
	t.Fatalf("no scenario %s on ticket %d", id, ticketID)
	return ""
}

// TestOwnerEditRoute_EditsSealedScenarioCheck proves the done-when test:
// posting a check edit for a sealed scenario rewrites its payload and
// writes exactly one owner_edit event holding the old and new check.
func TestOwnerEditRoute_EditsSealedScenarioCheck(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "1", "fix the bug")
	seedSealedScenarioArtifact(t, s, ticketID, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Check: testOldCheck, Given: "g", When: "w", Then: "t",
	})
	seedSealedScenarioArtifact(t, s, ticketID, response.Scenario{
		ID: "s2", Kind: response.ScenarioKindBehavior, Check: "go test ./other", Given: "g", When: "w", Then: "t",
	})

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID), `{"target":"scenario","ref":"s1","action":"edit","check":"`+testNewCheck+`"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %q", resp.StatusCode, readBody(t, resp))
	}

	if got := readScenarioCheck(t, s, ticketID, "s1"); got != testNewCheck {
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
	seedSealedScenarioArtifact(t, s, ticketID, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Check: testOldCheck, Given: "g", When: "w", Then: "t",
	})

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID), `{"target":"scenario","ref":"s1","action":"edit","given":""}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %q", resp.StatusCode, readBody(t, resp))
	}

	if got := readScenarioCheck(t, s, ticketID, "s1"); got != testOldCheck {
		t.Errorf("s1 check_cmd = %q, want unchanged %q", got, testOldCheck)
	}
	if n, err := s.CountEvents(t.Context(), ticketID, store.EventKindOwnerEdit, store.EventFilter{}); err != nil || n != 0 {
		t.Errorf("owner_edit events = %d (err %v), want 0", n, err)
	}
}

// TestOwnerEditRoute_RefusesClaimedTicket proves an edit is refused 409
// with the exact claimed text while a run holds the ticket's claim, and
// that the payload is unchanged.
func TestOwnerEditRoute_RefusesClaimedTicket(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "1", "fix the bug")
	seedSealedScenarioArtifact(t, s, ticketID, response.Scenario{
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

	if got := readScenarioCheck(t, s, ticketID, "s1"); got != testOldCheck {
		t.Errorf("s1 check_cmd = %q, want unchanged %q", got, testOldCheck)
	}
}
