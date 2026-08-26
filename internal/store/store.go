// Package store owns the database: schema, connection lifecycle and every
// query the application runs. Nothing outside this package writes SQL.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // cgo-free SQLite driver
)

//go:embed schema.sql
var schemaSQL string

// Common errors callers are expected to branch on.
var (
	ErrNotFound     = errors.New("store: not found")
	ErrConflict     = errors.New("store: conflict")
	ErrUnbalanced   = errors.New("store: ledger transaction does not balance")
	ErrInsufficient = errors.New("store: insufficient balance")
)

// Clock lets tests move time without sleeping. Production uses time.Now.
type Clock func() time.Time

// Store is a handle on the database.
type Store struct {
	db    *sql.DB
	clock Clock
}

// Open connects to the SQLite database at path, applies the schema and returns
// a ready Store. The path ":memory:" gives an in-process database for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		// A shared cache keeps every connection in the pool looking at the
		// same in-memory database rather than each creating its own.
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite takes a single writer. Serialising connections avoids
	// SQLITE_BUSY entirely at the cost of write concurrency we do not need.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	return &Store{db: db, clock: time.Now}, nil
}

// WithClock replaces the store's clock. Test-only.
func (s *Store) WithClock(clock Clock) *Store {
	s.clock = clock
	return s
}

// Now is the store's idea of the current time, in UTC.
func (s *Store) Now() time.Time { return s.clock().UTC() }

// DB exposes the raw handle for the few callers that need it (health checks).
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Tx runs fn inside a transaction, committing on success and rolling back on
// any error or panic. Every multi-statement write in this package goes through
// it: a partially applied bet or ledger transaction is not recoverable.
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Timestamp renders a time in the string format every timestamp column uses.
func Timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseTimestamp reads a stored timestamp back.
func ParseTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("store: %q is not a timestamp", s)
}

// nullString converts an empty string to SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt64 converts a zero id to SQL NULL.
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// derefInt64 reads a nullable integer column.
func derefInt64(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

// derefString reads a nullable text column.
func derefString(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}
