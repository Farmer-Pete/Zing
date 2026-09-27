package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrClaimLost is Reserve's fence failure: the ticket is no longer claimed
// by owner at exactly expires when Reserve checks (design section 4.5), the
// same "lease changed out from under this write" case CommitHandlerResult
// reports as applied=false because it has a boolean to report through;
// Reserve has none, so it reports a sentinel instead, and writes nothing.
var ErrClaimLost = errors.New("store: claim lost")

// Reserved is the run Reserve created: the session it belongs to, the run's
// own id, and the turn Reserve computed for it (design section 4.5).
type Reserved struct {
	SessionID, RunID int64
	Turn             int
}

// Reserve is the one pre-commit write a handler may make under its claim
// (design D13, section 4.5). One transaction: (1) fence on the exact lease
// the caller holds -- no row means the lease has already moved on, and
// Reserve returns ErrClaimLost having written nothing; (2) su.ID nil creates
// a new session with external_id NULL (SessionUpsert fills it in on a later
// commit, once the runtime call returns one); a non-nil su.ID must already
// belong to ticketID, checked with the same verifySessionForTicket
// CommitHandlerResult's resume path uses; (3) the session's next turn, one
// past its highest so far, or 0 for a session with none yet; (4) a run row
// for that turn with outcome, exit_code, and agent_seconds all NULL, the
// placeholder a later CommitHandlerResult, or ExpireClaims's reconcile on a
// crash or a shutdown, terminalizes.
func (s *Store) Reserve(ctx context.Context, ticketID int64, owner string, expires time.Time, su SessionUpsert, model string) (Reserved, error) {
	expires = truncateExpires(expires)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reserved{}, fmt.Errorf("reserve: begin tx: %w", err)
	}
	defer rollback(tx)

	var fenced int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM tickets WHERE id = ? AND claim_owner = ? AND claim_expires_at = ?`,
		ticketID, owner, formatTime(expires)).Scan(&fenced)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Reserved{}, ErrClaimLost
	case err != nil:
		return Reserved{}, fmt.Errorf("reserve: fence ticket %d: %w", ticketID, err)
	}

	var sessionID int64
	if su.ID != nil {
		if err = verifySessionForTicket(ctx, tx, ticketID, *su.ID); err != nil {
			return Reserved{}, fmt.Errorf("reserve: %w", err)
		}
		sessionID = *su.ID
	} else {
		var sessRes sql.Result
		sessRes, err = tx.ExecContext(ctx,
			`INSERT INTO sessions (ticket_id, job, runtime, external_id) VALUES (?, ?, ?, NULL)`,
			ticketID, su.Job, su.Runtime)
		if err != nil {
			return Reserved{}, fmt.Errorf("reserve: insert session: %w", err)
		}
		sessionID, err = sessRes.LastInsertId()
		if err != nil {
			return Reserved{}, fmt.Errorf("reserve: insert session: %w", err)
		}
	}

	var turn int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(turn), -1) + 1 FROM runs WHERE session_id = ?`, sessionID).Scan(&turn)
	if err != nil {
		return Reserved{}, fmt.Errorf("reserve: next turn for session %d: %w", sessionID, err)
	}

	var runRes sql.Result
	runRes, err = tx.ExecContext(ctx,
		`INSERT INTO runs (session_id, turn, model, outcome, exit_code, agent_seconds) VALUES (?, ?, ?, NULL, NULL, NULL)`,
		sessionID, turn, model)
	if err != nil {
		return Reserved{}, fmt.Errorf("reserve: insert run: %w", err)
	}
	var runID int64
	runID, err = runRes.LastInsertId()
	if err != nil {
		return Reserved{}, fmt.Errorf("reserve: insert run: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return Reserved{}, fmt.Errorf("reserve: commit tx: %w", err)
	}

	slog.Info("run reserved", "ticket_id", ticketID, "session_id", sessionID, "run_id", runID, "job", su.Job, "turn", turn)
	return Reserved{SessionID: sessionID, RunID: runID, Turn: turn}, nil
}
