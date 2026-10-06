package store

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

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
func insertCheckRerunEvent(t *testing.T, s *Store, ticketID, runID int64, check response.CheckName, sha string) int64 {
	t.Helper()
	msg, err := NewEvent(ticketID, EventKindCheckRerun, response.CheckRerunEvent{Check: check, SHA: sha})
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

	idTestA := insertCheckRerunEvent(t, s, ticketID, 1, response.CheckNameTest, shaA)
	idLintA := insertCheckRerunEvent(t, s, ticketID, 1, response.CheckNameLint, shaA)
	idTestB := insertCheckRerunEvent(t, s, ticketID, 2, response.CheckNameTest, shaB)
	insertCheckRerunEvent(t, s, otherTicketID, 1, response.CheckNameTest, shaA)

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
		{Check: response.CheckNameTest, SHA: shaA},
		{Check: response.CheckNameLint, SHA: shaA},
		{Check: response.CheckNameTest, SHA: shaB},
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
		if got != wantPayloads[i] {
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
	msg, err := NewEvent(ticketID, EventKindCheckRerun, response.CheckRerunEvent{Check: response.CheckNameTest, SHA: sha})
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

	badMsg, err := NewEvent(ticketID, EventKindCheckRerun, response.CheckRerunEvent{Check: response.CheckNameTest, SHA: "deadbee"})
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
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"}`),
			},
			wantSchemaErr,
		},
		{
			"bad check enum",
			eventMessage(`{"check":"fmt","sha":"` + validSHA + `"}`),
			wantSchemaErr,
		},
		{
			"bad sha pattern",
			eventMessage(`{"check":"test","sha":"deadbee"}`),
			wantSchemaErr,
		},
		{
			"extra property",
			eventMessage(`{"check":"test","sha":"` + validSHA + `","x":1}`),
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
				m := eventMessage(`{"check":"test","sha":"` + validSHA + `"}`)
				m.Type = msgTypeState
				return m
			}(),
			"want update",
		},
		{
			"empty event kind",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: new(""),
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"}`),
			},
			"must not be empty",
		},
		{
			"plain update takes no payload",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"}`),
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
	want := []string{"check_rerun", "owner_edit"}
	if !slices.Equal(got, want) {
		t.Errorf("EventKinds() = %v, want %v", got, want)
	}
}
