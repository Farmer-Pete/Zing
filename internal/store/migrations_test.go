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
	rows, err := s.db.QueryContext(t.Context(), "PRAGMA table_info(tickets)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(tickets): %v", err)
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
