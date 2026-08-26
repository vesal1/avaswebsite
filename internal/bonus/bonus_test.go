package bonus

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
	t       *testing.T
	store   *store.Store
	bonus   *Service
	cfg     *config.Config
	userID  int64
	staffID int64
	now     time.Time
}

func newRig(t *testing.T, configure func(*config.Config)) *rig {
	t.Helper()
	s, err := store.OpenAndMigrate(filepath.Join(t.TempDir(), "bonus.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	r := &rig{t: t, store: s, now: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s.WithClock(func() time.Time { return r.now })

	r.cfg = config.Load()
	r.cfg.Env = "development"
	if configure != nil {
		configure(r.cfg)
	}
	r.bonus = New(s, r.cfg).WithClock(func() time.Time { return r.now })

	ctx := context.Background()
	r.userID, err = s.CreateUser(ctx, store.NewUser{
		Email: "punter@example.com", PasswordHash: "x", DisplayName: "Punter",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.staffID, err = s.CreateUser(ctx, store.NewUser{
		Email: "staff@example.com", PasswordHash: "x", DisplayName: "Staff",
		DateOfBirth: "1985-01-01", Country: "GB", Role: store.RoleAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) assertLedgerSound() {
	r.t.Helper()
	problems, err := r.store.CheckLedger(context.Background())
	if err != nil {
		r.t.Fatalf("check ledger: %v", err)
	}
	for _, problem := range problems {
		r.t.Errorf("ledger invariant broken: %s", problem)
	}
}

func (r *rig) cash() int64 {
	r.t.Helper()
	balance, err := r.store.BalanceSat(context.Background(), r.userID)
	if err != nil {
		r.t.Fatal(err)
	}
	return balance
}

func (r *rig) bonusBalance() int64 {
	r.t.Helper()
	balance, err := r.store.BonusBalanceSat(context.Background(), r.userID)
	if err != nil {
		r.t.Fatal(err)
	}
	return balance
}

func TestGrantCreditsBonusNotCash(t *testing.T) {
	// The central rule of the package: a bonus is not spendable-as-cash money.
	r := newRig(t, nil)
	ctx := context.Background()

	grant, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, Kind: store.BonusFreeCredit,
		AmountSat: 1_000_000, WageringX100: 500, ValidDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.WageringRequiredSat != 5_000_000 {
		t.Errorf("wagering required = %d, want 5000000 (5x)", grant.WageringRequiredSat)
	}
	if r.bonusBalance() != 1_000_000 {
		t.Errorf("bonus balance = %d, want 1000000", r.bonusBalance())
	}
	if r.cash() != 0 {
		t.Errorf("cash = %d, want 0: a bonus must not land in withdrawable cash", r.cash())
	}
	r.assertLedgerSound()
}

func TestZeroWageringBonusConvertsImmediately(t *testing.T) {
	// A bonus with nothing to clear is cash in all but name. Leaving it in the
	// bonus account would strand it there forever.
	r := newRig(t, nil)
	ctx := context.Background()

	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, Kind: store.BonusComp, AmountSat: 250_000, WageringX100: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if r.cash() != 250_000 {
		t.Errorf("cash = %d, want 250000", r.cash())
	}
	if r.bonusBalance() != 0 {
		t.Errorf("bonus balance = %d, want 0", r.bonusBalance())
	}
	r.assertLedgerSound()
}

func TestWageringClearsTheBonusIntoCash(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, Kind: store.BonusFreeCredit,
		AmountSat: 100_000, WageringX100: 200, // needs 200,000 wagered
	}); err != nil {
		t.Fatal(err)
	}

	// Sportsbook contributes 50%, so 400,000 of stakes clears 200,000.
	for i := int64(1); i <= 4; i++ {
		if err := r.bonus.RecordStake(ctx, r.userID, i, 100_000, 2000, "sportsbook"); err != nil {
			t.Fatal(err)
		}
	}

	if r.bonusBalance() != 0 {
		t.Errorf("bonus balance = %d, want 0 once cleared", r.bonusBalance())
	}
	if r.cash() != 100_000 {
		t.Errorf("cash = %d, want the cleared bonus converted", r.cash())
	}
	grants, err := r.store.BonusGrantsForUser(ctx, r.userID)
	if err != nil {
		t.Fatal(err)
	}
	if grants[0].Status != store.GrantCompleted {
		t.Errorf("grant status = %s, want completed", grants[0].Status)
	}
	r.assertLedgerSound()
}

func TestWageringIsNotCreditedTwiceForTheSameBet(t *testing.T) {
	// Settlement can be replayed. Counting a stake twice would clear a bonus
	// the customer had not actually earned.
	r := newRig(t, nil)
	ctx := context.Background()

	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 100_000, WageringX100: 1000, // 1,000,000 needed
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := r.bonus.RecordStake(ctx, r.userID, 42, 200_000, 2000, "sportsbook"); err != nil {
			t.Fatal(err)
		}
	}
	grants, err := r.store.BonusGrantsForUser(ctx, r.userID)
	if err != nil {
		t.Fatal(err)
	}
	if grants[0].WageringDoneSat != 100_000 {
		t.Errorf("wagering done = %d, want 100000 from a single counted bet",
			grants[0].WageringDoneSat)
	}
	r.assertLedgerSound()
}

func TestShortPricesDoNotClearABonus(t *testing.T) {
	// Without a minimum price, a bonus clears risk-free on 1.01 shots.
	r := newRig(t, nil)
	ctx := context.Background()

	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 100_000, WageringX100: 200, MinOddsMilli: 1500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.bonus.RecordStake(ctx, r.userID, 1, 500_000, 1010, "sportsbook"); err != nil {
		t.Fatal(err)
	}
	grants, _ := r.store.BonusGrantsForUser(ctx, r.userID)
	if grants[0].WageringDoneSat != 0 {
		t.Errorf("a 1.01 stake credited %d toward wagering", grants[0].WageringDoneSat)
	}

	// A qualifying price does count.
	if err := r.bonus.RecordStake(ctx, r.userID, 2, 100_000, 2000, "sportsbook"); err != nil {
		t.Fatal(err)
	}
	grants, _ = r.store.BonusGrantsForUser(ctx, r.userID)
	if grants[0].WageringDoneSat != 50_000 {
		t.Errorf("wagering done = %d, want 50000", grants[0].WageringDoneSat)
	}
	r.assertLedgerSound()
}

func TestContributionRatesDifferByProduct(t *testing.T) {
	if ContributionRateBps("slots") <= ContributionRateBps("sportsbook") {
		t.Error("slots should contribute at least as much as sports")
	}
	if ContributionRateBps("poker") >= ContributionRateBps("sportsbook") {
		t.Error("poker should contribute less than sports")
	}
	if ContributionRateBps("something else") != 5_000 {
		t.Error("an unknown product should fall back to a middling rate")
	}
}

func TestForfeitReturnsBonusAndLeavesCashAlone(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	// Give the customer some real money alongside the bonus.
	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 300_000, WageringX100: 0,
	}); err != nil {
		t.Fatal(err)
	}
	cashBefore := r.cash()

	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 500_000, WageringX100: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	forfeited, err := r.bonus.Forfeit(ctx, r.userID, "wants to withdraw")
	if err != nil {
		t.Fatal(err)
	}
	if forfeited != 500_000 {
		t.Errorf("forfeited %d, want 500000", forfeited)
	}
	if r.bonusBalance() != 0 {
		t.Errorf("bonus balance = %d after forfeiting, want 0", r.bonusBalance())
	}
	if r.cash() != cashBefore {
		t.Errorf("forfeiting a bonus changed cash from %d to %d; a customer's own money must not be touched",
			cashBefore, r.cash())
	}
	r.assertLedgerSound()
}

func TestForfeitWithNothingActiveIsAnError(t *testing.T) {
	r := newRig(t, nil)
	if _, err := r.bonus.Forfeit(context.Background(), r.userID, "nothing here"); !errors.Is(err, ErrNothingToForfeit) {
		t.Errorf("got %v, want ErrNothingToForfeit", err)
	}
}

func TestExpiredBonusIsReclaimed(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 400_000, WageringX100: 500, ValidDays: 7,
	}); err != nil {
		t.Fatal(err)
	}
	// Not yet due.
	if closed, err := r.bonus.ExpireOverdue(ctx); err != nil || closed != 0 {
		t.Fatalf("expired %d bonuses early (err %v)", closed, err)
	}

	r.now = r.now.AddDate(0, 0, 8)
	closed, err := r.bonus.ExpireOverdue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("expired %d bonuses, want 1", closed)
	}
	if r.bonusBalance() != 0 {
		t.Errorf("bonus balance = %d after expiry, want 0", r.bonusBalance())
	}
	r.assertLedgerSound()
}

func TestPartlySpentBonusCannotConvertMoreThanRemains(t *testing.T) {
	// Bonus money is one fungible pot that staking draws down. Converting the
	// amount originally awarded would mint cash out of nothing.
	r := newRig(t, nil)
	ctx := context.Background()

	grant, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 1_000_000, WageringX100: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the customer staking 600,000 of the bonus and losing it.
	if err := r.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(r.now), store.TxnSpec{
			Kind: store.TxnBetStake,
			Entries: []store.Entry{
				{Account: store.AccountUserBonus, UserID: r.userID, AmountSat: -600_000},
				{Account: store.AccountHouseRevenue, AmountSat: 600_000},
			},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Now clear the wagering, which converts what is left.
	if err := r.bonus.RecordStake(ctx, r.userID, 1, 2_000_000, 2000, "sportsbook"); err != nil {
		t.Fatal(err)
	}

	if r.cash() > 400_000 {
		t.Errorf("converted %d to cash, but only 400000 of bonus remained", r.cash())
	}
	if r.bonusBalance() < 0 {
		t.Errorf("bonus balance went negative: %d", r.bonusBalance())
	}
	_ = grant
	r.assertLedgerSound()
}

func TestTestCreditsAreRefusedInProduction(t *testing.T) {
	r := newRig(t, func(cfg *config.Config) {
		cfg.Env = "production"
		cfg.AllowTestCredits = false
	})
	_, err := r.bonus.Grant(context.Background(), GrantRequest{
		UserID: r.userID, Kind: store.BonusTestCredit, AmountSat: 1_000_000,
	})
	if !errors.Is(err, ErrTestCreditsRefused) {
		t.Errorf("got %v, want ErrTestCreditsRefused", err)
	}
	if r.bonusBalance() != 0 || r.cash() != 0 {
		t.Error("a refused test credit still moved money")
	}
}

func TestTestCreditsWorkInDevelopment(t *testing.T) {
	r := newRig(t, nil)
	grant, err := r.bonus.Grant(context.Background(), GrantRequest{
		UserID: r.userID, Kind: store.BonusTestCredit, AmountSat: 1_000_000, WageringX100: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !grant.IsTest {
		t.Error("a test credit should be flagged as test money")
	}
	if r.cash() != 1_000_000 {
		t.Errorf("cash = %d, want 1000000", r.cash())
	}
	r.assertLedgerSound()
}

func TestOfferRespectsMatchCapAndPerUserLimit(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	offerID, err := r.store.CreateBonusOffer(ctx, store.BonusOffer{
		Code: "MATCH50", Name: "50% up to 0.005", Kind: store.BonusDepositMatch,
		MatchBps: 5_000, MaxAmountSat: 500_000, MinDepositSat: 100_000,
		WageringX100: 500, ValidDays: 30, MaxPerUser: 1, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A deposit under the minimum does not qualify.
	if _, err := r.bonus.GrantFromOffer(ctx, r.userID, offerID, 0, 50_000); !errors.Is(err, ErrOfferUnavailable) {
		t.Errorf("a below-minimum deposit gave %v, want ErrOfferUnavailable", err)
	}

	// A large deposit is capped at the maximum award.
	grant, err := r.bonus.GrantFromOffer(ctx, r.userID, offerID, 0, 10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if grant.AmountSat != 500_000 {
		t.Errorf("awarded %d, want the 500000 cap", grant.AmountSat)
	}

	// And it cannot be taken twice.
	if _, err := r.bonus.GrantFromOffer(ctx, r.userID, offerID, 0, 1_000_000); !errors.Is(err, ErrAlreadyTaken) {
		t.Errorf("got %v, want ErrAlreadyTaken", err)
	}
	r.assertLedgerSound()
}

func TestCancelReturnsTheBonus(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	grant, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 750_000, WageringX100: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.bonus.Cancel(ctx, grant.ID, r.staffID, "granted in error"); err != nil {
		t.Fatal(err)
	}
	if r.bonusBalance() != 0 {
		t.Errorf("bonus balance = %d after cancelling, want 0", r.bonusBalance())
	}
	if err := r.bonus.Cancel(ctx, grant.ID, r.staffID, "again"); err == nil {
		t.Error("cancelling twice should be refused")
	}
	r.assertLedgerSound()
}

func TestSummaryShowsWhatBlocksAWithdrawal(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()

	if _, err := r.bonus.Grant(ctx, GrantRequest{
		UserID: r.userID, AmountSat: 200_000, WageringX100: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := r.bonus.Summary(ctx, r.userID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.BonusBalanceSat != 200_000 {
		t.Errorf("bonus balance = %d", summary.BonusBalanceSat)
	}
	if summary.TotalRemainingWageringSat != 2_000_000 {
		t.Errorf("remaining wagering = %d, want 2000000", summary.TotalRemainingWageringSat)
	}
	if len(summary.Active) != 1 {
		t.Errorf("active grants = %d, want 1", len(summary.Active))
	}
	if summary.Active[0].ProgressBps() != 0 {
		t.Errorf("progress = %d bps, want 0", summary.Active[0].ProgressBps())
	}
}
