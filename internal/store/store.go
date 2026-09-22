// Package store opens and migrates the Zing SQLite database.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adlio/schema"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is an open, migrated Zing database.
type Store struct {
	db      *sql.DB
	schemas *schemaSet
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

	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if err = runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	schemas, err := loadSchemas()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load schemas: %w", err)
	}

	return &Store{db: db, schemas: schemas}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

// Artifact is a row to insert into the artifacts table. Payload is validated
// against the artifacts/<Type> schema before insert.
type Artifact struct {
	TicketID int64
	RunID    *int64
	Type     string
	Version  int
	Payload  json.RawMessage
	SealedAt *time.Time
}

// messagePayloadTypes are the only message types that carry a payload (section 8.2).
var messagePayloadTypes = map[string]bool{
	"question":   true,
	"answer":     true,
	"escalation": true,
	"state":      true,
}

// Message is a row to insert into the messages table. Payload is required
// and validated for the four payload-carrying types (question, answer,
// escalation, state); every other type must carry no payload.
type Message struct {
	TicketID int64
	RunID    *int64
	ParentID *int64
	Type     string
	Author   string
	State    *string
	Body     string
	Payload  json.RawMessage
	BatchID  *int64
	ReadAt   *time.Time
}

// InsertArtifact validates a.Payload against its schema, then inserts the row.
func (s *Store) InsertArtifact(ctx context.Context, a Artifact) (int64, error) {
	version := a.Version
	switch {
	case version < 0:
		return 0, fmt.Errorf("insert artifact: version %d must not be negative", version)
	case version == 0:
		version = 1 // the SQL column default
	}

	if err := s.schemas.validate("artifacts", a.Type, a.Payload); err != nil {
		return 0, err
	}

	var sealedAt *string
	if a.SealedAt != nil {
		formatted := a.SealedAt.UTC().Format(time.RFC3339)
		sealedAt = &formatted
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO artifacts (ticket_id, run_id, type, version, payload, sealed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.TicketID, a.RunID, a.Type, version, string(a.Payload), sealedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("insert artifact: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert artifact: %w", err)
	}
	return id, nil
}

// InsertMessage validates m.Payload for the four payload-carrying message
// types, rejects a payload on every other type, then inserts the row.
func (s *Store) InsertMessage(ctx context.Context, m Message) (int64, error) {
	var payloadParam *string
	if messagePayloadTypes[m.Type] {
		if len(m.Payload) == 0 {
			return 0, fmt.Errorf("message type %s requires a payload", m.Type)
		}
		if err := s.schemas.validate("messages", m.Type, m.Payload); err != nil {
			return 0, err
		}
		text := string(m.Payload)
		payloadParam = &text
	} else if len(m.Payload) != 0 {
		return 0, fmt.Errorf("message type %s takes no payload", m.Type)
	}

	var readAt *string
	if m.ReadAt != nil {
		formatted := m.ReadAt.UTC().Format(time.RFC3339)
		readAt = &formatted
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (ticket_id, run_id, parent_id, type, author, state, body, payload, batch_id, read_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.TicketID, m.RunID, m.ParentID, m.Type, m.Author, m.State, m.Body, payloadParam, m.BatchID, readAt,
	)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	return id, nil
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
