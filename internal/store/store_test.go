package store

import (
	"path/filepath"
	"testing"
)

var wantTables = []string{
	"projects", "tickets", "sessions", "runs", "messages",
	"artifacts", "push_subscriptions", "settings",
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
