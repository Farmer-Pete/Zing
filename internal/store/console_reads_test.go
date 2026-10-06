package store

import (
	"strings"
	"testing"
	"time"
)

// Literals this file repeats three or more times; testProject,
// testAuthorZing, testTypeUpdate, testBodyProgress, testTypeQuestion,
// testTypeScenario, testStatePlanning, questionStateOpen, and
// testWaitingQuestions are already declared elsewhere in this package.
const (
	testTypeFinding  = "finding"
	testProjectAlpha = "alpha"
	testProjectBeta  = "beta"
)

// seedProjectNamed inserts (or reuses) a project by name, the minimum a
// console read needs to join against.
func seedProjectNamed(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	id, err := s.EnsureProject(t.Context(), Project{
		Name: name, RepoURL: "https://github.com/x/" + name, LocalPath: "/tmp/" + name, Tracker: testProject.Tracker,
	})
	if err != nil {
		t.Fatalf("EnsureProject(%s): %v", name, err)
	}
	return id
}

// insertWaitingTicket inserts a queued ticket on projectID and immediately
// sets waiting_on to "questions", arranging a blocking fixture (design
// section 6.8) without going through commit.go's atomic transition.
func insertWaitingTicket(t *testing.T, s *Store, projectID int64, ref string) int64 {
	t.Helper()
	id, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: ref, Title: "t " + ref, State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(%s): %v", ref, err)
	}
	setTicketWaiting(t, s, id, testWaitingQuestions)
	return id
}

// insertZingUpdate inserts an unread, zing-authored "update" message on
// ticketID, the simplest message that satisfies the unread predicate
// (design section 6.8).
func insertZingUpdate(t *testing.T, s *Store, ticketID int64) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress,
	})
	if err != nil {
		t.Fatalf("insert zing update: %v", err)
	}
	return id
}

// markMessageRead sets read_at on id directly, the way console_writes.go's
// MarkRead will (Task 7); this file only reads, so it reaches past that
// seam to arrange a read fixture.
func markMessageRead(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE messages SET read_at = ? WHERE id = ?`, formatTime(time.Now()), id); err != nil {
		t.Fatalf("mark message %d read: %v", id, err)
	}
}

// insertQuestionWithBody inserts a "question" message carrying a real
// multi-line body, the fixture QuestionSummary's title/preview tests need
// (insertOpenQuestion, commit_test.go, uses the option key itself as the
// body, too short to exercise title extraction).
func insertQuestionWithBody(t *testing.T, s *Store, ticketID int64, key, body, state string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, Type: testTypeQuestion, Author: testAuthorZing,
		State: new(state), Body: body, Payload: questionPayload(key),
	})
	if err != nil {
		t.Fatalf("insert question %s: %v", key, err)
	}
	return id
}

// messageCreatedAt reads back one message's created_at, parsed the way
// scanMessage does, so a test can assert a QuestionSummary or InboxItem
// time against the value the trigger actually wrote (design section 6.16).
func messageCreatedAt(t *testing.T, s *Store, id int64) time.Time {
	t.Helper()
	var raw string
	if err := s.db.QueryRowContext(t.Context(), `SELECT created_at FROM messages WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read created_at for message %d: %v", id, err)
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parse created_at %q: %v", raw, err)
	}
	return ts
}

func TestListProjects_OrderedByNameThenID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	betaID := seedProjectNamed(t, s, testProjectBeta)
	alphaID := seedProjectNamed(t, s, testProjectAlpha)

	got, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListProjects returned %d projects, want 2", len(got))
	}
	if got[0].ID != alphaID || got[0].Name != testProjectAlpha {
		t.Errorf("ListProjects[0] = %+v, want project %s (%d)", got[0], testProjectAlpha, alphaID)
	}
	if got[1].ID != betaID || got[1].Name != testProjectBeta {
		t.Errorf("ListProjects[1] = %+v, want project %s (%d)", got[1], testProjectBeta, betaID)
	}
}

// TestInboxItems_BlockingFirstThenNewestMessageIDDesc builds three blocking
// tickets (one with no message, two with messages at different newest ids)
// and two unread-only tickets, and asserts the full inbox order: blocking
// first, by newest message id descending within each group, then ticket id
// (design section 7.2).
func TestInboxItems_BlockingFirstThenNewestMessageIDDesc(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	ticketA := insertWaitingTicket(t, s, projectID, "a") // blocking, no message
	ticketB := insertWaitingTicket(t, s, projectID, "b") // blocking, older message
	insertZingUpdate(t, s, ticketB)
	ticketC := insertWaitingTicket(t, s, projectID, "c") // blocking, newer message
	insertZingUpdate(t, s, ticketC)

	ticketD, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "d", Title: "t d", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(d): %v", err)
	}
	insertZingUpdate(t, s, ticketD) // unread, not blocking, older
	ticketE, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "e", Title: "t e", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(e): %v", err)
	}
	insertZingUpdate(t, s, ticketE) // unread, not blocking, newer

	got, err := s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems: %v", err)
	}

	want := []int64{ticketC, ticketB, ticketA, ticketE, ticketD}
	if len(got) != len(want) {
		t.Fatalf("InboxItems returned %d items, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Ticket.ID != w {
			t.Errorf("InboxItems[%d].Ticket.ID = %d, want %d", i, got[i].Ticket.ID, w)
		}
	}

	// ticketA is blocking but carries no message, so NewestAt must be nil
	// (design section 7.2) and it must not appear as unread-excluded.
	if got[2].NewestAt != nil {
		t.Errorf("InboxItems for the messageless blocking ticket: NewestAt = %v, want nil", got[2].NewestAt)
	}
}

// TestInboxItems_ExcludesNeitherBlockingNorUnread proves a ticket with no
// waiting flag and no unread message is left out of the inbox, and that a
// message already marked read does not count as unread.
func TestInboxItems_ExcludesNeitherBlockingNorUnread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	quiet, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "quiet", Title: "quiet", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(quiet): %v", err)
	}
	readID := insertZingUpdate(t, s, quiet)
	markMessageRead(t, s, readID)

	// A "you"-authored message never counts as unread either (design
	// section 6.8: author must be zing), even carrying one of the four
	// otherwise-unread types.
	if _, err = s.InsertMessage(t.Context(), Message{
		TicketID: quiet, Type: testTypeUpdate, Author: "you", Body: "thanks",
	}); err != nil {
		t.Fatalf("insert your update: %v", err)
	}

	got, err := s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems: %v", err)
	}
	for _, item := range got {
		if item.Ticket.ID == quiet {
			t.Errorf("InboxItems includes the quiet ticket %d: %+v", quiet, item)
		}
	}
}

// TestInboxItems_OpenQuestionSummaries proves the joined read fills each
// InboxItem's OpenQuestions in message-id order, with the right key, title
// (the body's first line), preview, and time, and excludes a question that
// is no longer open.
func TestInboxItems_OpenQuestionSummaries(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)
	ticketID := insertWaitingTicket(t, s, projectID, "q")

	q1ID := insertQuestionWithBody(t, s, ticketID, "Q1", "Ship the change now?\nSome extra context.", questionStateOpen)
	q2ID := insertQuestionWithBody(t, s, ticketID, "Q2", "Roll back instead?", questionStateOpen)
	insertQuestionWithBody(t, s, ticketID, "Q3", "Already decided", questionStateAnswered) // not open; excluded

	q1Time := messageCreatedAt(t, s, q1ID)
	q2Time := messageCreatedAt(t, s, q2ID)

	got, err := s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("InboxItems returned %d items, want 1", len(got))
	}
	item := got[0]
	if item.ProjectName != testProjectAlpha {
		t.Errorf("InboxItems[0].ProjectName = %q, want %q", item.ProjectName, testProjectAlpha)
	}
	if len(item.OpenQuestions) != 2 {
		t.Fatalf("OpenQuestions = %+v, want exactly 2 (Q1, Q2, in message-id order; Q3 is answered)", item.OpenQuestions)
	}

	q1 := item.OpenQuestions[0]
	if q1.Key != "Q1" || q1.Title != "Ship the change now?" || q1.Preview != "Ship the change now?" {
		t.Errorf("OpenQuestions[0] = %+v, want key Q1, title/preview \"Ship the change now?\"", q1)
	}
	if !q1.Time.Equal(q1Time) {
		t.Errorf("OpenQuestions[0].Time = %v, want %v (Q1's own created_at)", q1.Time, q1Time)
	}

	q2 := item.OpenQuestions[1]
	if q2.Key != "Q2" || q2.Title != "Roll back instead?" {
		t.Errorf("OpenQuestions[1] = %+v, want key Q2, title \"Roll back instead?\"", q2)
	}
	if !q2.Time.Equal(q2Time) {
		t.Errorf("OpenQuestions[1].Time = %v, want %v (Q2's own created_at)", q2.Time, q2Time)
	}
}

// TestInboxItems_PreviewTruncatesLongTitle proves Preview stays one short
// line even when the question body's first line is long, while Title keeps
// the full line.
func TestInboxItems_PreviewTruncatesLongTitle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)
	ticketID := insertWaitingTicket(t, s, projectID, "long")

	longLine := strings.Repeat("x", 120)
	insertQuestionWithBody(t, s, ticketID, "Q1", longLine, questionStateOpen)

	got, err := s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems: %v", err)
	}
	if len(got) != 1 || len(got[0].OpenQuestions) != 1 {
		t.Fatalf("InboxItems = %+v, want exactly one item with one open question", got)
	}
	q := got[0].OpenQuestions[0]
	if q.Title != longLine {
		t.Errorf("Title = %q (len %d), want the full 120-rune line", q.Title, len(q.Title))
	}
	previewRunes := []rune(q.Preview)
	if len(previewRunes) != questionPreviewMaxRunes+1 { // +1 for the appended ellipsis rune
		t.Errorf("Preview = %q (%d runes), want %d runes plus an ellipsis", q.Preview, len(previewRunes), questionPreviewMaxRunes)
	}
	if q.Preview == q.Title {
		t.Error("Preview equals Title for a title longer than questionPreviewMaxRunes; want it truncated")
	}
}

// testStateEscalated and testStateAbandoned round up the terminal-state
// literals TestInboxItems_TerminalTicketNeverUnread needs, alongside the
// already-declared testStateDone (commit_test.go): named once rather than
// repeated, the same reasoning testStateDone itself follows.
const (
	testStateEscalated = "escalated"
	testStateAbandoned = "abandoned"
)

// TestInboxItems_TerminalTicketNeverUnread proves the terminal argument
// (design section 6.8's goal "InboxItems leaves out unread-only tickets
// whose state is in machine.States.Terminal") only ever drops an
// unread-only ticket: a done ticket with an unread message is excluded,
// while an escalated ticket that is also blocking (waiting_on set) stays
// listed whatever its state, and a queued ticket's unread message is
// unaffected. Passing nil, as every pre-existing caller does, leaves the
// unread branch unfiltered and lists all three.
func TestInboxItems_TerminalTicketNeverUnread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	done, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "finished", Title: "t finished", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(done): %v", err)
	}
	insertZingUpdate(t, s, done)
	setTicketState(t, s, done, testStateDone)

	escalated := insertWaitingTicket(t, s, projectID, "escalated")
	setTicketState(t, s, escalated, testStateEscalated)

	queued, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "queued", Title: "t queued", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(queued): %v", err)
	}
	insertZingUpdate(t, s, queued)

	terminal := []string{testStateDone, testStateEscalated, testStateAbandoned}
	got, err := s.InboxItems(t.Context(), terminal)
	if err != nil {
		t.Fatalf("InboxItems(terminal): %v", err)
	}
	if inboxHasTicket(got, done) {
		t.Errorf("InboxItems(terminal) includes the done ticket %d: %+v", done, got)
	}
	if !inboxHasTicket(got, escalated) {
		t.Errorf("InboxItems(terminal) excludes the blocking escalated ticket %d: %+v", escalated, got)
	}
	if !inboxHasTicket(got, queued) {
		t.Errorf("InboxItems(terminal) excludes the queued ticket %d: %+v", queued, got)
	}

	gotAll, err := s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems(nil): %v", err)
	}
	for _, id := range []int64{done, escalated, queued} {
		if !inboxHasTicket(gotAll, id) {
			t.Errorf("InboxItems(nil) excludes ticket %d: %+v", id, gotAll)
		}
	}
}

// inboxHasTicket reports whether items contains ticketID.
func inboxHasTicket(items []InboxItem, ticketID int64) bool {
	for i := range items {
		if items[i].Ticket.ID == ticketID {
			return true
		}
	}
	return false
}

// TestLiveTickets_PlanningTicketWithNothingUnreadIsListed proves the
// sidebar's new read picks up a quiet, non-terminal ticket that
// InboxItems' blocking-or-unread filter would leave out (ticket, #106 bug
// 4): a planning ticket with no messages at all still shows somewhere a
// mouse can reach.
func TestLiveTickets_PlanningTicketWithNothingUnreadIsListed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	ticketID, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "planning1", Title: "t planning1", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	setTicketState(t, s, ticketID, testStatePlanning)

	terminal := []string{testStateDone, testStateEscalated, testStateAbandoned}
	got, err := s.LiveTickets(t.Context(), terminal)
	if err != nil {
		t.Fatalf("LiveTickets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LiveTickets returned %d tickets, want 1: %+v", len(got), got)
	}
	if got[0].Ticket.ID != ticketID {
		t.Errorf("LiveTickets[0].Ticket.ID = %d, want %d", got[0].Ticket.ID, ticketID)
	}
	if got[0].Unread {
		t.Error("LiveTickets[0].Unread = true, want false for a ticket with no messages")
	}
	if got[0].OpenQuestionCount != 0 {
		t.Errorf("LiveTickets[0].OpenQuestionCount = %d, want 0", got[0].OpenQuestionCount)
	}
	if got[0].Ticket.WaitingOn != nil {
		t.Errorf("LiveTickets[0].Ticket.WaitingOn = %v, want nil", got[0].Ticket.WaitingOn)
	}
}

// TestLiveTickets_ExcludesTerminalUnlessBlocking proves LiveTickets' own
// membership rule: a terminal ticket is excluded unless it is blocking, and
// every ticket is included when terminal is nil.
func TestLiveTickets_ExcludesTerminalUnlessBlocking(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	done, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "done1", Title: "t done1", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(done): %v", err)
	}
	insertZingUpdate(t, s, done)
	setTicketState(t, s, done, testStateDone)

	escalated := insertWaitingTicket(t, s, projectID, "escalated1")
	setTicketState(t, s, escalated, testStateEscalated)

	queued, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "queued1", Title: "t queued1", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(queued): %v", err)
	}

	terminal := []string{testStateDone, testStateEscalated, testStateAbandoned}
	got, err := s.LiveTickets(t.Context(), terminal)
	if err != nil {
		t.Fatalf("LiveTickets(terminal): %v", err)
	}
	if liveHasTicket(got, done) {
		t.Errorf("LiveTickets(terminal) includes the done ticket %d: %+v", done, got)
	}
	if !liveHasTicket(got, escalated) {
		t.Errorf("LiveTickets(terminal) excludes the blocking escalated ticket %d: %+v", escalated, got)
	}
	if !liveHasTicket(got, queued) {
		t.Errorf("LiveTickets(terminal) excludes the queued ticket %d: %+v", queued, got)
	}

	gotAll, err := s.LiveTickets(t.Context(), nil)
	if err != nil {
		t.Fatalf("LiveTickets(nil): %v", err)
	}
	for _, id := range []int64{done, escalated, queued} {
		if !liveHasTicket(gotAll, id) {
			t.Errorf("LiveTickets(nil) excludes ticket %d: %+v", id, gotAll)
		}
	}
}

// TestLiveTickets_OrdersByIssueNumberOnly proves LiveTickets' full ordering
// is issueNumberOrder alone (#106 bug 5, c8's owner decision (a)): a
// blocking or unread ticket sorts by its ref like any other, not first --
// numeric refs of any length sorted correctly, then non-numeric refs as
// text. It also proves the variadic extra destinations land correctly: the
// blocking ticket's OpenQuestionCount matches its open question rows.
func TestLiveTickets_OrdersByIssueNumberOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	mkQuiet := func(ref string) int64 {
		id, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: ref, Title: "t " + ref, State: ticketStateQueued})
		if err != nil {
			t.Fatalf("InsertTicket(%s): %v", ref, err)
		}
		return id
	}

	mkQuiet("102")
	mkQuiet("65")
	unread := mkQuiet("9")
	insertZingUpdate(t, s, unread)
	blocking := insertWaitingTicket(t, s, projectID, "200")
	insertQuestionWithBody(t, s, blocking, "Q1", "Question one", questionStateOpen)
	insertQuestionWithBody(t, s, blocking, "Q2", "Question two", questionStateOpen)
	mkQuiet("100000000000000000001")
	mkQuiet("99999999999999999999")
	mkQuiet("007")
	const nonNumericRef = "abc"
	mkQuiet(nonNumericRef)
	// nonNumericBeforeDigits sorts before every digit string in plain text
	// order ('#' < '0' in ASCII), which exercises issueNumberOrder's
	// numeric-refs-first partition: without it, this ref would otherwise
	// sort before every numeric ref instead of after all of them.
	const nonNumericBeforeDigits = "#5"
	mkQuiet(nonNumericBeforeDigits)

	terminal := []string{testStateDone, testStateEscalated, testStateAbandoned}
	got, err := s.LiveTickets(t.Context(), terminal)
	if err != nil {
		t.Fatalf("LiveTickets: %v", err)
	}
	wantRefs := []string{"007", "9", "65", "102", "200", "99999999999999999999", "100000000000000000001", nonNumericBeforeDigits, nonNumericRef}
	if len(got) != len(wantRefs) {
		t.Fatalf("LiveTickets returned %d tickets, want %d: %+v", len(got), len(wantRefs), got)
	}
	for i, want := range wantRefs {
		if got[i].Ticket.TrackerRef != want {
			t.Errorf("LiveTickets[%d].Ticket.TrackerRef = %q, want %q", i, got[i].Ticket.TrackerRef, want)
		}
	}
	if got[4].OpenQuestionCount != 2 {
		t.Errorf("LiveTickets[4] (ref 200) OpenQuestionCount = %d, want 2", got[4].OpenQuestionCount)
	}
}

// TestLiveTickets_TiesOnNormalizedRefBreakByID proves that two refs
// normalizing to the same stripped digit string (here "007" and "7", both
// stripping to "7") tie all the way through issueNumberOrder's numeric
// columns and fall through to ticket id, rather than falling back to
// comparing the raw, zero-padded ref text.
func TestLiveTickets_TiesOnNormalizedRefBreakByID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	mkQuiet := func(ref string) int64 {
		id, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: ref, Title: "t " + ref, State: ticketStateQueued})
		if err != nil {
			t.Fatalf("InsertTicket(%s): %v", ref, err)
		}
		return id
	}

	first := mkQuiet("7")
	second := mkQuiet("007")

	got, err := s.LiveTickets(t.Context(), nil)
	if err != nil {
		t.Fatalf("LiveTickets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LiveTickets returned %d tickets, want 2: %+v", len(got), got)
	}
	if got[0].Ticket.ID != first || got[1].Ticket.ID != second {
		t.Errorf("LiveTickets order = [%d, %d], want [%d, %d] (lower id first)",
			got[0].Ticket.ID, got[1].Ticket.ID, first, second)
	}
}

// TestLiveTickets_ParkedUntil proves LiveTickets' own MAX(capped_until)
// subquery (#45): ParkedUntil is nil for a ticket with no capped run, and
// equals the greatest capped_until for a ticket with two parked runs.
func TestLiveTickets_ParkedUntil(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	_, quiet := seedQueuedTicket(t, s, "1")

	_, parked := seedQueuedTicket(t, s, "2")
	owner, expires := claimForCommit(t, s, parked)
	reserveOpenRun(t, s, parked, owner, expires, testStatePlanning)
	earlier := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := s.ParkRuns(ctx, parked, owner, expires, earlier, ""); err != nil {
		t.Fatalf("ParkRuns(earlier): %v", err)
	}

	owner, expires = claimForCommit(t, s, parked)
	reserveOpenRun(t, s, parked, owner, expires, testStateBuilding)
	later := earlier.Add(time.Hour)
	if _, err := s.ParkRuns(ctx, parked, owner, expires, later, ""); err != nil {
		t.Fatalf("ParkRuns(later): %v", err)
	}

	got, err := s.LiveTickets(ctx, nil)
	if err != nil {
		t.Fatalf("LiveTickets: %v", err)
	}

	quietLT := liveTicketByID(got, quiet)
	if quietLT == nil {
		t.Fatalf("LiveTickets excludes quiet ticket %d: %+v", quiet, got)
	}
	if quietLT.ParkedUntil != nil {
		t.Errorf("quiet ticket ParkedUntil = %v, want nil", quietLT.ParkedUntil)
	}

	parkedLT := liveTicketByID(got, parked)
	if parkedLT == nil {
		t.Fatalf("LiveTickets excludes parked ticket %d: %+v", parked, got)
	}
	if parkedLT.ParkedUntil == nil || !parkedLT.ParkedUntil.Equal(later) {
		t.Errorf("parked ticket ParkedUntil = %v, want %v", parkedLT.ParkedUntil, later)
	}
}

// liveTicketByID returns the item in items naming ticketID, or nil.
func liveTicketByID(items []LiveTicket, ticketID int64) *LiveTicket {
	for i := range items {
		if items[i].Ticket.ID == ticketID {
			return &items[i]
		}
	}
	return nil
}

// liveHasTicket reports whether items contains ticketID.
func liveHasTicket(items []LiveTicket, ticketID int64) bool {
	for i := range items {
		if items[i].Ticket.ID == ticketID {
			return true
		}
	}
	return false
}

// TestTicketsByProject_OrderedByIssueNumberThenID proves TicketsByProject
// orders by issueNumberOrder (Task 3), not by tracker_ref as text: refs 10,
// 2, 31 come back as 2, 10, 31, not the text order 10, 2, 31.
func TestTicketsByProject_OrderedByIssueNumberThenID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectAID := seedProjectNamed(t, s, testProjectAlpha)
	projectBID := seedProjectNamed(t, s, testProjectBeta)

	refs := []string{"10", "2", "31"}
	ids := make(map[string]int64, len(refs))
	for _, ref := range refs {
		id, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectAID, TrackerRef: ref, Title: "t", State: ticketStateQueued})
		if err != nil {
			t.Fatalf("InsertTicket(%s): %v", ref, err)
		}
		ids[ref] = id
	}
	other, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectBID, TrackerRef: "1", Title: "other project", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(other project): %v", err)
	}

	got, err := s.TicketsByProject(t.Context(), projectAID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	wantRefs := []string{"2", "10", "31"}
	if len(got) != len(wantRefs) {
		t.Fatalf("TicketsByProject returned %d tickets, want %d", len(got), len(wantRefs))
	}
	for i, ref := range wantRefs {
		if got[i].ID != ids[ref] {
			t.Errorf("TicketsByProject[%d].ID = %d, want %d (tracker_ref %q)", i, got[i].ID, ids[ref], ref)
		}
	}
	for _, item := range got {
		if item.ID == other {
			t.Error("TicketsByProject leaked a ticket from a different project")
		}
	}
}

// TestRecentTickets_NewestTicketFirst proves RecentTickets orders by ticket
// id descending (#106 bug 5, c8's owner decision (a)), not by message
// recency: ticketOld and ticketNew each get a sent update, in that order,
// and ticketQuietFirst and ticketQuietSecond get none, yet the order is
// purely newest-ticket-first.
func TestRecentTickets_NewestTicketFirst(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	ticketOld, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "old", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(old): %v", err)
	}
	insertZingUpdate(t, s, ticketOld)

	ticketNew, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "new", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(new): %v", err)
	}
	insertZingUpdate(t, s, ticketNew)

	ticketQuietFirst, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "quiet1", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(quiet1): %v", err)
	}
	ticketQuietSecond, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "quiet2", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(quiet2): %v", err)
	}

	got, err := s.RecentTickets(t.Context())
	if err != nil {
		t.Fatalf("RecentTickets: %v", err)
	}
	want := []int64{ticketQuietSecond, ticketQuietFirst, ticketNew, ticketOld}
	if len(got) != len(want) {
		t.Fatalf("RecentTickets returned %d tickets, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("RecentTickets[%d].ID = %d, want %d", i, got[i].ID, w)
		}
	}
}

// TestRecentTickets_NewMessageDoesNotReorder proves a ticket's position on
// the Recent page survives it gaining a new message (#106 bug 5): ticketOld
// is created first (so it has the lower id), then ticketNew. A real, sent
// update and a draft, both added to ticketOld afterward, must not move it
// ahead of ticketNew.
func TestRecentTickets_NewMessageDoesNotReorder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	projectID := seedProjectNamed(t, s, testProjectAlpha)

	ticketOld, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "old", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(old): %v", err)
	}

	ticketNew, err := s.InsertTicket(t.Context(), Ticket{ProjectID: projectID, TrackerRef: "new", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(new): %v", err)
	}

	insertZingUpdate(t, s, ticketOld)
	if _, err = s.SaveDraft(t.Context(), DraftInput{TicketID: ticketOld, Text: "a draft reply after the real update"}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	got, err := s.RecentTickets(t.Context())
	if err != nil {
		t.Fatalf("RecentTickets: %v", err)
	}
	want := []int64{ticketNew, ticketOld}
	if len(got) != len(want) {
		t.Fatalf("RecentTickets returned %d tickets, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("RecentTickets[%d].ID = %d, want %d (a later message on ticketOld must not outrank ticketNew)", i, got[i].ID, w)
		}
	}
}

func TestFeedMessages_NewestFirstAndLimitClamped(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")

	m1, err := s.InsertMessage(t.Context(), Message{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: "one"})
	if err != nil {
		t.Fatalf("insert m1: %v", err)
	}
	m2, err := s.InsertMessage(t.Context(), Message{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: "two"})
	if err != nil {
		t.Fatalf("insert m2: %v", err)
	}
	m3, err := s.InsertMessage(t.Context(), Message{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: "three"})
	if err != nil {
		t.Fatalf("insert m3: %v", err)
	}

	t.Run("normal limit", func(t *testing.T) {
		t.Parallel()
		got, err := s.FeedMessages(t.Context(), 2)
		if err != nil {
			t.Fatalf("FeedMessages(2): %v", err)
		}
		if len(got) != 2 || got[0].ID != m3 || got[1].ID != m2 {
			t.Errorf("FeedMessages(2) = %v, want [%d, %d]", messageIDs(got), m3, m2)
		}
	})

	t.Run("zero clamps to one", func(t *testing.T) {
		t.Parallel()
		got, err := s.FeedMessages(t.Context(), 0)
		if err != nil {
			t.Fatalf("FeedMessages(0): %v", err)
		}
		if len(got) != 1 || got[0].ID != m3 {
			t.Errorf("FeedMessages(0) = %v, want [%d] (clamped to 1)", messageIDs(got), m3)
		}
	})

	t.Run("negative clamps to one", func(t *testing.T) {
		t.Parallel()
		got, err := s.FeedMessages(t.Context(), -5)
		if err != nil {
			t.Fatalf("FeedMessages(-5): %v", err)
		}
		if len(got) != 1 || got[0].ID != m3 {
			t.Errorf("FeedMessages(-5) = %v, want [%d] (clamped to 1)", messageIDs(got), m3)
		}
	})

	t.Run("over 200 clamps to 200", func(t *testing.T) {
		t.Parallel()
		got, err := s.FeedMessages(t.Context(), 5000)
		if err != nil {
			t.Fatalf("FeedMessages(5000): %v", err)
		}
		if len(got) != 3 || got[0].ID != m3 || got[1].ID != m2 || got[2].ID != m1 {
			t.Errorf("FeedMessages(5000) = %v, want [%d, %d, %d] (only 3 exist)", messageIDs(got), m3, m2, m1)
		}
	})
}

func messageIDs(rows []MessageRow) []int64 {
	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

func TestGetArtifact_ReturnsGreatestVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")

	scenario := func(id string) []byte {
		return []byte(`{"id":"` + id + `","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`)
	}
	if _, err := s.InsertArtifact(t.Context(), Artifact{TicketID: ticketID, Type: testTypeScenario, Version: 1, Payload: scenario("s1")}); err != nil {
		t.Fatalf("insert v1: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), Artifact{TicketID: ticketID, Type: testTypeScenario, Version: 3, Payload: scenario("s3")}); err != nil {
		t.Fatalf("insert v3: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), Artifact{TicketID: ticketID, Type: testTypeScenario, Version: 2, Payload: scenario("s2")}); err != nil {
		t.Fatalf("insert v2: %v", err)
	}

	got, ok, err := s.GetArtifact(t.Context(), ticketID, testTypeScenario)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if !ok {
		t.Fatal("GetArtifact: ok = false, want true")
	}
	if got.Version != 3 {
		t.Errorf("GetArtifact.Version = %d, want 3 (the greatest)", got.Version)
	}
	if string(got.Payload) != string(scenario("s3")) {
		t.Errorf("GetArtifact.Payload = %s, want the v3 payload", got.Payload)
	}
}

func TestGetArtifact_AbsentReturnsFalseNoError(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")

	got, ok, err := s.GetArtifact(t.Context(), ticketID, testTypeScenario)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if ok {
		t.Errorf("GetArtifact for a missing type: ok = true, got = %+v, want false", got)
	}
}

// TestListArtifacts_OrderedByTypeVersionDescID proves the full ORDER BY:
// type ascending, then version descending, then id ascending as the final
// tie-break for two rows at the same (type, version).
func TestListArtifacts_OrderedByTypeVersionDescID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketID := seedQueuedTicket(t, s, "1")

	scenario := func(id string) []byte {
		return []byte(`{"id":"` + id + `","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`)
	}
	finding := []byte(`{"lens":"tests","severity":"minor","location":"x:1","text":"y","fix":"z",` +
		`"id":"r1f1","round":1,"sha":"` + testFortyHexSHA + `","lenses":["tests"]}`)

	// scenarioV1First and scenarioV1Second tie on (type, version); each
	// carries a distinct payload id so the returned order (Artifact has no
	// id field of its own) can still prove the id tie-break.
	if _, err := s.InsertArtifact(t.Context(), Artifact{TicketID: ticketID, Type: testTypeScenario, Version: 1, Payload: scenario("s1")}); err != nil {
		t.Fatalf("insert scenario v1 (first): %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), Artifact{TicketID: ticketID, Type: testTypeScenario, Version: 1, Payload: scenario("s2")}); err != nil {
		t.Fatalf("insert scenario v1 (second): %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), Artifact{TicketID: ticketID, Type: testTypeScenario, Version: 2, Payload: scenario("s3")}); err != nil {
		t.Fatalf("insert scenario v2: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), Artifact{TicketID: ticketID, Type: testTypeFinding, Version: 1, Payload: finding}); err != nil {
		t.Fatalf("insert finding v1: %v", err)
	}

	got, err := s.ListArtifacts(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("ListArtifacts returned %d rows, want 4", len(got))
	}

	// "finding" < "scenario" lexically; within scenario, version 2 before
	// version 1; within the tied (scenario, 1) pair, insertion (id) order.
	if got[0].Type != testTypeFinding {
		t.Errorf("ListArtifacts[0].Type = %q, want %q", got[0].Type, testTypeFinding)
	}
	if got[1].Type != testTypeScenario || got[1].Version != 2 {
		t.Errorf("ListArtifacts[1] = %+v, want scenario v2", got[1])
	}
	if got[2].Type != testTypeScenario || got[2].Version != 1 || string(got[2].Payload) != string(scenario("s1")) {
		t.Errorf("ListArtifacts[2] = %s, want the first-inserted scenario v1 row", got[2].Payload)
	}
	if got[3].Type != testTypeScenario || got[3].Version != 1 || string(got[3].Payload) != string(scenario("s2")) {
		t.Errorf("ListArtifacts[3] = %s, want the second-inserted scenario v1 row", got[3].Payload)
	}
}

func TestSessionsForTicket_OrderedByIDAndScoped(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")

	first := insertSession(t, s, ticketA, testStatePlanning)
	second := insertSession(t, s, ticketA, testJobBuild)
	insertSession(t, s, ticketB, testStatePlanning) // a different ticket; must not appear

	got, err := s.SessionsForTicket(t.Context(), ticketA)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	if len(got) != 2 || got[0].ID != first || got[1].ID != second {
		t.Errorf("SessionsForTicket = %+v, want [%d, %d] in id order", got, first, second)
	}
}

func TestRunsForTicket_OrderedByIDAndScoped(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")

	sessA1 := insertSession(t, s, ticketA, testStatePlanning)
	sessA2 := insertSession(t, s, ticketA, testJobBuild)
	sessB := insertSession(t, s, ticketB, testStatePlanning)

	runA1 := insertQuestionRun(t, s, sessA1)
	runA2 := insertQuestionRun(t, s, sessA2)
	insertQuestionRun(t, s, sessB) // a different ticket's run; must not appear

	got, err := s.RunsForTicket(t.Context(), ticketA)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(got) != 2 || got[0].ID != runA1 || got[1].ID != runA2 {
		t.Errorf("RunsForTicket = %+v, want [%d, %d] in id (insertion) order", got, runA1, runA2)
	}
}

func TestGetSetting_ExistingMissingAndNullValue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	got, ok, err := s.GetSetting(t.Context(), "log_level")
	if err != nil {
		t.Fatalf("GetSetting(log_level): %v", err)
	}
	if !ok || got != "info" {
		t.Errorf("GetSetting(log_level) = (%q, %v), want (info, true)", got, ok)
	}

	got, ok, err = s.GetSetting(t.Context(), "last_good_binary")
	if err != nil {
		t.Fatalf("GetSetting(last_good_binary): %v", err)
	}
	if !ok || got != "" {
		t.Errorf("GetSetting(last_good_binary) = (%q, %v), want (\"\", true) for a NULL-valued key", got, ok)
	}

	got, ok, err = s.GetSetting(t.Context(), "does-not-exist")
	if err != nil {
		t.Fatalf("GetSetting(does-not-exist): %v", err)
	}
	if ok {
		t.Errorf("GetSetting(does-not-exist) = (%q, %v), want ok = false", got, ok)
	}
}
