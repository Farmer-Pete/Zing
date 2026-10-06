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
		if err := Project(1, []store.Ticket{ticket}, nil, nil).Render(t.Context(), &sb); err != nil {
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

// testAbandonedState is store.Ticket.State's "abandoned" value, repeated
// across this file's #65 tests (goconst).
const testAbandonedState = "abandoned"

// TestProjectRendersEarlierAttempts proves Project's earlier argument (#65)
// renders a head ticket's older attempts behind an "earlier attempts (N)"
// toggle with a thread link to each, and never shows the raw -abandoned-K
// ref anywhere on the page.
func TestProjectRendersEarlierAttempts(t *testing.T) {
	t.Parallel()

	head := store.Ticket{ID: 9, TrackerRef: "41", Title: "restart me", State: "planning"}
	old := store.Ticket{ID: 4, TrackerRef: "41-abandoned-1", Title: "restart me", State: testAbandonedState}
	earlier := map[int64][]store.Ticket{9: {old}}

	var sb strings.Builder
	if err := Project(1, []store.Ticket{head}, nil, earlier).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Project.Render: %v", err)
	}
	got := sb.String()

	if !strings.Contains(got, "earlier attempts (1)") {
		t.Errorf(`Project missing "earlier attempts (1)"; got:\n%s`, got)
	}
	wantLink := `data-on:click__prevent="document.getElementById(&#39;stream-ctl&#39;).dispatchEvent(new CustomEvent(&#39;zing-nav&#39;,{detail:{view:&#39;thread&#39;,open:4,project:0}}))"`
	if !strings.Contains(got, wantLink) {
		t.Errorf("Project missing a thread link to the earlier attempt (ticket 4); got:\n%s", got)
	}
	if strings.Contains(got, "41-abandoned-1") {
		t.Errorf("Project rendered the raw -abandoned-K ref; got:\n%s", got)
	}
}

// TestProjectStripsSuffixFromHeadRef proves that when the head ticket
// itself still carries a retired ref -- every attempt for that issue is
// retired, so groupAttempts picked the highest-attempt ticket as the head
// -- Project's row for it shows the stripped base, never the raw suffix.
func TestProjectStripsSuffixFromHeadRef(t *testing.T) {
	t.Parallel()

	head := store.Ticket{ID: 6, TrackerRef: "53-abandoned-1", Title: "retired issue", State: testAbandonedState}

	var sb strings.Builder
	if err := Project(1, []store.Ticket{head}, nil, nil).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Project.Render: %v", err)
	}
	got := sb.String()

	if !strings.Contains(got, `<span class="ref">53</span>`) {
		t.Errorf("Project row ref span = want 53; got:\n%s", got)
	}
	if strings.Contains(got, "53-abandoned-1") {
		t.Errorf("Project rendered the raw -abandoned-K ref; got:\n%s", got)
	}
}

// TestInboxStripsSuffixFromRef proves the Inbox card's ref span (#65) shows
// a retired ticket's base ref, never the raw -abandoned-K suffix.
func TestInboxStripsSuffixFromRef(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{ID: 6, TrackerRef: "53-abandoned-1", Title: "retired issue", State: testAbandonedState}
	groups := []InboxGroup{
		{ProjectName: "demo", Items: []store.InboxItem{{Ticket: ticket}}},
	}

	var sb strings.Builder
	if err := Inbox(groups).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Inbox.Render: %v", err)
	}
	got := sb.String()

	if !strings.Contains(got, `<span class="ref">53</span>`) {
		t.Errorf("Inbox card ref span = want 53; got:\n%s", got)
	}
	if strings.Contains(got, "53-abandoned-1") {
		t.Errorf("Inbox rendered the raw -abandoned-K ref; got:\n%s", got)
	}
}
