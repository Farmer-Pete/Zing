package console_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/store"
)

// otherProject is a second project, distinct from testProject, so the
// Inbox grouping test can prove groups cluster by project rather than by
// insertion order.
var otherProject = store.Project{
	Name: "other", RepoURL: "https://github.com/x/other", LocalPath: "/tmp/other", Tracker: "github",
}

// mainFrame opens one /stream connection for (view, open, project), reads
// its three initial frames, and returns just the #main one: the seam every
// test in this file reads a view's rendered content through (design
// section 11, "views": "real store").
func mainFrame(t *testing.T, base, view string, open, project int64) string {
	t.Helper()
	resp, r, cancel := openStream(t, base, view, open, project)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _ := readInitialFrames(t, r)
	return main
}

// mustIndex fails the test unless needle appears in haystack, and returns
// its position so callers can compare two needles' relative order.
func mustIndex(t *testing.T, haystack, needle string) int {
	t.Helper()
	i := strings.Index(haystack, needle)
	if i < 0 {
		t.Fatalf("wanted %q in:\n%s", needle, haystack)
	}
	return i
}

// TestInboxGroupsByProjectBlockingFirst proves the Inbox view groups
// InboxItems by project under the heading of each project's
// first-encountered item, and that within a group the store's own
// blocking-first, newest-message-id-first order survives (design section
// 6.5, 7.2).
//
// Two blocking tickets (one per project) and one unread-only ticket are
// seeded so that store.InboxItems' own order (blocking first, then newest
// message id descending) interleaves the two projects: ticketB1 (project
// "other") gets the newer question, so it sorts before ticketA1 (project
// "acme"), and ticketA2 (project "acme", unread only) sorts last of the
// three but still lands in the "acme" group opened by ticketA1.
func TestInboxGroupsByProjectBlockingFirst(t *testing.T) {
	s := newConsoleTestStore(t)

	ticketA1 := seedTicketIn(t, s, testProject, "acme#1", "A1 blocking")
	seedOpenQuestion(t, s, ticketA1)

	ticketB1 := seedTicketIn(t, s, otherProject, "other#1", "B1 blocking")
	seedOpenQuestion(t, s, ticketB1)

	ticketA2 := seedTicketIn(t, s, testProject, "acme#2", "A2 unread")
	seedUnreadUpdate(t, s, ticketA2, "an update")

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	main := mainFrame(t, srv.URL, "inbox", 0, 0)

	otherHeading := mustIndex(t, main, ">other<")
	acmeHeading := mustIndex(t, main, ">acme<")
	if otherHeading > acmeHeading {
		t.Errorf("expected the \"other\" project group (newer blocking ticket) before \"acme\"; got:\n%s", main)
	}

	a1 := mustIndex(t, main, "A1 blocking")
	a2 := mustIndex(t, main, "A2 unread")
	if a1 > a2 {
		t.Errorf("expected A1 (blocking) before A2 (unread only) within the acme group; got:\n%s", main)
	}
	if a1 < acmeHeading {
		t.Errorf("expected A1 to render under the acme heading, not before it; got:\n%s", main)
	}

	// The exact class="ib-thread" attribute, not a bare substring match:
	// "ib-thread" is also a substring of the row div's class="ib-thread-row",
	// which would otherwise double-count every card.
	if got := strings.Count(main, `class="ib-thread"`); got != 3 {
		t.Errorf("ib-thread card count = %d, want 3; got:\n%s", got, main)
	}
	if got := strings.Count(main, `class="ib-q"`); got != 2 {
		t.Errorf("ib-q row count = %d, want 2 (one per blocking ticket's open question); got:\n%s", got, main)
	}
}

// TestRecentOrdersByNewestMessageThenNoMessageLast proves Recent's order:
// newest message id descending, a ticket with no message sorting last
// (design section 7.2).
func TestRecentOrdersByNewestMessageThenNoMessageLast(t *testing.T) {
	s := newConsoleTestStore(t)

	seedTicket(t, s, "r#1", "Ticket X no messages")
	ticketY := seedTicket(t, s, "r#2", "Ticket Y older message")
	seedStateMessage(t, s, ticketY, "queued", "planning", "start Y")
	ticketZ := seedTicket(t, s, "r#3", "Ticket Z newer message")
	seedStateMessage(t, s, ticketZ, "queued", "planning", "start Z")

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	main := mainFrame(t, srv.URL, "recent", 0, 0)

	z := mustIndex(t, main, "Ticket Z newer message")
	y := mustIndex(t, main, "Ticket Y older message")
	x := mustIndex(t, main, "Ticket X no messages")
	if z >= y || y >= x {
		t.Errorf("expected order Z, Y, X (newest message first, no-message ticket last); got:\n%s", main)
	}
}

// TestFeedOrdersNewestMessageFirst proves Feed renders the newest messages
// across every ticket, newest first by id (design section 7.2).
func TestFeedOrdersNewestMessageFirst(t *testing.T) {
	s := newConsoleTestStore(t)

	ticketID := seedTicket(t, s, "f#1", "Feed ticket")
	seedUnreadUpdate(t, s, ticketID, "first update")
	seedUnreadUpdate(t, s, ticketID, "second update")

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	main := mainFrame(t, srv.URL, "feed", 0, 0)

	second := mustIndex(t, main, "second update")
	first := mustIndex(t, main, "first update")
	if second > first {
		t.Errorf("expected the second (newer) transition before the first; got:\n%s", main)
	}
}

// TestProjectScopesAndOrdersByTrackerRef proves Project shows only the
// requested project's tickets, ordered by tracker_ref then id, regardless
// of insertion order (design section 6.5, 7.2), and that a ticket from a
// different project never appears.
func TestProjectScopesAndOrdersByTrackerRef(t *testing.T) {
	s := newConsoleTestStore(t)

	// Inserted out of tracker_ref order (b before a) so the assertion below
	// proves the view sorts by tracker_ref, not by insertion or id order.
	seedTicketIn(t, s, testProject, "p#b", "Project ticket B")
	seedTicketIn(t, s, testProject, "p#a", "Project ticket A")
	seedTicketIn(t, s, otherProject, "p#z", "Other project's ticket")

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	projects, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var projectID int64
	for _, p := range projects {
		if p.Name == testProject.Name {
			projectID = p.ID
		}
	}
	if projectID == 0 {
		t.Fatalf("could not find project id for %q among %+v", testProject.Name, projects)
	}

	main := mainFrame(t, srv.URL, "project", 0, projectID)

	a := mustIndex(t, main, "Project ticket A")
	b := mustIndex(t, main, "Project ticket B")
	if a > b {
		t.Errorf("expected tracker_ref order (p#a before p#b); got:\n%s", main)
	}
	if strings.Contains(main, "Other project's ticket") {
		t.Errorf("Project view leaked a ticket from a different project; got:\n%s", main)
	}
}

// TestThreadRendersMessagesReadOnly proves the Thread view renders a
// ticket's messages in id order -- a state separator, a plain row for
// every other type, and a question as a read-only group -- with no
// interactive chip or composer markup (design section 6.6, Task 3 scope).
func TestThreadRendersMessagesReadOnly(t *testing.T) {
	s := newConsoleTestStore(t)

	ticketID := seedTicket(t, s, "t#1", "Thread ticket")
	seedStateMessage(t, s, ticketID, "queued", "planning", "picked up")
	seedUnreadUpdate(t, s, ticketID, "working on it")
	seedOpenQuestion(t, s, ticketID)

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if !strings.Contains(main, "Thread ticket") {
		t.Errorf("thread frame missing the ticket title; got:\n%s", main)
	}
	if !strings.Contains(main, "state-separator") || !strings.Contains(main, "queued -&gt; planning") {
		t.Errorf("thread frame missing the state separator; got:\n%s", main)
	}
	if !strings.Contains(main, "working on it") {
		t.Errorf("thread frame missing the update row's body; got:\n%s", main)
	}
	if !strings.Contains(main, "How should the greeting read?") {
		t.Errorf("thread frame missing the question's title; got:\n%s", main)
	}
	if !strings.Contains(main, "Pick the greeting style for GET /hello.") {
		t.Errorf("thread frame missing the question's body; got:\n%s", main)
	}
	if !strings.Contains(main, "Plain hello") || !strings.Contains(main, "hello, world") {
		t.Errorf("thread frame missing both option labels; got:\n%s", main)
	}
	if !strings.Contains(main, "waiting on you") {
		t.Errorf("thread frame missing the open question's pill label; got:\n%s", main)
	}

	// Read only: no chip button and no composer wiring (design section 6.6,
	// Task 3 scope: "WITHOUT the interactive composer or chips").
	if strings.Contains(main, "$answer") || strings.Contains(main, "@post('/answer')") {
		t.Errorf("thread frame rendered interactive answer controls, want read-only; got:\n%s", main)
	}
	if strings.Contains(main, "<button") {
		t.Errorf("thread frame rendered a button, want read-only; got:\n%s", main)
	}
}

// TestThreadOpenZeroOrMissingRendersEmptyThread proves the Thread view's
// nil-ticket guard: open=0 (no ticket open, including an absent signal,
// which ReadSignals leaves at its zero value) or an id naming no ticket
// both render the empty placeholder rather than erroring (design section
// 6.6, carried over from Package 3's patchThread guard).
func TestThreadOpenZeroOrMissingRendersEmptyThread(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	cases := []struct {
		name string
		open int64
	}{
		{"zero", 0},
		{"missing ticket", 999999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			main := mainFrame(t, srv.URL, "thread", tc.open, 0)
			if !strings.Contains(main, `id="main"`) {
				t.Errorf("GET /stream(view=thread,open=%d) frame missing #main; got:\n%s", tc.open, main)
			}
			if !strings.Contains(main, "Select a ticket.") {
				t.Errorf("GET /stream(view=thread,open=%d) frame missing the empty placeholder; got:\n%s", tc.open, main)
			}
		})
	}
}
