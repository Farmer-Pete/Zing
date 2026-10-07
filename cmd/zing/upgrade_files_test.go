package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPruneBackups_KeepsFiveNewest proves pruneBackups keeps the backupKeep
// newest backups by ModTime, breaking ties by name descending, and leaves
// zing.db untouched.
func TestPruneBackups_KeepsFiveNewest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now()
	touch := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		modTime := now.Add(-age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		return path
	}

	names := []string{
		backupPrefix + "a", // 1 minute, tied with b
		backupPrefix + "b", // 1 minute, tied with a; b sorts before a (name descending)
		backupPrefix + "c", // 2 minutes
		backupPrefix + "d", // 3 minutes
		backupPrefix + "e", // 4 minutes
		backupPrefix + "f", // 5 minutes
		backupPrefix + "g", // 6 minutes, oldest
	}
	ages := []time.Duration{
		1 * time.Minute, 1 * time.Minute, 2 * time.Minute, 3 * time.Minute,
		4 * time.Minute, 5 * time.Minute, 6 * time.Minute,
	}
	for i, name := range names {
		touch(name, ages[i])
	}

	dbPath := touch("zing.db", 0)

	if err := pruneBackups(dir, backupKeep); err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}

	// Newest 5 by (modTime desc, name desc): b, a, c, d, e. f and g must be gone.
	wantKept := []string{backupPrefix + "b", backupPrefix + "a", backupPrefix + "c", backupPrefix + "d", backupPrefix + "e"}
	for _, name := range wantKept {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("want %s to remain: %v", name, err)
		}
	}
	wantRemoved := []string{backupPrefix + "f", backupPrefix + "g"}
	for _, name := range wantRemoved {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("want %s removed, stat err = %v", name, err)
		}
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("zing.db must be untouched: %v", err)
	}
}
