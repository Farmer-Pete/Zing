// build_reads.go adds the read-only queries the building state machine
// consults (design section 4.2): the ticket's stored plan, its landed and
// proposed build reports, its file (extra-path) events, one build unit's
// session and newest run, and the pending/delivered marker CHECK checks
// before re-running.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"zing/internal/response"
)

// StoredPlan returns the ticket's max-version plan artifact, decoded. ok is
// false, with no error, when the ticket has no plan artifact yet.
func (s *Store) StoredPlan(ctx context.Context, ticketID int64) (plan response.Plan, version int, ok bool, err error) {
	var payload []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT version, payload FROM artifacts WHERE ticket_id = ? AND type = 'plan' ORDER BY version DESC LIMIT 1`,
		ticketID).Scan(&version, &payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return response.Plan{}, 0, false, nil
	case err != nil:
		return response.Plan{}, 0, false, fmt.Errorf("stored plan for ticket %d: %w", ticketID, err)
	}

	if err = json.Unmarshal(payload, &plan); err != nil {
		return response.Plan{}, 0, false, fmt.Errorf("stored plan for ticket %d: decode: %w", ticketID, err)
	}
	return plan, version, true, nil
}

// BuildReportRow is one build_report artifact, decoded, with the artifact's
// own id (the ORDER BY key) and the run that produced it.
type BuildReportRow struct {
	ArtifactID int64
	RunID      int64
	Report     response.BuildReport
}

// BuildReports returns every build_report artifact of the ticket, ORDER BY
// artifacts.id (insertion order): every task unit's report that ever
// returned ok, landed or not.
func (s *Store) BuildReports(ctx context.Context, ticketID int64) ([]BuildReportRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, payload FROM artifacts WHERE ticket_id = ? AND type = 'build_report' ORDER BY id`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("build reports for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []BuildReportRow
	for rows.Next() {
		var row BuildReportRow
		var runID sql.NullInt64
		var payload []byte
		if err := rows.Scan(&row.ArtifactID, &runID, &payload); err != nil {
			return nil, fmt.Errorf("build reports for ticket %d: %w", ticketID, err)
		}
		if runID.Valid {
			row.RunID = runID.Int64
		}
		if err := json.Unmarshal(payload, &row.Report); err != nil {
			return nil, fmt.Errorf("build reports for ticket %d: decode artifact %d: %w", ticketID, row.ArtifactID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("build reports for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// FileEventRow is one file artifact, decoded, with the artifact's own id
// (the ORDER BY key) and the run that produced it, when the row carries one.
type FileEventRow struct {
	ArtifactID int64
	RunID      *int64
	File       response.FileArtifact
}

// FileEvents returns every file artifact of the ticket, ORDER BY
// artifacts.id (insertion order): every event about every undeclared path a
// build or perimeter run ever proposed, described, or decided (design
// section 4.1, 6.4). Rows are append-only; the newest row per path wins,
// left to the caller to reduce.
func (s *Store) FileEvents(ctx context.Context, ticketID int64) ([]FileEventRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, payload FROM artifacts WHERE ticket_id = ? AND type = 'file' ORDER BY id`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("file events for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []FileEventRow
	for rows.Next() {
		var row FileEventRow
		var runID sql.NullInt64
		var payload []byte
		if err := rows.Scan(&row.ArtifactID, &runID, &payload); err != nil {
			return nil, fmt.Errorf("file events for ticket %d: %w", ticketID, err)
		}
		if runID.Valid {
			row.RunID = &runID.Int64
		}
		if err := json.Unmarshal(payload, &row.File); err != nil {
			return nil, fmt.Errorf("file events for ticket %d: decode artifact %d: %w", ticketID, row.ArtifactID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("file events for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// UnitSession returns the newest session of job "build" whose first run
// (turn 0) has task_n = taskN (taskN 0 matches task_n IS NULL, a fix unit),
// its state under maxResumes (the same rules LatestSession applies), and
// its newest run. ok is false, with no error, when no such session exists.
func (s *Store) UnitSession(ctx context.Context, ticketID int64, taskN, maxResumes int) (Session, SessionState, Run, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.ticket_id, s.job, s.runtime, s.external_id, s.resumes
		 FROM sessions s
		 JOIN runs r0 ON r0.session_id = s.id AND r0.turn = 0
		 WHERE s.ticket_id = ? AND s.job = 'build'
		   AND ((? = 0 AND r0.task_n IS NULL) OR r0.task_n = ?)
		 ORDER BY s.id DESC LIMIT 1`,
		ticketID, taskN, taskN)

	var sess Session
	var externalID sql.NullString
	err := row.Scan(&sess.ID, &sess.TicketID, &sess.Job, &sess.Runtime, &externalID, &sess.Resumes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Session{}, SessionNone, Run{}, false, nil
	case err != nil:
		return Session{}, SessionNone, Run{}, false, fmt.Errorf("unit session for ticket %d task %d: %w", ticketID, taskN, err)
	}
	if externalID.Valid {
		if externalID.String == "" {
			return Session{}, SessionNone, Run{}, false, fmt.Errorf("store: session %d has an empty external_id", sess.ID)
		}
		sess.ExternalID = &externalID.String
	}

	var state SessionState
	switch {
	case sess.ExternalID == nil:
		state = SessionIdless
	case sess.Resumes >= maxResumes:
		state = SessionExhausted
	default:
		state = SessionOpen
	}

	newestRow := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE session_id = ? ORDER BY turn DESC LIMIT 1`, sess.ID)
	newest, err := scanRun(newestRow)
	if err != nil {
		return Session{}, SessionNone, Run{}, false, fmt.Errorf("unit session for ticket %d task %d: newest run: %w", ticketID, taskN, err)
	}

	return sess, state, newest, true, nil
}

// Marker returns the newest "update" message of the ticket whose first line
// equals head exactly (design section 4.2, 6.4): "claims ok run 4" never
// matches a row "claims ok run 42", the exact-match rule CHECK's own
// pending/delivered pair needs. Candidates are narrowed with a LIKE prefix
// query, then matched in Go by exact first line, newest first.
func (s *Store) Marker(ctx context.Context, ticketID int64, head string) (MessageRow, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type = ? AND body LIKE ? ORDER BY id DESC`,
		ticketID, msgTypeUpdate, head+"%")
	if err != nil {
		return MessageRow{}, false, fmt.Errorf("marker %q for ticket %d: %w", head, ticketID, err)
	}
	defer rows.Close()

	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return MessageRow{}, false, fmt.Errorf("marker %q for ticket %d: %w", head, ticketID, err)
		}
		firstLine, _, _ := strings.Cut(m.Body, "\n")
		if firstLine == head {
			return m, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return MessageRow{}, false, fmt.Errorf("marker %q for ticket %d: %w", head, ticketID, err)
	}
	return MessageRow{}, false, nil
}
