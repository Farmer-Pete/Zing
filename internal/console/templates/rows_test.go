package templates

import (
	"strings"
	"testing"

	"zing/internal/store"
)

// TestTicketRowsCarryThreadNav proves every ticket row -- Recent's,
// Project's and the Inbox card's top line -- opens its thread on click,
// through the same zing-nav dispatch #nav's threadLink uses, rather than
// sitting as inert markup a mouse can't reach.
func TestTicketRowsCarryThreadNav(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{ID: 7, TrackerRef: "7", Title: "Fix the thing", State: "planning"}
	wantClick := `data-on:click__prevent="document.getElementById(&#39;stream-ctl&#39;).dispatchEvent(new CustomEvent(&#39;zing-nav&#39;,{detail:{view:&#39;thread&#39;,open:7,project:0}}))"`

	t.Run("Recent row", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := Recent([]store.Ticket{ticket}).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Recent.Render: %v", err)
		}
		got := sb.String()
		if !strings.Contains(got, wantClick) {
			t.Errorf("Recent row missing thread nav click handler; got:\n%s", got)
		}
		if !strings.Contains(got, `<a href="#" class="ticket-row"`) {
			t.Errorf("Recent row is not a ticket-row link; got:\n%s", got)
		}
	})

	t.Run("Project row", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := Project(1, []store.Ticket{ticket}, nil).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Project.Render: %v", err)
		}
		got := sb.String()
		if !strings.Contains(got, wantClick) {
			t.Errorf("Project row missing thread nav click handler; got:\n%s", got)
		}
		if !strings.Contains(got, `<a href="#" class="ticket-row"`) {
			t.Errorf("Project row is not a ticket-row link; got:\n%s", got)
		}
	})

	t.Run("Inbox card", func(t *testing.T) {
		t.Parallel()
		groups := []InboxGroup{
			{ProjectName: "demo", Items: []store.InboxItem{{Ticket: ticket}}},
		}
		var sb strings.Builder
		if err := Inbox(groups).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Inbox.Render: %v", err)
		}
		got := sb.String()
		if !strings.Contains(got, wantClick) {
			t.Errorf("Inbox card missing thread nav click handler; got:\n%s", got)
		}
		if !strings.Contains(got, `<a href="#" class="ib-thread-row"`) {
			t.Errorf("Inbox card row is not an ib-thread-row link; got:\n%s", got)
		}
	})
}
