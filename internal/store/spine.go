package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// EnsureProject returns the id of the project named p.Name, inserting it
// first if no project by that name exists yet. For an existing project
// whose stored default_branch differs from a non-empty p.DefaultBranch, it
// reconciles the column to p.DefaultBranch first, so a default branch
// edited in zing.toml is picked up on the next start rather than silently
// ignored (PKG5-PLAN.md section 9).
func (s *Store) EnsureProject(ctx context.Context, p Project) (int64, error) {
	var (
		id           int64
		storedBranch string
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, default_branch FROM projects WHERE name = ?`, p.Name).Scan(&id, &storedBranch)
	switch {
	case err == nil:
		if p.DefaultBranch != "" && p.DefaultBranch != storedBranch {
			if _, updateErr := s.db.ExecContext(ctx, `UPDATE projects SET default_branch = ? WHERE id = ?`, p.DefaultBranch, id); updateErr != nil {
				return 0, fmt.Errorf("ensure project %s: reconcile default_branch: %w", p.Name, updateErr)
			}
			slog.Info("project default_branch reconciled", "project_id", id, "name", p.Name,
				"from", storedBranch, "to", p.DefaultBranch)
		}
		slog.Info("project ensured", "project_id", id, "name", p.Name, "branch", "existing")
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("ensure project %s: %w", p.Name, err)
	}

	defaultBranch := p.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, local_path, tracker, default_branch) VALUES (?, ?, ?, ?, ?)`,
		p.Name, p.RepoURL, p.LocalPath, p.Tracker, defaultBranch)
	if err != nil {
		return 0, fmt.Errorf("ensure project %s: insert: %w", p.Name, err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ensure project %s: %w", p.Name, err)
	}
	slog.Info("project ensured", "project_id", id, "name", p.Name, "branch", "inserted")
	return id, nil
}

// InsertTicket inserts t, which must be in the "queued" state (every ticket
// starts there; a later state is reached only through a committed
// transition, commit.go's job). UNIQUE(project_id, tracker_ref) rejects a
// duplicate intake of the same tracker ref on the same project.
func (s *Store) InsertTicket(ctx context.Context, t Ticket) (int64, error) {
	if t.State != ticketStateQueued {
		return 0, fmt.Errorf("insert ticket: state must be queued, got %q", t.State)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tickets (project_id, tracker_ref, title, body, kind, state, waiting_on, parent_ticket_id, branch, pr_url, claim_owner, claim_expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ProjectID, t.TrackerRef, t.Title, t.Body, t.Kind, t.State, t.WaitingOn,
		t.ParentTicketID, t.Branch, t.PRURL, t.ClaimOwner, formatTimePtr(t.ClaimExpiresAt),
	)
	if err != nil {
		return 0, fmt.Errorf("insert ticket %s: %w", t.TrackerRef, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert ticket %s: %w", t.TrackerRef, err)
	}
	slog.Info("ticket inserted", "ticket_id", id, "tracker_ref", t.TrackerRef, "branch", ticketStateQueued)
	return id, nil
}

// truncateExpires normalizes a claim/commit lease expiry to whole-second,
// UTC precision, matching the precision the claim_expires_at TEXT column
// round-trips through (formatTime, rows.go, uses RFC 3339 with no
// fractional seconds). Claim and CommitHandlerResult each call this on
// their own incoming expires, so a caller that hands the identical
// time.Time to both gets a fence that matches without truncating it itself
// (design section 6.3).
func truncateExpires(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}

// Claim conditionally sets the claim owner and expiry on ticket id, only
// when it is currently unclaimed. It returns whether the claim was taken.
func (s *Store) Claim(ctx context.Context, id int64, owner string, expires time.Time) (bool, error) {
	expires = truncateExpires(expires)
	res, err := s.db.ExecContext(ctx,
		`UPDATE tickets SET claim_owner = ?, claim_expires_at = ? WHERE id = ? AND claim_owner IS NULL`,
		owner, formatTime(expires), id)
	if err != nil {
		return false, fmt.Errorf("claim ticket %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim ticket %d: %w", id, err)
	}
	claimed := n == 1
	branch := "refused"
	if claimed {
		branch = "claimed"
	}
	slog.Info("claim attempted", "ticket_id", id, "owner", owner, "branch", branch)
	return claimed, nil
}

// ExpireClaims clears the claim on every ticket whose claim_expires_at is at
// or before now, and returns the ids it cleared. onlyOwner, when non-empty,
// scopes the expiry to claims owned by exactly that owner (design section
// 4.2, 5.3): with a lock-holding serve reclaiming foreign claims itself
// (dispatch.Config.ReclaimForeign), a foreign claim must never be expired
// here, so a live orphan's claim is never cleared out from under reclaim.
// Every caller but the dispatcher's ReclaimForeign path passes "", today's
// behavior of expiring every owner's claims. It runs in one transaction:
// for each expiring ticket, reconcileReservedRunsTx (design D13, section 4.5)
// terminalizes any run left reserved with no outcome -- a crash, or an
// ErrCanceled shutdown that left no commit -- before that ticket's own claim
// is cleared, so the reconcile and the clear land together. Sharing one
// BeginTx across the read and every clear gives the same atomicity the prior
// single RETURNING statement did (SetMaxOpenConns(1) means this Store's sole
// connection is held for the whole transaction, so no other write can land
// between the scan that finds an expiring ticket and the clear that follows
// it).
func (s *Store) ExpireClaims(ctx context.Context, now time.Time, onlyOwner string) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("expire claims: begin tx: %w", err)
	}
	defer rollback(tx)

	ids, err := expiringTicketIDsTx(ctx, tx, now, onlyOwner)
	if err != nil {
		return nil, fmt.Errorf("expire claims: %w", err)
	}

	for _, id := range ids {
		if err := reconcileReservedRunsTx(ctx, tx, id); err != nil {
			return nil, fmt.Errorf("expire claims: reconcile ticket %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET claim_owner = NULL, claim_expires_at = NULL WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("expire claims: clear ticket %d: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("expire claims: commit tx: %w", err)
	}

	for _, id := range ids {
		slog.Info("claim expired", "ticket_id", id)
	}
	return ids, nil
}

// expiringTicketIDsTx returns every ticket id whose claim is set and expires
// at or before now, the set ExpireClaims reconciles and clears inside its
// one transaction. onlyOwner, when non-empty, additionally restricts the set
// to claims owned by exactly that owner (ExpireClaims above).
func expiringTicketIDsTx(ctx context.Context, tx *sql.Tx, now time.Time, onlyOwner string) ([]int64, error) {
	query := `SELECT id FROM tickets
		 WHERE claim_owner IS NOT NULL AND claim_expires_at IS NOT NULL AND claim_expires_at <= ?`
	args := []any{formatTime(now)}
	if onlyOwner != "" {
		query += ` AND claim_owner = ?`
		args = append(args, onlyOwner)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select expiring: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("select expiring: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select expiring: %w", err)
	}
	return ids, nil
}

// reservedRunTarget is one runs row reconcileReservedRunsTx is about to
// terminalize: enough to log it (design section 9's "reserved run
// reconciled" event), read before the update so the log can name each
// affected run by id, session, and job.
type reservedRunTarget struct {
	runID, sessionID int64
	job              string
}

// reservedRunTargetsTx returns every run with no outcome yet on any session
// belonging to ticketID -- the exact set the UPDATE in
// reconcileReservedRunsTx is about to terminalize.
func reservedRunTargetsTx(ctx context.Context, tx *sql.Tx, ticketID int64) ([]reservedRunTarget, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.id, r.session_id, s.job FROM runs r
		 JOIN sessions s ON s.id = r.session_id
		 WHERE r.outcome IS NULL AND r.session_id IN (SELECT id FROM sessions WHERE ticket_id = ?)`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("select reserved runs for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []reservedRunTarget
	for rows.Next() {
		var r reservedRunTarget
		if err := rows.Scan(&r.runID, &r.sessionID, &r.job); err != nil {
			return nil, fmt.Errorf("scan reserved run for ticket %d: %w", ticketID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select reserved runs for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// reconcileReservedRunsTx terminalizes every run left with a null outcome on
// ticketID's sessions as outcome=error, exit_code=-1, agent_seconds floored
// at 0 (design D13, section 4.5): the placeholder Reserve inserted under a
// claim that expired before any handler commit landed, whether that is a
// crash or an ErrCanceled shutdown that deliberately left the lease to
// expire rather than releasing it. It runs inside tx, before ExpireClaims
// clears ticketID's claim, so the reconcile and the claim clear share one
// commit.
func reconcileReservedRunsTx(ctx context.Context, tx *sql.Tx, ticketID int64) error {
	targets, err := reservedRunTargetsTx(ctx, tx, ticketID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET outcome = 'error', exit_code = -1, agent_seconds = COALESCE(agent_seconds, 0)
		 WHERE outcome IS NULL AND session_id IN (SELECT id FROM sessions WHERE ticket_id = ?)`,
		ticketID); err != nil {
		return fmt.Errorf("reconcile reserved runs for ticket %d: %w", ticketID, err)
	}

	for _, r := range targets {
		slog.Warn("reserved run reconciled", "ticket_id", ticketID, "session_id", r.sessionID, "run_id", r.runID, "job", r.job)
	}
	return nil
}

// SetDraining sets the "draining" setting flag.
func (s *Store) SetDraining(ctx context.Context, on bool) error {
	return s.setFlag(ctx, "draining", on)
}

// SetStopped sets the "stopped" setting flag.
func (s *Store) SetStopped(ctx context.Context, on bool) error {
	return s.setFlag(ctx, "stopped", on)
}

func (s *Store) setFlag(ctx context.Context, key string, on bool) error {
	value := "false"
	if on {
		value = "true"
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, value, key); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	return nil
}

// Flags reads the "draining" and "stopped" settings flags.
func (s *Store) Flags(ctx context.Context) (draining, stopped bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN ('draining', 'stopped')`)
	if err != nil {
		return false, false, fmt.Errorf("read flags: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var value sql.NullString
		if err := rows.Scan(&key, &value); err != nil {
			return false, false, fmt.Errorf("read flags: %w", err)
		}
		on := value.Valid && value.String == "true"
		switch key {
		case "draining":
			draining = on
		case "stopped":
			stopped = on
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("read flags: %w", err)
	}
	return draining, stopped, nil
}
