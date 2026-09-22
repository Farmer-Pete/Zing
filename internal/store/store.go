// Package store opens and migrates the Zing SQLite database.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/adlio/schema"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is an open, migrated Zing database.
type Store struct {
	db *sql.DB
}

// DefaultPath returns the default database path, ~/.zing/zing.db.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return filepath.Join(home, ".zing", "zing.db"), nil
}

// Open opens the SQLite database at path in WAL mode with foreign keys on,
// runs any pending migrations, and returns the ready Store.
//
// On any failure after sql.Open succeeds, Open closes the database before
// returning, so a failed open leaks no connection.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// The pragmas ride the DSN so every pooled connection gets them; an
	// Exec-set pragma would only affect one connection. Escape the three
	// DSN-significant bytes so a ?, #, or % in the path cannot corrupt the query.
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	dsn := "file:" + esc + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1) // one local writer; raise once read concurrency matters (WAL allows concurrent readers)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if err := runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	migrator := schema.NewMigrator(schema.WithDialect(schema.SQLite), schema.WithContext(ctx))
	migs, err := schema.FSMigrations(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if err := migrator.Apply(db, migs); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
