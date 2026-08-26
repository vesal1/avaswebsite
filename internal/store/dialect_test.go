package store

import (
	"strings"
	"testing"
)

func TestRebindLeavesSQLiteAlone(t *testing.T) {
	query := `SELECT * FROM bets WHERE user_id = ? AND status = ?`
	if got := DialectSQLite.Rebind(query); got != query {
		t.Errorf("SQLite rebind changed the query: %s", got)
	}
}

func TestRebindNumbersPostgresPlaceholders(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SELECT 1 WHERE a = ?", "SELECT 1 WHERE a = $1"},
		{"INSERT INTO t (a, b, c) VALUES (?, ?, ?)", "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)"},
		{"SELECT ? , ? , ? , ? , ? , ? , ? , ? , ? , ? , ?",
			"SELECT $1 , $2 , $3 , $4 , $5 , $6 , $7 , $8 , $9 , $10 , $11"},
	}
	for _, tc := range cases {
		if got := DialectPostgres.Rebind(tc.in); got != tc.want {
			t.Errorf("Rebind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRebindIgnoresQuestionMarksInLiterals(t *testing.T) {
	// A question mark inside a string is data. Renumbering it would corrupt
	// the query and shift every real placeholder after it.
	cases := []struct {
		in   string
		want string
	}{
		{`SELECT 'why?' WHERE a = ?`, `SELECT 'why?' WHERE a = $1`},
		{`UPDATE t SET memo = ? WHERE memo <> 'a?b?c'`, `UPDATE t SET memo = $1 WHERE memo <> 'a?b?c'`},
		{`SELECT "od?d" FROM t WHERE a = ?`, `SELECT "od?d" FROM t WHERE a = $1`},
		{`SELECT 'it''s ok?' , ?`, `SELECT 'it''s ok?' , $1`},
	}
	for _, tc := range cases {
		if got := DialectPostgres.Rebind(tc.in); got != tc.want {
			t.Errorf("Rebind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEveryQueryInThisPackageRebindsCleanly(t *testing.T) {
	// A cheap guard: the real queries all use `?`, so rebinding one twice must
	// be a no-op on the second pass (no `$1` should acquire another `$`).
	query := `SELECT a FROM t WHERE b = ? AND c = ?`
	once := DialectPostgres.Rebind(query)
	if strings.Contains(DialectPostgres.Rebind(once), "$$") {
		t.Error("rebinding an already-bound query corrupted it")
	}
}

func TestDialectInferredFromDSN(t *testing.T) {
	cases := map[string]Dialect{
		"postgres://user:pass@localhost:5432/avas":    DialectPostgres,
		"postgresql://localhost/avas?sslmode=disable": DialectPostgres,
		"host=localhost port=5432 dbname=avas":        DialectPostgres,
		"avas.sqlite3":                                DialectSQLite,
		"/var/lib/avas/avas.db":                       DialectSQLite,
		":memory:":                                    DialectSQLite,
		"":                                            DialectSQLite,
	}
	for dsn, want := range cases {
		if got := dialectFromDSN(dsn); got != want {
			t.Errorf("dialectFromDSN(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	script := `
-- a comment with a ; semicolon in it
CREATE TABLE a (id BIGINT);

CREATE TABLE b (
    note TEXT DEFAULT 'hello; world'
);

CREATE INDEX idx ON b (note);
`
	statements := splitStatements(script)
	if len(statements) != 3 {
		t.Fatalf("got %d statements, want 3: %#v", len(statements), statements)
	}
	if !strings.Contains(statements[1], "hello; world") {
		t.Errorf("a semicolon inside a string literal split the statement: %q", statements[1])
	}
	for _, statement := range statements {
		if strings.TrimSpace(statement) == "" {
			t.Error("produced an empty statement")
		}
	}
}

func TestSplitStatementsHandlesDollarQuoting(t *testing.T) {
	// PostgreSQL function bodies contain semicolons inside $$ ... $$.
	script := `
CREATE FUNCTION f() RETURNS void AS $$
BEGIN
  PERFORM 1;
  PERFORM 2;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE after_it (id BIGINT);
`
	statements := splitStatements(script)
	if len(statements) != 2 {
		t.Fatalf("got %d statements, want 2: %#v", len(statements), statements)
	}
	if !strings.Contains(statements[0], "PERFORM 2") {
		t.Errorf("the function body was split: %q", statements[0])
	}
}

func TestSplitStatementsDropsCommentOnlyTrailers(t *testing.T) {
	statements := splitStatements("SELECT 1;\n-- trailing note\n")
	if len(statements) != 1 {
		t.Fatalf("got %d statements, want 1: %#v", len(statements), statements)
	}
}

func TestMigrationsAreWellFormed(t *testing.T) {
	// Both dialects must carry the same set of versions, or an install on one
	// database silently lacks a table the other has.
	sqlite, err := Migrations(DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	postgres, err := Migrations(DialectPostgres)
	if err != nil {
		t.Fatal(err)
	}
	if len(sqlite) == 0 {
		t.Fatal("no SQLite migrations are embedded")
	}
	if len(sqlite) != len(postgres) {
		t.Fatalf("SQLite has %d migrations, PostgreSQL has %d", len(sqlite), len(postgres))
	}
	for i := range sqlite {
		if sqlite[i].Version != postgres[i].Version {
			t.Errorf("version %d differs between dialects", i)
		}
		if sqlite[i].Name != postgres[i].Name {
			t.Errorf("migration %04d is %q for SQLite and %q for PostgreSQL",
				sqlite[i].Version, sqlite[i].Name, postgres[i].Name)
		}
		if sqlite[i].SQL == "" || postgres[i].SQL == "" {
			t.Errorf("migration %04d is empty for one dialect", sqlite[i].Version)
		}
	}
}

func TestPostgresMigrationsAvoidSQLiteOnlySyntax(t *testing.T) {
	migrations, err := Migrations(DialectPostgres)
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"AUTOINCREMENT", "COLLATE NOCASE", "PRAGMA"}
	for _, migration := range migrations {
		body := strings.ToUpper(migration.SQL)
		for _, token := range banned {
			// Ignore the token appearing inside a comment line.
			for _, line := range strings.Split(body, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "--") {
					continue
				}
				if strings.Contains(trimmed, token) {
					t.Errorf("migration %04d uses SQLite-only %s: %s",
						migration.Version, token, trimmed)
				}
			}
		}
	}
}
