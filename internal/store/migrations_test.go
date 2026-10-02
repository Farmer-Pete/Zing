package store

import (
	"strings"
	"testing"
)

// TestMigration0004Columns proves migration 0004_ticket_poll.sql installed
// the three nullable poll columns and their CHECK constraints, and the
// tickets_next_poll_idx index (design section 16, DD1): on a database that
// already carries rows, poll_interval_s refuses 20 (outside 30 to 300) and
// poll_fingerprint refuses a 10-character value (not 64), while a legal
// write of all three columns round-trips.
func TestMigration0004Columns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)

	found := ticketColumnNames(t, s)
	for _, col := range []string{"next_poll_at", "poll_interval_s", "poll_fingerprint"} {
		if !found[col] {
			t.Errorf("tickets.%s column not found after migration 0004", col)
		}
	}

	var indexName string
	if err := s.db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'tickets_next_poll_idx'").Scan(&indexName); err != nil {
		t.Errorf("tickets_next_poll_idx: %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE tickets SET poll_interval_s = 20 WHERE id = 1`); err == nil {
		t.Error("UPDATE poll_interval_s = 20: want a CHECK constraint error, got nil")
	}

	shortFingerprint := strings.Repeat("a", 10)
	if _, err := s.db.ExecContext(ctx, `UPDATE tickets SET poll_fingerprint = ? WHERE id = 1`, shortFingerprint); err == nil {
		t.Error("UPDATE poll_fingerprint = <10 chars>: want a CHECK constraint error, got nil")
	}

	fingerprint := strings.Repeat("a", 64)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tickets SET next_poll_at = ?, poll_interval_s = ?, poll_fingerprint = ? WHERE id = 1`,
		"2026-10-01T00:00:00Z", 30, fingerprint,
	); err != nil {
		t.Fatalf("UPDATE with legal poll values: %v", err)
	}

	var gotNextPollAt, gotFingerprint string
	var gotIntervalS int
	if err := s.db.QueryRowContext(ctx,
		"SELECT next_poll_at, poll_interval_s, poll_fingerprint FROM tickets WHERE id = 1",
	).Scan(&gotNextPollAt, &gotIntervalS, &gotFingerprint); err != nil {
		t.Fatalf("read poll columns: %v", err)
	}
	if gotNextPollAt != "2026-10-01T00:00:00Z" || gotIntervalS != 30 || gotFingerprint != fingerprint {
		t.Errorf("poll columns = (%q, %d, %q), want (%q, 30, %q)",
			gotNextPollAt, gotIntervalS, gotFingerprint, "2026-10-01T00:00:00Z", fingerprint)
	}
}

// ticketColumnNames returns the set of column names PRAGMA table_info
// reports for the tickets table.
func ticketColumnNames(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	return tableColumnNames(t, s, "tickets")
}

// tableColumnNames returns the set of column names PRAGMA table_info
// reports for table.
func tableColumnNames(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if scanErr := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); scanErr != nil {
			t.Fatalf("scan table_info row: %v", scanErr)
		}
		found[name] = true
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("table_info rows: %v", rowsErr)
	}
	return found
}

// TestMigration0005Columns proves migration 0005_runs_interrupt.sql installed
// the four new runs columns and their CHECK constraints (design section
// 5.1, D6): interrupted refuses anything but 0 or 1, pgid refuses a
// non-positive value, proc_start refuses an empty string, and a legal write
// of all four round-trips. It also proves rolling compatibility: an
// existing run row (inserted before this migration's columns existed, in
// spirit -- every column here is nullable or defaulted) reads interrupted
// back as 0.
func TestMigration0005Columns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, 1, 'planning', 'claude')`,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	found := tableColumnNames(t, s, "runs")
	for _, col := range []string{"interrupted", "pgid", "proc_start", "started_at"} {
		if !found[col] {
			t.Errorf("runs.%s column not found after migration 0005", col)
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (session_id, turn) VALUES (1, 0)`); err != nil {
		t.Fatalf("insert run with defaults: %v", err)
	}
	var interrupted int
	if err := s.db.QueryRowContext(ctx, `SELECT interrupted FROM runs WHERE session_id = 1 AND turn = 0`).Scan(&interrupted); err != nil {
		t.Fatalf("read back interrupted default: %v", err)
	}
	if interrupted != 0 {
		t.Errorf("interrupted default = %d, want 0", interrupted)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE runs SET interrupted = 2 WHERE session_id = 1 AND turn = 0`); err == nil {
		t.Error("UPDATE interrupted = 2: want a CHECK constraint error, got nil")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runs SET pgid = 0 WHERE session_id = 1 AND turn = 0`); err == nil {
		t.Error("UPDATE pgid = 0: want a CHECK constraint error, got nil")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runs SET pgid = -1 WHERE session_id = 1 AND turn = 0`); err == nil {
		t.Error("UPDATE pgid = -1: want a CHECK constraint error, got nil")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runs SET proc_start = '' WHERE session_id = 1 AND turn = 0`); err == nil {
		t.Error("UPDATE proc_start = '': want a CHECK constraint error, got nil")
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE runs SET interrupted = 1, pgid = 4242, proc_start = ?, started_at = ? WHERE session_id = 1 AND turn = 0`,
		"123.000456", "2026-10-02T00:00:00Z",
	); err != nil {
		t.Fatalf("UPDATE with legal values: %v", err)
	}

	var gotPGID int
	var gotProcStart, gotStartedAt string
	if err := s.db.QueryRowContext(ctx,
		`SELECT interrupted, pgid, proc_start, started_at FROM runs WHERE session_id = 1 AND turn = 0`,
	).Scan(&interrupted, &gotPGID, &gotProcStart, &gotStartedAt); err != nil {
		t.Fatalf("read back legal values: %v", err)
	}
	if interrupted != 1 || gotPGID != 4242 || gotProcStart != "123.000456" || gotStartedAt != "2026-10-02T00:00:00Z" {
		t.Errorf("runs columns = (%d, %d, %q, %q), want (1, 4242, %q, %q)",
			interrupted, gotPGID, gotProcStart, gotStartedAt, "123.000456", "2026-10-02T00:00:00Z")
	}
}
