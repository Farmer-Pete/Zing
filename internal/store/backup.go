package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// BackupTo writes a consistent copy of the database to path with VACUUM INTO,
// replacing any file already there.
func (s *Store) BackupTo(ctx context.Context, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: remove %s: %w", path, err)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("backup: vacuum into %s: %w", path, err)
	}
	return nil
}
