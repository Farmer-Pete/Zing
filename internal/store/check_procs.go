package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CheckKind is a CHECK command's kind: one of CheckKindFix, CheckKindLint
// or CheckKindTest. Each value must be in migration 0009's check_procs.kind
// CHECK list; TestCheckKindsAccepted enforces this by recording every value
// of CheckKinds against the migrated schema.
type CheckKind string

// The CHECK command kinds that check_procs.kind accepts (migration 0009).
const (
	CheckKindFix  CheckKind = "fix"
	CheckKindLint CheckKind = "lint"
	CheckKindTest CheckKind = "test"
)

// CheckKinds returns the three kinds check_procs.kind accepts, in CHECK's
// run order: fix, lint, test.
func CheckKinds() []CheckKind { return []CheckKind{CheckKindFix, CheckKindLint, CheckKindTest} }

// RecordCheckStart records the process group of the CHECK command a ticket's
// claim holder just started (#55, #45 parity), replacing any earlier row:
// only the claim holder runs CHECK, so a ticket never has two commands at
// once. It is fenced on the live claim exactly as Reserve is, so a serve
// that lost its claim records nothing and gets ErrClaimLost. procStart ""
// is stored as NULL (an unverified group); pgid <= 0 is an error and
// nothing is written. It returns the record's generation, which the caller
// passes to ClearCheckStart: no later record ever reuses it.
func (s *Store) RecordCheckStart(ctx context.Context, ticketID int64, owner string, expires time.Time, kind CheckKind, pgid int, procStart string, startedAt, budgetStartedAt time.Time) (int64, error) {
	if pgid <= 0 {
		return 0, fmt.Errorf("record check start: ticket %d: pgid %d is not a process group", ticketID, pgid)
	}
	var procStartParam *string
	if procStart != "" {
		procStartParam = &procStart
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("record check start: begin tx: %w", err)
	}
	defer rollback(tx)

	var fenced int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM tickets WHERE id = ? AND claim_owner = ? AND claim_expires_at = ?`,
		ticketID, owner, formatTime(truncateExpires(expires))).Scan(&fenced)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrClaimLost
	case err != nil:
		return 0, fmt.Errorf("record check start: fence ticket %d: %w", ticketID, err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		ticketID, string(kind), pgid, procStartParam, formatTime(startedAt), formatTime(budgetStartedAt))
	if err != nil {
		return 0, fmt.Errorf("record check start: ticket %d: %w", ticketID, err)
	}
	gen, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record check start: ticket %d: gen: %w", ticketID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("record check start: commit tx: %w", err)
	}
	return gen, nil
}

// ClearCheckStart deletes ticketID's CHECK row once its command has ended.
// It matches on the record's generation, so it never deletes a newer
// command's row.
func (s *Store) ClearCheckStart(ctx context.Context, ticketID, gen int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM check_procs WHERE ticket_id = ? AND gen = ?`, ticketID, gen,
	); err != nil {
		return fmt.Errorf("clear check start: ticket %d: %w", ticketID, err)
	}
	return nil
}

// OpenCheck is a ticket's recorded CHECK command: enough identity for the
// dispatcher to decide whether its process group is still alive.
type OpenCheck struct {
	Gen             int64 // the record's generation, unique for all time
	Kind            CheckKind
	PGID            int
	ProcStart       *string
	StartedAt       time.Time
	BudgetStartedAt time.Time
}

// AsOpenRun presents c as an open build run whose start is the check
// budget's start, so the dispatcher's orphan rules and deadline formula
// (StartedAt + the build timeout + claim grace) apply to it unchanged.
func (c OpenCheck) AsOpenRun() OpenRun {
	return OpenRun{RunID: 0, Job: "build", PGID: &c.PGID, ProcStart: c.ProcStart, StartedAt: &c.BudgetStartedAt}
}

// openCheckForTicket reads ticketID's CHECK row; ok is false when there is
// none.
func openCheckForTicket(ctx context.Context, q queryer, ticketID int64) (OpenCheck, bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT gen, kind, pgid, proc_start, started_at, budget_started_at FROM check_procs WHERE ticket_id = ?`, ticketID)
	if err != nil {
		return OpenCheck{}, false, fmt.Errorf("open check for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return OpenCheck{}, false, fmt.Errorf("open check for ticket %d: %w", ticketID, err)
		}
		return OpenCheck{}, false, nil
	}
	var c OpenCheck
	var procStart sql.NullString
	var startedAt, budgetStartedAt string
	if err = rows.Scan(&c.Gen, &c.Kind, &c.PGID, &procStart, &startedAt, &budgetStartedAt); err != nil {
		return OpenCheck{}, false, fmt.Errorf("scan open check for ticket %d: %w", ticketID, err)
	}
	if procStart.Valid {
		c.ProcStart = &procStart.String
	}
	if c.StartedAt, err = time.Parse(fixedTimeLayout, startedAt); err != nil {
		return OpenCheck{}, false, fmt.Errorf("parse check started_at for ticket %d: %w", ticketID, err)
	}
	if c.BudgetStartedAt, err = time.Parse(fixedTimeLayout, budgetStartedAt); err != nil {
		return OpenCheck{}, false, fmt.Errorf("parse check budget_started_at for ticket %d: %w", ticketID, err)
	}
	return c, true, nil
}

// execer is satisfied by *sql.DB and *sql.Tx alike.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// deleteCheckIfSame deletes ticketID's CHECK row only while it is still
// exactly c's record (its generation), and reports whether it did. pgid and
// proc_start alone cannot tell a replacement with the same pgid and no
// start token apart.
func deleteCheckIfSame(ctx context.Context, e execer, ticketID int64, c OpenCheck) (bool, error) {
	res, err := e.ExecContext(ctx,
		`DELETE FROM check_procs WHERE ticket_id = ? AND gen = ?`, ticketID, c.Gen)
	if err != nil {
		return false, fmt.Errorf("delete check for ticket %d: %w", ticketID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete check for ticket %d: %w", ticketID, err)
	}
	return n > 0, nil
}

// ClearDeadCheck deletes ticketID's CHECK row once the dispatcher has judged
// c's process gone, but only while the row still names exactly c's process:
// a command started since is never cleared. ok reports whether it did.
func (s *Store) ClearDeadCheck(ctx context.Context, ticketID int64, c OpenCheck) (bool, error) {
	return deleteCheckIfSame(ctx, s.db, ticketID, c)
}

// ExpiringCheck is one expiring claim whose ticket still records a CHECK
// command.
type ExpiringCheck struct {
	TicketID int64
	Check    OpenCheck
}

// ExpiringChecks returns every claim ExpireClaims(now, onlyOwner) would
// consider that still records a CHECK command. ExpireClaims skips those
// tickets, so the dispatcher judges each command's process group first and
// clears the row of a gone one with ClearDeadCheck.
func (s *Store) ExpiringChecks(ctx context.Context, now time.Time, onlyOwner string) ([]ExpiringCheck, error) {
	ids, err := s.expiringCheckTicketIDs(ctx, now, onlyOwner)
	if err != nil {
		return nil, fmt.Errorf("expiring checks: %w", err)
	}
	out := make([]ExpiringCheck, 0, len(ids))
	for _, id := range ids {
		c, ok, err := openCheckForTicket(ctx, s.db, id)
		if err != nil {
			return nil, fmt.Errorf("expiring checks: %w", err)
		}
		if ok {
			out = append(out, ExpiringCheck{TicketID: id, Check: c})
		}
	}
	return out, nil
}

// expiringCheckTicketIDs returns the ids ExpiringChecks reports. It reads
// every id before ExpiringChecks queries again, since the store holds one
// connection.
func (s *Store) expiringCheckTicketIDs(ctx context.Context, now time.Time, onlyOwner string) ([]int64, error) {
	query := `SELECT t.id FROM tickets t JOIN check_procs c ON c.ticket_id = t.id
		 WHERE t.claim_owner IS NOT NULL AND t.claim_expires_at IS NOT NULL AND t.claim_expires_at <= ?`
	args := []any{formatTime(now)}
	if onlyOwner != "" {
		query += ` AND t.claim_owner = ?`
		args = append(args, onlyOwner)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CheckProc returns ticketID's recorded CHECK command; ok is false when none
// is recorded.
func (s *Store) CheckProc(ctx context.Context, ticketID int64) (OpenCheck, bool, error) {
	return openCheckForTicket(ctx, s.db, ticketID)
}
