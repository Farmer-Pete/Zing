package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Ticket is a row in the tickets table. There is no updated_at column;
// priority tie-breaks by TrackerRef, the external id (design section 10 step
// 5). State is one of the nine TicketState values and WaitingOn, when
// non-nil, is one of the eight waiting flags (migrations/0001_init.sql).
type Ticket struct {
	ID             int64
	ProjectID      int64
	TrackerRef     string // the external id; also the priority tie-break key
	Title, Body    string
	Kind           *string // nil, "bug", or "feature"
	State          string  // one of the nine TicketState values
	WaitingOn      *string // nil or one of the eight waiting flags
	ParentTicketID *int64
	Branch, PRURL  *string
	ClaimOwner     *string
	ClaimExpiresAt *time.Time
}

// Session is a row in the sessions table.
type Session struct {
	ID, TicketID int64
	Job, Runtime string
	ExternalID   *string
	Resumes      int
}

// Run is a row in the runs table. Turn is 0-based (design section 9: a
// session's first run is turn 0).
type Run struct {
	ID, SessionID          int64
	Turn                   int
	Lens                   *string
	TaskN                  *int
	Model, Outcome         *string
	AgentSeconds, ExitCode *int
}

// Project is a row in the projects table.
type Project struct {
	ID                                int64
	Name, RepoURL, LocalPath, Tracker string
	DefaultBranch                     string
}

// MessageRow is a messages table row: the id plus every field the store's
// Message (store.go) already carries, plus CreatedAt (design section 6.16).
// Message itself has no id field, so a read that needs one (every read in
// this file) returns a MessageRow instead; commit.go's own inserts still
// return a bare id from LastInsertId and take a Message, unchanged.
// CreatedAt is nil only for a row read before migration 0002 ever ran
// (impossible against a freshly migrated database); every insert path is
// backed by the messages_set_created_at trigger, so a row this package reads
// always carries one.
type MessageRow struct {
	ID        int64
	CreatedAt *time.Time
	Message
}

// ticketStateQueued is the state every ticket starts in; InsertTicket
// enforces it (a later state is reached only through a committed
// transition, commit.go's job in Task 2).
const ticketStateQueued = "queued"

// ticketColumns is the tickets column list, in table-declaration order, used
// by every ticket read so a single scanTicket stays correct for all of them.
const ticketColumns = `id, project_id, tracker_ref, title, body, kind, state, waiting_on, parent_ticket_id, branch, pr_url, claim_owner, claim_expires_at`

// messageColumns is the messages column list, id first, then the store's
// Message (store.go) fields in that struct's order, then created_at
// (migration 0002, design section 6.16).
const messageColumns = `id, ticket_id, run_id, parent_id, type, author, state, body, payload, batch_id, read_at, created_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so scanTicket and
// scanMessage work for a single-row QueryRowContext and a multi-row
// QueryContext loop alike.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTicket scans one row of ticketColumns, in that order, into a Ticket.
func scanTicket(rs rowScanner) (Ticket, error) {
	var t Ticket
	var kind, waitingOn, branch, prURL, claimOwner, claimExpiresAt sql.NullString
	var parentTicketID sql.NullInt64

	if err := rs.Scan(
		&t.ID, &t.ProjectID, &t.TrackerRef, &t.Title, &t.Body,
		&kind, &t.State, &waitingOn, &parentTicketID,
		&branch, &prURL, &claimOwner, &claimExpiresAt,
	); err != nil {
		return Ticket{}, err
	}

	if kind.Valid {
		t.Kind = &kind.String
	}
	if waitingOn.Valid {
		t.WaitingOn = &waitingOn.String
	}
	if parentTicketID.Valid {
		t.ParentTicketID = &parentTicketID.Int64
	}
	if branch.Valid {
		t.Branch = &branch.String
	}
	if prURL.Valid {
		t.PRURL = &prURL.String
	}
	if claimOwner.Valid {
		t.ClaimOwner = &claimOwner.String
	}
	if claimExpiresAt.Valid {
		ts, err := time.Parse(time.RFC3339, claimExpiresAt.String)
		if err != nil {
			return Ticket{}, fmt.Errorf("parse claim_expires_at: %w", err)
		}
		t.ClaimExpiresAt = &ts
	}
	return t, nil
}

// scanMessage scans one row of messageColumns, in that order, into a
// MessageRow.
func scanMessage(rs rowScanner) (MessageRow, error) {
	var row MessageRow
	var runID, parentID, batchID sql.NullInt64
	var state, body, payload, readAt, createdAt sql.NullString

	if err := rs.Scan(
		&row.ID, &row.TicketID, &runID, &parentID, &row.Type, &row.Author,
		&state, &body, &payload, &batchID, &readAt, &createdAt,
	); err != nil {
		return MessageRow{}, err
	}

	if runID.Valid {
		row.RunID = &runID.Int64
	}
	if parentID.Valid {
		row.ParentID = &parentID.Int64
	}
	if state.Valid {
		row.State = &state.String
	}
	// messages.body is nullable; a NULL row (only reachable through a raw
	// insert, since InsertMessage always binds a Go string) reads back as ""
	// rather than failing the scan.
	if body.Valid {
		row.Body = body.String
	}
	if payload.Valid {
		row.Payload = json.RawMessage(payload.String)
	}
	if batchID.Valid {
		row.BatchID = &batchID.Int64
	}
	if readAt.Valid {
		ts, err := time.Parse(time.RFC3339, readAt.String)
		if err != nil {
			return MessageRow{}, fmt.Errorf("parse read_at: %w", err)
		}
		row.ReadAt = &ts
	}
	if createdAt.Valid {
		ts, err := time.Parse(time.RFC3339Nano, createdAt.String)
		if err != nil {
			return MessageRow{}, fmt.Errorf("parse created_at: %w", err)
		}
		row.CreatedAt = &ts
	}
	return row, nil
}

// formatTime renders t the same way store.go's InsertArtifact and
// InsertMessage do, so every TEXT timestamp column in the schema uses one
// format: UTC RFC 3339.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// formatTimePtr is formatTime for a nullable timestamp field.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	f := formatTime(*t)
	return &f
}
