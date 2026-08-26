package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.sqlite3")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	first, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("the first migrate applied nothing")
	}
	second, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Errorf("the second migrate applied %d migrations, want 0", second)
	}
}

// TestLedgerRebuildPreservesHistory is the test that matters for migration
// 0002: SQLite cannot alter a CHECK constraint, so ledger_entries is rebuilt.
// A ledger is append-only history, and a rebuild that dropped or renumbered a
// row would destroy the audit trail the ledger exists to provide.
func TestLedgerRebuildPreservesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rebuild.sqlite3")

	// Bring a database up to 0001 only, then write real ledger history into it.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.migrateTo(ctx, 1); err != nil {
		t.Fatal(err)
	}

	userID, err := s.CreateUser(ctx, NewUser{
		Email: "a@example.com", PasswordHash: "x", DisplayName: "A",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Tx(ctx, func(tx *Tx) error {
			_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
				Kind: TxnDeposit,
				Entries: []Entry{
					{Account: AccountUserCash, UserID: userID, AmountSat: 100_000},
					{Account: AccountExternalBitcoin, AmountSat: -100_000},
				},
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	before, err := s.countLedgerEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeBalance, err := s.BalanceSat(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	beforeIDs, err := s.ledgerEntryIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Now apply 0002, which rebuilds the table.
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}

	after, err := s.countLedgerEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("ledger entries went from %d to %d across the rebuild", before, after)
	}
	afterBalance, err := s.BalanceSat(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if afterBalance != beforeBalance {
		t.Errorf("balance changed across the rebuild: %d then %d", beforeBalance, afterBalance)
	}
	afterIDs, err := s.ledgerEntryIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(afterIDs, ",") != strings.Join(beforeIDs, ",") {
		t.Errorf("entry ids were renumbered: %v then %v", beforeIDs, afterIDs)
	}

	problems, err := s.CheckLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Errorf("ledger problems after the rebuild: %v", problems)
	}

	// The whole point of the rebuild: the new account is now accepted.
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
			Kind: TxnBonus,
			Entries: []Entry{
				{Account: AccountUserBonus, UserID: userID, AmountSat: 50_000},
				{Account: AccountHousePromotions, AmountSat: -50_000},
			},
		})
		return err
	}); err != nil {
		t.Errorf("the new bonus account was refused after the rebuild: %v", err)
	}

	// And bonus money still has to be attributable to somebody.
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
			Kind: TxnBonus,
			Entries: []Entry{
				{Account: AccountUserBonus, AmountSat: 50_000},
				{Account: AccountHousePromotions, AmountSat: -50_000},
			},
		})
		return err
	}); err == nil {
		t.Error("an unattributed bonus entry was accepted")
	}
	s.Close()
}

func TestEditedMigrationIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dirty.sqlite3")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate someone editing an already-applied migration file.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err == nil {
		t.Fatal("migrating over an edited migration should be refused")
	}
}

// migrateTo applies migrations up to and including a version. Test-only: it
// exists so a test can stand at an older schema and then upgrade.
func (s *Store) migrateTo(ctx context.Context, target int) error {
	files, err := Migrations(s.db.dialect)
	if err != nil {
		return err
	}
	if err := s.ensureMigrationsTable(ctx); err != nil {
		return err
	}
	for _, migration := range files {
		if migration.Version > target {
			break
		}
		if err := s.runMigration(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) countLedgerEntries(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries`).Scan(&count)
	return count, err
}

func (s *Store) ledgerEntryIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM ledger_entries ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
