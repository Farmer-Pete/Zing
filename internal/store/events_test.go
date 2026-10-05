package store

import (
	"context"
	"encoding/json"
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

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)

	idTestA := insertCheckRerunEvent(t, s, ticketID, 1, response.CheckNameTest, shaA)
	idLintA := insertCheckRerunEvent(t, s, ticketID, 1, response.CheckNameLint, shaA)
	idTestB := insertCheckRerunEvent(t, s, ticketID, 2, response.CheckNameTest, shaB)

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
// leaving the existing "plain update takes no payload" rule intact.
func TestInsertEventRejects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	seedProjectAndTicket(t, s)
	const ticketID = 1

	validSHA := strings.Repeat("a", 40)
	bogusKind := "bogus"

	cases := []struct {
		name string
		msg  Message
	}{
		{
			"unknown kind",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: &bogusKind,
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"}`),
			},
		},
		{
			"bad check enum",
			mustEventMessage(t, ticketID, `{"check":"fmt","sha":"`+validSHA+`"}`),
		},
		{
			"bad sha pattern",
			mustEventMessage(t, ticketID, `{"check":"test","sha":"deadbee"}`),
		},
		{
			"extra property",
			mustEventMessage(t, ticketID, `{"check":"test","sha":"`+validSHA+`","x":1}`),
		},
		{
			"event kind with no payload",
			Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: &[]string{EventKindCheckRerun}[0]},
		},
		{
			"event kind with wrong message type",
			Message{
				TicketID: ticketID, Type: msgTypeState, Author: authorSystem, EventKind: &[]string{EventKindCheckRerun}[0],
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"}`),
			},
		},
		{
			"empty event kind",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: &[]string{""}[0],
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"}`),
			},
		},
		{
			"plain update takes no payload",
			Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
				Payload: json.RawMessage(`{"check":"test","sha":"` + validSHA + `"}`),
			},
		},
	}

	before := countRows(t, s, `SELECT COUNT(*) FROM messages`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := s.InsertMessage(ctx, tc.msg); err == nil {
				t.Errorf("InsertMessage(%s) = nil error, want one", tc.name)
			}
		})
	}
	t.Cleanup(func() {
		// t.Context() is already canceled by the time Cleanup runs (it waits
		// for the parallel subtests above), so this reads with
		// context.Background() instead.
		var after int
		if err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages`).Scan(&after); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if after != before {
			t.Errorf("messages row count = %d, want unchanged %d", after, before)
		}
	})
}

// mustEventMessage builds a check_rerun event Message with a raw payload,
// for TestInsertEventRejects' invalid-payload cases.
func mustEventMessage(t *testing.T, ticketID int64, payload string) Message {
	t.Helper()
	kind := EventKindCheckRerun
	return Message{
		TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: &kind,
		Payload: json.RawMessage(payload),
	}
}

// TestEventKinds proves EventKinds lists exactly the committed event
// schemas, by file name, sorted.
func TestEventKinds(t *testing.T) {
	t.Parallel()
	got := EventKinds()
	want := []string{"check_rerun"}
	if len(got) != len(want) {
		t.Fatalf("EventKinds() = %v, want %v", got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("EventKinds()[%d] = %q, want %q", i, got[i], k)
		}
	}
}
