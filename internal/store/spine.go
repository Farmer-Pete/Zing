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
// first if no project by that name exists yet.
func (s *Store) EnsureProject(ctx context.Context, p Project) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, p.Name).Scan(&id)
	switch {
	case err == nil:
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
// or before now, and returns the ids it cleared. RETURNING makes the clear
// and the id collection one statement, so no ticket clears between the scan
// that finds it and the update that would otherwise re-select it.
func (s *Store) ExpireClaims(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE tickets SET claim_owner = NULL, claim_expires_at = NULL
		 WHERE claim_owner IS NOT NULL AND claim_expires_at IS NOT NULL AND claim_expires_at <= ?
		 RETURNING id`,
		formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("expire claims: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("expire claims: %w", err)
		}
		ids = append(ids, id)
		slog.Info("claim expired", "ticket_id", id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("expire claims: %w", err)
	}
	return ids, nil
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
