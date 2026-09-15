package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Dialect names a supported database. The query layer is written once, in
// SQLite's `?` placeholder style, and rebound per dialect at the point of
// execution. That keeps a single copy of every query rather than two that
// drift apart.
type Dialect string

const (
	// DialectSQLite is the zero-dependency default, suitable for development
	// and single-server installs.
	DialectSQLite Dialect = "sqlite"
	// DialectPostgres is the supported production database.
	DialectPostgres Dialect = "postgres"
)

// driverName maps a dialect onto its registered database/sql driver.
func (d Dialect) driverName() string {
	switch d {
	case DialectPostgres:
		return "pgx"
	default:
		return "sqlite"
	}
}

// Rebind converts `?` placeholders to the dialect's own form.
//
// Question marks inside string literals and quoted identifiers are left alone,
// because a `?` in a memo or a LIKE pattern is data, not a parameter.
func (d Dialect) Rebind(query string) string {
	if d != DialectPostgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)

	position := 1
	var quote byte // 0 when not inside a literal
	for i := 0; i < len(query); i++ {
		c := query[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				// A doubled quote is an escaped quote, not the end.
				if i+1 < len(query) && query[i+1] == quote {
					b.WriteByte(quote)
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			b.WriteByte(c)
		case '?':
			b.WriteString("$")
			b.WriteString(itoa(position))
			position++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for n > 0 {
		pos--
		digits[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[pos:])
}

// ---------------------------------------------------------------------------
// Connection wrappers
//
// DB and Tx mirror the parts of database/sql the store uses, rebinding every
// query on the way through. Handlers and services never see a raw *sql.DB, so
// a query cannot reach the database without passing through the dialect.
// ---------------------------------------------------------------------------

// DB is a dialect-aware database handle.
type DB struct {
	db      *sql.DB
	dialect Dialect
}

// Dialect reports which database this handle talks to.
func (d *DB) Dialect() Dialect { return d.dialect }

// Raw exposes the underlying handle for health checks and migrations.
func (d *DB) Raw() *sql.DB { return d.db }

// ExecContext runs a statement.
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, d.dialect.Rebind(query), args...)
}

// QueryContext runs a query returning rows.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, d.dialect.Rebind(query), args...)
}

// QueryRowContext runs a query returning at most one row.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, d.dialect.Rebind(query), args...)
}

// PingContext checks the connection.
func (d *DB) PingContext(ctx context.Context) error { return d.db.PingContext(ctx) }

// InsertID runs an INSERT and returns the generated primary key.
//
// The two databases disagree about how to get it: SQLite reports it from the
// driver, PostgreSQL requires RETURNING. Callers should not have to care.
func (d *DB) InsertID(ctx context.Context, query string, args ...any) (int64, error) {
	if d.dialect == DialectPostgres {
		var id int64
		err := d.db.QueryRowContext(ctx, d.dialect.Rebind(query+" RETURNING id"), args...).Scan(&id)
		return id, err
	}
	result, err := d.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// Tx is a dialect-aware transaction.
type Tx struct {
	tx      *sql.Tx
	dialect Dialect
}

// ExecContext runs a statement inside the transaction.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

// QueryContext runs a query inside the transaction.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.dialect.Rebind(query), args...)
}

// QueryRowContext runs a single-row query inside the transaction.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.dialect.Rebind(query), args...)
}

// InsertID runs an INSERT inside the transaction and returns the new id.
func (t *Tx) InsertID(ctx context.Context, query string, args ...any) (int64, error) {
	if t.dialect == DialectPostgres {
		var id int64
		err := t.tx.QueryRowContext(ctx, t.dialect.Rebind(query+" RETURNING id"), args...).Scan(&id)
		return id, err
	}
	result, err := t.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// scanner is the shared shape of *sql.Row and *sql.Rows, so a scan helper can
// take either.
type scanner interface {
	Scan(dest ...any) error
}

// dialectFromDSN infers the dialect from a connection string.
//
// A PostgreSQL URL or key/value string is recognised; anything else is treated
// as a SQLite file path, which is what a bare filename should mean.
func dialectFromDSN(dsn string) Dialect {
	trimmed := strings.TrimSpace(strings.ToLower(dsn))
	switch {
	case strings.HasPrefix(trimmed, "postgres://"),
		strings.HasPrefix(trimmed, "postgresql://"):
		return DialectPostgres
	case strings.Contains(trimmed, "host=") && strings.Contains(trimmed, "dbname="):
		return DialectPostgres
	default:
		return DialectSQLite
	}
}

// describeDialect is used in error messages and the admin health view.
func describeDialect(d Dialect) string {
	switch d {
	case DialectPostgres:
		return "PostgreSQL"
	case DialectSQLite:
		return "SQLite"
	default:
		return fmt.Sprintf("unknown (%s)", d)
	}
}
