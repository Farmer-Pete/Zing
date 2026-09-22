package store

import (
	"path/filepath"
	"testing"
)

// Table and type name literals repeated across this package's tests.
const (
	testTableMessages  = "messages"
	testTableArtifacts = "artifacts"
	testTypeQuestion   = "question"
	testTypeState      = "state"
	testTypePlanreview = "planreview"
	testAuthorZing     = "zing"
)

var wantTables = []string{
	"projects", "tickets", "sessions", "runs", testTableMessages,
	testTableArtifacts, "push_subscriptions", "settings",
}

func TestOpen_CreatesAllTables(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "zing.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, table := range wantTables {
		var name string
		err := s.db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestOpen_WALMode(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "zing.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestOpen_SecondOpenIsNoOp(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "zing.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	for _, table := range wantTables {
		var name string
		err := second.db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after second Open: %v", table, err)
		}
	}
}

// TestOpen_FailingOpenLeavesNothingWedged forces a deterministic failure by
// passing an existing directory as the database path: MkdirAll on its parent
// succeeds, but the driver cannot open a directory as a SQLite file, so
// PingContext (or the migration step) fails. This exercises the close-on-error
// path in Open. A later Open on a good path must still succeed, proving the
// failed attempt leaked no connection.
func TestOpen_FailingOpenLeavesNothingWedged(t *testing.T) {
	ctx := t.Context()
	dirAsPath := t.TempDir()

	s, err := Open(ctx, dirAsPath)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open with a directory path: want error, got nil")
	}
	if s != nil {
		t.Errorf("Open with a directory path: want nil Store, got %v", s)
	}

	goodPath := filepath.Join(t.TempDir(), "zing.db")
	good, err := Open(ctx, goodPath)
	if err != nil {
		t.Fatalf("Open on a good path after a failed Open: %v", err)
	}
	_ = good.Close()
}

// seedProjectAndTicket inserts a project (id 1) and a ticket on it (id 1),
// the shared setup every other table's row references by foreign key.
func seedProjectAndTicket(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_url, local_path, tracker) VALUES (1, 'zing', 'https://github.com/x/zing', '/tmp/zing', 'github')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state) VALUES (1, 1, '42', 'fix the bug', 'queued')`,
	); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
}

// TestStore_RoundTripAllTables inserts into, and reads back from, every one
// of the eight tables, exercising each table's foreign keys, CHECK
// constraints, and unique indexes on a valid row.
func TestStore_RoundTripAllTables(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, 1, 'planning', 'claude')`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (id, session_id, turn, outcome) VALUES (1, 1, 0, 'ok')`,
	); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	stateID, err := s.InsertMessage(ctx, Message{
		TicketID: 1, Type: testTypeState, Author: testAuthorZing,
		Payload: []byte(`{"from":"queued","to":"planning","reason":"started"}`),
	})
	if err != nil {
		t.Fatalf("insert state message: %v", err)
	}
	if _, err := s.InsertMessage(ctx, Message{
		TicketID: 1, ParentID: &stateID, Type: "update", Author: testAuthorZing, Body: "progress",
	}); err != nil {
		t.Fatalf("insert update message (no payload): %v", err)
	}

	if _, err := s.InsertArtifact(ctx, Artifact{
		TicketID: 1, Type: "scenario",
		Payload: []byte(`{"id":"s1","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`),
	}); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (endpoint, keys_json) VALUES ('https://push.example/1', '{}')`,
	); err != nil {
		t.Fatalf("insert push subscription: %v", err)
	}

	var draining string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'draining'").Scan(&draining); err != nil {
		t.Fatalf("read seeded settings: %v", err)
	}
	if draining != "false" {
		t.Errorf("settings.draining = %q, want %q", draining, "false")
	}
}

// TestStore_ConstraintViolations proves the CHECK, foreign key, and unique
// constraints in the migration actually reject a bad row.
func TestStore_ConstraintViolations(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	t.Run("bad foreign key", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO sessions (ticket_id, job, runtime) VALUES (999, 'planning', 'claude')`)
		if err == nil {
			t.Error("insert with a nonexistent ticket_id: want error, got nil")
		}
	})

	t.Run("bad enum", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO tickets (project_id, tracker_ref, title, state) VALUES (1, '99', 't', 'bogus')`)
		if err == nil {
			t.Error("insert with an invalid state: want error, got nil")
		}
	})

	t.Run("duplicate unique key", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO projects (name, repo_url, local_path, tracker) VALUES ('zing', 'x', 'y', 'github')`)
		if err == nil {
			t.Error("insert with a duplicate project name: want error, got nil")
		}
	})
}

// TestInsertMessage_BindsPayloadAsText and TestInsertArtifact_BindsPayloadAsText
// prove payload rides as string(payload), never the raw []byte: modernc/sqlite
// binds a []byte as a BLOB, which would break the TEXT storage contract.
func TestInsertMessage_BindsPayloadAsText(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	id, err := s.InsertMessage(ctx, Message{
		TicketID: 1, Type: testTypeState, Author: testAuthorZing,
		Payload: []byte(`{"from":"queued","to":"planning","reason":"started"}`),
	})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	var sqlType string
	if err := s.db.QueryRowContext(ctx, "SELECT typeof(payload) FROM messages WHERE id = ?", id).Scan(&sqlType); err != nil {
		t.Fatalf("query typeof(payload): %v", err)
	}
	if sqlType != "text" {
		t.Errorf("typeof(payload) = %q, want %q", sqlType, "text")
	}
}

func TestInsertArtifact_BindsPayloadAsText(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	id, err := s.InsertArtifact(ctx, Artifact{
		TicketID: 1, Type: "scenario",
		Payload: []byte(`{"id":"s1","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`),
	})
	if err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}

	var sqlType string
	if err := s.db.QueryRowContext(ctx, "SELECT typeof(payload) FROM artifacts WHERE id = ?", id).Scan(&sqlType); err != nil {
		t.Fatalf("query typeof(payload): %v", err)
	}
	if sqlType != "text" {
		t.Errorf("typeof(payload) = %q, want %q", sqlType, "text")
	}
}
