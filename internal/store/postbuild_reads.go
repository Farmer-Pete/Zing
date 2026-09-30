// postbuild_reads.go adds the reads design section 5.1's watermark rule
// needs (D18, D22): every marker whose first line starts with a given
// prefix (a fix request or a landed marker, matched family by family, not
// message by message), the ticket's highest run id (the watermark a
// request marker's own "after run <R>" line records before its first run
// is ever reserved), the newest session of a job started after that
// watermark, and one session's own run ids (the identity a fix unit's
// build reports are scoped against).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// MarkersWithPrefix returns every "update" message of the ticket whose
// first line starts with prefix, ORDER BY id (oldest first): design
// section 5.1's own by-prefix match (Marker matches a marker's first line
// exactly; this one matches a whole marker family, such as every "fix
// requested ..." or "fix landed ..." row).
func (s *Store) MarkersWithPrefix(ctx context.Context, ticketID int64, prefix string) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type = ? AND body LIKE ? ORDER BY id`,
		ticketID, msgTypeUpdate, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("markers with prefix %q for ticket %d: %w", prefix, ticketID, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("markers with prefix %q for ticket %d: %w", prefix, ticketID, err)
		}
		firstLine, _, _ := strings.Cut(m.Body, "\n")
		if strings.HasPrefix(firstLine, prefix) {
			out = append(out, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("markers with prefix %q for ticket %d: %w", prefix, ticketID, err)
	}
	return out, nil
}

// MaxRunID returns the ticket's highest run id, or 0 when it has none yet
// (design D18, section 5.1): R in a request marker's own "after run <R>"
// line, read in the same handler invocation that writes the marker, before
// the unit's first run is ever reserved.
func (s *Store) MaxRunID(ctx context.Context, ticketID int64) (int64, error) {
	var maxID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(r.id) FROM runs r JOIN sessions s ON s.id = r.session_id WHERE s.ticket_id = ?`,
		ticketID).Scan(&maxID)
	if err != nil {
		return 0, fmt.Errorf("max run id for ticket %d: %w", ticketID, err)
	}
	if !maxID.Valid {
		return 0, nil
	}
	return maxID.Int64, nil
}

// SessionAfter returns the newest session of job whose turn-0 run id is
// greater than afterRunID, and its own newest run, classified against
// maxResumes the same way UnitSession classifies its own (design D18,
// section 5.1: "the unit owns the sessions of its job whose turn-0 run id
// is greater than R ... SessionAfter ... returns the newest of them").
// Units of one job never overlap in time on one ticket, so the newest
// session after the watermark is always the current unit's. ok is false,
// with no error, when no such session exists.
func (s *Store) SessionAfter(ctx context.Context, ticketID int64, job string, afterRunID int64, maxResumes int) (Session, SessionState, Run, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.ticket_id, s.job, s.runtime, s.external_id, s.resumes
		 FROM sessions s
		 JOIN runs r0 ON r0.session_id = s.id AND r0.turn = 0
		 WHERE s.ticket_id = ? AND s.job = ? AND r0.id > ?
		 ORDER BY s.id DESC LIMIT 1`,
		ticketID, job, afterRunID)

	var sess Session
	var externalID sql.NullString
	err := row.Scan(&sess.ID, &sess.TicketID, &sess.Job, &sess.Runtime, &externalID, &sess.Resumes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Session{}, SessionNone, Run{}, false, nil
	case err != nil:
		return Session{}, SessionNone, Run{}, false, fmt.Errorf("session after run %d for ticket %d job %s: %w", afterRunID, ticketID, job, err)
	}
	if externalID.Valid {
		if externalID.String == "" {
			return Session{}, SessionNone, Run{}, false, fmt.Errorf("store: session %d has an empty external_id", sess.ID)
		}
		sess.ExternalID = &externalID.String
	}

	state, err := classifySession(sess, maxResumes)
	if err != nil {
		return Session{}, SessionNone, Run{}, false, err
	}

	newestRow := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE session_id = ? ORDER BY turn DESC LIMIT 1`, sess.ID)
	newest, err := scanRun(newestRow)
	if err != nil {
		return Session{}, SessionNone, Run{}, false, fmt.Errorf("session after run %d for ticket %d job %s: newest run: %w", afterRunID, ticketID, job, err)
	}

	return sess, state, newest, true, nil
}

// SessionRunIDs returns sessionID's own run ids, ordered by turn (design
// section 5.3): "the unit's build reports are the rows whose RunID is in
// SessionRunIDs of any session after the watermark."
func (s *Store) SessionRunIDs(ctx context.Context, sessionID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE session_id = ? ORDER BY turn`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session run ids for session %d: %w", sessionID, err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("session run ids for session %d: %w", sessionID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session run ids for session %d: %w", sessionID, err)
	}
	return out, nil
}
