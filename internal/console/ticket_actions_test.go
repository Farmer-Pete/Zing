// ticket_actions_test.go: POST /tickets/{id}/abandon route tests (#65),
// driven against a real store and a real httptest server, the same
// boundary owner_edit_test.go already exercises for POST /tickets/{id}/edit.
package console_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"zing/internal/bus"
)

// abandonPath builds POST /tickets/{id}/abandon's path.
func abandonPath(ticketID int64) string {
	return "/tickets/" + strconv.FormatInt(ticketID, 10) + "/abandon"
}

// TestAbandonRoute_AbandonsAQueuedTicket proves POST /tickets/{id}/abandon
// moves an unclaimed queued ticket to abandoned and answers 204, leaving its
// tracker_ref unchanged (task 3's own named test; retire-on-restart is a
// later task).
func TestAbandonRoute_AbandonsAQueuedTicket(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	ticketID := seedTicket(t, s, "1", "fix the bug")

	resp := doRequest(t, mutationRequest(t, srv, abandonPath(ticketID), "{}"))
	if resp.StatusCode != http.StatusNoContent {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.State != "abandoned" {
		t.Errorf("state = %q, want abandoned", ticket.State)
	}
	if ticket.TrackerRef != "1" {
		t.Errorf("tracker_ref = %q, want unchanged 1", ticket.TrackerRef)
	}
}

// TestAbandonRoute_ClaimedTicketIs409 proves a claimed ticket is refused
// with store.AbandonClaimedReason as the exact body, and that the refusal
// changes nothing.
func TestAbandonRoute_ClaimedTicketIs409(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	ticketID := seedTicket(t, s, "1", "fix the bug")
	claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatalf("Claim returned false, want true")
	}

	resp := doRequest(t, mutationRequest(t, srv, abandonPath(ticketID), "{}"))
	if resp.StatusCode != http.StatusConflict {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body != "A run holds this ticket; try again when it finishes." {
		t.Errorf("body = %q, want the claim sentence", body)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.State != "queued" {
		t.Errorf("state = %q, want unchanged queued", ticket.State)
	}
}

// TestAbandonRoute_AlreadyAbandonedIs409 proves a second abandon on the same
// ticket is refused as terminal, naming the ticket's current state.
func TestAbandonRoute_AlreadyAbandonedIs409(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	ticketID := seedTicket(t, s, "1", "fix the bug")

	first := doRequest(t, mutationRequest(t, srv, abandonPath(ticketID), "{}"))
	if first.StatusCode != http.StatusNoContent {
		_ = first.Body.Close()
		t.Fatalf("first abandon status = %d, want 204", first.StatusCode)
	}
	_ = first.Body.Close()

	second := doRequest(t, mutationRequest(t, srv, abandonPath(ticketID), "{}"))
	if second.StatusCode != http.StatusConflict {
		_ = second.Body.Close()
		t.Fatalf("second abandon status = %d, want 409", second.StatusCode)
	}
	body := readBody(t, second)
	want := "ticket " + strconv.FormatInt(ticketID, 10) + " is already abandoned"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}
