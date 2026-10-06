package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// RecordRunStart records the agent process of run runID: its process group
// id, the group leader's start token, and the start time, all in one
// transaction (design section 5.3, 7.1). pgid <= 0 records only startedAt
// (and sessionExternalID, below): the fake runtime's own OnStart call has no
// real process to identify, so it reports pgid 0 and procStart is ignored.
// When sessionExternalID is non-empty and the run's session has no external
// id yet, it is filled in; a different stored value is never overwritten
// (design section 5.4) -- that case is logged at WARN and the process
// identity is still committed. It returns an error only when the
// transaction itself fails.
func (s *Store) RecordRunStart(ctx context.Context, runID int64, pgid int, procStart string, startedAt time.Time, sessionExternalID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record run start: begin tx: %w", err)
	}
	defer rollback(tx)

	var pgidParam *int
	var procStartParam *string
	if pgid > 0 {
		pgidParam = &pgid
		if procStart != "" {
			procStartParam = &procStart
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET pgid = ?, proc_start = ?, started_at = ? WHERE id = ?`,
		pgidParam, procStartParam, formatTime(startedAt), runID,
	); err != nil {
		return fmt.Errorf("record run start: run %d: %w", runID, err)
	}

	if sessionExternalID != "" {
		if err := setSessionExternalIDAtStartTx(ctx, tx, runID, sessionExternalID); err != nil {
			return fmt.Errorf("record run start: run %d: %w", runID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record run start: commit tx: %w", err)
	}
	return nil
}

// setSessionExternalIDAtStartTx sets the external id of runID's session to
// externalID, but only while it is still NULL (design section 5.4): a
// different stored value is never overwritten, since a session's first
// terminalizing commit (upsertSessionTx) is the one place allowed to
// decide the external id when the two ever disagree. It logs at WARN
// rather than erroring, so a start-time identity mismatch never fails the
// run that reported it.
func setSessionExternalIDAtStartTx(ctx context.Context, tx *sql.Tx, runID int64, externalID string) error {
	var sessionID int64
	if err := tx.QueryRowContext(ctx, `SELECT session_id FROM runs WHERE id = ?`, runID).Scan(&sessionID); err != nil {
		return fmt.Errorf("get session for run %d: %w", runID, err)
	}

	var stored sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT external_id FROM sessions WHERE id = ?`, sessionID).Scan(&stored); err != nil {
		return fmt.Errorf("get session %d external_id: %w", sessionID, err)
	}
	if stored.Valid {
		if stored.String != externalID {
			slog.Warn("session external id differs at start", "run_id", runID, "session_id", sessionID, "stored", stored.String, "reported", externalID)
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET external_id = ? WHERE id = ? AND external_id IS NULL`, externalID, sessionID,
	); err != nil {
		return fmt.Errorf("set session %d external_id: %w", sessionID, err)
	}
	return nil
}

// ForeignClaim is one ticket claimed by an owner other than self (design
// section 5.3, 6.3): what reclaimForeign (a later milestone) walks to tell
// a live orphan of a dead serve from a dead one.
type ForeignClaim struct {
	TicketID int64
	Owner    string
	Expires  time.Time
	Open     []OpenRun // runs of the ticket's sessions with a null outcome
	// Check is the ticket's recorded CHECK command (#55), nil when none is
	// recorded: reclaim judges its process group by the same rules as an
	// open run's.
	Check *OpenCheck
}

// OpenRun is one run of a ForeignClaim's ticket with a null outcome: enough
// identity (PGID, ProcStart, StartedAt) for reclaimForeign to decide whether
// its process group is still alive.
type OpenRun struct {
	RunID     int64
	Job       string
	PGID      *int
	ProcStart *string
	StartedAt *time.Time
}

// claimRow is one tickets row ForeignClaims reads before resolving each
// one's open runs.
type claimRow struct {
	ticketID int64
	owner    string
	expires  string
}

// foreignClaimRows returns every tickets row claimed by an owner other than
// self, the set ForeignClaims then resolves into full ForeignClaim values.
func foreignClaimRows(ctx context.Context, s *Store, self string) ([]claimRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, claim_owner, claim_expires_at FROM tickets
		 WHERE claim_owner IS NOT NULL AND claim_owner != ? AND claim_expires_at IS NOT NULL`,
		self)
	if err != nil {
		return nil, fmt.Errorf("foreign claims: %w", err)
	}
	defer rows.Close()

	var claimRows []claimRow
	for rows.Next() {
		var c claimRow
		if err := rows.Scan(&c.ticketID, &c.owner, &c.expires); err != nil {
			return nil, fmt.Errorf("foreign claims: scan: %w", err)
		}
		claimRows = append(claimRows, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("foreign claims: %w", err)
	}
	return claimRows, nil
}

// ForeignClaims returns every ticket claimed by an owner other than self,
// each with its sessions' open (null-outcome) runs (design section 5.3,
// 6.3) and its recorded CHECK command, if any (#55).
func (s *Store) ForeignClaims(ctx context.Context, self string) ([]ForeignClaim, error) {
	claimRows, err := foreignClaimRows(ctx, s, self)
	if err != nil {
		return nil, err
	}

	out := make([]ForeignClaim, 0, len(claimRows))
	for _, c := range claimRows {
		expires, perr := time.Parse(time.RFC3339, c.expires)
		if perr != nil {
			return nil, fmt.Errorf("foreign claims: parse claim_expires_at for ticket %d: %w", c.ticketID, perr)
		}
		open, oerr := openRunsForTicket(ctx, s.db, c.ticketID)
		if oerr != nil {
			return nil, fmt.Errorf("foreign claims: %w", oerr)
		}
		fc := ForeignClaim{TicketID: c.ticketID, Owner: c.owner, Expires: expires, Open: open}
		check, hasCheck, cerr := openCheckForTicket(ctx, s.db, c.ticketID)
		if cerr != nil {
			return nil, fmt.Errorf("foreign claims: %w", cerr)
		}
		if hasCheck {
			fc.Check = &check
		}
		out = append(out, fc)
	}
	return out, nil
}

// queryer is satisfied by *sql.DB and *sql.Tx alike, so openRunsForTicket
// can run against either.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// openRunsForTicket returns every run with a null outcome on ticketID's
// sessions, each with the process identity reclaimForeign needs.
func openRunsForTicket(ctx context.Context, q queryer, ticketID int64) ([]OpenRun, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT r.id, s.job, r.pgid, r.proc_start, r.started_at FROM runs r
		 JOIN sessions s ON s.id = r.session_id
		 WHERE r.outcome IS NULL AND r.session_id IN (SELECT id FROM sessions WHERE ticket_id = ?)
		 ORDER BY r.id`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("open runs for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []OpenRun
	for rows.Next() {
		var r OpenRun
		var pgid sql.NullInt64
		var procStart, startedAt sql.NullString
		if err := rows.Scan(&r.RunID, &r.Job, &pgid, &procStart, &startedAt); err != nil {
			return nil, fmt.Errorf("scan open run for ticket %d: %w", ticketID, err)
		}
		if pgid.Valid {
			n := int(pgid.Int64)
			r.PGID = &n
		}
		if procStart.Valid {
			r.ProcStart = &procStart.String
		}
		if startedAt.Valid {
			ts, perr := time.Parse(fixedTimeLayout, startedAt.String)
			if perr != nil {
				return nil, fmt.Errorf("parse started_at for run %d: %w", r.RunID, perr)
			}
			r.StartedAt = &ts
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("open runs for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// InterruptRuns marks every run of ticketID with a null outcome as
// outcome='error', interrupted=1, exit_code=-1, and agent_seconds computed
// from started_at to now (or the run's existing agent_seconds, floored at
// 0, when started_at is NULL), and clears the ticket's claim, all in one
// transaction fenced on owner and expires matching the ticket's live claim
// exactly -- the same "lease changed out from under this write" fence
// Reserve uses (design section 5.3). applied is false, err nil, when the
// fence finds no matching claim.
func (s *Store) InterruptRuns(ctx context.Context, ticketID int64, owner string, expires time.Time) (applied bool, err error) {
	_, applied, err = s.interruptClaimedRuns(ctx, ticketID, owner, expires, interruptOpts{})
	return applied, err
}

// ReclaimClaim is InterruptRuns fenced on the foreign owner and expiry
// ForeignClaims reads (design section 5.3, 6.3): reclaiming a dead serve's
// claim and recording a shutdown interrupt are the same database write
// under a different caller's fence, so both share interruptClaimedRuns.
// check is the CHECK row ForeignClaims read and reclaim judged gone (nil
// when it read none). The row is deleted in the same transaction, but only
// while it is still exactly that record (its generation); a row
// recorded since, or one that appeared where none was read, keeps the
// claim (applied false) so a later pass can judge the new process (#55).
func (s *Store) ReclaimClaim(ctx context.Context, ticketID int64, owner string, expires time.Time, check *OpenCheck) (applied bool, err error) {
	_, applied, err = s.interruptClaimedRuns(ctx, ticketID, owner, expires, interruptOpts{
		AfterFence: func(tx *sql.Tx) (bool, error) {
			if check != nil {
				return deleteCheckIfSame(ctx, tx, ticketID, *check)
			}
			_, present, presentErr := openCheckForTicket(ctx, tx, ticketID)
			return !present, presentErr
		},
	})
	return applied, err
}

// interruptOpts is interruptClaimedRuns' own optional behavior (r2f20): a
// bare positional nil, nil, nil at a call site is not self-describing, and
// a future caller adding a fourth mode would mean yet another positional
// nil at every existing one. The zero value is a plain interrupt
// (InterruptRuns).
type interruptOpts struct {
	// AfterFence runs after the claim fence passes (ReclaimClaim's own
	// generation check); false keeps the claim and writes nothing. Nil for
	// a plain interrupt or a park.
	AfterFence func(*sql.Tx) (bool, error)
	// Park, when set, is ParkRuns' own reset instant: every swept run's
	// capped_until is stamped with it, the settings claude_hold_until row
	// rises to it if later, and one "parked until" update is written.
	Park *time.Time
	// Finish is ParkRuns' own already-finished lens runs (owner decision
	// Q6), terminalized by their own real outcome before the sweep below
	// ever sees them, so they keep it instead of being swept as capped.
	Finish []Run
}

// interruptClaimedRuns is the shared transaction body behind InterruptRuns,
// ReclaimClaim and ParkRuns (design section 5.3; #45 for park): fence on the
// exact claim, run opts.AfterFence when set, terminalize every run named in
// opts.Finish by its own real outcome, then terminalize every run of
// ticketID's sessions still open as interrupted, then clear the claim.
// runIDs is the ascending ids of the runs swept (never opts.Finish's own),
// nil when none were open; a caller already holds opts.Finish's own ids, so
// they are not returned again.
func (s *Store) interruptClaimedRuns(
	ctx context.Context, ticketID int64, owner string, expires time.Time, opts interruptOpts,
) (runIDs []int64, applied bool, err error) {
	expires = truncateExpires(expires)
	now := time.Now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("interrupt runs: begin tx: %w", err)
	}
	defer rollback(tx)

	var fenced int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM tickets WHERE id = ? AND claim_owner = ? AND claim_expires_at = ?`,
		ticketID, owner, formatTime(expires)).Scan(&fenced)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("interrupt runs: fence ticket %d: %w", ticketID, err)
	}

	if opts.AfterFence != nil {
		proceed, fenceErr := opts.AfterFence(tx)
		if fenceErr != nil {
			return nil, false, fmt.Errorf("interrupt runs: ticket %d: %w", ticketID, fenceErr)
		}
		if !proceed {
			slog.Info("reclaim deferred: check command changed", "ticket_id", ticketID)
			return nil, false, nil
		}
	}

	for _, r := range opts.Finish {
		if finishErr := updateRunTx(ctx, tx, r, ticketID); finishErr != nil {
			return nil, false, fmt.Errorf("interrupt runs: finish run %d: %w", r.ID, finishErr)
		}
	}

	targets, err := interruptTargetsTx(ctx, tx, ticketID)
	if err != nil {
		return nil, false, fmt.Errorf("interrupt runs: %w", err)
	}

	parkParam := formatTimePtr(opts.Park)
	for _, target := range targets {
		seconds := interruptedAgentSeconds(target, now, expires)
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET outcome = 'error', interrupted = 1, exit_code = -1, agent_seconds = ?, capped_until = ? WHERE id = ?`,
			seconds, parkParam, target.runID,
		); err != nil {
			return nil, false, fmt.Errorf("interrupt runs: update run %d: %w", target.runID, err)
		}
		runIDs = append(runIDs, target.runID)
	}

	if opts.Park != nil && len(runIDs) != 0 {
		if err := upsertClaudeHoldTx(ctx, tx, *opts.Park); err != nil {
			return nil, false, fmt.Errorf("interrupt runs: ticket %d: %w", ticketID, err)
		}
		if err := s.insertParkedMarkerTx(ctx, tx, ticketID, *opts.Park, runIDs); err != nil {
			return nil, false, fmt.Errorf("interrupt runs: ticket %d: %w", ticketID, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE tickets SET claim_owner = NULL, claim_expires_at = NULL WHERE id = ?`, ticketID,
	); err != nil {
		return nil, false, fmt.Errorf("interrupt runs: clear ticket %d: %w", ticketID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("interrupt runs: commit tx: %w", err)
	}

	for _, target := range targets {
		slog.Warn("run interrupted", "ticket_id", ticketID, "run_id", target.runID, "job", target.job)
	}
	for _, r := range opts.Finish {
		slog.Info("run finished by park", "ticket_id", ticketID, "run_id", r.ID, "outcome", stringOrEmpty(r.Outcome))
	}
	return runIDs, true, nil
}

// stringOrEmpty is *string's own "" when nil, for a log attribute that must
// not itself be a typed nil pointer.
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// interruptTarget is one open run interruptClaimedRuns is about to
// terminalize.
type interruptTarget struct {
	runID        int64
	job          string
	startedAt    *time.Time
	agentSeconds *int
}

// interruptTargetsTx returns every run with a null outcome on ticketID's
// sessions, the exact set interruptClaimedRuns's UPDATE is about to
// terminalize, read before the update so each can be logged by id and job.
// Ordered by id ascending, so a park's own runIDs (and its marker body) are
// deterministic.
func interruptTargetsTx(ctx context.Context, tx *sql.Tx, ticketID int64) ([]interruptTarget, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.id, s.job, r.started_at, r.agent_seconds FROM runs r
		 JOIN sessions s ON s.id = r.session_id
		 WHERE r.outcome IS NULL AND r.session_id IN (SELECT id FROM sessions WHERE ticket_id = ?)
		 ORDER BY r.id`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("select open runs for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []interruptTarget
	for rows.Next() {
		var target interruptTarget
		var startedAt sql.NullString
		var agentSeconds sql.NullInt64
		if err := rows.Scan(&target.runID, &target.job, &startedAt, &agentSeconds); err != nil {
			return nil, fmt.Errorf("scan open run for ticket %d: %w", ticketID, err)
		}
		if startedAt.Valid {
			ts, perr := time.Parse(fixedTimeLayout, startedAt.String)
			if perr != nil {
				return nil, fmt.Errorf("parse started_at for run %d: %w", target.runID, perr)
			}
			target.startedAt = &ts
		}
		if agentSeconds.Valid {
			n := int(agentSeconds.Int64)
			target.agentSeconds = &n
		}
		out = append(out, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select open runs for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// interruptedAgentSeconds computes the agent_seconds interruptClaimedRuns
// writes for target (design section 5.3, PR review fix E1): when
// started_at is set, the whole seconds from started_at to end, where end
// is now capped at claimExpires -- the agent cannot legitimately run longer
// than its own claim's lease (timeout + claimGrace), so a reclaim that
// happens long after the lease lapsed (a replacement serve starting hours
// after a crash) must not charge the ticket for how long the dead serve
// sat down. Either way, the result is floored at the run's own existing
// agent_seconds (0 when that is also NULL): a clock skew or a started_at
// recorded after end can never undercharge below what was already there,
// and an existing value this run already earned is never reduced.
func interruptedAgentSeconds(target interruptTarget, now, claimExpires time.Time) int {
	existing := 0
	if target.agentSeconds != nil {
		existing = max(*target.agentSeconds, 0)
	}
	if target.startedAt == nil {
		return existing
	}
	end := now
	if claimExpires.Before(end) {
		end = claimExpires
	}
	span := int(end.Sub(*target.startedAt) / time.Second)
	return max(max(span, 0), existing)
}
