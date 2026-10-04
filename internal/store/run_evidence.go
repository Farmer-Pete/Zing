package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RunEvidence is one run's kept evidence (migration 0007): the agent's
// final message, the stderr file path, and the transcript path. A nil
// field is NULL; a non-nil field is never empty (CHECK length > 0 in the
// migration).
type RunEvidence struct {
	FinalMessage   *string
	StderrPath     *string
	TranscriptPath *string
}

// StderrFileName is the one file name job.writeStderrFile ever writes for
// runID, directly inside <DataDir>/runs. console.handleRunFile checks a
// run's stored stderr_path against this same name before it will serve the
// file, so the shape is owned here rather than duplicated at each caller.
func StderrFileName(runID int64) string {
	return fmt.Sprintf("run-%d-stderr.log", runID)
}

// RecordRunEvidence writes runID's three evidence columns in one UPDATE. A
// nil field writes NULL. It returns an error wrapping sql.ErrNoRows when no
// run has that id. It logs nothing; its caller logs a failure.
func (s *Store) RecordRunEvidence(ctx context.Context, runID int64, ev RunEvidence) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET final_message = ?, stderr_path = ?, transcript_path = ? WHERE id = ?`,
		ev.FinalMessage, ev.StderrPath, ev.TranscriptPath, runID)
	if err != nil {
		return fmt.Errorf("record run evidence: run %d: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record run evidence: run %d: %w", runID, err)
	}
	if n == 0 {
		// RowsAffected reporting 0 doesn't by itself prove the row is
		// missing: an UPDATE that sets every column to the value it
		// already held can also report 0. Confirm absence before
		// claiming sql.ErrNoRows.
		var exists int
		existsErr := s.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, runID).Scan(&exists)
		switch {
		case errors.Is(existsErr, sql.ErrNoRows):
			return fmt.Errorf("record run evidence: run %d: %w", runID, sql.ErrNoRows)
		case existsErr != nil:
			return fmt.Errorf("record run evidence: run %d: %w", runID, existsErr)
		}
	}
	return nil
}

// RunEvidenceByID reads one run's evidence and the ticket its session
// belongs to. The error wraps sql.ErrNoRows when no run has runID, and
// ticketID is then 0.
func (s *Store) RunEvidenceByID(ctx context.Context, runID int64) (int64, RunEvidence, error) {
	var ticketID int64
	var ev RunEvidence
	err := s.db.QueryRowContext(ctx,
		`SELECT s.ticket_id, r.final_message, r.stderr_path, r.transcript_path
		 FROM runs r JOIN sessions s ON s.id = r.session_id WHERE r.id = ?`, runID,
	).Scan(&ticketID, &ev.FinalMessage, &ev.StderrPath, &ev.TranscriptPath)
	if err != nil {
		return 0, RunEvidence{}, fmt.Errorf("run evidence for run %d: %w", runID, err)
	}
	return ticketID, ev, nil
}

// RunEvidenceForTicket returns run id -> RunEvidence for every run on
// ticketID's sessions. A run whose three columns are all NULL is still
// present, with three nil fields.
func (s *Store) RunEvidenceForTicket(ctx context.Context, ticketID int64) (map[int64]RunEvidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.final_message, r.stderr_path, r.transcript_path
		 FROM runs r JOIN sessions s ON s.id = r.session_id WHERE s.ticket_id = ?`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("run evidence for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	out := make(map[int64]RunEvidence)
	for rows.Next() {
		var id int64
		var ev RunEvidence
		if err := rows.Scan(&id, &ev.FinalMessage, &ev.StderrPath, &ev.TranscriptPath); err != nil {
			return nil, fmt.Errorf("run evidence for ticket %d: %w", ticketID, err)
		}
		out[id] = ev
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run evidence for ticket %d: %w", ticketID, err)
	}
	return out, nil
}
