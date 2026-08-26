package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	// A file in the test's temp dir rather than :memory:, so each test gets a
	// genuinely isolated database.
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func makeUser(t *testing.T, s *Store, email string) int64 {
	t.Helper()
	id, err := s.CreateUser(context.Background(), NewUser{
		Email: email, PasswordHash: "x", DisplayName: "Test",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func TestSchemaApplies(t *testing.T) {
	s := testStore(t)
	if err := s.db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestDuplicateEmailIsRejected(t *testing.T) {
	s := testStore(t)
	makeUser(t, s, "punter@example.com")
	_, err := s.CreateUser(context.Background(), NewUser{
		Email: "PUNTER@example.com", PasswordHash: "x", DisplayName: "Other",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err == nil {
		t.Fatal("a duplicate email should be rejected regardless of case")
	}
}

func TestLedgerRejectsUnbalancedTransaction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")

	err := s.Tx(ctx, func(tx *sql.Tx) error {
		_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
			Kind: TxnDeposit,
			Entries: []Entry{
				{Account: AccountUserCash, UserID: userID, AmountSat: 100_000},
				{Account: AccountExternalBitcoin, AmountSat: -90_000}, // out by 10,000
			},
		})
		return err
	})
	if err == nil {
		t.Fatal("an unbalanced transaction must be refused")
	}

	total, err := s.LedgerTotal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("the rejected transaction left %d sat behind", total)
	}
}

func TestLedgerRejectsUnattributedUserCash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
			Kind: TxnDeposit,
			Entries: []Entry{
				{Account: AccountUserCash, AmountSat: 100_000}, // no user id
				{Account: AccountExternalBitcoin, AmountSat: -100_000},
			},
		})
		return err
	})
	if err == nil {
		t.Fatal("customer money must always be attributable to a customer")
	}
}

func TestBalanceIsDerivedFromTheLedger(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")

	post := func(amount int64, kind string) {
		t.Helper()
		if err := s.Tx(ctx, func(tx *sql.Tx) error {
			_, err := PostTxn(ctx, tx, Timestamp(s.Now()), TxnSpec{
				Kind: kind,
				Entries: []Entry{
					{Account: AccountUserCash, UserID: userID, AmountSat: amount},
					{Account: AccountExternalBitcoin, AmountSat: -amount},
				},
			})
			return err
		}); err != nil {
			t.Fatalf("post %d: %v", amount, err)
		}
	}
	post(500_000, TxnDeposit)
	post(-200_000, TxnWithdrawal)

	balance, err := s.BalanceSat(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 300_000 {
		t.Errorf("balance = %d, want 300000", balance)
	}

	problems, err := s.CheckLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Errorf("ledger problems: %v", problems)
	}
}

func TestCheckLedgerSpotsNegativeBalance(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")

	// Posting a balanced but overdrawing transaction: the ledger stays balanced
	// overall, so only the per-user check can catch it.
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
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
		t.Fatalf("expected a negative balance problem, got %v", problems)
	}
}

func TestDepositCannotBeCreditedTwice(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")

	depositID, err := s.RecordDeposit(ctx, Deposit{
		UserID: userID, Address: "bcrt1qexample", TxID: "abc123", Vout: 0,
		AmountSat: 250_000, Confirmations: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	credit := func() error {
		return s.Tx(ctx, func(tx *sql.Tx) error {
			at := Timestamp(s.Now())
			txnID, err := PostTxn(ctx, tx, at, TxnSpec{
				Kind: TxnDeposit, RefType: "deposit", RefID: depositID,
				Entries: []Entry{
					{Account: AccountUserCash, UserID: userID, AmountSat: 250_000},
					{Account: AccountDepositSuspense, AmountSat: -250_000},
				},
			})
			if err != nil {
				return err
			}
			return MarkDepositCreditedTx(ctx, tx, depositID, txnID, at)
		})
	}

	if err := credit(); err != nil {
		t.Fatalf("first credit: %v", err)
	}
	if err := credit(); err == nil {
		t.Fatal("a deposit must not be creditable twice")
	}

	balance, err := s.BalanceSat(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 250_000 {
		t.Errorf("balance = %d, want 250000: the deposit was double-credited", balance)
	}
}

func TestRecordDepositIsIdempotentPerOutput(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")

	first, err := s.RecordDeposit(ctx, Deposit{
		UserID: userID, Address: "bcrt1q", TxID: "tx1", Vout: 0, AmountSat: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The same output seen again with more confirmations must update, not insert.
	second, err := s.RecordDeposit(ctx, Deposit{
		UserID: userID, Address: "bcrt1q", TxID: "tx1", Vout: 0, AmountSat: 100_000,
		Confirmations: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same output produced two deposit rows: %d and %d", first, second)
	}
	deposits, err := s.DepositsForUser(ctx, userID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deposits) != 1 {
		t.Fatalf("got %d deposits, want 1", len(deposits))
	}
	if deposits[0].Confirmations != 4 || deposits[0].Status != DepositConfirmed {
		t.Errorf("deposit not updated: %+v", deposits[0])
	}
}

func TestEffectiveLimitIgnoresFutureIncreases(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// A tight limit in force now.
	if _, err := s.SetPlayerLimit(ctx, PlayerLimit{
		UserID: userID, Kind: LimitDepositDaily, Amount: 100_000,
		EffectiveFrom: now.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// A loosening that has not yet taken effect.
	if _, err := s.SetPlayerLimit(ctx, PlayerLimit{
		UserID: userID, Kind: LimitDepositDaily, Amount: 5_000_000,
		EffectiveFrom: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	amount, ok, err := s.EffectiveLimit(ctx, userID, LimitDepositDaily, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a limit to be in force")
	}
	if amount != 100_000 {
		t.Errorf("effective limit = %d, want the tighter 100000", amount)
	}

	// Once the cooling-off period passes, the looser limit applies.
	amount, _, err = s.EffectiveLimit(ctx, userID, LimitDepositDaily, now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if amount != 100_000 {
		t.Errorf("effective limit = %d; MIN keeps the tighter limit until it is revoked", amount)
	}
}

func TestActiveExclusionRespectsWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	if _, err := s.CreateExclusion(ctx, Exclusion{
		UserID: userID, Kind: ExclusionCoolOff,
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if _, active, err := s.ActiveExclusion(ctx, userID, now); err != nil || !active {
		t.Fatalf("expected an active exclusion, active=%v err=%v", active, err)
	}
	if _, active, err := s.ActiveExclusion(ctx, userID, now.Add(2*time.Hour)); err != nil || active {
		t.Fatalf("expected the cool-off to have expired, active=%v err=%v", active, err)
	}
}

func TestPermanentSelfExclusionNeverExpires(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	userID := makeUser(t, s, "a@example.com")
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	if _, err := s.CreateExclusion(ctx, Exclusion{
		UserID: userID, Kind: ExclusionSelfExclusion,
		StartsAt: now, Permanent: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, active, err := s.ActiveExclusion(ctx, userID, now.AddDate(50, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Error("a permanent self-exclusion must never lapse")
	}
}
