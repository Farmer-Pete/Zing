package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/adlio/schema"
)

// Migration file names shared by the tests that apply a prefix of them by
// hand through a raw sql.Open handle.
const (
	migFile0001 = "0001_init.sql"
	migFile0002 = "0002_message_created_at.sql"
	migFile0003 = "0003_sessions_external_id_nonempty.sql"
	migFile0004 = "0004_ticket_poll.sql"
	migFile0005 = "0005_runs_interrupt.sql"
	migFile0006 = "0006_check_procs.sql"
	migFile0007 = "0007_run_evidence.sql"
	migFile0008 = "0008_message_event_kind.sql"

	// testTimestamp is an arbitrary fixed timestamp used where a test needs
	// one but the value itself is not under test.
	testTimestamp = "2026-10-03T00:00:00Z"
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

// TestMigration0006CheckProcs proves migration 0006_check_procs.sql created
// the check_procs table and its CHECK constraints (#55): pgid 0, an
// unknown kind, and an empty proc_start are refused, and a legal row is
// accepted.
func TestMigration0006CheckProcs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)

	found := tableColumnNames(t, s, "check_procs")
	for _, col := range []string{"ticket_id", "kind", "pgid", "proc_start", "started_at", "budget_started_at"} {
		if !found[col] {
			t.Errorf("check_procs.%s column not found after migration 0006", col)
		}
	}
	const insert = `INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (1, ?, ?, NULL, '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`
	if _, err := s.db.ExecContext(ctx, insert, "test", 0); err == nil {
		t.Error("INSERT pgid 0: want a CHECK constraint error, got nil")
	}
	if _, err := s.db.ExecContext(ctx, insert, "fmt", 4242); err == nil {
		t.Error("INSERT kind fmt: want a CHECK constraint error, got nil")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (1, 'test', 4242, '', '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`,
	); err == nil {
		t.Error("INSERT proc_start '': want a CHECK constraint error, got nil")
	}
	if _, err := s.db.ExecContext(ctx, insert, "lint", 4242); err != nil {
		t.Errorf("INSERT a legal row: %v", err)
	}
}

// TestMigration0007_AppliesOverPopulated0006 proves migration
// 0007_run_evidence.sql applies cleanly to a database already carrying rows
// through 0006: it adds final_message, stderr_path and transcript_path to
// runs, leaves existing rows NULL in all three, rejects an empty-string
// write to any of them, and rejects a final_message over the 65536-byte
// cap (the CHECK clauses measure bytes via length(CAST(x AS BLOB))).
func TestMigration0007_AppliesOverPopulated0006(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := dbPath(t)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	applyMigration := func(pattern string) {
		t.Helper()
		migrator := schema.NewMigrator(schema.WithDialect(schema.SQLite), schema.WithContext(ctx))
		migs, fsErr := schema.FSMigrations(migrationsFS, pattern)
		if fsErr != nil {
			t.Fatalf("FSMigrations(%s): %v", pattern, fsErr)
		}
		if applyErr := migrator.Apply(db, migs); applyErr != nil {
			t.Fatalf("apply %s: %v", pattern, applyErr)
		}
	}

	for _, file := range []string{
		migFile0001, migFile0002, migFile0003,
		migFile0004, migFile0005, migFile0006,
	} {
		applyMigration("migrations/" + file)
	}

	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_url, local_path, tracker) VALUES (1, 'zing', 'https://github.com/x/zing', '/tmp/zing', 'github')`,
	); execErr != nil {
		t.Fatalf("seed project: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state) VALUES (1, 1, '42', 'fix the bug', 'queued')`,
	); execErr != nil {
		t.Fatalf("seed ticket: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, 1, 'planning', 'claude')`,
	); execErr != nil {
		t.Fatalf("seed session: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO runs (id, session_id, turn) VALUES (1, 1, 0)`,
	); execErr != nil {
		t.Fatalf("seed run 1: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO runs (id, session_id, turn) VALUES (2, 1, 1)`,
	); execErr != nil {
		t.Fatalf("seed run 2: %v", execErr)
	}

	applyMigration("migrations/" + migFile0007)

	rows, err := db.QueryContext(ctx,
		`SELECT id, final_message, stderr_path, transcript_path FROM runs ORDER BY id`)
	if err != nil {
		t.Fatalf("query runs after migration: %v", err)
	}
	defer rows.Close()

	var got []int64
	for rows.Next() {
		var id int64
		var finalMessage, stderrPath, transcriptPath sql.NullString
		if scanErr := rows.Scan(&id, &finalMessage, &stderrPath, &transcriptPath); scanErr != nil {
			t.Fatalf("scan run row: %v", scanErr)
		}
		if finalMessage.Valid || stderrPath.Valid || transcriptPath.Valid {
			t.Errorf("run %d evidence columns = (%v, %v, %v), want all NULL", id, finalMessage, stderrPath, transcriptPath)
		}
		got = append(got, id)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("run rows: %v", rowsErr)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("run ids after migration = %v, want [1 2]", got)
	}

	for _, col := range []string{"final_message", "stderr_path", "transcript_path"} {
		if _, execErr := db.ExecContext(ctx, `UPDATE runs SET `+col+` = '' WHERE id = 1`); execErr == nil {
			t.Errorf("UPDATE %s = '': want a CHECK constraint error, got nil", col)
		}
	}

	// hex(randomblob(32769)) is 65538 ASCII bytes, just over the 65536-byte cap.
	if _, execErr := db.ExecContext(ctx,
		`UPDATE runs SET final_message = hex(randomblob(32769)) WHERE id = 1`,
	); execErr == nil {
		t.Error("UPDATE final_message to 65538 bytes: want a CHECK constraint error, got nil")
	}
}

// TestMigration0008_AppliesOverPopulated0007 proves migration
// 0008_message_event_kind.sql applies cleanly to a database already
// carrying rows through 0007: event_kind on an existing row reads back
// NULL, and the new column's CHECK ties a non-NULL event_kind to a
// non-empty value, type 'update', and a non-NULL payload.
func TestMigration0008_AppliesOverPopulated0007(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := dbPath(t)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	applyMigration := func(pattern string) {
		t.Helper()
		migrator := schema.NewMigrator(schema.WithDialect(schema.SQLite), schema.WithContext(ctx))
		migs, fsErr := schema.FSMigrations(migrationsFS, pattern)
		if fsErr != nil {
			t.Fatalf("FSMigrations(%s): %v", pattern, fsErr)
		}
		if applyErr := migrator.Apply(db, migs); applyErr != nil {
			t.Fatalf("apply %s: %v", pattern, applyErr)
		}
	}

	for _, file := range []string{
		migFile0001, migFile0002, migFile0003,
		migFile0004, migFile0005, migFile0006, migFile0007,
	} {
		applyMigration("migrations/" + file)
	}

	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_url, local_path, tracker) VALUES (1, 'zing', 'https://github.com/x/zing', '/tmp/zing', 'github')`,
	); execErr != nil {
		t.Fatalf("seed project: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state) VALUES (1, 1, '42', 'fix the bug', 'queued')`,
	); execErr != nil {
		t.Fatalf("seed ticket: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO messages (id, ticket_id, type, author, body) VALUES (1, 1, 'update', 'system', 'progress')`,
	); execErr != nil {
		t.Fatalf("seed update message: %v", execErr)
	}

	applyMigration("migrations/" + migFile0008)

	var eventKind sql.NullString
	if scanErr := db.QueryRowContext(ctx, `SELECT event_kind FROM messages WHERE id = 1`).Scan(&eventKind); scanErr != nil {
		t.Fatalf("read event_kind after migration: %v", scanErr)
	}
	if eventKind.Valid {
		t.Errorf("message 1 event_kind = %v, want NULL", eventKind)
	}

	rejects := []struct {
		name string
		stmt string
	}{
		{
			"event_kind on a non-update row with a payload",
			`INSERT INTO messages (id, ticket_id, type, author, payload, event_kind) VALUES (2, 1, 'state', 'system', '{}', 'check_rerun')`,
		},
		{
			"type update with event_kind set and a NULL payload",
			`INSERT INTO messages (id, ticket_id, type, author, event_kind) VALUES (3, 1, 'update', 'system', 'check_rerun')`,
		},
		{
			"empty event_kind",
			`INSERT INTO messages (id, ticket_id, type, author, payload, event_kind) VALUES (4, 1, 'update', 'system', '{}', '')`,
		},
	}
	for _, tc := range rejects {
		if _, execErr := db.ExecContext(ctx, tc.stmt); execErr == nil {
			t.Errorf("%s: want a CHECK constraint error, got nil", tc.name)
		}
	}

	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO messages (id, ticket_id, type, author, payload, event_kind) VALUES (5, 1, 'update', 'system', '{}', 'check_rerun')`,
	); execErr != nil {
		t.Errorf("insert valid event row: %v", execErr)
	}
}

// TestMigration0009_AppliesOverPopulated0008 proves migration
// 0009_check_procs_fix_kind.sql rebuilds check_procs over a database already
// carrying rows through 0008, with foreign_keys ON as store.Open runs it
// (migration 0003's rebuild failed under that same condition): every
// existing row survives with its gen and every other column unchanged, the
// AUTOINCREMENT high-water mark carries over so a new row gets a fresh gen
// rather than reusing a deleted one, the widened CHECK accepts kind fix
// while still rejecting an unknown kind, a non-positive pgid and an empty
// proc_start, the UNIQUE constraint on ticket_id still refuses a second row
// for the same ticket, PRAGMA foreign_key_check finds nothing broken, and
// check_procs still cascades on a deleted ticket.
func TestMigration0009_AppliesOverPopulated0008(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := dbPath(t)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	applyMigration := func(pattern string) {
		t.Helper()
		migrator := schema.NewMigrator(schema.WithDialect(schema.SQLite), schema.WithContext(ctx))
		migs, fsErr := schema.FSMigrations(migrationsFS, pattern)
		if fsErr != nil {
			t.Fatalf("FSMigrations(%s): %v", pattern, fsErr)
		}
		if applyErr := migrator.Apply(db, migs); applyErr != nil {
			t.Fatalf("apply %s: %v", pattern, applyErr)
		}
	}

	for _, file := range []string{
		migFile0001, migFile0002, migFile0003,
		migFile0004, migFile0005, migFile0006, migFile0007,
		migFile0008,
	} {
		applyMigration("migrations/" + file)
	}

	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_url, local_path, tracker) VALUES (1, 'zing', 'https://github.com/x/zing', '/tmp/zing', 'github')`,
	); execErr != nil {
		t.Fatalf("seed project: %v", execErr)
	}
	for _, id := range []int{1, 2, 3} {
		if _, execErr := db.ExecContext(ctx,
			`INSERT INTO tickets (id, project_id, tracker_ref, title, state) VALUES (?, 1, ?, 'fix the bug', 'queued')`,
			id, id,
		); execErr != nil {
			t.Fatalf("seed ticket %d: %v", id, execErr)
		}
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (1, 'test', 4242, '77.000001', '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`,
	); execErr != nil {
		t.Fatalf("seed check_procs for ticket 1: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (2, 'lint', 4300, NULL, '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`,
	); execErr != nil {
		t.Fatalf("seed check_procs for ticket 2: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx,
		`INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (3, 'test', 4400, NULL, '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`,
	); execErr != nil {
		t.Fatalf("seed check_procs for ticket 3: %v", execErr)
	}
	if _, execErr := db.ExecContext(ctx, `DELETE FROM check_procs WHERE ticket_id = 3`); execErr != nil {
		t.Fatalf("delete check_procs for ticket 3: %v", execErr)
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close raw handle: %v", closeErr)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	type row struct {
		gen                                    int64
		kind, procStart, startedAt, budgetedAt string
		pgid                                   int
	}
	read := func(ticketID int64) row {
		t.Helper()
		var r row
		var procStart sql.NullString
		if scanErr := s.db.QueryRowContext(ctx,
			`SELECT gen, kind, pgid, proc_start, started_at, budget_started_at FROM check_procs WHERE ticket_id = ?`, ticketID,
		).Scan(&r.gen, &r.kind, &r.pgid, &procStart, &r.startedAt, &r.budgetedAt); scanErr != nil {
			t.Fatalf("read check_procs for ticket %d: %v", ticketID, scanErr)
		}
		if procStart.Valid {
			r.procStart = procStart.String
		}
		return r
	}

	got1 := read(1)
	want1 := row{gen: 1, kind: "test", pgid: 4242, procStart: "77.000001", startedAt: testTimestamp, budgetedAt: testTimestamp}
	if got1 != want1 {
		t.Errorf("ticket 1 row = %+v, want %+v", got1, want1)
	}
	got2 := read(2)
	want2 := row{gen: 2, kind: "lint", pgid: 4300, startedAt: testTimestamp, budgetedAt: testTimestamp}
	if got2 != want2 {
		t.Errorf("ticket 2 row = %+v, want %+v", got2, want2)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (3, 'fix', 4500, NULL, '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("insert fix row for ticket 3: %v", err)
	}
	gen3, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if gen3 != 4 {
		t.Errorf("fix row gen = %d, want 4 (the deleted ticket 3 row's gen is never reused)", gen3)
	}

	rejects := []struct {
		name string
		stmt string
	}{
		{"unknown kind", `INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (1, 'fmt', 4242, NULL, '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`},
		{"non-positive pgid", `INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (1, 'fix', 0, NULL, '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`},
		{"empty proc_start", `INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (1, 'fix', 4242, '', '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`},
		{"second row for ticket 1", `INSERT INTO check_procs (ticket_id, kind, pgid, proc_start, started_at, budget_started_at) VALUES (1, 'lint', 4600, NULL, '2026-10-03T00:00:00Z', '2026-10-03T00:00:00Z')`},
	}
	for _, tc := range rejects {
		if _, execErr := s.db.ExecContext(ctx, tc.stmt); execErr == nil {
			t.Errorf("%s: want a constraint error, got nil", tc.name)
		}
	}

	// fkRows is scoped to this closure so its defer closes it before the
	// DELETE below runs: the store limits itself to one connection
	// (SetMaxOpenConns(1)), and that DELETE would block forever waiting for
	// it if a failing foreign_key_check left rows open until the test ended
	// (review r1f8).
	func() {
		fkRows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatalf("PRAGMA foreign_key_check: %v", err)
		}
		defer fkRows.Close()
		if fkRows.Next() {
			t.Error("PRAGMA foreign_key_check: want no rows, got at least one")
		}
		if fkErr := fkRows.Err(); fkErr != nil {
			t.Fatalf("foreign_key_check rows: %v", fkErr)
		}
	}()

	if _, execErr := s.db.ExecContext(ctx, `DELETE FROM tickets WHERE id = 2`); execErr != nil {
		t.Fatalf("delete ticket 2: %v", execErr)
	}
	if n := checkProcCount(t, s, 2); n != 0 {
		t.Errorf("check_procs rows for ticket 2 after its deletion = %d, want 0 (ON DELETE CASCADE)", n)
	}
}

// TestMigration0009CappedUntil proves migration 0009_runs_capped_until.sql
// installed runs.capped_until and its CHECK (#45): the column exists;
// setting it on a row with interrupted = 0 is rejected (capped_until ties to
// interrupted = 1); and a legal write round-trips through SessionNewestRun
// as Run.CappedUntil.
func TestMigration0009CappedUntil(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)
	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, 1, 'planning', 'claude')`,
	); execErr != nil {
		t.Fatalf("seed session: %v", execErr)
	}
	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO runs (id, session_id, turn) VALUES (1, 1, 0)`,
	); execErr != nil {
		t.Fatalf("seed run: %v", execErr)
	}

	found := tableColumnNames(t, s, "runs")
	if !found["capped_until"] {
		t.Error("runs.capped_until column not found after migration 0009")
	}

	if _, execErr := s.db.ExecContext(ctx,
		`UPDATE runs SET capped_until = '2026-10-05T16:20:00Z' WHERE id = 1`,
	); execErr == nil {
		t.Error("UPDATE capped_until with interrupted = 0: want a CHECK constraint error, got nil")
	}

	if _, execErr := s.db.ExecContext(ctx,
		`UPDATE runs SET interrupted = 1, capped_until = '2026-10-05T16:20:00Z' WHERE id = 1`,
	); execErr != nil {
		t.Fatalf("UPDATE with legal values: %v", execErr)
	}

	run, ok, err := s.SessionNewestRun(ctx, 1)
	if err != nil {
		t.Fatalf("SessionNewestRun: %v", err)
	}
	if !ok {
		t.Fatal("SessionNewestRun: ok = false, want true")
	}
	want, parseErr := time.Parse(time.RFC3339, "2026-10-05T16:20:00Z")
	if parseErr != nil {
		t.Fatalf("parse want: %v", parseErr)
	}
	if run.CappedUntil == nil || !run.CappedUntil.Equal(want) {
		t.Errorf("run.CappedUntil = %v, want %v", run.CappedUntil, want)
	}
}

// TestMigration0010SplitChildren proves migration 0010_split_children.sql
// installed the ticket_dependencies table and the tickets.split_key column
// (#74's planner split): ticket_dependencies accepts a row between two
// distinct tickets and rejects a self-dependency (its CHECK), an UPDATE
// setting split_key to a non-c-prefixed value fails the column's own CHECK,
// and two children of the same parent sharing a split_key violate
// tickets_split_key_uk.
func TestMigration0010SplitChildren(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)

	found := tableColumnNames(t, s, "tickets")
	if !found["split_key"] {
		t.Fatal("tickets.split_key column not found after migration 0010")
	}

	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state, parent_ticket_id) VALUES (2, 1, '43', 'a child', 'queued', 1)`,
	); execErr != nil {
		t.Fatalf("seed child ticket: %v", execErr)
	}

	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO ticket_dependencies (ticket_id, depends_on_ticket_id) VALUES (2, 1)`,
	); execErr != nil {
		t.Errorf("insert dependency between distinct tickets: %v", execErr)
	}

	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO ticket_dependencies (ticket_id, depends_on_ticket_id) VALUES (2, 2)`,
	); execErr == nil {
		t.Error("insert self-dependency: want a CHECK constraint error, got nil")
	}

	if _, execErr := s.db.ExecContext(ctx,
		`UPDATE tickets SET split_key = 'x1' WHERE id = 2`,
	); execErr == nil {
		t.Error("UPDATE split_key to a non-c-prefixed value: want a CHECK constraint error, got nil")
	}

	if _, execErr := s.db.ExecContext(ctx,
		`UPDATE tickets SET split_key = 'c1' WHERE id = 2`,
	); execErr != nil {
		t.Fatalf("UPDATE split_key to a legal value: %v", execErr)
	}

	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state, parent_ticket_id, split_key) VALUES (3, 1, '44', 'another child', 'queued', 1, 'c1')`,
	); execErr == nil {
		t.Error("insert second child with the same parent and split_key: want tickets_split_key_uk violation, got nil")
	}
}

// TestMigration0011TicketText proves migration 0011_ticket_text.sql
// installed tickets.tracker_body and tickets.owner_comments (#98):
// owner_comments defaults to an empty string for a raw INSERT that names
// neither column, and InsertTicket sets tracker_body equal to body.
func TestMigration0011TicketText(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	found := tableColumnNames(t, s, "tickets")
	for _, col := range []string{"tracker_body", "owner_comments"} {
		if !found[col] {
			t.Errorf("tickets.%s column not found after migration 0011", col)
		}
	}

	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_url, local_path, tracker) VALUES (1, 'zing', 'https://github.com/x/zing', '/tmp/zing', 'github')`,
	); execErr != nil {
		t.Fatalf("seed project: %v", execErr)
	}
	if _, execErr := s.db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state) VALUES (1, 1, '42', 'fix the bug', 'queued')`,
	); execErr != nil {
		t.Fatalf("seed ticket: %v", execErr)
	}
	var ownerComments string
	var trackerBody sql.NullString
	if scanErr := s.db.QueryRowContext(ctx,
		`SELECT owner_comments, tracker_body FROM tickets WHERE id = 1`,
	).Scan(&ownerComments, &trackerBody); scanErr != nil {
		t.Fatalf("read back raw insert: %v", scanErr)
	}
	if ownerComments != "" {
		t.Errorf("owner_comments default = %q, want \"\"", ownerComments)
	}
	if trackerBody.Valid {
		t.Errorf("tracker_body for a raw INSERT = %v, want NULL", trackerBody)
	}

	ticketID, err := s.InsertTicket(ctx, Ticket{ProjectID: 1, TrackerRef: "43", Title: "t", Body: "the body", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	ticket, getErr := s.GetTicket(ctx, ticketID)
	if getErr != nil {
		t.Fatalf("GetTicket: %v", getErr)
	}
	if ticket.TrackerBody == nil || *ticket.TrackerBody != "the body" {
		t.Errorf("InsertTicket's tracker_body = %v, want %q", ticket.TrackerBody, "the body")
	}
}
