package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrationFS embed.FS

// ErrDirtySchema is returned when the database has a migration recorded whose
// file no longer matches what was applied.
var ErrDirtySchema = errors.New("store: schema does not match the migration files")

// Migration is one versioned schema change.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// AppliedMigration is a migration recorded in the database.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// MigrationState pairs what is on disk with what the database has applied.
type MigrationState struct {
	Version   int
	Name      string
	Applied   bool
	AppliedAt time.Time
	// Mismatch is set when the file's checksum differs from the one recorded
	// at the time it was applied, meaning an applied migration has been edited.
	Mismatch bool
}

// Migrations returns the migration files for a dialect, in version order.
func Migrations(dialect Dialect) ([]Migration, error) {
	dir := path.Join("migrations", string(dialect))
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, fmt.Errorf("store: no migrations for %s: %w", describeDialect(dialect), err)
	}

	out := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(migrationFS, path.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	for i, migration := range out {
		if i > 0 && migration.Version == out[i-1].Version {
			return nil, fmt.Errorf("store: two migrations share version %d", migration.Version)
		}
	}
	return out, nil
}

// parseMigrationName reads "0003_add_bonus_tables.sql".
func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	number, rest, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("store: migration %q is not named <version>_<name>.sql", filename)
	}
	version, err := strconv.Atoi(number)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("store: migration %q has no version number", filename)
	}
	return version, strings.ReplaceAll(rest, "_", " "), nil
}

// ensureMigrationsTable creates the bookkeeping table if it is missing. It is
// written to be valid in both dialects without going through a migration
// itself, since it is what records migrations.
func (s *Store) ensureMigrationsTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     BIGINT PRIMARY KEY,
			name        TEXT NOT NULL,
			checksum    TEXT NOT NULL,
			applied_at  TEXT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	return nil
}

// AppliedMigrations lists what the database believes it has run.
func (s *Store) AppliedMigrations(ctx context.Context) (map[int]AppliedMigration, error) {
	if err := s.ensureMigrationsTable(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]AppliedMigration)
	for rows.Next() {
		var record AppliedMigration
		var at string
		if err := rows.Scan(&record.Version, &record.Name, &record.Checksum, &at); err != nil {
			return nil, err
		}
		if record.AppliedAt, err = ParseTimestamp(at); err != nil {
			return nil, err
		}
		applied[record.Version] = record
	}
	return applied, rows.Err()
}

// MigrationStatus reports every migration and whether it has been applied.
func (s *Store) MigrationStatus(ctx context.Context) ([]MigrationState, error) {
	files, err := Migrations(s.db.dialect)
	if err != nil {
		return nil, err
	}
	applied, err := s.AppliedMigrations(ctx)
	if err != nil {
		return nil, err
	}

	states := make([]MigrationState, 0, len(files))
	for _, migration := range files {
		state := MigrationState{Version: migration.Version, Name: migration.Name}
		if record, ok := applied[migration.Version]; ok {
			state.Applied = true
			state.AppliedAt = record.AppliedAt
			state.Mismatch = record.Checksum != migration.Checksum
		}
		states = append(states, state)
	}
	return states, nil
}

// Migrate applies every pending migration and returns how many it ran.
//
// Each migration runs in its own transaction alongside the row recording it,
// so a failure leaves the database on the last complete version rather than
// half-way through one. PostgreSQL honours this for DDL; SQLite does too.
//
// An applied migration whose file has since been edited is refused rather than
// re-run. Silently ignoring it would let two environments believe they share a
// schema when they do not.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	files, err := Migrations(s.db.dialect)
	if err != nil {
		return 0, err
	}
	applied, err := s.AppliedMigrations(ctx)
	if err != nil {
		return 0, err
	}

	var ran int
	for _, migration := range files {
		if record, ok := applied[migration.Version]; ok {
			if record.Checksum != migration.Checksum {
				return ran, fmt.Errorf("%w: migration %04d (%s) was applied on %s but its file has changed since",
					ErrDirtySchema, migration.Version, migration.Name,
					record.AppliedAt.Format(time.RFC3339))
			}
			continue
		}

		if err := s.runMigration(ctx, migration); err != nil {
			return ran, err
		}
		ran++
	}
	return ran, nil
}

func (s *Store) runMigration(ctx context.Context, migration Migration) error {
	raw, err := s.db.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %04d: %w", migration.Version, err)
	}
	defer func() { _ = raw.Rollback() }()

	for _, statement := range splitStatements(migration.SQL) {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("store: migration %04d (%s) failed: %w\nstatement: %s",
				migration.Version, migration.Name, err, truncateSQL(statement))
		}
	}

	tx := &Tx{tx: raw, dialect: s.db.dialect}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		migration.Version, migration.Name, migration.Checksum, Timestamp(s.Now())); err != nil {
		return fmt.Errorf("store: record migration %04d: %w", migration.Version, err)
	}
	if err := raw.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %04d: %w", migration.Version, err)
	}
	return nil
}

// splitStatements breaks a migration file into individual statements on
// semicolons, ignoring those inside string literals, dollar-quoted blocks and
// comments. SQLite's driver will not accept several statements in one Exec,
// and splitting also produces a usable error message naming the statement that
// failed.
func splitStatements(script string) []string {
	var statements []string
	var current strings.Builder

	var (
		inLineComment  bool
		inBlockComment bool
		quote          byte
		dollarTag      string
	)

	for i := 0; i < len(script); i++ {
		c := script[i]

		switch {
		case inLineComment:
			current.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		case inBlockComment:
			current.WriteByte(c)
			if c == '/' && i > 0 && script[i-1] == '*' {
				inBlockComment = false
			}
			continue
		case dollarTag != "":
			current.WriteByte(c)
			if c == '$' && strings.HasPrefix(script[i:], dollarTag) {
				current.WriteString(script[i+1 : i+len(dollarTag)])
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		case quote != 0:
			current.WriteByte(c)
			if c == quote {
				if i+1 < len(script) && script[i+1] == quote {
					current.WriteByte(quote)
					i++
					continue
				}
				quote = 0
			}
			continue
		}

		switch {
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			inLineComment = true
			current.WriteByte(c)
		case c == '/' && i+1 < len(script) && script[i+1] == '*':
			inBlockComment = true
			current.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			current.WriteByte(c)
		case c == '$':
			if tag := dollarQuoteTag(script[i:]); tag != "" {
				dollarTag = tag
				current.WriteString(tag)
				i += len(tag) - 1
				continue
			}
			current.WriteByte(c)
		case c == ';':
			statements = appendStatement(statements, current.String())
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	return appendStatement(statements, current.String())
}

// dollarQuoteTag recognises the opening of a PostgreSQL dollar-quoted block
// such as $$ or $body$, which may contain semicolons.
func dollarQuoteTag(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	for i := 1; i < len(s) && i < 64; i++ {
		c := s[i]
		if c == '$' {
			return s[:i+1]
		}
		isIdentifier := c == '_' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')
		if !isIdentifier {
			return ""
		}
	}
	return ""
}

func appendStatement(statements []string, candidate string) []string {
	if isBlankOrComment(candidate) {
		return statements
	}
	return append(statements, strings.TrimSpace(candidate))
}

// isBlankOrComment reports whether a fragment carries no executable SQL.
func isBlankOrComment(fragment string) bool {
	for _, line := range strings.Split(fragment, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		return false
	}
	return true
}

func truncateSQL(statement string) string {
	statement = strings.TrimSpace(statement)
	if len(statement) <= 300 {
		return statement
	}
	return statement[:300] + " ..."
}

// MigrationSQL returns a migration's text, for `avas migrate -print`.
func MigrationSQL(dialect Dialect, version int) (string, error) {
	files, err := Migrations(dialect)
	if err != nil {
		return "", err
	}
	for _, migration := range files {
		if migration.Version == version {
			return migration.SQL, nil
		}
	}
	return "", fmt.Errorf("store: no migration %d for %s", version, describeDialect(dialect))
}

var _ = sql.ErrNoRows
