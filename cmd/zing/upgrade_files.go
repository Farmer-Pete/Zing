package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	backupKeep   = 5
	backupPrefix = "zing.db.pre-upgrade-"
)

// pruneBackups keeps the keep newest backups in dataDir: ModTime newest
// first, ties broken by name descending. It removes every other backup and
// returns every remove error joined.
func pruneBackups(dataDir string, keep int) error {
	matches, err := filepath.Glob(filepath.Join(dataDir, backupPrefix+"*"))
	if err != nil {
		return err
	}

	type backup struct {
		path    string
		modTime int64
	}

	backups := make([]backup, 0, len(matches))
	for _, path := range matches {
		info, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return statErr
		}
		backups = append(backups, backup{path: path, modTime: info.ModTime().UnixNano()})
	}

	slices.SortFunc(backups, func(a, b backup) int {
		if c := cmp.Compare(b.modTime, a.modTime); c != 0 {
			return c
		}
		return cmp.Compare(b.path, a.path)
	})

	var errs []error
	for _, b := range backups[min(keep, len(backups)):] {
		if rmErr := os.Remove(b.path); rmErr != nil {
			errs = append(errs, rmErr)
		}
	}
	return errors.Join(errs...)
}

const (
	markerPending    = "pending"
	markerAttempted  = "attempted"
	markerRolledBack = "rolled_back"

	upgradeMarkerFile = "upgrade.json"
)

// upgradeMarker records the state of one upgrade across the restart that
// swaps in the new binary.
type upgradeMarker struct {
	FromSHA      string `json:"from_sha"`
	ToSHA        string `json:"to_sha"`
	TicketID     int64  `json:"ticket_id"`
	State        string `json:"state"`
	HasNext      bool   `json:"has_next"`
	NextSHA      string `json:"next_sha"`
	NextTicketID int64  `json:"next_ticket_id"`
	WrittenAt    string `json:"written_at"`
}

// loadUpgradeMarker reads DATA_DIR/upgrade.json. A missing file is found
// false with a nil error.
func loadUpgradeMarker(dataDir string) (m upgradeMarker, found bool, err error) {
	data, err := os.ReadFile(filepath.Join(dataDir, upgradeMarkerFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return upgradeMarker{}, false, nil
		}
		return upgradeMarker{}, false, fmt.Errorf("upgrade: read upgrade.json: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return upgradeMarker{}, false, fmt.Errorf("upgrade: read upgrade.json: %w", err)
	}
	return m, true, nil
}

// saveUpgradeMarker sets m.WrittenAt to now in RFC3339 UTC, writes
// upgrade.json.tmp with mode 0600, and renames it over upgrade.json.
func saveUpgradeMarker(dataDir string, m upgradeMarker) error {
	m.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("upgrade: write upgrade.json: %w", err)
	}
	tmp := filepath.Join(dataDir, upgradeMarkerFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("upgrade: write upgrade.json: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dataDir, upgradeMarkerFile)); err != nil {
		return fmt.Errorf("upgrade: write upgrade.json: %w", err)
	}
	return nil
}

// saveCarry records a request that arrived after the drain began, so the
// next serve can pick it up. With hasCarry false it touches nothing.
func saveCarry(dataDir string, carry upgradeRequest, hasCarry bool) error {
	if !hasCarry {
		return nil
	}
	m, found, err := loadUpgradeMarker(dataDir)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("upgrade: save carry: no upgrade.json")
	}
	m.HasNext, m.NextSHA, m.NextTicketID = true, carry.SHA, carry.TicketID
	return saveUpgradeMarker(dataDir, m)
}
