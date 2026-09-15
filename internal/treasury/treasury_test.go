package treasury

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/store"
)

type rig struct {
	t        *testing.T
	store    *store.Store
	treasury *Service
	cfg      *config.Config
	userID   int64
	alice    int64
	bob      int64
	now      time.Time
}

func newRig(t *testing.T, configure func(*config.Config)) *rig {
	t.Helper()
	s, err := store.OpenAndMigrate(filepath.Join(t.TempDir(), "treasury.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	r := &rig{t: t, store: s, now: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s.WithClock(func() time.Time { return r.now })

	r.cfg = config.Load()
	r.cfg.ManualApprovalSat = 10_000_000
	r.cfg.MaxManualAdjustSat = 1_000_000_000
	if configure != nil {
		configure(r.cfg)
	}
	r.treasury = New(s, r.cfg).WithClock(func() time.Time { return r.now })

	ctx := context.Background()
	for _, spec := range []struct {
		email string
		role  string
		dst   *int64
	}{
		{"punter@example.com", store.RoleCustomer, &r.userID},
		{"alice@example.com", store.RoleCompliance, &r.alice},
		{"bob@example.com", store.RoleCompliance, &r.bob},
	} {
		id, err := s.CreateUser(ctx, store.NewUser{
			Email: spec.email, PasswordHash: "x", DisplayName: spec.email,
			DateOfBirth: "1990-01-01", Country: "GB", Role: spec.role,
		})
		if err != nil {
			t.Fatal(err)
		}
		*spec.dst = id
	}
	return r
}

func (r *rig) fund(amountSat int64) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(r.now), store.TxnSpec{
			Kind: store.TxnDeposit,
			Entries: []store.Entry{
				{Account: store.AccountUserCash, UserID: r.userID, AmountSat: amountSat},
				{Account: store.AccountExternalBitcoin, AmountSat: -amountSat},
			},
		})
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) balance() int64 {
	r.t.Helper()
	balance, err := r.store.BalanceSat(context.Background(), r.userID)
	if err != nil {
		r.t.Fatal(err)
	}
	return balance
}

func (r *rig) assertLedgerSound() {
	r.t.Helper()
	problems, err := r.store.CheckLedger(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	for _, problem := range problems {
		r.t.Errorf("ledger invariant broken: %s", problem)
	}
}

func TestSmallCreditAppliesImmediately(t *testing.T) {
	r := newRig(t, nil)
	outcome, err := r.treasury.Request(context.Background(),
		r.userID, 500_000, r.alice, "goodwill after the settlement delay")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Applied {
		t.Error("a small adjustment should apply without a second approver")
	}
	if r.balance() != 500_000 {
		t.Errorf("balance = %d, want 500000", r.balance())
	}
	r.assertLedgerSound()
}

func TestLargeAdjustmentWaitsForASecondPerson(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	outcome, err := r.treasury.Request(ctx, r.userID, 50_000_000, r.alice, "disputed settlement")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Applied {
		t.Fatal("a large adjustment applied without approval")
	}
	if !outcome.NeedsSecond {
		t.Error("a large adjustment should be flagged as needing a second person")
	}
	if r.balance() != 0 {
		t.Errorf("balance = %d before approval, want 0", r.balance())
	}

	// The requester cannot wave through their own request.
	if _, err := r.treasury.Approve(ctx, outcome.AdjustmentID, r.alice, "looks fine to me"); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("self-approval gave %v, want ErrSelfApproval", err)
	}
	if r.balance() != 0 {
		t.Errorf("balance = %d after a refused self-approval, want 0", r.balance())
	}

	// A second person can.
	if _, err := r.treasury.Approve(ctx, outcome.AdjustmentID, r.bob, "checked the bet history"); err != nil {
		t.Fatal(err)
	}
	if r.balance() != 50_000_000 {
		t.Errorf("balance = %d, want 50000000", r.balance())
	}
	r.assertLedgerSound()
}

func TestApprovingTwiceDoesNotPayTwice(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	outcome, err := r.treasury.Request(ctx, r.userID, 50_000_000, r.alice, "disputed settlement")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.treasury.Approve(ctx, outcome.AdjustmentID, r.bob, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.treasury.Approve(ctx, outcome.AdjustmentID, r.bob, "ok again"); !errors.Is(err, ErrNotPending) {
		t.Errorf("second approval gave %v, want ErrNotPending", err)
	}
	if r.balance() != 50_000_000 {
		t.Errorf("balance = %d, want 50000000: the adjustment was applied twice", r.balance())
	}
	r.assertLedgerSound()
}

func TestRejectionMovesNoMoney(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	outcome, err := r.treasury.Request(ctx, r.userID, 50_000_000, r.alice, "disputed settlement")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.treasury.Reject(ctx, outcome.AdjustmentID, r.bob, "the bet settled correctly"); err != nil {
		t.Fatal(err)
	}
	if r.balance() != 0 {
		t.Errorf("balance = %d after rejection, want 0", r.balance())
	}
	if err := r.treasury.Reject(ctx, outcome.AdjustmentID, r.bob, "again"); !errors.Is(err, ErrNotPending) {
		t.Errorf("rejecting twice gave %v, want ErrNotPending", err)
	}
	r.assertLedgerSound()
}

func TestDebitCannotOverdraw(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	r.fund(100_000)

	_, err := r.treasury.Request(ctx, r.userID, -500_000, r.alice, "clawback")
	if !errors.Is(err, ErrWouldOverdraw) {
		t.Errorf("got %v, want ErrWouldOverdraw", err)
	}
	if r.balance() != 100_000 {
		t.Errorf("balance = %d, want it untouched at 100000", r.balance())
	}
	r.assertLedgerSound()
}

func TestDebitWithinBalanceWorks(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	r.fund(1_000_000)

	if _, err := r.treasury.Request(ctx, r.userID, -400_000, r.alice, "duplicate credit reversed"); err != nil {
		t.Fatal(err)
	}
	if r.balance() != 600_000 {
		t.Errorf("balance = %d, want 600000", r.balance())
	}
	r.assertLedgerSound()
}

func TestReasonIsRequired(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	for _, reason := range []string{"", "   ", "\t"} {
		if _, err := r.treasury.Request(ctx, r.userID, 100_000, r.alice, reason); !errors.Is(err, ErrReasonRequired) {
			t.Errorf("Request with reason %q gave %v, want ErrReasonRequired", reason, err)
		}
	}
}

func TestCeilingIsEnforced(t *testing.T) {
	r := newRig(t, func(cfg *config.Config) { cfg.MaxManualAdjustSat = 1_000_000 })
	ctx := context.Background()
	if _, err := r.treasury.Request(ctx, r.userID, 5_000_000, r.alice, "too big"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
	// The ceiling applies to debits too, not just credits.
	if _, err := r.treasury.Request(ctx, r.userID, -5_000_000, r.alice, "too big"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("negative side: got %v, want ErrTooLarge", err)
	}
}

func TestAdjustmentIsAudited(t *testing.T) {
	// A manual adjustment mints or destroys customer balance with no deposit
	// or bet behind it. If it is not in the audit trail it did not happen.
	r := newRig(t, nil)
	ctx := context.Background()

	if _, err := r.treasury.Request(ctx, r.userID, 250_000, r.alice, "goodwill"); err != nil {
		t.Fatal(err)
	}
	entries, err := r.store.AuditForUser(ctx, r.userID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var requested bool
	for _, entry := range entries {
		if entry.Action == "adjustment_requested" {
			requested = true
		}
	}
	if !requested {
		t.Error("the adjustment request was not written to the audit log")
	}
}
