// Package store owns the database: schema, connection lifecycle and every
// query the application runs. Nothing outside this package writes SQL.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver
	_ "modernc.org/sqlite"             // cgo-free SQLite driver
)

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
	db    *DB
	clock Clock
}

// Open connects to the database named by dsn and returns a ready Store.
//
// The dialect is inferred: a PostgreSQL URL or key/value string connects to
// PostgreSQL, anything else is a SQLite file path. ":memory:" gives an
// in-process SQLite database for tests.
//
// Open does not create or alter tables. Schema changes go through Migrate, so
// that a running server never silently reshapes a production database.
func Open(dsn string) (*Store, error) {
	dialect := dialectFromDSN(dsn)

	connection := dsn
	if dialect == DialectSQLite {
		if dsn == ":memory:" {
			// A shared cache keeps every pooled connection looking at the same
			// in-memory database rather than each creating its own.
			connection = "file::memory:?cache=shared"
		}
		// Foreign keys are off by default in SQLite and have to be asked for
		// on every connection, or a cascade silently does nothing.
		if !strings.Contains(connection, "_pragma=") {
			separator := "?"
			if strings.Contains(connection, "?") {
				separator = "&"
			}
			connection += separator + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
		}
	}

	db, err := sql.Open(dialect.driverName(), connection)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", describeDialect(dialect), err)
	}

	switch dialect {
	case DialectPostgres:
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(30 * time.Minute)
	default:
		// SQLite takes a single writer. Serialising connections avoids
		// SQLITE_BUSY entirely, at the cost of write concurrency that a
		// single-server install does not need.
		db.SetMaxOpenConns(1)
		db.SetConnMaxLifetime(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: cannot reach %s: %w", describeDialect(dialect), err)
	}

	if dialect == DialectSQLite {
		// WAL keeps readers from blocking behind the writer.
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: enable WAL: %w", err)
		}
	}

	return &Store{db: &DB{db: db, dialect: dialect}, clock: time.Now}, nil
}

// OpenAndMigrate opens the database and brings the schema up to date. It is
// what development and tests use; production runs `avas migrate` separately so
// a deploy cannot half-apply a schema change under live traffic.
func OpenAndMigrate(dsn string) (*Store, error) {
	s, err := Open(dsn)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// WithClock replaces the store's clock. Test-only.
func (s *Store) WithClock(clock Clock) *Store {
	s.clock = clock
	return s
}

// Now is the store's idea of the current time, in UTC.
func (s *Store) Now() time.Time { return s.clock().UTC() }

// DB exposes the dialect-aware handle.
func (s *Store) DB() *DB { return s.db }

// Dialect reports which database this store talks to.
func (s *Store) Dialect() Dialect { return s.db.dialect }

// DialectName is the human-readable database name, for the admin health view.
func (s *Store) DialectName() string { return describeDialect(s.db.dialect) }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.db.Close() }

// Tx runs fn inside a transaction, committing on success and rolling back on
// any error or panic. Every multi-statement write in this package goes through
// it: a partially applied bet or ledger transaction is not recoverable.
func (s *Store) Tx(ctx context.Context, fn func(*Tx) error) error {
	raw, err := s.db.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	tx := &Tx{tx: raw, dialect: s.db.dialect}
	defer func() {
		if r := recover(); r != nil {
			_ = raw.Rollback()
			panic(r)
		}
	}()
	if err := fn(tx); err != nil {
		_ = raw.Rollback()
		return err
	}
	if err := raw.Commit(); err != nil {
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
