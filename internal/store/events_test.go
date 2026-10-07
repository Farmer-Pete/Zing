package store

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
)

// seedSessionAndTwoRuns raw-inserts session 1 and runs 1 and 2 on ticketID,
// the way migrations_test.go seeds a session and runs, so events_test.go's
// tests have a run id to scope EventFilter.RunID against.
func seedSessionAndTwoRuns(t *testing.T, s *Store, ticketID int64) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, ?, 'planning', 'claude')`, ticketID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO runs (id, session_id, turn) VALUES (1, 1, 0)`); err != nil {
		t.Fatalf("seed run 1: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO runs (id, session_id, turn) VALUES (2, 1, 1)`); err != nil {
		t.Fatalf("seed run 2: %v", err)
	}
}

// insertCheckRerunEvent builds and inserts a check_rerun event on ticketID
// for runID, returning the inserted message id.
func insertCheckRerunEvent(t *testing.T, s *Store, ticketID, runID int64, check, sha string) int64 {
	t.Helper()
	msg, err := NewEvent(ticketID, EventKindCheckRerun, response.CheckRerunEvent{
		Check: check, SHA: sha, RunID: 1, CheckRunID: 1, Reason: response.RerunReasonFlaky,
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	msg.RunID = &runID
	id, err := s.InsertMessage(t.Context(), msg)
	if err != nil {
		t.Fatalf("InsertMessage(check_rerun): %v", err)
	}
	return id
}

// TestEventsCountBySHAAndRun proves Events and CountEvents scope a kind to
// a ticket, optionally narrowed by run id and/or sha, and leave free-text
// markers alone.
func TestEventsCountBySHAAndRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	seedProjectAndTicket(t, s)
	const ticketID = 1
	seedSessionAndTwoRuns(t, s, ticketID)

	const otherTicketID = 2
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state) VALUES (?, 1, '43', 'a second ticket', 'queued')`,
		otherTicketID,
	); err != nil {
		t.Fatalf("seed second ticket: %v", err)
	}

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)

	idTestA := insertCheckRerunEvent(t, s, ticketID, 1, testCheckKindTest, shaA)
	idLintA := insertCheckRerunEvent(t, s, ticketID, 1, testCheckKindLint, shaA)
	idTestB := insertCheckRerunEvent(t, s, ticketID, 2, testCheckKindTest, shaB)
	insertCheckRerunEvent(t, s, otherTicketID, 1, testCheckKindTest, shaA)

	if _, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: "check failed delivered run 1",
	}); err != nil {
		t.Fatalf("InsertMessage(free-text marker): %v", err)
	}

	one := int64(1)
	cases := []struct {
		name string
		f    EventFilter
		want int
	}{
		{"sha A", EventFilter{SHA: shaA}, 2},
		{"sha B", EventFilter{SHA: shaB}, 1},
		{"run 1", EventFilter{RunID: &one}, 2},
		{"run 1 and sha B", EventFilter{RunID: &one, SHA: shaB}, 0},
		{"no filter", EventFilter{}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, err := s.CountEvents(ctx, ticketID, EventKindCheckRerun, tc.f)
			if err != nil {
				t.Fatalf("CountEvents: %v", err)
			}
			if n != tc.want {
				t.Errorf("CountEvents(%+v) = %d, want %d", tc.f, n, tc.want)
			}
		})
	}

	if n, err := s.CountEvents(ctx, otherTicketID, EventKindCheckRerun, EventFilter{}); err != nil || n != 1 {
		t.Fatalf("CountEvents(otherTicket) = (%d, %v), want (1, nil)", n, err)
	}
	if rows, err := s.Events(ctx, otherTicketID, EventKindCheckRerun, EventFilter{}); err != nil || len(rows) != 1 {
		t.Fatalf("Events(otherTicket) = (%d rows, %v), want (1, nil)", len(rows), err)
	}

	rows, err := s.Events(ctx, ticketID, EventKindCheckRerun, EventFilter{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantIDs := []int64{idTestA, idLintA, idTestB}
	wantPayloads := []response.CheckRerunEvent{
		{Check: testCheckKindTest, SHA: shaA, RunID: 1, CheckRunID: 1, Reason: response.RerunReasonFlaky},
		{Check: testCheckKindLint, SHA: shaA, RunID: 1, CheckRunID: 1, Reason: response.RerunReasonFlaky},
		{Check: testCheckKindTest, SHA: shaB, RunID: 1, CheckRunID: 1, Reason: response.RerunReasonFlaky},
	}
	if len(rows) != len(wantIDs) {
		t.Fatalf("Events = %d rows, want %d", len(rows), len(wantIDs))
	}
	for i, row := range rows {
		if row.ID != wantIDs[i] {
			t.Errorf("rows[%d].ID = %d, want %d (id order)", i, row.ID, wantIDs[i])
		}
		if row.EventKind == nil || *row.EventKind != EventKindCheckRerun {
			t.Errorf("rows[%d].EventKind = %v, want %q", i, row.EventKind, EventKindCheckRerun)
		}
		if row.Body != "" {
			t.Errorf("rows[%d].Body = %q, want empty", i, row.Body)
		}
		var got response.CheckRerunEvent
		if unmarshalErr := json.Unmarshal(row.Payload, &got); unmarshalErr != nil {
			t.Fatalf("unmarshal rows[%d].Payload: %v", i, unmarshalErr)
		}
		if !reflect.DeepEqual(got, wantPayloads[i]) {
			t.Errorf("rows[%d] payload = %+v, want %+v", i, got, wantPayloads[i])
		}
	}

	markers, err := s.MarkersWithPrefix(ctx, ticketID, "check failed delivered run ")
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(markers) != 1 || markers[0].Body != "check failed delivered run 1" {
		t.Fatalf("MarkersWithPrefix = %+v, want exactly the free-text row", markers)
	}

	if _, err := s.Events(ctx, ticketID, "", EventFilter{}); err == nil {
		t.Error("Events(kind \"\") = nil error, want one")
	}
	if _, err := s.CountEvents(ctx, ticketID, "", EventFilter{}); err == nil {
		t.Error("CountEvents(kind \"\") = nil error, want one")
	}
}

// TestCommitHandlerResultWritesEvent proves HandlerCommit.Messages writes a
// typed event through insertMessageTx the same way InsertMessage does,
// validating the payload and rejecting (and committing nothing) on failure.
func TestCommitHandlerResultWritesEvent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)

	sha := strings.Repeat("a", 40)
	msg, err := NewEvent(ticketID, EventKindCheckRerun, response.CheckRerunEvent{
		Check: testCheckKindTest, SHA: sha, RunID: 1, CheckRunID: 1, Reason: response.RerunReasonFlaky,
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: testForeignOwner, Expires: expires, Messages: []Message{msg},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
	if n, countErr := s.CountEvents(ctx, ticketID, EventKindCheckRerun, EventFilter{}); countErr != nil || n != 1 {
		t.Fatalf("CountEvents after valid commit = (%d, %v), want (1, nil)", n, countErr)
	}

	badMsg, err := NewEvent(ticketID, EventKindCheckRerun, response.CheckRerunEvent{
		Check: testCheckKindTest, SHA: "deadbee", RunID: 1, CheckRunID: 1, Reason: response.RerunReasonFlaky,
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if _, commitErr := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: testForeignOwner, Expires: expires, Messages: []Message{badMsg},
	}); commitErr == nil {
		t.Error("CommitHandlerResult with an invalid sha = nil error, want one")
	}
	if n, countErr := s.CountEvents(ctx, ticketID, EventKindCheckRerun, EventFilter{}); countErr != nil || n != 1 {
		t.Fatalf("CountEvents after rejected commit = (%d, %v), want unchanged (1, nil)", n, countErr)
	}
}

// TestInsertEventRejects proves messagePayloadParam rejects an unknown
// kind, a payload that fails its schema, a missing payload, a non-update
// type, and an empty kind, each leaving the messages table unchanged, while
// leaving the existing "plain update takes no payload" rule intact. Each
// case's wantErr substring pins the rejection to messagePayloadParam's own
// check, not to the DB's CHECK constraint backstop.
func TestInsertEventRejects(t *testing.T) {
	// Not t.Parallel(): the row-count check below reads after the subtests
	// above run, which only holds if they run sequentially.
	s := newTestStore(t)
	ctx := t.Context()
	seedProjectAndTicket(t, s)
	const ticketID = 1
	const wantSchemaErr = "does not match schema"

	validSHA := strings.Repeat("a", 40)
	const validRest = `,"run_id":1,"check_run_id":1,"reason":"flaky"`

	// eventMessage builds a check_rerun event Message on ticketID with a
	// raw payload, for the invalid-payload cases below.
	eventMessage := func(payload string) Message {
		kind := EventKindCheckRerun
		return Message{
			TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: &kind,
			Payload: json.RawMessage(payload),
		}
	}

	cases := []struct {
		name    string
		msg     Message
		wantErr string
	}{
		{
			"unknown kind",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: new("bogus"),
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"` + validRest + `}`),
			},
			wantSchemaErr,
		},
		{
			"empty check",
			eventMessage(`{"check":"","sha":"` + validSHA + `"` + validRest + `}`),
			wantSchemaErr,
		},
		{
			"bad sha pattern",
			eventMessage(`{"check":"test","sha":"deadbee"` + validRest + `}`),
			wantSchemaErr,
		},
		{
			"bad reason enum",
			eventMessage(`{"check":"test","sha":"` + validSHA + `","run_id":1,"check_run_id":1,"reason":"maybe"}`),
			wantSchemaErr,
		},
		{
			"missing check_run_id",
			eventMessage(`{"check":"test","sha":"` + validSHA + `","run_id":1,"reason":"flaky"}`),
			wantSchemaErr,
		},
		{
			"extra property",
			eventMessage(`{"check":"test","sha":"` + validSHA + `"` + validRest + `,"x":1}`),
			wantSchemaErr,
		},
		{
			"event kind with no payload",
			eventMessage(""),
			"requires a payload",
		},
		{
			"event kind with wrong message type",
			func() Message {
				m := eventMessage(`{"check":"test","sha":"` + validSHA + `"` + validRest + `}`)
				m.Type = msgTypeState
				return m
			}(),
			"want update",
		},
		{
			"empty event kind",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: new(""),
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"` + validRest + `}`),
			},
			"must not be empty",
		},
		{
			"plain update takes no payload",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"` + validRest + `}`),
			},
			"takes no payload",
		},
	}

	before := countRows(t, s, `SELECT COUNT(*) FROM messages`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.InsertMessage(ctx, tc.msg)
			if err == nil {
				t.Fatalf("InsertMessage(%s) = nil error, want one", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("InsertMessage(%s) error = %q, want substring %q", tc.name, err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "CHECK constraint") {
				t.Errorf("InsertMessage(%s) error = %q, want rejection before the DB CHECK backstop", tc.name, err)
			}
		})
	}
	if after := countRows(t, s, `SELECT COUNT(*) FROM messages`); after != before {
		t.Errorf("messages row count = %d, want unchanged %d", after, before)
	}
}

// TestEventKinds proves EventKinds lists exactly the committed event
// schemas, by file name, sorted.
func TestEventKinds(t *testing.T) {
	t.Parallel()
	got := EventKinds()
	want := []string{"budget_raised", "check_rerun", "check_rerun_passed", "owner_edit", "plan_unblock", "stale_base"}
	if !slices.Equal(got, want) {
		t.Errorf("EventKinds() = %v, want %v", got, want)
	}
}

// TestBudgetRaisedMinutes proves BudgetRaisedMinutes sums every
// budget_raised event's minutes on a ticket, leaves another ticket alone,
// and stays unchanged when a commit carrying an invalid event is rejected.
// It writes every event through CommitHandlerResult, the only path
// production uses (retryCapBudget's commit), rather than InsertMessage
// directly.
func TestBudgetRaisedMinutes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	projectID, ticketA := seedQueuedTicket(t, s, "42")
	ticketB, insertErr := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "43", Title: "a second ticket", State: ticketStateQueued})
	if insertErr != nil {
		t.Fatalf("InsertTicket(B): %v", insertErr)
	}

	if got, err := s.BudgetRaisedMinutes(ctx, ticketA); err != nil || got != 0 {
		t.Fatalf("BudgetRaisedMinutes(A, no events) = (%d, %v), want (0, nil)", got, err)
	}

	for range 2 {
		expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		if ok, err := s.Claim(ctx, ticketA, testForeignOwner, expires); err != nil || !ok {
			t.Fatalf("Claim(A) = (%v, %v), want (true, nil)", ok, err)
		}
		msg, err := NewEvent(ticketA, EventKindBudgetRaised, response.BudgetRaisedEvent{Minutes: 60})
		if err != nil {
			t.Fatalf("NewEvent: %v", err)
		}
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketA, Owner: testForeignOwner, Expires: expires, Messages: []Message{msg},
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult(budget_raised): %v", err)
		}
		if !applied {
			t.Fatal("CommitHandlerResult(budget_raised): applied = false, want true")
		}
	}

	got, err := s.BudgetRaisedMinutes(ctx, ticketA)
	if err != nil {
		t.Fatalf("BudgetRaisedMinutes(A): %v", err)
	}
	if got != 120 {
		t.Errorf("BudgetRaisedMinutes(A) = %d, want 120", got)
	}

	gotB, err := s.BudgetRaisedMinutes(ctx, ticketB)
	if err != nil || gotB != 0 {
		t.Fatalf("BudgetRaisedMinutes(B) = (%d, %v), want (0, nil)", gotB, err)
	}

	badExpires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if ok, claimErr := s.Claim(ctx, ticketA, testForeignOwner, badExpires); claimErr != nil || !ok {
		t.Fatalf("Claim(A, before bad commit) = (%v, %v), want (true, nil)", ok, claimErr)
	}
	badMsg, err := NewEvent(ticketA, EventKindBudgetRaised, response.BudgetRaisedEvent{Minutes: 0})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if _, commitErr := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketA, Owner: testForeignOwner, Expires: badExpires, Messages: []Message{badMsg},
	}); commitErr == nil {
		t.Error("CommitHandlerResult(budget_raised, minutes 0) = nil error, want one")
	}

	gotAfter, err := s.BudgetRaisedMinutes(ctx, ticketA)
	if err != nil || gotAfter != 120 {
		t.Fatalf("BudgetRaisedMinutes(A) after rejected commit = (%d, %v), want (120, nil)", gotAfter, err)
	}
}

// TestNewestPlanUnblock proves NewestPlanUnblock returns false on a ticket
// with no plan_unblock events, and otherwise the newest one, decoded.
func TestNewestPlanUnblock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	seedProjectAndTicket(t, s)
	const ticketID = 1

	if _, ok, err := s.NewestPlanUnblock(ctx, ticketID); err != nil || ok {
		t.Fatalf("NewestPlanUnblock(no events) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	msg2, err := NewEvent(ticketID, EventKindPlanUnblock, response.PlanUnblockEvent{
		PlanVersion: 2, FindingIDs: []string{"p2-f1"}, Guidance: "Drop task 2.",
	})
	if err != nil {
		t.Fatalf("NewEvent(v2): %v", err)
	}
	if _, insertErr := s.InsertMessage(ctx, msg2); insertErr != nil {
		t.Fatalf("InsertMessage(v2): %v", insertErr)
	}

	msg3, err := NewEvent(ticketID, EventKindPlanUnblock, response.PlanUnblockEvent{
		PlanVersion: 3, FindingIDs: []string{"p3-f2"}, Guidance: "Drop task 4.",
	})
	if err != nil {
		t.Fatalf("NewEvent(v3): %v", err)
	}
	if _, insertErr := s.InsertMessage(ctx, msg3); insertErr != nil {
		t.Fatalf("InsertMessage(v3): %v", insertErr)
	}

	got, ok, err := s.NewestPlanUnblock(ctx, ticketID)
	if err != nil {
		t.Fatalf("NewestPlanUnblock: %v", err)
	}
	if !ok {
		t.Fatal("NewestPlanUnblock: ok = false, want true")
	}
	want := response.PlanUnblockEvent{PlanVersion: 3, FindingIDs: []string{"p3-f2"}, Guidance: "Drop task 4."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NewestPlanUnblock = %+v, want %+v", got, want)
	}
}
