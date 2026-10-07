package dispatch_test

import (
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/dispatch"
)

// TestDispatcherTickRefreshesTicketText proves serve's own wiring (#98,
// task 5): runAndCommit sets Deps.Source to the dispatcher itself, and
// Dispatcher.IssueText resolves the binding for a real planning tick's
// refreshTicket call. A comment posted on the tracker after the ticket has
// reached planning reaches the store on the next tick, as a changed
// OwnerComments and a refresh message, through nothing but a real
// dispatch.Tick over the checked-in fixture tracker.
func TestDispatcherTickRefreshesTicketText(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: "fixture-user"}}

	tr := newFixtureTracker(t)
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	var ticketID int64
	const maxTicks = 5
	for i := range maxTicks {
		if err := d.Tick(t.Context()); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
		ticket, found, err := s.TicketByRef(t.Context(), projectID, testFixtureRef)
		if err != nil {
			t.Fatalf("TicketByRef: %v", err)
		}
		if found {
			ticketID = ticket.ID
			if ticket.State == testStatePlanning {
				break
			}
		}
	}
	if ticketID == 0 {
		t.Fatalf("ticket %s never appeared after %d ticks", testFixtureRef, maxTicks)
	}
	if ticket := getTicket(t, s, ticketID); ticket.State != testStatePlanning {
		t.Fatalf("ticket state = %q, want %q after %d ticks", ticket.State, testStatePlanning, maxTicks)
	}

	if err := tr.Comment(t.Context(), testProject.Name, testFixtureRef, "Use serve."); err != nil {
		t.Fatalf("Comment: %v", err)
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (refresh): %v", err)
	}

	final := getTicket(t, s, ticketID)
	wantComments := "Comment by fixture-user:\nUse serve."
	if final.OwnerComments != wantComments {
		t.Errorf("OwnerComments = %q, want %q", final.OwnerComments, wantComments)
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	found := false
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, "ticket refreshed from tracker") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("messages = %+v, want one starting with %q", msgs, "ticket refreshed from tracker")
	}
}
