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
// NextPollAt, PollIntervalS, and PollFingerprint are migration 0004's three
// babysit-poll columns (design D8, section 16): nil NextPollAt means due
// now, or no poll pending; PollIntervalS, when set, is 30 to 300; and
// PollFingerprint, when set, is 64 lowercase hex characters.
type Ticket struct {
	ID              int64
	ProjectID       int64
	TrackerRef      string // the external id; also the priority tie-break key
	Title, Body     string
	Kind            *string // nil, "bug", or "feature"
	State           string  // one of the nine TicketState values
	WaitingOn       *string // nil or one of the eight waiting flags
	ParentTicketID  *int64
	Branch, PRURL   *string
	ClaimOwner      *string
	ClaimExpiresAt  *time.Time
	NextPollAt      *time.Time // nil: due now, or no babysit poll pending
	PollIntervalS   *int       // 30..300 when set
	PollFingerprint *string    // 64 lowercase hex when set
	// TrackerBody is the issue body as last read from the tracker (migration
	// 0011): nil only for a row inserted before that migration (or by an
	// older binary) whose body the console has never edited.
	TrackerBody *string
	// OwnerComments is tracker.RenderComments text from the last planning
	// refresh (migration 0011); "" when none.
	OwnerComments string
}

// TicketText is a planning refresh's write (migration 0011): Body nil
// leaves tickets.body and tracker_body untouched; a non-nil Body sets both
// together. OwnerComments is always written.
type TicketText struct {
	Body          *string
	OwnerComments string
}

// PollUpdate sets a ticket's three poll columns together (design section
// 4.2, D8): NextAt and IntervalS schedule the next poll, Fingerprint is the
// CI/thread/head fingerprint that backoff compares against on the next poll
// (8.3). CommitHandlerResult validates Fingerprint against
// ^[0-9a-f]{64}$ and IntervalS against 30..300 before any write.
type PollUpdate struct {
	NextAt      time.Time // UTC, second precision
	IntervalS   int       // 30..300
	Fingerprint string    // 64 lowercase hex; see the validation rule below
}

// PollSchedule moves next_poll_at and poll_interval_s and leaves
// poll_fingerprint as it is, possibly NULL. Used when a poll's GitHub reads
// failed (8.3), so no fingerprint can be computed.
type PollSchedule struct {
	NextAt    time.Time // UTC, second precision
	IntervalS int       // 30..300
}

// Session is a row in the sessions table.
type Session struct {
	ID, TicketID int64
	Job, Runtime string
	ExternalID   *string
	Resumes      int
}

// Run is a row in the runs table. Turn is 0-based (design section 9: a
// session's first run is turn 0). Interrupted, PGID, ProcStart, and
// StartedAt are migration 0005's four columns (design section 5.1, D6):
// Interrupted marks a run a shutdown or a dead-serve reclaim cut off,
// resumable for free (D5); PGID, ProcStart, and StartedAt are the agent
// process identity RecordRunStart records at the start handshake (section
// 7.1), used to tell a live orphan from a dead one (section 6.3).
// CappedUntil is migration 0009's column: non-nil only when Interrupted is
// true, set once by ParkRuns and never cleared, the reset instant a Claude
// session limit (or a hold refusal) parked this run until (#45).
type Run struct {
	ID, SessionID          int64
	Turn                   int
	Lens                   *string
	TaskN                  *int
	Model, Outcome         *string
	AgentSeconds, ExitCode *int
	Interrupted            bool
	PGID                   *int
	ProcStart              *string
	StartedAt              *time.Time
	CappedUntil            *time.Time
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
// The binary before Package 9 names its own columns explicitly here too, so
// it ignores migration 0004's three trailing poll columns (section 16).
const ticketColumns = `id, project_id, tracker_ref, title, body, kind, state, waiting_on, parent_ticket_id, branch, pr_url, claim_owner, claim_expires_at, next_poll_at, poll_interval_s, poll_fingerprint, tracker_body, owner_comments`

// messageColumns is the messages column list, id first, then the store's
// Message (store.go) fields in that struct's order (event_kind last,
// migration 0008), then created_at (migration 0002, design section 6.16).
const messageColumns = `id, ticket_id, run_id, parent_id, type, author, state, body, payload, batch_id, read_at, event_kind, created_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so scanTicket and
// scanMessage work for a single-row QueryRowContext and a multi-row
// QueryContext loop alike.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTicket scans one row of ticketColumns, in that order, into a Ticket.
// extra holds destinations for any columns a caller selects after
// ticketColumns (LiveTickets' unread flag and open-question count); they
// are scanned in the same call, in order.
func scanTicket(rs rowScanner, extra ...any) (Ticket, error) {
	var t Ticket
	var kind, waitingOn, branch, prURL, claimOwner, claimExpiresAt sql.NullString
	var parentTicketID sql.NullInt64
	var nextPollAt, pollFingerprint sql.NullString
	var pollIntervalS sql.NullInt64
	var trackerBody sql.NullString

	dest := []any{ //nolint:prealloc // the literal spells out ticketColumns's order; extra is variadic and appended once below, not grown in a loop
		&t.ID, &t.ProjectID, &t.TrackerRef, &t.Title, &t.Body,
		&kind, &t.State, &waitingOn, &parentTicketID,
		&branch, &prURL, &claimOwner, &claimExpiresAt,
		&nextPollAt, &pollIntervalS, &pollFingerprint,
		&trackerBody, &t.OwnerComments,
	}
	if err := rs.Scan(append(dest, extra...)...); err != nil {
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
	if nextPollAt.Valid {
		ts, err := time.Parse(fixedTimeLayout, nextPollAt.String)
		if err != nil {
			return Ticket{}, fmt.Errorf("parse next_poll_at: %w", err)
		}
		t.NextPollAt = &ts
	}
	if pollIntervalS.Valid {
		n := int(pollIntervalS.Int64)
		t.PollIntervalS = &n
	}
	if pollFingerprint.Valid {
		t.PollFingerprint = &pollFingerprint.String
	}
	if trackerBody.Valid {
		t.TrackerBody = &trackerBody.String
	}
	return t, nil
}

// scanMessage scans one row of messageColumns, in that order, into a
// MessageRow.
func scanMessage(rs rowScanner) (MessageRow, error) {
	var row MessageRow
	var runID, parentID, batchID sql.NullInt64
	var state, body, payload, readAt, eventKind, createdAt sql.NullString

	if err := rs.Scan(
		&row.ID, &row.TicketID, &runID, &parentID, &row.Type, &row.Author,
		&state, &body, &payload, &batchID, &readAt, &eventKind, &createdAt,
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
	if eventKind.Valid {
		row.EventKind = &eventKind.String
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

// fixedTimeLayout is formatTime's one fixed-width UTC timestamp format
// (design DD1): always 20 characters, truncated to whole seconds, ending
// "Z". Its output is byte-identical to time.RFC3339's own rendering of a
// UTC time (no fractional-second field, and "Z07:00" already collapses to a
// literal "Z" at zero offset), so this changes no byte this package already
// wrote -- only makes the truncation explicit and gives SQLite's TEXT
// comparison (next_poll_at <= ?) a format that always orders two values the
// same way their underlying instants order.
const fixedTimeLayout = "2006-01-02T15:04:05Z"

// formatTime renders t through fixedTimeLayout (design DD1), the one helper
// every poll timestamp -- written or compared -- goes through, and every
// other TEXT timestamp column in the schema already matches byte for byte.
func formatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(fixedTimeLayout)
}

// formatTimePtr is formatTime for a nullable timestamp field.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	f := formatTime(*t)
	return &f
}
