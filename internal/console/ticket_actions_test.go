// ticket_actions_test.go: POST /tickets/{id}/abandon route tests (#65),
// driven against a real store and a real httptest server, the same
// boundary owner_edit_test.go already exercises for POST /tickets/{id}/edit.
package console_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/tracker"
)

// testTicketActionsAbandoned and testTicketActionsFreshBody are local to
// this file (not console_test.go's shared block) so a second use within a
// single test here does not read as a repeated literal to goconst, without
// adding a package-wide constant only this file would use.
const (
	testTicketActionsAbandoned = "abandoned"
	testTicketActionsFreshBody = "do it again"
)

// abandonPath builds POST /tickets/{id}/abandon's path.
func abandonPath(ticketID int64) string {
	return "/tickets/" + strconv.FormatInt(ticketID, 10) + "/abandon"
}

// restartPath builds POST /tickets/{id}/restart's path.
func restartPath(ticketID int64) string {
	return "/tickets/" + strconv.FormatInt(ticketID, 10) + "/restart"
}

// restartResponse decodes POST /tickets/{id}/restart's 200 body,
// {"ticket":<new id>}.
func restartResponse(t *testing.T, resp *http.Response) int64 {
	t.Helper()
	var body struct {
		Ticket int64 `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode restart response: %v", err)
	}
	_ = resp.Body.Close()
	return body.Ticket
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
	if ticket.State != testTicketActionsAbandoned {
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
	if ticket.State != testStateQueued {
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

// TestRestartRoute_NewQueuedTicketOldAbandonedWithHistory proves POST
// /tickets/{id}/restart abandons the old ticket, renames its ref to
// "5-abandoned-1", and inserts a fresh queued ticket for the same issue
// with no messages of its own, keeping the old ticket's history plus the
// restart's own abandon message.
func TestRestartRoute_NewQueuedTicketOldAbandonedWithHistory(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	seedPickupProject(t, s)

	oldID := seedTicket(t, s, "5", "fix the bug")
	seedStateMessage(t, s, oldID, testStateQueued, testPlanningLiteral, "started planning")

	tr := newPickupTestTracker()
	tr.issues["5"] = tracker.Ticket{Ref: "5", Title: "fresh look", Body: testTicketActionsFreshBody}

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)

	resp := doRequest(t, mutationRequest(t, srv, restartPath(oldID), "{}"))
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	newID := restartResponse(t, resp)
	if newID == 0 || newID == oldID {
		t.Fatalf("new ticket id = %d, want a fresh id distinct from %d", newID, oldID)
	}

	newTicket, err := s.GetTicket(t.Context(), newID)
	if err != nil {
		t.Fatalf("GetTicket(new): %v", err)
	}
	if newTicket.TrackerRef != "5" || newTicket.State != testStateQueued {
		t.Errorf("new ticket = %+v, want ref=5 state=queued", newTicket)
	}
	newMsgs, err := s.ListMessages(t.Context(), newID)
	if err != nil {
		t.Fatalf("ListMessages(new): %v", err)
	}
	if len(newMsgs) != 0 {
		t.Errorf("new ticket messages = %d, want 0: %+v", len(newMsgs), newMsgs)
	}

	oldTicket, err := s.GetTicket(t.Context(), oldID)
	if err != nil {
		t.Fatalf("GetTicket(old): %v", err)
	}
	if oldTicket.TrackerRef != "5-abandoned-1" || oldTicket.State != testTicketActionsAbandoned {
		t.Errorf("old ticket = %+v, want ref=5-abandoned-1 state=abandoned", oldTicket)
	}
	oldMsgs, err := s.ListMessages(t.Context(), oldID)
	if err != nil {
		t.Fatalf("ListMessages(old): %v", err)
	}
	if len(oldMsgs) != 2 {
		t.Fatalf("old ticket messages = %d, want 2: %+v", len(oldMsgs), oldMsgs)
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(oldMsgs[1].Payload, &payload); err != nil {
		t.Fatalf("unmarshal abandon message payload: %v", err)
	}
	if payload.Reason != "owner restarted from planning" {
		t.Errorf("abandon message reason = %q, want %q", payload.Reason, "owner restarted from planning")
	}

	comments := tr.recordedComments()
	if len(comments) != 1 {
		t.Fatalf("pickup comments posted = %d, want exactly 1: %+v", len(comments), comments)
	}
	if comments[0].ref != "5" {
		t.Errorf("pickup comment ref = %q, want 5", comments[0].ref)
	}
}

// TestRestartRoute_ClosedIssueLeavesOldTicketAlone proves a restart that
// finds the issue closed refuses with 409 and changes nothing.
func TestRestartRoute_ClosedIssueLeavesOldTicketAlone(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	ticketID := seedTicket(t, s, "5", "fix the bug")

	tr := newPickupTestTracker()
	tr.issueErr["5"] = tracker.ErrIssueClosed

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)

	resp := doRequest(t, mutationRequest(t, srv, restartPath(ticketID), "{}"))
	if resp.StatusCode != http.StatusConflict {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body != "issue #5 is closed" {
		t.Errorf("body = %q, want %q", body, "issue #5 is closed")
	}

	tickets, err := s.TicketsByProject(t.Context(), projectID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 1 || tickets[0].State != testStateQueued || tickets[0].TrackerRef != "5" {
		t.Fatalf("tickets = %+v, want one queued ticket at ref 5", tickets)
	}
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages = %d, want 0", len(msgs))
	}
}

// TestRestartRoute_ClaimedTicketIs409 proves a claimed ticket is refused
// with store.AbandonClaimedReason before any tracker call.
func TestRestartRoute_ClaimedTicketIs409(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	seedPickupProject(t, s)

	ticketID := seedTicket(t, s, "5", "fix the bug")
	claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatalf("Claim returned false, want true")
	}

	tr := newPickupTestTracker()
	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)

	resp := doRequest(t, mutationRequest(t, srv, restartPath(ticketID), "{}"))
	if resp.StatusCode != http.StatusConflict {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body != "A run holds this ticket; try again when it finishes." {
		t.Errorf("body = %q, want the claim sentence", body)
	}

	tickets, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(tickets))
	}
	if len(tr.recordedComments()) != 0 {
		t.Errorf("comments posted = %d, want 0", len(tr.recordedComments()))
	}
}

// TestRestartRoute_AbandonedTicketWithLiveSuccessorIs409 proves a second
// restart of the same abandoned ticket, once a live successor exists,
// refuses naming the successor and leaves the ticket count unchanged.
func TestRestartRoute_AbandonedTicketWithLiveSuccessorIs409(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	seedPickupProject(t, s)

	ticketA := seedTicket(t, s, "5", "fix the bug")

	tr := newPickupTestTracker()
	tr.issues["5"] = tracker.Ticket{Ref: "5", Title: "fresh look", Body: testTicketActionsFreshBody}

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)

	first := doRequest(t, mutationRequest(t, srv, restartPath(ticketA), "{}"))
	if first.StatusCode != http.StatusOK {
		_ = first.Body.Close()
		t.Fatalf("first restart status = %d, want 200", first.StatusCode)
	}
	ticketB := restartResponse(t, first)

	second := doRequest(t, mutationRequest(t, srv, restartPath(ticketA), "{}"))
	if second.StatusCode != http.StatusConflict {
		_ = second.Body.Close()
		t.Fatalf("second restart status = %d, want 409", second.StatusCode)
	}
	body := readBody(t, second)
	want := "issue #5 is already ticket " + strconv.FormatInt(ticketB, 10)
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}

	tickets, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("tickets = %d, want 2", len(tickets))
	}
}
