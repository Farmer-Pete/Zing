package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// GetTicket reads the ticket with id, or a wrapped sql.ErrNoRows if none exists.
func (s *Store) GetTicket(ctx context.Context, id int64) (Ticket, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, id)
	t, err := scanTicket(row)
	if err != nil {
		return Ticket{}, fmt.Errorf("get ticket %d: %w", id, err)
	}
	return t, nil
}

// TicketByRef finds the ticket for (projectID, ref), the intake dedup check.
// A missing ticket is not an error: ok is false and err is nil.
func (s *Store) TicketByRef(ctx context.Context, projectID int64, ref string) (Ticket, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+ticketColumns+` FROM tickets WHERE project_id = ? AND tracker_ref = ?`, projectID, ref)
	t, err := scanTicket(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Ticket{}, false, nil
		}
		return Ticket{}, false, fmt.Errorf("ticket by ref %s: %w", ref, err)
	}
	return t, true, nil
}

// ListAllTickets returns every ticket, ordered by id, for the console.
func (s *Store) ListAllTickets(ctx context.Context) ([]Ticket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ticketColumns+` FROM tickets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list all tickets: %w", err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("list all tickets: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all tickets: %w", err)
	}
	return out, nil
}

// ListReadyCandidates returns every unclaimed, non-waiting ticket whose
// state is not in terminal, ordered by id for a deterministic result. The
// store applies no priority order: tracker_ref is TEXT, so SQL would sort
// "fake#10" before "fake#2"; the dispatcher parses the numeric external id
// and orders candidates in Go (section 6.2).
func (s *Store) ListReadyCandidates(ctx context.Context, terminal []string) ([]Ticket, error) {
	query := `SELECT ` + ticketColumns + ` FROM tickets WHERE claim_owner IS NULL AND waiting_on IS NULL`
	args := make([]any, 0, len(terminal))
	if len(terminal) > 0 {
		placeholders := make([]string, len(terminal))
		for i, state := range terminal {
			placeholders[i] = "?"
			args = append(args, state)
		}
		// The dynamic part is a fixed number of "?" placeholders, one per
		// terminal state; every value rides as a bind argument below, never
		// concatenated into the query text.
		query += ` AND state NOT IN (` + strings.Join(placeholders, ", ") + `)` //nolint:gosec // G202: placeholders only, values are bind args
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list ready candidates: %w", err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("list ready candidates: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ready candidates: %w", err)
	}
	return out, nil
}

// ListMessages returns every message for ticketID, ordered by id ascending.
func (s *Store) ListMessages(ctx context.Context, ticketID int64) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? ORDER BY id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list messages for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("list messages for ticket %d: %w", ticketID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// OpenSession returns the newest session for (ticketID, job), the resume
// lookup a planning (or building) handler makes to decide first-entry
// versus resume. ok is false, with no error, when no such session exists.
func (s *Store) OpenSession(ctx context.Context, ticketID int64, job string) (Session, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, ticket_id, job, runtime, external_id, resumes
		 FROM sessions WHERE ticket_id = ? AND job = ? ORDER BY id DESC LIMIT 1`,
		ticketID, job)

	var sess Session
	var externalID sql.NullString
	err := row.Scan(&sess.ID, &sess.TicketID, &sess.Job, &sess.Runtime, &externalID, &sess.Resumes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("open session for ticket %d job %s: %w", ticketID, job, err)
	}
	if externalID.Valid {
		sess.ExternalID = &externalID.String
	}
	return sess, true, nil
}

// QuestionsByState returns every "question" message on ticketID whose
// messages.state equals state (the canonical question lifecycle value,
// section 8), ordered by id.
func (s *Store) QuestionsByState(ctx context.Context, ticketID int64, state string) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type = 'question' AND state = ? ORDER BY id`,
		ticketID, state)
	if err != nil {
		return nil, fmt.Errorf("questions by state %s for ticket %d: %w", state, ticketID, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("questions by state %s for ticket %d: %w", state, ticketID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("questions by state %s for ticket %d: %w", state, ticketID, err)
	}
	return out, nil
}

// GetMessage reads the message with id, or a wrapped sql.ErrNoRows if none exists.
func (s *Store) GetMessage(ctx context.Context, id int64) (MessageRow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, id)
	m, err := scanMessage(row)
	if err != nil {
		return MessageRow{}, fmt.Errorf("get message %d: %w", id, err)
	}
	return m, nil
}

// CountActiveRuns counts tickets that are claimed and not waiting, the
// max_parallel guard's input.
func (s *Store) CountActiveRuns(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tickets WHERE claim_owner IS NOT NULL AND waiting_on IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active runs: %w", err)
	}
	return n, nil
}
