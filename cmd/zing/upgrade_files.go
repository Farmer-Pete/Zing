package main

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
