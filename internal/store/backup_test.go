package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStore_BackupTo proves BackupTo replaces any existing file at path with
// a consistent, independently openable copy of the database.
func TestStore_BackupTo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)

	backupPath := filepath.Join(t.TempDir(), "zing.db.pre-upgrade-abc123")
	if err = os.WriteFile(backupPath, []byte("junk"), 0o600); err != nil {
		t.Fatalf("seed junk file: %v", err)
	}

	if err = s.BackupTo(ctx, backupPath); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}

	backup, err := Open(ctx, backupPath)
	if err != nil {
		t.Fatalf("Open backup: %v", err)
	}
	defer func() { _ = backup.Close() }()

	if err := backup.VerifyTables(ctx); err != nil {
		t.Errorf("VerifyTables on backup: %v", err)
	}

	var name string
	if err := backup.db.QueryRowContext(ctx, "SELECT name FROM projects WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("read seeded project from backup: %v", err)
	}
	if name != "zing" {
		t.Errorf("backup project name = %q, want zing", name)
	}
}
