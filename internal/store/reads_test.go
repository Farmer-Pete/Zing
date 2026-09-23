package store

import (
	"testing"
	"time"
)

// seedQueuedTicket inserts a project and one queued ticket on it through the
// spine helpers under test, and returns the project and ticket ids.
func seedQueuedTicket(t *testing.T, s *Store, ref string) (projectID, ticketID int64) {
	t.Helper()
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err = s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: ref, Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return projectID, ticketID
}

// setTicketState and setTicketWaiting reach past the spine helpers (this
// package builds no SetState or SetWaitingOn write helper; that is
// commit.go's job in Task 2) to arrange fixture state directly for a read
// test.
func setTicketState(t *testing.T, s *Store, ticketID int64, state string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE tickets SET state = ? WHERE id = ?`, state, ticketID); err != nil {
		t.Fatalf("set ticket state: %v", err)
	}
}

func setTicketWaiting(t *testing.T, s *Store, ticketID int64, waiting string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE tickets SET waiting_on = ? WHERE id = ?`, waiting, ticketID); err != nil {
		t.Fatalf("set ticket waiting_on: %v", err)
	}
}

// insertSession inserts a sessions row directly: this package builds no
// InsertSession write helper for Task 1 (sessions are created inside the
// atomic commit, commit.go, Task 2).
func insertSession(t *testing.T, s *Store, ticketID int64, job string) int64 {
	t.Helper()
	res, err := s.db.ExecContext(t.Context(),
		`INSERT INTO sessions (ticket_id, job, runtime) VALUES (?, ?, 'fake')`, ticketID, job)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

func questionPayload(key string) []byte {
	return []byte(`{"key":"` + key + `","kind":"question","state":"open","recommended":"a","options":[{"key":"a","text":"Do it"},{"key":"b","text":"Don't"}]}`)
}

func TestTicketByRef_FindsAnExistingTicket(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	projectID, ticketID := seedQueuedTicket(t, s, "42")

	got, ok, err := s.TicketByRef(ctx, projectID, "42")
	if err != nil {
		t.Fatalf("TicketByRef: %v", err)
	}
	if !ok {
		t.Fatal("TicketByRef for an existing ref: ok = false, want true")
	}
	if got.ID != ticketID {
		t.Errorf("TicketByRef.ID = %d, want %d", got.ID, ticketID)
	}
}

func TestTicketByRef_AbsentRefReturnsFalseNoError(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	projectID, _ := seedQueuedTicket(t, s, "42")

	got, ok, err := s.TicketByRef(ctx, projectID, "does-not-exist")
	if err != nil {
		t.Fatalf("TicketByRef: %v", err)
	}
	if ok {
		t.Errorf("TicketByRef for a missing ref: ok = true, got = %+v, want false", got)
	}
}

func TestListAllTickets_OrderedByID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	refs := []string{"10", "2", "31"}
	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		var id int64
		id, err = s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: ref, Title: "t", State: ticketStateQueued})
		if err != nil {
			t.Fatalf("InsertTicket(%s): %v", ref, err)
		}
		ids = append(ids, id)
	}

	got, err := s.ListAllTickets(ctx)
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("ListAllTickets returned %d tickets, want %d", len(got), len(ids))
	}
	for i, want := range ids {
		if got[i].ID != want {
			t.Errorf("ListAllTickets[%d].ID = %d, want %d (insertion order by id)", i, got[i].ID, want)
		}
	}
}

func TestListReadyCandidates_ExcludesClaimedWaitingAndTerminal(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	ready, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "ready", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(ready): %v", err)
	}
	claimed, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "2", Title: "claimed", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(claimed): %v", err)
	}
	setTicketState(t, s, claimed, testStatePlanning)
	if _, err = s.Claim(ctx, claimed, "host-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Claim(claimed): %v", err)
	}
	waiting, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "3", Title: "waiting", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(waiting): %v", err)
	}
	setTicketState(t, s, waiting, testStatePlanning)
	setTicketWaiting(t, s, waiting, "questions")
	done, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "4", Title: "done", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(done): %v", err)
	}
	setTicketState(t, s, done, "done")

	got, err := s.ListReadyCandidates(ctx, []string{"done", "escalated", "abandoned"})
	if err != nil {
		t.Fatalf("ListReadyCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != ready {
		t.Errorf("ListReadyCandidates = %v, want exactly [%d]", ticketIDs(got), ready)
	}
}

func TestListReadyCandidates_EmptyTerminalStillFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	got, err := s.ListReadyCandidates(ctx, nil)
	if err != nil {
		t.Fatalf("ListReadyCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != ticketID {
		t.Errorf("ListReadyCandidates(nil terminal) = %v, want [%d]", ticketIDs(got), ticketID)
	}
}

func ticketIDs(ts []Ticket) []int64 {
	ids := make([]int64, len(ts))
	for i := range ts {
		ids[i] = ts[i].ID
	}
	return ids
}

func TestListMessages_OrderedByID(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	first, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: "state", Author: testAuthorZing,
		Payload: []byte(`{"from":"queued","to":"planning","reason":"picked up"}`),
	})
	if err != nil {
		t.Fatalf("insert first message: %v", err)
	}
	second, err := s.InsertMessage(ctx, Message{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress})
	if err != nil {
		t.Fatalf("insert second message: %v", err)
	}

	got, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListMessages returned %d messages, want 2", len(got))
	}
	if got[0].Type != "state" || got[1].Type != testTypeUpdate {
		t.Errorf("ListMessages types = [%s, %s], want [state, update] (insertion/id order)", got[0].Type, got[1].Type)
	}
	if got[0].ID != first || got[1].ID != second {
		t.Errorf("ListMessages ids = [%d, %d], want [%d, %d] (the ids InsertMessage returned)",
			got[0].ID, got[1].ID, first, second)
	}
}

func TestListMessages_ScopedToOneTicket(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")

	if _, err := s.InsertMessage(ctx, Message{TicketID: ticketA, Type: testTypeUpdate, Author: testAuthorZing, Body: "a"}); err != nil {
		t.Fatalf("insert message for ticket A: %v", err)
	}
	if _, err := s.InsertMessage(ctx, Message{TicketID: ticketB, Type: testTypeUpdate, Author: testAuthorZing, Body: "b"}); err != nil {
		t.Fatalf("insert message for ticket B: %v", err)
	}

	got, err := s.ListMessages(ctx, ticketA)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 || got[0].Body != "a" {
		t.Errorf("ListMessages(ticketA) = %+v, want exactly one message with body \"a\"", got)
	}
}

func TestQuestionsByState_FiltersByLifecycleState(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	openState := "open"
	answeredState := "answered"

	openID, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: "question", Author: testAuthorZing, State: &openState,
		Body: "Q1", Payload: questionPayload("Q1"),
	})
	if err != nil {
		t.Fatalf("insert open question: %v", err)
	}
	answeredID, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: "question", Author: testAuthorZing, State: &answeredState,
		Body: "Q2", Payload: questionPayload("Q2"),
	})
	if err != nil {
		t.Fatalf("insert answered question: %v", err)
	}

	open, err := s.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 || open[0].Body != "Q1" {
		t.Errorf("QuestionsByState(open) = %+v, want exactly one question with body Q1", open)
	}
	if len(open) == 1 && open[0].ID != openID {
		t.Errorf("QuestionsByState(open)[0].ID = %d, want %d (the id InsertMessage returned)", open[0].ID, openID)
	}

	answered, err := s.QuestionsByState(ctx, ticketID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered): %v", err)
	}
	if len(answered) != 1 || answered[0].Body != "Q2" {
		t.Errorf("QuestionsByState(answered) = %+v, want exactly one question with body Q2", answered)
	}
	if len(answered) == 1 && answered[0].ID != answeredID {
		t.Errorf("QuestionsByState(answered)[0].ID = %d, want %d (the id InsertMessage returned)", answered[0].ID, answeredID)
	}

	resolved, err := s.QuestionsByState(ctx, ticketID, "resolved")
	if err != nil {
		t.Fatalf("QuestionsByState(resolved): %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("QuestionsByState(resolved) = %+v, want none", resolved)
	}
}

func TestOpenSession_ReturnsNewestForTicketAndJob(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	insertSession(t, s, ticketID, testStatePlanning)
	newest := insertSession(t, s, ticketID, testStatePlanning)
	insertSession(t, s, ticketID, "build") // a different job; must not be returned

	got, ok, err := s.OpenSession(ctx, ticketID, testStatePlanning)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !ok {
		t.Fatal("OpenSession: ok = false, want true")
	}
	if got.ID != newest {
		t.Errorf("OpenSession.ID = %d, want the newest planning session %d", got.ID, newest)
	}
	if got.Job != testStatePlanning {
		t.Errorf("OpenSession.Job = %q, want planning", got.Job)
	}
}

func TestOpenSession_NoneForJobReturnsFalse(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	_, ok, err := s.OpenSession(ctx, ticketID, testStatePlanning)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if ok {
		t.Error("OpenSession with no session for the job: ok = true, want false")
	}
}

func TestFirstRun_ReturnsLowestTurnForSession(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sessID := insertSession(t, s, ticketID, testStatePlanning)

	run0 := insertQuestionRun(t, s, sessID)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO runs (session_id, turn, outcome) VALUES (?, 1, 'ready')`, sessID); err != nil {
		t.Fatalf("insert turn-1 run: %v", err)
	}

	got, ok, err := s.FirstRun(ctx, sessID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if got.ID != run0 {
		t.Errorf("FirstRun.ID = %d, want the turn-0 run %d", got.ID, run0)
	}
	if got.Turn != 0 {
		t.Errorf("FirstRun.Turn = %d, want 0", got.Turn)
	}
	if got.SessionID != sessID {
		t.Errorf("FirstRun.SessionID = %d, want %d", got.SessionID, sessID)
	}
}

func TestFirstRun_NoRunsReturnsFalse(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sessID := insertSession(t, s, ticketID, testStatePlanning)

	_, ok, err := s.FirstRun(ctx, sessID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if ok {
		t.Error("FirstRun with no runs: ok = true, want false")
	}
}

func TestQuestionsByRun_ScopedToOneRun(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sessID := insertSession(t, s, ticketID, testStatePlanning)
	runA := insertQuestionRun(t, s, sessID)
	runB := insertQuestionRun(t, s, sessID)

	openState := "open"
	answeredState := "answered"

	qA, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, RunID: &runA, Type: msgTypeQuestion, Author: testAuthorZing, State: &answeredState,
		Body: "Q1", Payload: questionPayload("Q1"),
	})
	if err != nil {
		t.Fatalf("insert question for run A: %v", err)
	}
	if _, err = s.InsertMessage(ctx, Message{
		TicketID: ticketID, RunID: &runB, Type: msgTypeQuestion, Author: testAuthorZing, State: &answeredState,
		Body: "Q2", Payload: questionPayload("Q2"),
	}); err != nil {
		t.Fatalf("insert question for run B: %v", err)
	}
	if _, err = s.InsertMessage(ctx, Message{
		TicketID: ticketID, RunID: &runA, Type: msgTypeQuestion, Author: testAuthorZing, State: &openState,
		Body: "Q3", Payload: questionPayload("Q3"),
	}); err != nil {
		t.Fatalf("insert open question for run A: %v", err)
	}

	got, err := s.QuestionsByRun(ctx, runA, "answered")
	if err != nil {
		t.Fatalf("QuestionsByRun: %v", err)
	}
	if len(got) != 1 || got[0].ID != qA {
		t.Errorf("QuestionsByRun(runA, answered) = %+v, want exactly [%d]", got, qA)
	}
}

func TestGetMessage_RoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	id, err := s.InsertMessage(ctx, Message{TicketID: ticketID, Type: testTypeUpdate, Author: testAuthorZing, Body: testBodyProgress})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	got, err := s.GetMessage(ctx, id)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.ID == 0 {
		t.Error("GetMessage.ID = 0, want the row id InsertMessage returned")
	}
	if got.ID != id {
		t.Errorf("GetMessage.ID = %d, want %d (the id InsertMessage returned)", got.ID, id)
	}
	if got.TicketID != ticketID || got.Body != testBodyProgress || got.Type != testTypeUpdate {
		t.Errorf("GetMessage = %+v, want ticket %d, body progress, type update", got, ticketID)
	}
}

func TestCountActiveRuns_CountsClaimedNotWaiting(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	active, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "active", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(active): %v", err)
	}
	setTicketState(t, s, active, testStatePlanning)
	if _, err = s.Claim(ctx, active, "host-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Claim(active): %v", err)
	}

	claimedWaiting, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "2", Title: "waiting", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(claimedWaiting): %v", err)
	}
	setTicketState(t, s, claimedWaiting, testStatePlanning)
	if _, err = s.Claim(ctx, claimedWaiting, "host-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Claim(claimedWaiting): %v", err)
	}
	setTicketWaiting(t, s, claimedWaiting, "questions")

	if _, err = s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "3", Title: "unclaimed", State: ticketStateQueued}); err != nil {
		t.Fatalf("InsertTicket(unclaimed): %v", err)
	}

	n, err := s.CountActiveRuns(ctx)
	if err != nil {
		t.Fatalf("CountActiveRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("CountActiveRuns = %d, want 1 (only the claimed, non-waiting ticket)", n)
	}
}
