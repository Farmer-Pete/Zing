package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUpgradeMarker_RoundTrip proves saveUpgradeMarker and loadUpgradeMarker
// round-trip every field, write upgrade.json atomically with mode 0600, and
// that a missing file is found false with a nil error.
func TestUpgradeMarker_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, found, err := loadUpgradeMarker(dir); err != nil || found {
		t.Fatalf("loadUpgradeMarker on missing file: found=%v err=%v", found, err)
	}

	states := []string{markerPending, markerAttempted, markerRolledBack}
	for _, state := range states {
		m := upgradeMarker{
			FromSHA:      "0123456789ab",
			ToSHA:        "fedcba9876543210fedcba9876543210fedcba9",
			TicketID:     7,
			State:        state,
			HasNext:      true,
			NextSHA:      "abcdef0123456789abcdef0123456789abcdef01",
			NextTicketID: 9,
		}
		if err := saveUpgradeMarker(dir, m); err != nil {
			t.Fatalf("saveUpgradeMarker: %v", err)
		}

		if _, err := os.Stat(filepath.Join(dir, upgradeMarkerFile+".tmp")); !os.IsNotExist(err) {
			t.Errorf("upgrade.json.tmp must not remain, stat err = %v", err)
		}

		info, err := os.Stat(filepath.Join(dir, upgradeMarkerFile))
		if err != nil {
			t.Fatalf("stat upgrade.json: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("upgrade.json mode = %o, want 0600", mode)
		}

		got, found, err := loadUpgradeMarker(dir)
		if err != nil || !found {
			t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, err)
		}

		writtenAt, err := time.Parse(time.RFC3339, got.WrittenAt)
		if err != nil {
			t.Fatalf("parse written_at: %v", err)
		}

		want := m
		got.WrittenAt, want.WrittenAt = "", ""
		if got != want {
			t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
		}
		if writtenAt.Location() != time.UTC {
			t.Errorf("written_at location = %v, want UTC", writtenAt.Location())
		}
	}
}

// TestUpgradeMarker_LoadMalformedFails proves a malformed upgrade.json gives
// an error.
func TestUpgradeMarker_LoadMalformedFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, upgradeMarkerFile), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := loadUpgradeMarker(dir); err == nil {
		t.Fatal("loadUpgradeMarker: want error for malformed file")
	}
}

// TestSaveCarry proves saveCarry leaves the marker untouched when hasCarry
// is false, sets has_next/next_sha/next_ticket_id and keeps every other
// field when hasCarry is true, and errors when no marker exists yet.
func TestSaveCarry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := saveCarry(dir, upgradeRequest{TicketID: 9, SHA: "abc"}, false); err != nil {
		t.Fatalf("saveCarry with hasCarry false: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, upgradeMarkerFile)); !os.IsNotExist(err) {
		t.Errorf("upgrade.json created with hasCarry false, stat err = %v", err)
	}

	orig := upgradeMarker{
		FromSHA:  "0123456789ab",
		ToSHA:    "fedcba9876543210fedcba9876543210fedcba9",
		TicketID: 7,
		State:    markerPending,
	}
	if err := saveUpgradeMarker(dir, orig); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	before, readErr := os.ReadFile(filepath.Join(dir, upgradeMarkerFile))
	if readErr != nil {
		t.Fatalf("read upgrade.json: %v", readErr)
	}
	if saveErr := saveCarry(dir, upgradeRequest{TicketID: 9, SHA: "abc"}, false); saveErr != nil {
		t.Fatalf("saveCarry with hasCarry false over an existing marker: %v", saveErr)
	}
	after, readErr := os.ReadFile(filepath.Join(dir, upgradeMarkerFile))
	if readErr != nil {
		t.Fatalf("read upgrade.json: %v", readErr)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("upgrade.json changed with hasCarry false: before %s, after %s", before, after)
	}

	carry := upgradeRequest{TicketID: 42, SHA: "abcdef0123456789abcdef0123456789abcdef01"}
	if err := saveCarry(dir, carry, true); err != nil {
		t.Fatalf("saveCarry: %v", err)
	}

	got, found, err := loadUpgradeMarker(dir)
	if err != nil || !found {
		t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, err)
	}
	if !got.HasNext || got.NextSHA != carry.SHA || got.NextTicketID != carry.TicketID {
		t.Errorf("marker = %+v, want has_next true, next_sha %q, next_ticket_id %d", got, carry.SHA, carry.TicketID)
	}
	gotUnrelated := got
	gotUnrelated.HasNext, gotUnrelated.NextSHA, gotUnrelated.NextTicketID, gotUnrelated.WrittenAt = false, "", 0, ""
	wantUnrelated := orig
	wantUnrelated.WrittenAt = ""
	if gotUnrelated != wantUnrelated {
		t.Errorf("marker changed unrelated fields: got %+v, want fields matching %+v", gotUnrelated, wantUnrelated)
	}

	missing := t.TempDir()
	if err := saveCarry(missing, carry, true); err == nil {
		t.Fatal("saveCarry with no existing marker: want error")
	}
}

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
		backupPrefix + "a", // 1 minute
		backupPrefix + "b", // 2 minutes
		backupPrefix + "c", // 3 minutes
		backupPrefix + "d", // 4 minutes
		backupPrefix + "e", // 5 minutes, tied with f, at the keep=5 boundary
		backupPrefix + "f", // 5 minutes, tied with e; f sorts before e (name descending), so f is the one kept
		backupPrefix + "g", // 6 minutes, oldest
	}
	ages := []time.Duration{
		1 * time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute,
		5 * time.Minute, 5 * time.Minute, 6 * time.Minute,
	}
	for i, name := range names {
		touch(name, ages[i])
	}

	dbPath := touch("zing.db", 0)

	if err := pruneBackups(dir, backupKeep); err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}

	// Newest 5 by (modTime desc, name desc): a, b, c, d, f. The e/f tie
	// decides whether f (kept) or e (removed) lands in the top 5.
	wantKept := []string{backupPrefix + "a", backupPrefix + "b", backupPrefix + "c", backupPrefix + "d", backupPrefix + "f"}
	for _, name := range wantKept {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("want %s to remain: %v", name, err)
		}
	}
	wantRemoved := []string{backupPrefix + "e", backupPrefix + "g"}
	for _, name := range wantRemoved {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("want %s removed, stat err = %v", name, err)
		}
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("zing.db must be untouched: %v", err)
	}
}
