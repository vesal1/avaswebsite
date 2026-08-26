package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// postgresDSN returns the DSN to test against, or "" to skip.
//
// PostgreSQL is the supported production database, so the invariants that
// protect customer money are exercised against a real server rather than
// inferred from SQLite passing. Set AVAS_TEST_POSTGRES to run these.
func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AVAS_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set AVAS_TEST_POSTGRES to run the PostgreSQL integration tests")
	}
	return dsn
}

// freshPostgres gives each test its own schema inside the shared database, so
// tests cannot see each other's rows and none of them needs a cleanup step
// that could be skipped on failure.
func freshPostgres(t *testing.T) *Store {
	t.Helper()
	dsn := postgresDSN(t)

	schema := "test_" + strings.ToLower(strings.NewReplacer("/", "", "-", "", " ", "_").Replace(t.Name()))
	if len(schema) > 60 {
		schema = schema[:60]
	}

	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	scoped := fmt.Sprintf("%s%ssearch_path=%s", dsn, separator, schema)

	admin, err := Open(dsn)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	ctx := context.Background()
	if _, err := admin.db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := admin.db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	admin.Close()

	s, err := OpenAndMigrate(scoped)
	if err != nil {
		t.Fatalf("migrate PostgreSQL schema: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		if cleanup, err := Open(dsn); err == nil {
			_, _ = cleanup.db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			cleanup.Close()
		}
	})
	return s
}

func TestPostgresMigratesAndIsIdempotent(t *testing.T) {
	s := freshPostgres(t)
	ctx := context.Background()

	// Already migrated by the helper, so a second run must be a no-op rather
	// than an error or a duplicate apply.
	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Errorf("a second migrate applied %d migrations, want 0", applied)
	}

	states, err := s.MigrationStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if !state.Applied {
			t.Errorf("migration %04d (%s) did not apply", state.Version, state.Name)
		}
		if state.Mismatch {
			t.Errorf("migration %04d checksum mismatch", state.Version)
		}
	}
}

func TestPostgresLedgerInvariants(t *testing.T) {
	s := freshPostgres(t)
	ctx := context.Background()

	userID, err := s.CreateUser(ctx, NewUser{
		Email: "pg@example.com", PasswordHash: "x", DisplayName: "PG",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A balanced deposit.
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
			Kind: TxnDeposit,
			Entries: []Entry{
				{Account: AccountUserCash, UserID: userID, AmountSat: 500_000},
				{Account: AccountExternalBitcoin, AmountSat: -500_000},
			},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	balance, err := s.BalanceSat(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 500_000 {
		t.Errorf("balance = %d, want 500000", balance)
	}

	// An unbalanced one must be refused on PostgreSQL exactly as on SQLite.
	err = s.Tx(ctx, func(tx *Tx) error {
		_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
			Kind: TxnDeposit,
			Entries: []Entry{
				{Account: AccountUserCash, UserID: userID, AmountSat: 100_000},
				{Account: AccountExternalBitcoin, AmountSat: -90_000},
			},
		})
		return err
	})
	if err == nil {
		t.Fatal("an unbalanced transaction was accepted")
	}

	problems, err := s.CheckLedger(ctx)
	if err != nil {
		t.Fatalf("CheckLedger on PostgreSQL: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("ledger problems: %v", problems)
	}
}

func TestPostgresCatchesNegativeBalance(t *testing.T) {
	// The negative-balance check uses an aggregate in HAVING, which PostgreSQL
	// and SQLite disagree about. This is the test that catches it.
	s := freshPostgres(t)
	ctx := context.Background()

	userID, err := s.CreateUser(ctx, NewUser{
		Email: "pg@example.com", PasswordHash: "x", DisplayName: "PG",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
			Kind: TxnAdjustment,
			Entries: []Entry{
				{Account: AccountUserCash, UserID: userID, AmountSat: -50_000},
				{Account: AccountHouseRevenue, AmountSat: 50_000},
			},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	problems, err := s.CheckLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || problems[0].Kind != "negative_balance" {
		t.Fatalf("got %v, want one negative_balance problem", problems)
	}
}

func TestPostgresDepositIsUniquePerOutput(t *testing.T) {
	s := freshPostgres(t)
	ctx := context.Background()

	userID, err := s.CreateUser(ctx, NewUser{
		Email: "pg@example.com", PasswordHash: "x", DisplayName: "PG",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := s.RecordDeposit(ctx, Deposit{
		UserID: userID, Address: "bcrt1q", TxID: "tx1", Vout: 0, AmountSat: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RecordDeposit(ctx, Deposit{
		UserID: userID, Address: "bcrt1q", TxID: "tx1", Vout: 0, AmountSat: 100_000,
		Confirmations: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same on-chain output produced two deposit rows: %d and %d", first, second)
	}
}

func TestPostgresCaseInsensitiveEmail(t *testing.T) {
	// SQLite got this from COLLATE NOCASE. PostgreSQL gets it from the store
	// normalising addresses before they are written or looked up, so both
	// databases have to behave the same way here.
	s := freshPostgres(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, NewUser{
		Email: "Punter@Example.com", PasswordHash: "x", DisplayName: "PG",
		DateOfBirth: "1990-01-01", Country: "GB",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, NewUser{
		Email: "punter@example.COM", PasswordHash: "x", DisplayName: "Other",
		DateOfBirth: "1990-01-01", Country: "GB",
	}); err == nil {
		t.Error("a duplicate email in different case was accepted")
	}
	if _, err := s.GetUserByEmail(ctx, "PUNTER@EXAMPLE.COM"); err != nil {
		t.Errorf("looking up an email in a different case failed: %v", err)
	}
}
