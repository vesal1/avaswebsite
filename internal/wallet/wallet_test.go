package wallet

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vesal1/avaswebsite/internal/bitcoin"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/store"
)

type harness struct {
	store  *store.Store
	mock   *bitcoin.Mock
	wallet *Service
	cfg    *config.Config
	userID int64
	now    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "wallet.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	h := &harness{store: s, now: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)}
	s.WithClock(func() time.Time { return h.now })

	h.cfg = config.Load()
	h.cfg.AllowedCountries = []string{"GB"}
	h.cfg.BitcoinNetwork = bitcoin.Regtest
	h.cfg.DepositConfirmations = 2
	h.cfg.MinDepositSat = 20_000
	h.cfg.MinWithdrawalSat = 50_000
	h.cfg.WithdrawalFeeSat = 2_500
	h.cfg.ManualReviewSat = 100_000_000
	h.cfg.AMLReportingSat = 100_000_000

	h.mock = bitcoin.NewMock(bitcoin.Regtest).WithClock(func() time.Time { return h.now })
	comp := compliance.New(s, h.cfg).WithClock(func() time.Time { return h.now })
	h.wallet = New(s, h.mock, comp, h.cfg)

	ctx := context.Background()
	h.userID, err = s.CreateUser(ctx, store.NewUser{
		Email: "punter@example.com", PasswordHash: "x", DisplayName: "Punter",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKYCStatus(ctx, h.userID, store.KYCVerified); err != nil {
		t.Fatal(err)
	}
	return h
}

// assertLedgerSound fails the test if any ledger invariant is broken. Every
// money-moving test calls it: a wallet that produces the right balance through
// an unbalanced ledger is still broken.
func (h *harness) assertLedgerSound(t *testing.T) {
	t.Helper()
	problems, err := h.store.CheckLedger(context.Background())
	if err != nil {
		t.Fatalf("check ledger: %v", err)
	}
	for _, problem := range problems {
		t.Errorf("ledger invariant broken: %s", problem)
	}
}

func (h *harness) deposit(t *testing.T, amountSat int64, confirmations int) string {
	t.Helper()
	ctx := context.Background()
	address, err := h.wallet.EnsureDepositAddress(ctx, h.userID)
	if err != nil {
		t.Fatalf("ensure address: %v", err)
	}
	if _, err := h.mock.CreditPayment(address, amountSat, confirmations); err != nil {
		t.Fatalf("simulate payment: %v", err)
	}
	return address
}

func TestDepositAddressIsStableAndValid(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, err := h.wallet.EnsureDepositAddress(ctx, h.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := bitcoin.ValidateAddress(first, bitcoin.Regtest); err != nil {
		t.Errorf("issued address is invalid: %v", err)
	}
	second, err := h.wallet.EnsureDepositAddress(ctx, h.userID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("a customer should keep the same deposit address until it is retired")
	}
}

func TestDepositIsNotCreditedBeforeConfirmations(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 500_000, 1) // one confirmation, policy requires two

	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	balance, err := h.wallet.Balance(ctx, h.userID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		t.Errorf("balance = %d, want 0: an unconfirmed deposit was credited", balance)
	}
	h.assertLedgerSound(t)
}

func TestDepositIsCreditedOnceConfirmed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 500_000, 3)

	result, err := h.wallet.SyncDeposits(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Credited != 1 {
		t.Fatalf("credited %d deposits, want 1 (errors: %v)", result.Credited, result.Errors)
	}
	balance, _ := h.wallet.Balance(ctx, h.userID)
	if balance != 500_000 {
		t.Errorf("balance = %d, want 500000", balance)
	}
	h.assertLedgerSound(t)
}

func TestRepeatedSyncDoesNotDoubleCredit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 500_000, 3)

	for i := 0; i < 5; i++ {
		if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	balance, _ := h.wallet.Balance(ctx, h.userID)
	if balance != 500_000 {
		t.Errorf("balance = %d after five syncs, want 500000", balance)
	}
	h.assertLedgerSound(t)
}

func TestDepositBreachingALimitIsHeldNotCredited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	comp := compliance.New(h.store, h.cfg).WithClock(func() time.Time { return h.now })
	if _, err := comp.SetLimit(ctx, h.userID, store.LimitDepositDaily, 100_000); err != nil {
		t.Fatal(err)
	}
	h.deposit(t, 500_000, 3) // well over the limit

	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	balance, _ := h.wallet.Balance(ctx, h.userID)
	if balance != 0 {
		t.Errorf("balance = %d: a deposit past the customer's own limit was credited", balance)
	}

	flags, err := h.store.FlagsForUser(ctx, h.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) == 0 {
		t.Error("holding a deposit should raise a flag for someone to action")
	}
	h.assertLedgerSound(t)
}

func TestWithdrawalDebitsImmediately(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 1_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	if _, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 400_000); err != nil {
		t.Fatal(err)
	}

	// The requested amount and its fee leave the spendable balance at once, so
	// the same funds cannot also be staked.
	balance, _ := h.wallet.Balance(ctx, h.userID)
	want := int64(1_000_000 - 400_000 - 2_500)
	if balance != want {
		t.Errorf("balance = %d, want %d", balance, want)
	}
	h.assertLedgerSound(t)
}

func TestWithdrawalCannotOverdraw(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 100_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	_, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 99_000)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("got %v, want ErrInsufficientFunds: the fee must be covered too", err)
	}
	h.assertLedgerSound(t)
}

func TestRejectedWithdrawalReturnsTheFeeToo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 1_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	id, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.wallet.RejectWithdrawal(ctx, id, h.userID, "address on a sanctions list"); err != nil {
		t.Fatal(err)
	}

	balance, _ := h.wallet.Balance(ctx, h.userID)
	if balance != 1_000_000 {
		t.Errorf("balance = %d, want the full 1000000 back including the fee", balance)
	}
	h.assertLedgerSound(t)
}

func TestCustomerCanCancelTheirOwnRequestOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 1_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	other, err := h.store.CreateUser(ctx, store.NewUser{
		Email: "other@example.com", PasswordHash: "x", DisplayName: "Other",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	id, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.wallet.CancelWithdrawal(ctx, other, id); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("another customer cancelled the withdrawal: %v", err)
	}
	if err := h.wallet.CancelWithdrawal(ctx, h.userID, id); err != nil {
		t.Fatal(err)
	}
	balance, _ := h.wallet.Balance(ctx, h.userID)
	if balance != 1_000_000 {
		t.Errorf("balance = %d after cancelling, want 1000000", balance)
	}
	h.assertLedgerSound(t)
}

func TestReleasedWithdrawalLeavesTheLedgerBalanced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 1_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	id, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.wallet.ReleaseWithdrawal(ctx, id, h.userID); err != nil {
		t.Fatal(err)
	}

	withdrawal, err := h.store.GetWithdrawal(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawal.Status != store.WithdrawalBroadcast {
		t.Errorf("status = %s, want broadcast", withdrawal.Status)
	}
	if withdrawal.TxID == "" {
		t.Error("a broadcast withdrawal should record its txid")
	}
	// Nothing should be parked in suspense once the coins are on the wire.
	suspense, err := h.store.AccountBalanceSat(ctx, store.AccountWithdrawalSuspense)
	if err != nil {
		t.Fatal(err)
	}
	if suspense != 0 {
		t.Errorf("withdrawal suspense = %d, want 0", suspense)
	}
	h.assertLedgerSound(t)
}

func TestWithdrawalRequiresVerifiedIdentity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.SetKYCStatus(ctx, h.userID, store.KYCNone); err != nil {
		t.Fatal(err)
	}
	h.deposit(t, 1_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	_, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 400_000)
	refusal, ok := compliance.AsRefusal(err)
	if !ok || refusal.Code != compliance.CodeKYCRequired {
		t.Errorf("got %v, want a KYC refusal", err)
	}
}

func TestSelfExcludedCustomerCanStillWithdraw(t *testing.T) {
	// Self-exclusion stops play, not access to your own money. Trapping funds
	// would give a customer a reason not to exclude themselves.
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 1_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	comp := compliance.New(h.store, h.cfg).WithClock(func() time.Time { return h.now })
	if err := comp.SelfExclude(ctx, h.userID, 0, "taking a break for good"); err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	if _, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 400_000); err != nil {
		t.Fatalf("a self-excluded customer must still be able to withdraw: %v", err)
	}
	h.assertLedgerSound(t)
}

func TestLargeWithdrawalGoesToReview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 500_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	payee, _ := h.mock.NewDepositAddress(ctx, "payee")
	id, err := h.wallet.RequestWithdrawal(ctx, h.userID, payee.Address, 200_000_000)
	if err != nil {
		t.Fatal(err)
	}
	withdrawal, err := h.store.GetWithdrawal(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawal.Status != store.WithdrawalReview {
		t.Errorf("status = %s, want review for an amount over the threshold", withdrawal.Status)
	}
	h.assertLedgerSound(t)
}

func TestWithdrawalToInvalidAddressIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deposit(t, 1_000_000, 3)
	if _, err := h.wallet.SyncDeposits(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}

	for _, address := range []string{
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", // mainnet, wrong network
		"bcrt1qmistyped",
		"",
	} {
		if _, err := h.wallet.RequestWithdrawal(ctx, h.userID, address, 400_000); !errors.Is(err, bitcoin.ErrInvalidAddress) {
			t.Errorf("RequestWithdrawal(%q) = %v, want ErrInvalidAddress", address, err)
		}
	}
	balance, _ := h.wallet.Balance(ctx, h.userID)
	if balance != 1_000_000 {
		t.Errorf("a refused withdrawal moved money: balance = %d", balance)
	}
	h.assertLedgerSound(t)
}

func TestPaymentToUnknownAddressIsNotCredited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A payment to an address the book never issued: it must not land on
	// anyone's balance, and it must be surfaced rather than swallowed.
	stray, err := h.mock.NewDepositAddress(ctx, "not-a-customer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mock.CreditPayment(stray.Address, 750_000, 6); err != nil {
		t.Fatal(err)
	}

	result, err := h.wallet.SyncDeposits(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Credited != 0 {
		t.Errorf("credited %d payments to an unknown address", result.Credited)
	}
	if len(result.Errors) == 0 {
		t.Error("an unattributable payment should be reported, not ignored")
	}
	h.assertLedgerSound(t)
}
