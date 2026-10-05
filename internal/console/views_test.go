package console_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/response"
	"zing/internal/store"
)

// otherProject is a second project, distinct from testProject, so the
// Inbox grouping test can prove groups cluster by project rather than by
// insertion order.
var otherProject = store.Project{
	Name: "other", RepoURL: "https://github.com/x/other", LocalPath: "/tmp/other", Tracker: testTrackerGitHub,
}

// mainFrame opens one /stream connection for (view, open, project), reads
// its four initial frames, and returns just the #main one: the seam every
// test in this file reads a view's rendered content through (design
// section 11, "views": "real store").
func mainFrame(t *testing.T, base, view string, open, project int64) string {
	t.Helper()
	resp, r, cancel := openStream(t, base, view, open, project)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
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
	t.Parallel()
	s := newConsoleTestStore(t)

	ticketA1 := seedTicketIn(t, s, testProject, "acme#1", "A1 blocking")
	seedOpenQuestion(t, s, ticketA1)

	ticketB1 := seedTicketIn(t, s, otherProject, "other#1", "B1 blocking")
	seedOpenQuestion(t, s, ticketB1)

	ticketA2 := seedTicketIn(t, s, testProject, "acme#2", "A2 unread")
	seedUnreadUpdate(t, s, ticketA2, "an update")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

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

// TestNavListsEveryLiveTicket proves the sidebar's new membership rule
// (design section 6.3, 6.8, #106 bug 4): a quiet, non-terminal ticket shows
// in #nav even with no unread message, and a terminal ticket with an unread
// message does not. It also proves navComponent's Debug line carries only
// the count and ticket ids, never a ticket's title.
//
// Not parallel: it calls slog.SetDefault below to capture a log line, which
// swaps the process-wide default logger.
func TestNavListsEveryLiveTicket(t *testing.T) {
	s := newConsoleTestStore(t)

	planMe := seedTicket(t, s, "live#1", "Plan me")
	transitionTicket(t, s, planMe, response.TicketStatePlanning)

	shipped := seedTicket(t, s, "live#2", "Shipped")
	transitionTicket(t, s, shipped, response.TicketStateDone)
	seedUnreadUpdate(t, s, shipped, "an unread update")

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))

	nav := navFrame(t, srv.URL)

	if !strings.Contains(nav, "Plan me") {
		t.Errorf("nav frame missing the quiet planning ticket; got:\n%s", nav)
	}
	if !strings.Contains(nav, `<span class="pill">planning</span>`) {
		t.Errorf("nav frame missing the planning ticket's state pill; got:\n%s", nav)
	}
	if strings.Contains(nav, "Shipped") {
		t.Errorf("nav frame lists the done ticket; got:\n%s", nav)
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, "console: nav live tickets") {
		t.Fatalf("missing the nav live tickets debug line; got:\n%s", logOut)
	}
	if !strings.Contains(logOut, "count=1 ") {
		t.Errorf("nav debug line missing count=1; got:\n%s", logOut)
	}
	wantIDs := fmt.Sprintf("ticket_ids=[%d]", planMe)
	if !strings.Contains(logOut, wantIDs) {
		t.Errorf("nav debug line missing %s; got:\n%s", wantIDs, logOut)
	}
	if strings.Contains(logOut, "Plan me") || strings.Contains(logOut, "Shipped") {
		t.Errorf("nav debug line leaked a ticket title; got:\n%s", logOut)
	}
}

// navFrame opens one /stream connection for the inbox view, reads its
// initial frames, and returns just the #nav one.
func navFrame(t *testing.T, base string) string {
	t.Helper()
	resp, r, cancel := openStream(t, base, "inbox", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	nav, _, _, _ := readInitialFrames(t, r)
	return nav
}

// TestNavOrderStable proves the sidebar's order is stable under the two
// everyday events that used to reshuffle it (#106 bug 5, c8's owner
// decision (a)): a ticket gaining a gate, and a ticket gaining (then
// losing, on open) an unread message. Both tickets start as plain, quiet
// tickets whose refs ("s#1", "s#2") are non-numeric, so issueNumberOrder
// sorts them as text with "s#1" first; the event under test must not move
// "s#2" above "s#1".
func TestNavOrderStable(t *testing.T) {
	t.Parallel()
	t.Run("a gate does not move a row", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)

		seedTicket(t, s, "s#1", "Nav first")
		second := seedTicket(t, s, "s#2", "Nav second")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

		before := navFrame(t, srv.URL)
		firstIdx := mustIndex(t, before, "Nav first")
		secondIdx := mustIndex(t, before, "Nav second")
		if firstIdx > secondIdx {
			t.Fatalf("expected \"Nav first\" before \"Nav second\" before the gate; got:\n%s", before)
		}

		seedOpenQuestion(t, s, second)

		after := navFrame(t, srv.URL)
		firstIdx = mustIndex(t, after, "Nav first")
		secondIdx = mustIndex(t, after, "Nav second")
		if secondIdx < firstIdx {
			t.Errorf("after a gate, \"Nav second\" rendered before \"Nav first\"; got:\n%s", after)
		}
		if !strings.Contains(after, "badge-blocking") {
			t.Errorf("nav frame missing the gated ticket's badge-blocking; got:\n%s", after)
		}
	})

	t.Run("an unread update does not move a row", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)

		seedTicket(t, s, "s#1", "Nav first")
		second := seedTicket(t, s, "s#2", "Nav second")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

		before := navFrame(t, srv.URL)
		firstIdx := mustIndex(t, before, "Nav first")
		secondIdx := mustIndex(t, before, "Nav second")
		if firstIdx > secondIdx {
			t.Fatalf("expected \"Nav first\" before \"Nav second\" before the unread update; got:\n%s", before)
		}

		seedUnreadUpdate(t, s, second, "an update")

		after := navFrame(t, srv.URL)
		firstIdx = mustIndex(t, after, "Nav first")
		secondIdx = mustIndex(t, after, "Nav second")
		if secondIdx < firstIdx {
			t.Errorf("after an unread update, \"Nav second\" rendered before \"Nav first\"; got:\n%s", after)
		}
		if !strings.Contains(after, "badge-unread") {
			t.Errorf("nav frame missing the updated ticket's badge-unread; got:\n%s", after)
		}
	})
}

// TestRecentOrdersByNewestTicketFirst proves Recent's order: newest ticket
// first (ticket id descending), unaffected by which ticket's messages
// arrived most recently (#106 bug 5, c8's owner decision (a)). X, Y, and Z
// are created in that order (X oldest, Z newest), then Y and Z each get a
// state message, and X gets one last -- making X's message the newest if
// the page still sorted by message recency. The order must still be Z, Y,
// X, by ticket id alone.
func TestRecentOrdersByNewestTicketFirst(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)

	ticketX := seedTicket(t, s, "r#1", "Ticket X")
	ticketY := seedTicket(t, s, "r#2", "Ticket Y")
	seedStateMessage(t, s, ticketY, "queued", testPlanningLiteral, "start Y")
	ticketZ := seedTicket(t, s, "r#3", "Ticket Z")
	seedStateMessage(t, s, ticketZ, "queued", testPlanningLiteral, "start Z")
	seedStateMessage(t, s, ticketX, "queued", testPlanningLiteral, "start X")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	main := mainFrame(t, srv.URL, "recent", 0, 0)

	z := mustIndex(t, main, "Ticket Z")
	y := mustIndex(t, main, "Ticket Y")
	x := mustIndex(t, main, "Ticket X")
	if z >= y || y >= x {
		t.Errorf("expected order Z, Y, X (newest ticket first); got:\n%s", main)
	}
}

// TestFeedOrdersNewestMessageFirst proves Feed renders the newest messages
// across every ticket, newest first by id (design section 7.2).
func TestFeedOrdersNewestMessageFirst(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)

	ticketID := seedTicket(t, s, "f#1", "Feed ticket")
	seedUnreadUpdate(t, s, ticketID, "first update")
	seedUnreadUpdate(t, s, ticketID, "second update")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	main := mainFrame(t, srv.URL, "feed", 0, 0)

	second := mustIndex(t, main, "second update")
	first := mustIndex(t, main, "first update")
	if second > first {
		t.Errorf("expected the second (newer) transition before the first; got:\n%s", main)
	}
}

// TestFeedRendersStateAndAnswerContentNotBlank proves the Feed view decodes
// a state row's transition and a sent answer row's chosen option through
// the same displayBody logic the Thread view already used (design section
// 6.5, 6.6; code review fix, PR #16), rather than showing each row's raw
// Body -- always empty for these two types, since a commit never sets a
// state row's Body (the transition lives in Payload) and SaveDraft/
// SendBatch never set an answer row's Body (the choice lives in Payload).
func TestFeedRendersStateAndAnswerContentNotBlank(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)

	ticketID := seedTicket(t, s, "f#3", "Feed decode ticket")
	// A transition other than every other call site's queued->planning, so
	// this fixture does not make seedStateMessage's from/to params look
	// unconditionally hardcodable to golangci-lint's unparam check.
	seedStateMessage(t, s, ticketID, testPlanningLiteral, "building", "start")

	questionID := seedOpenQuestion(t, s, ticketID)
	option := "b"
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID, Option: &option,
	}); err != nil {
		t.Fatalf("SaveDraft(option): %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "feed", 0, 0)

	if !strings.Contains(main, "planning -&gt; building") {
		t.Errorf("feed missing the state row's decoded transition; got:\n%s", main)
	}
	if !strings.Contains(main, "Option: b") {
		t.Errorf("feed missing the sent answer row's decoded option; got:\n%s", main)
	}
}

// TestFeedRendersMarkdown proves the bug fix: raw backticks in the Feed
// (design section 22.7's owner-reported locked-view complaint) --
// displayFeedMessages now runs each row's decoded Body through the same
// Render path the Thread view's own turns use (views.go's FeedRow), so a
// backtick renders as <code>, not a literal backtick.
func TestFeedRendersMarkdown(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "f#4", "Feed markdown ticket")
	seedUnreadUpdate(t, s, ticketID, "run `zing version` to check")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "feed", 0, 0)

	if !strings.Contains(main, "<code>zing version</code>") {
		t.Errorf("feed did not render the backtick span as code; got:\n%s", main)
	}
	if strings.Contains(main, "`zing version`") {
		t.Errorf("feed still shows the raw backticks; got:\n%s", main)
	}
}

// TestProjectScopesAndOrdersByTrackerRef proves Project shows only the
// requested project's tickets, ordered by tracker_ref then id, regardless
// of insertion order (design section 6.5, 7.2), and that a ticket from a
// different project never appears.
func TestProjectScopesAndOrdersByTrackerRef(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)

	// Inserted out of tracker_ref order (b before a) so the assertion below
	// proves the view sorts by tracker_ref, not by insertion or id order.
	seedTicketIn(t, s, testProject, "p#b", "Project ticket B")
	seedTicketIn(t, s, testProject, "p#a", "Project ticket A")
	seedTicketIn(t, s, otherProject, "p#z", "Other project's ticket")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

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

// transitionTicket claims ticketID and commits a transition to state, the
// only way (besides queued at intake) a real ticket reaches a given state
// (design section 6.3's commit path).
func transitionTicket(t *testing.T, s *store.Store, ticketID int64, state response.TicketState) {
	t.Helper()
	const owner = "test-owner"
	expires := time.Now().Add(10 * time.Minute)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatalf("Claim(%d): got false, want true", ticketID)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: string(state), Reason: "test: transition to " + string(state),
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult(%d, %s): %v", ticketID, state, err)
	}
	if !applied {
		t.Fatalf("CommitHandlerResult(%d, %s): applied = false, want true", ticketID, state)
	}
}

// TestProjectShowsLiveFirstAndClosedCollapsed proves the project page puts
// live work first, furthest-along state first, with terminal tickets in a
// collapsed "Closed (N)" section below (design section 6.5, Task 3, #106
// bug 4): #110 reviewing, #65 building, #102 building, #95 queued render
// live, in that order, and #96 done renders inside the collapsed details.
// A second project holding only a live ticket renders no closed section at
// all. It also proves mainComponent's project-sections Debug line carries
// only the project id and the live/closed counts, never a ticket's title.
//
// Not parallel: it calls slog.SetDefault below to capture a log line, which
// swaps the process-wide default logger.
func TestProjectShowsLiveFirstAndClosedCollapsed(t *testing.T) {
	s := newConsoleTestStore(t)

	ticket102 := seedTicketIn(t, s, testProject, "102", "Ticket 102")
	transitionTicket(t, s, ticket102, response.TicketStateBuilding)
	ticket65 := seedTicketIn(t, s, testProject, "65", "Ticket 65")
	transitionTicket(t, s, ticket65, response.TicketStateBuilding)
	ticket110 := seedTicketIn(t, s, testProject, "110", "Ticket 110")
	transitionTicket(t, s, ticket110, response.TicketStateReviewing)
	seedTicketIn(t, s, testProject, "95", "Ticket 95")
	ticket96 := seedTicketIn(t, s, testProject, "96", "Ticket 96")
	transitionTicket(t, s, ticket96, response.TicketStateDone)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))

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

	markers := []string{"Ticket 110", "Ticket 65", "Ticket 102", "Ticket 95", `id="project-closed"`, "Ticket 96"}
	indices := make([]int, len(markers))
	for i, m := range markers {
		indices[i] = mustIndex(t, main, m)
	}
	for i := 1; i < len(indices); i++ {
		if indices[i-1] >= indices[i] {
			t.Errorf("expected %q before %q; got:\n%s", markers[i-1], markers[i], main)
		}
	}

	if !strings.Contains(main, `data-preserve-attr="open"`) {
		t.Errorf("closed details missing data-preserve-attr=\"open\"; got:\n%s", main)
	}
	detailsStart := mustIndex(t, main, "<details")
	detailsTagEnd := strings.Index(main[detailsStart:], ">")
	if detailsTagEnd < 0 {
		t.Fatalf("closed details tag has no closing '>'; got:\n%s", main)
	}
	detailsTag := main[detailsStart : detailsStart+detailsTagEnd]
	withoutPreserveAttr := strings.ReplaceAll(detailsTag, `data-preserve-attr="open"`, "")
	if strings.Contains(withoutPreserveAttr, "open") {
		t.Errorf("closed details should start collapsed (no bare open attribute); got tag:\n%s", detailsTag)
	}
	if !strings.Contains(main, "Closed (1)") {
		t.Errorf("expected summary text \"Closed (1)\"; got:\n%s", main)
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, "console: project sections") {
		t.Fatalf("missing the project sections debug line; got:\n%s", logOut)
	}
	if !strings.Contains(logOut, fmt.Sprintf("project_id=%d", projectID)) {
		t.Errorf("project sections debug line missing project_id=%d; got:\n%s", projectID, logOut)
	}
	if !strings.Contains(logOut, "live=4") || !strings.Contains(logOut, "closed=1") {
		t.Errorf("project sections debug line missing live=4 closed=1; got:\n%s", logOut)
	}
	for _, title := range []string{"Ticket 110", "Ticket 65", "Ticket 102", "Ticket 95", "Ticket 96"} {
		if strings.Contains(logOut, title) {
			t.Errorf("project sections debug line leaked ticket title %q; got:\n%s", title, logOut)
		}
	}

	otherProjectID, err := s.EnsureProject(t.Context(), otherProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	seedTicketIn(t, s, otherProject, "1", "Other project's only ticket")
	otherMain := mainFrame(t, srv.URL, "project", 0, otherProjectID)
	if strings.Contains(otherMain, "project-closed") {
		t.Errorf("a project with no closed tickets should render no project-closed details; got:\n%s", otherMain)
	}
}

// TestThreadRendersMessagesReadOnly proves the Thread view renders a
// ticket's messages in id order -- a state separator, a plain row for
// every other type, and a question as a read-only group -- with no
// interactive chip or composer markup (design section 6.6, Task 3 scope).
// TestThreadRendersMessagesAndInteractiveQuestionControls proves the
// non-question rows still render exactly as Task 3 left them (a plain
// state separator and a plain message row), and that the question's own
// group now carries Task 6's interactive controls -- numbered option chips
// and a free reply input -- over the option kind's data (design section
// 6.6). TestQuestionKindsRenderTheirControls (question_kinds_test.go) is
// the fuller, per-kind version of this; this test's job is only to prove
// the surrounding non-question rows are undisturbed by the switch to an
// interactive question group.
func TestThreadRendersMessagesAndInteractiveQuestionControls(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)

	ticketID := seedTicket(t, s, "t#1", "Thread ticket")
	seedStateMessage(t, s, ticketID, "queued", testPlanningLiteral, "picked up")
	seedUnreadUpdate(t, s, ticketID, "working on it")
	seedOpenQuestion(t, s, ticketID)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

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

	// Interactive: the question kind (an option kind) renders two numbered
	// chips and a free reply input wired to console.js's postDraft contract
	// (design section 6.6, 6.4; Task 6 supersedes Task 3's read-only group).
	if !strings.Contains(main, `data-chip-index="1"`) || !strings.Contains(main, `data-chip-index="2"`) {
		t.Errorf("thread frame missing the question's numbered chips; got:\n%s", main)
	}
	if !strings.Contains(main, `class="reply-input"`) {
		t.Errorf("thread frame missing the question's free reply input; got:\n%s", main)
	}
}

// TestThreadAnsweredAndResolvedQuestionsRenderReadOnly proves an answered or
// resolved question's group drops its active controls -- option chips and
// the free reply input -- while still showing its context and lifecycle
// pill (design section 6.6, 6.7; code review fix, PR #16: questionGroup
// used to render those controls for every question regardless of state).
func TestThreadAnsweredAndResolvedQuestionsRenderReadOnly(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#5", "Thread answered/resolved ticket")

	answeredPayload := []byte(`{"key":"Q1","kind":"question","state":"answered","recommended":"a",` +
		`"options":[{"key":"a","text":"Plain hello"},{"key":"b","text":"hello, world"}]}`)
	answeredState := "answered"
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeQuestion, Author: "zing", State: &answeredState,
		Body: "Answered question\n\nAlready decided.", Payload: answeredPayload,
	}); err != nil {
		t.Fatalf("InsertMessage(answered question): %v", err)
	}

	resolvedState := "resolved" // named once; reused below so this file's literal "resolved" stays under goconst's threshold
	resolvedPayload := []byte(`{"key":"Q2","kind":"question","state":"` + resolvedState + `","recommended":"a",` +
		`"options":[{"key":"a","text":"Plain hello"},{"key":"b","text":"hello, world"}]}`)
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeQuestion, Author: "zing", State: &resolvedState,
		Body: "Resolved question\n\nSettled.", Payload: resolvedPayload,
	}); err != nil {
		t.Fatalf("InsertMessage(resolved question): %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	groups := splitQuestionGroups(t, main)
	for _, tc := range []struct {
		name, title, pill string
	}{
		{answeredState, "Answered question", answeredState}, // questionStateLabel's locked-answered mapping (views.go, bug fix 9)
		{resolvedState, "Resolved question", resolvedState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := findGroup(t, groups, tc.title)
			if strings.Contains(g, "data-chip-index") {
				t.Errorf("%s question still renders option chips; got:\n%s", tc.name, g)
			}
			if strings.Contains(g, `class="reply-input"`) {
				t.Errorf("%s question still renders the free reply input; got:\n%s", tc.name, g)
			}
			if !strings.Contains(g, tc.pill) {
				t.Errorf("%s question missing its %q lifecycle pill; got:\n%s", tc.name, tc.pill, g)
			}
		})
	}
}

// TestThreadGateContextRendersStoredPlan proves the seam Task 8 wires
// (design section 6.9): a gate question's context region renders the
// ticket's stored plan artifact in full, through the same RenderPlan path
// plan_test.go proves field by field, rather than the Task 6 placeholder.
func TestThreadGateContextRendersStoredPlan(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "gate#1", "Gate ticket")
	if err := console.SeedQuestionFixtures(t.Context(), s, ticketID); err != nil {
		t.Fatalf("SeedQuestionFixtures: %v", err)
	}

	payload, err := json.Marshal(fixturePlan())
	if err != nil {
		t.Fatalf("marshal fixture plan: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, Type: testArtifactTypePlan, Payload: payload,
	}); err != nil {
		t.Fatalf("InsertArtifact(plan): %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	groups := splitQuestionGroups(t, main)
	gate := findGroup(t, groups, "Approve the plan?")

	if !strings.Contains(gate, `class="q-context gate-context"`) {
		t.Fatalf("gate group missing its gate-context region; got:\n%s", gate)
	}
	if strings.Contains(gate, "No plan stored for this ticket yet.") {
		t.Errorf("gate context still shows the no-plan placeholder despite a stored plan; got:\n%s", gate)
	}
	for _, want := range []string{
		"<h2>Overview</h2>", "<h2>Design</h2>", "<h2>Delivery</h2>", "<h2>Review</h2>",
		"Ship a plan renderer that drops nothing.", // Overview.Objective
		"<pre class=\"mermaid\">",                  // Design.Shape's mermaid fence
		planMigrationFile,                          // a Migration
	} {
		if !strings.Contains(gate, want) {
			t.Errorf("gate context missing %q from the stored plan; got:\n%s", want, gate)
		}
	}
}

// TestThreadExcludesDraftRows proves the read-only Thread view renders only
// posted messages: a queued-but-unsent draft answer or reply must not
// appear (design section 6.6, 6.7, code review fix 2).
func TestThreadExcludesDraftRows(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#2", "Thread draft ticket")
	questionID := seedOpenQuestion(t, s, ticketID)

	option := "a"
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID, Option: &option,
	}); err != nil {
		t.Fatalf("SaveDraft(option): %v", err)
	}
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, Text: "an undrafted reply",
	}); err != nil {
		t.Fatalf("SaveDraft(text): %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if strings.Contains(main, "an undrafted reply") {
		t.Errorf("thread frame leaked an unsent draft reply's body; got:\n%s", main)
	}
	if strings.Contains(main, `class="message message-answer"`) {
		t.Errorf("thread frame leaked an unsent draft answer row; got:\n%s", main)
	}
}

// TestThreadRendersSentAnswerFromPayload proves a SENT answer row (design
// section 6.6, 6.7, code review fix 2) renders its chosen option out of
// Payload, since AnswerPayload messages never carry a Body: this is the
// "pick then send" path -- SaveDraft's option mode (what a fixed chip
// activation posts), then SendBatch -- rendering something visible, not
// the blank row the bug left behind. As of D31-5, this question (no
// planning run) shows that pick exactly once, as its own locked note: the
// duplicate "Answered:" plus "You:" the owner reported (bug fix 10 used to
// also format the very same answer into a turn) is gone, since an answer
// row is a turn only inside a planning conversation (views.go's
// turnContent).
func TestThreadRendersSentAnswerFromPayload(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#3", "Thread sent answer ticket")
	questionID := seedOpenQuestion(t, s, ticketID)

	option := "b"
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID, Option: &option,
	}); err != nil {
		t.Fatalf("SaveDraft(option): %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if !strings.Contains(main, `class="q-answered"`) || !strings.Contains(main, "Answered:") || !strings.Contains(main, "hello, world") {
		t.Errorf("thread frame missing the sent answer's locked note; got:\n%s", main)
	}
	if strings.Contains(main, "q-turns") {
		t.Errorf("thread frame still shows the sent answer a second time as a turn (the duplicate bug); got:\n%s", main)
	}
	if strings.Contains(main, "message-answer") {
		t.Errorf("thread frame still renders the sent answer as its own detached card; got:\n%s", main)
	}
}

// TestThreadRendersDraftReplyAndHint proves the bug fix at the root of the
// owner's report: a saved-but-unsent draft reply against an open question
// renders back as the reply box's own value (not a blank box the owner
// reads as "the text disappeared"), and the thread shows both the one-line
// "draft saved" banner (only once a draft exists) and the per-box "saved as
// a draft" hint, since sending is keyboard-only (Q31) and nothing on screen
// said so before this fix.
func TestThreadRendersDraftReplyAndHint(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#6", "Thread draft reply ticket")
	questionID := seedOpenQuestion(t, s, ticketID)

	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID, Text: "here is my reply",
	}); err != nil {
		t.Fatalf("SaveDraft(text): %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if !strings.Contains(main, `value="here is my reply"`) {
		t.Errorf("thread frame missing the draft reply as the reply box's value; got:\n%s", main)
	}
	if !strings.Contains(main, `class="draft-banner"`) {
		t.Errorf("thread frame missing the draft banner while a draft exists; got:\n%s", main)
	}
	if !strings.Contains(main, "Draft saved.") || !strings.Contains(main, "sends all drafts.") {
		t.Errorf("draft banner missing its expected wording; got:\n%s", main)
	}
	if !strings.Contains(main, `class="reply-hint"`) || !strings.Contains(main, "Saved as a draft.") {
		t.Errorf("thread frame missing the per-box \"saved as a draft\" hint; got:\n%s", main)
	}
}

// TestThreadNoDraftShowsNoBanner proves the banner is conditional (design
// section 6.7, bug fix): an open question with nothing drafted against it
// shows neither the banner nor a pre-filled reply box, so a ticket with no
// draft in progress reads exactly as it did before this fix.
func TestThreadNoDraftShowsNoBanner(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#7", "Thread no-draft ticket")
	seedOpenQuestion(t, s, ticketID)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if strings.Contains(main, `class="draft-banner"`) {
		t.Errorf("thread frame shows the draft banner with no draft in progress; got:\n%s", main)
	}
	if !strings.Contains(main, `value=""`) {
		t.Errorf("thread frame's reply box should start empty with no draft; got:\n%s", main)
	}
}

// TestThreadRendersDraftOptionPicked proves a saved-but-unsent option pick
// renders its chip "picked" (design section 6.6, 6.7, bug fix): the same
// client-visible state pickToggleExpr gives a chip on a fresh click, so a
// pick made, then abandoned mid-session (navigated away, or the tab
// reloaded) before Enter/the send chord still shows what was chosen.
func TestThreadRendersDraftOptionPicked(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#8", "Thread draft option ticket")
	questionID := seedOpenQuestion(t, s, ticketID)

	option := "b"
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID, Option: &option,
	}); err != nil {
		t.Fatalf("SaveDraft(option): %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if !strings.Contains(main, `class="chip picked" aria-pressed="true" data-chip-index="2"`) {
		t.Errorf("thread frame missing the drafted option's chip rendered picked; got:\n%s", main)
	}
	if strings.Contains(main, `class="chip picked" aria-pressed="true" data-chip-index="1"`) {
		t.Errorf("thread frame renders the undrafted chip picked; got:\n%s", main)
	}
}

// perimeterQuestionPayload builds a minimal, schema-valid perimeter
// question payload with two file items, for the item-kind draft tests
// below: hand-built rather than through console.SeedQuestionFixtures, so
// the test controls the question's own message id directly instead of
// reading it back out of ListMessages.
const perimeterQuestionPayload = `{"key":"Q1","kind":"perimeter","state":"open","recommended":"Accept every file",` +
	`"options":[],"items":[{"ref":"a.go","text":"Builder: x Change: y"},{"ref":"b.go","text":"Builder: x Change: y"}]}`

// seedOpenPerimeterQuestion inserts one open perimeter-kind question message
// on ticketID with two items, "a.go" and "b.go" (perimeterQuestionPayload),
// and returns its message id.
func seedOpenPerimeterQuestion(t *testing.T, s *store.Store, ticketID int64) int64 {
	t.Helper()
	openState := testQuestionStateOpen
	id, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeQuestion, Author: testAuthorZing, State: &openState,
		Body:    "Confirm the file perimeter\n\nReview each file.",
		Payload: []byte(perimeterQuestionPayload),
	})
	if err != nil {
		t.Fatalf("InsertMessage(perimeter question): %v", err)
	}
	return id
}

// TestThreadRendersDraftItemsPicked proves an item-kind question's own
// saved-but-unsent decisions render their buttons "picked" (design section
// 6.6, 9.2, bug fix), the same way optionChips' own draft does, and that a
// ref with nothing drafted against it renders no control picked.
func TestThreadRendersDraftItemsPicked(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#9", "Thread draft items ticket")
	questionID := seedOpenPerimeterQuestion(t, s, ticketID)

	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID,
		Item: &store.ItemDecision{Ref: "a.go", Decision: response.DecisionAccept},
	}); err != nil {
		t.Fatalf("SaveDraft(item): %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if !strings.Contains(main, `class="decision picked" aria-pressed="true" data-draft-ticket="`+strconv.FormatInt(ticketID, 10)+
		`" data-draft-question="`+strconv.FormatInt(questionID, 10)+`" data-item-ref="a.go" data-decision="accept"`) {
		t.Errorf("thread frame missing a.go's drafted accept decision rendered picked; got:\n%s", main)
	}
	if strings.Contains(main, `class="decision picked"`+` aria-pressed="true" data-draft-ticket="`+strconv.FormatInt(ticketID, 10)+
		`" data-draft-question="`+strconv.FormatInt(questionID, 10)+`" data-item-ref="b.go"`) {
		t.Errorf("thread frame renders b.go (no draft against it) picked; got:\n%s", main)
	}
}

// TestThreadDraftItemClearsAfterSend proves a sent draft stops rendering as
// picked on the next render (design section 6.7, bug fix): a.go's decision
// is drafted, then sent via SendBatch (the perimeter question stays open,
// since only one of its two items is decided); b.go is then drafted but
// left unsent. a.go's control must no longer show picked (its draft row is
// now state=sent, not state=draft), while b.go's still-unsent draft does.
func TestThreadDraftItemClearsAfterSend(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#10", "Thread draft clears after send ticket")
	questionID := seedOpenPerimeterQuestion(t, s, ticketID)

	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID,
		Item: &store.ItemDecision{Ref: "a.go", Decision: response.DecisionAccept},
	}); err != nil {
		t.Fatalf("SaveDraft(a.go accept): %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, QuestionID: &questionID,
		Item: &store.ItemDecision{Ref: "b.go", Decision: response.DecisionReject},
	}); err != nil {
		t.Fatalf("SaveDraft(b.go reject): %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	groups := splitQuestionGroups(t, main)
	g := findGroup(t, groups, "Confirm the file perimeter")

	if strings.Contains(g, `data-item-ref="a.go" data-decision="accept" data-on:click`) &&
		strings.Contains(g, `class="decision picked" aria-pressed="true" data-draft-ticket="`+strconv.FormatInt(ticketID, 10)+
			`" data-draft-question="`+strconv.FormatInt(questionID, 10)+`" data-item-ref="a.go"`) {
		t.Errorf("a.go's sent decision still renders picked after SendBatch; got:\n%s", g)
	}
	if !strings.Contains(g, `class="decision picked" aria-pressed="true" data-draft-ticket="`+strconv.FormatInt(ticketID, 10)+
		`" data-draft-question="`+strconv.FormatInt(questionID, 10)+`" data-item-ref="b.go" data-decision="reject"`) {
		t.Errorf("b.go's still-unsent draft decision missing its picked rendering; got:\n%s", g)
	}
}

// TestFeedExcludesDraftMessages proves the Feed view (design section 6.5,
// 7.2) shows only sent messages, not a queued-but-unsent draft (code review
// fix 2).
func TestFeedExcludesDraftMessages(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "f#2", "Feed draft ticket")
	seedUnreadUpdate(t, s, ticketID, "a real feed update")
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketID, Text: "a queued draft reply",
	}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "feed", 0, 0)

	if !strings.Contains(main, "a real feed update") {
		t.Errorf("feed missing the real, sent update; got:\n%s", main)
	}
	if strings.Contains(main, "a queued draft reply") {
		t.Errorf("feed leaked an unsent draft row; got:\n%s", main)
	}
}

// TestInboxOrdersByNewestSentMessageIgnoringDrafts proves InboxItems' sort
// and its newest-message display time are computed off each ticket's
// newest SENT message, not a later, still-queued draft (design section 7.2,
// code review fix 2): a draft's higher message id must not let its ticket
// jump ahead of one whose real message is newer. ticketA's real update (id
// 1) precedes ticketB's (id 2), but ticketA's draft (id 3, inserted last)
// has the greatest id of all three; without the fix that draft would make
// ticketA sort as the newest.
func TestInboxOrdersByNewestSentMessageIgnoringDrafts(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)

	ticketA := seedTicket(t, s, "d#1", "Ticket A older real update")
	seedUnreadUpdate(t, s, ticketA, "A's real update")

	ticketB := seedTicket(t, s, "d#2", "Ticket B newer real update")
	seedUnreadUpdate(t, s, ticketB, "B's real update")

	if _, err := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticketA, Text: "a draft reply after both real updates",
	}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "inbox", 0, 0)

	a := mustIndex(t, main, "Ticket A older real update")
	b := mustIndex(t, main, "Ticket B newer real update")
	if b > a {
		t.Errorf("expected ticket B (newer real update) before ticket A despite A's later draft; got:\n%s", main)
	}
}

// TestThreadOpenZeroOrMissingRendersEmptyThread proves the Thread view's
// nil-ticket guard: open=0 (no ticket open, including an absent signal,
// which ReadSignals leaves at its zero value) or an id naming no ticket
// both render the empty placeholder rather than erroring (design section
// 6.6, carried over from Package 3's patchThread guard).
func TestThreadOpenZeroOrMissingRendersEmptyThread(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	cases := []struct {
		name string
		open int64
	}{
		{"zero", 0},
		{"missing ticket", 999999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
