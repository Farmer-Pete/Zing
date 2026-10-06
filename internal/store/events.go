// events.go is the write and read API for typed events (#34 first step):
// a counter or a once-flag recorded as a validated JSON payload under a
// named kind, instead of free text in messages.body.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// tableEvents is the schema namespace a typed event's kind is validated
// under: internal/store/schemas/events/<kind>.json.
const tableEvents = "events"

// EventKindCheckRerun names the check_rerun event kind (#91's once-per-sha
// gate): one re-run of a check on a sha.
const EventKindCheckRerun = "check_rerun"

// EventKindCheckRerunPassed names the check_rerun_passed event kind (#91):
// a check that had a check_rerun passed once re-run, so no fix run follows.
const EventKindCheckRerunPassed = "check_rerun_passed"

// EventKindOwnerEdit names the owner_edit event kind (#41): one owner edit
// to a sealed scenario, a sealed plan's task, or the ticket body.
const EventKindOwnerEdit = "owner_edit"

// EventFilter narrows Events and CountEvents beyond ticket and kind.
type EventFilter struct {
	RunID *int64 // nil: any run
	SHA   string // "": any sha; a value that isn't 40-hex matches nothing
}

// NewEvent builds the update message for one typed event of kind on
// ticketID. Its payload is validated when the message is inserted
// (messagePayloadParam), not here. Callers set RunID or rely on
// HandlerCommit.AttachRunToMsgs.
func NewEvent(ticketID int64, kind string, payload any) (Message, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Message{}, fmt.Errorf("new event %s: %w", kind, err)
	}
	return Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, EventKind: &kind, Payload: b}, nil
}

// eventWhere is the WHERE clause and args Events and CountEvents share.
func eventWhere(ticketID int64, kind string, f EventFilter) (where string, args []any) {
	where = `ticket_id = ? AND event_kind = ?`
	args = []any{ticketID, kind}
	if f.RunID != nil {
		where += ` AND run_id = ?`
		args = append(args, *f.RunID)
	}
	if f.SHA != "" {
		where += ` AND json_extract(payload, '$.sha') = ?`
		args = append(args, f.SHA)
	}
	return where, args
}

// Events returns every event of kind on ticketID that matches f, ORDER BY id.
func (s *Store) Events(ctx context.Context, ticketID int64, kind string, f EventFilter) ([]MessageRow, error) {
	if kind == "" {
		return nil, errors.New("events: empty kind")
	}
	where, args := eventWhere(ticketID, kind, f)
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE `+where+` ORDER BY id`, args...) //nolint:gosec // G202: messageColumns and where are both fixed text, no user input
	if err != nil {
		return nil, fmt.Errorf("events %s for ticket %d: %w", kind, ticketID, err)
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("events %s for ticket %d: %w", kind, ticketID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events %s for ticket %d: %w", kind, ticketID, err)
	}
	return out, nil
}

// CountEvents counts the events of kind on ticketID that match f.
func (s *Store) CountEvents(ctx context.Context, ticketID int64, kind string, f EventFilter) (int, error) {
	if kind == "" {
		return 0, errors.New("count events: empty kind")
	}
	where, args := eventWhere(ticketID, kind, f)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count events %s for ticket %d: %w", kind, ticketID, err)
	}
	return n, nil
}

// EventKinds returns every event kind with a committed schema under
// schemas/events, sorted (fs.ReadDir sorts by name).
func EventKinds() []string {
	entries, err := fs.ReadDir(SchemaFS(), tableEvents)
	if err != nil {
		// embedded at build time: a missing events dir is a programming error
		panic(fmt.Sprintf("read event schemas: %v", err))
	}
	kinds := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".json" {
			kinds = append(kinds, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return kinds
}
