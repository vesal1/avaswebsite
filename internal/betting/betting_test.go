package betting

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/store"
)

type rig struct {
	t       *testing.T
	store   *store.Store
	betting *Service
	cfg     *config.Config
	user    store.User
	now     time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "betting.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	r := &rig{t: t, store: s, now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	s.WithClock(func() time.Time { return r.now })

	r.cfg = config.Load()
	r.cfg.AllowedCountries = []string{"GB"}
	r.cfg.MinStakeSat = 1_000
	r.cfg.MaxStakeSat = 50_000_000
	r.cfg.MaxPayoutSat = 500_000_000
	r.cfg.MaxParlayLegs = 15
	r.cfg.MaxLiabilityPerSel = 2_000_000_000

	comp := compliance.New(s, r.cfg).WithClock(func() time.Time { return r.now })
	r.betting = New(s, comp, r.cfg)

	ctx := context.Background()
	userID, err := s.CreateUser(ctx, store.NewUser{
		Email: "punter@example.com", PasswordHash: "x", DisplayName: "Punter",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKYCStatus(ctx, userID, store.KYCVerified); err != nil {
		t.Fatal(err)
	}
	r.user, err = s.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// fund credits a balance directly through the ledger, standing in for a
// settled deposit.
func (r *rig) fund(amountSat int64) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(r.now), store.TxnSpec{
			Kind: store.TxnDeposit,
			Entries: []store.Entry{
				{Account: store.AccountUserCash, UserID: r.user.ID, AmountSat: amountSat},
				{Account: store.AccountExternalBitcoin, AmountSat: -amountSat},
			},
		})
		return err
	}); err != nil {
		r.t.Fatalf("fund: %v", err)
	}
}

// match creates a football match with a 1X2 market and returns the event id
// and the three selection ids, home/draw/away.
func (r *rig) match(name string, homeOdds, drawOdds, awayOdds int64) (int64, [3]int64, [2]int64) {
	r.t.Helper()
	ctx := context.Background()

	eventID, err := r.store.CreateEvent(ctx, store.NewEvent{
		SportKey: "football", Name: name, Format: string(catalog.FormatMatch),
		StartsAt: r.now.Add(2 * time.Hour),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	homeID, err := r.store.AddParticipant(ctx, store.Participant{
		EventID: eventID, Name: name + " Home", HomeAway: "home", SortOrder: 0,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	awayID, err := r.store.AddParticipant(ctx, store.Participant{
		EventID: eventID, Name: name + " Away", HomeAway: "away", SortOrder: 1,
	})
	if err != nil {
		r.t.Fatal(err)
	}

	marketID, err := r.store.CreateMarket(ctx, store.NewMarket{
		EventID: eventID, Kind: string(catalog.KindMoneyline3), Title: "Match Result",
	})
	if err != nil {
		r.t.Fatal(err)
	}
	var selections [3]int64
	for i, spec := range []struct {
		code string
		odds int64
		pid  int64
	}{{"home", homeOdds, homeID}, {"draw", drawOdds, 0}, {"away", awayOdds, awayID}} {
		id, err := r.store.AddSelection(ctx, store.Selection{
			MarketID: marketID, ParticipantID: spec.pid, Name: spec.code,
			OutcomeCode: spec.code, OddsMilli: spec.odds, SortOrder: i,
		})
		if err != nil {
			r.t.Fatal(err)
		}
		selections[i] = id
	}
	return eventID, selections, [2]int64{homeID, awayID}
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

func (r *rig) balance() int64 {
	r.t.Helper()
	balance, err := r.store.BalanceSat(context.Background(), r.user.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return balance
}

func TestPlaceSingleDebitsStake(t *testing.T) {
	r := newRig(t)
	r.fund(1_000_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	placed, err := r.betting.Place(context.Background(), r.user, Slip{
		Legs:     []SlipLeg{{SelectionID: selections[0], ExpectedOddsMilli: 2000}},
		StakeSat: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if placed.Kind != store.BetSingle {
		t.Errorf("kind = %s, want single", placed.Kind)
	}
	if placed.PotentialPayoutSat != 200_000 {
		t.Errorf("potential payout = %d, want 200000", placed.PotentialPayoutSat)
	}
	if r.balance() != 900_000 {
		t.Errorf("balance = %d, want 900000", r.balance())
	}
	r.assertLedgerSound()
}

func TestWinningBetPaysOutAndBalancesTheLedger(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	eventID, selections, participants := r.match("Test", 2500, 3400, 4000)

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs:     []SlipLeg{{SelectionID: selections[0], ExpectedOddsMilli: 2500}},
		StakeSat: 100_000,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := r.betting.SettleEvent(ctx, eventID, Result{
		Participants: []ParticipantResult{
			{ParticipantID: participants[0], Score: 2, HasScore: true},
			{ParticipantID: participants[1], Score: 0, HasScore: true},
		},
	}, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.BetsSettled != 1 {
		t.Fatalf("settled %d bets, want 1 (problems: %v)", report.BetsSettled, report.Problems)
	}
	// 900,000 left after the stake, plus a 250,000 payout.
	if r.balance() != 1_150_000 {
		t.Errorf("balance = %d, want 1150000", r.balance())
	}
	// The house is down the customer's profit.
	revenue, err := r.store.AccountBalanceSat(ctx, store.AccountHouseRevenue)
	if err != nil {
		t.Fatal(err)
	}
	if revenue != -150_000 {
		t.Errorf("house revenue = %d, want -150000", revenue)
	}
	// Nothing should be left in escrow once every bet on the event is settled.
	escrow, err := r.store.AccountBalanceSat(ctx, store.AccountBetEscrow)
	if err != nil {
		t.Fatal(err)
	}
	if escrow != 0 {
		t.Errorf("bet escrow = %d, want 0", escrow)
	}
	r.assertLedgerSound()
}

func TestLosingBetKeepsTheStake(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	eventID, selections, participants := r.match("Test", 2500, 3400, 4000)

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs:     []SlipLeg{{SelectionID: selections[0]}},
		StakeSat: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.betting.SettleEvent(ctx, eventID, Result{
		Participants: []ParticipantResult{
			{ParticipantID: participants[0], Score: 0, HasScore: true},
			{ParticipantID: participants[1], Score: 3, HasScore: true},
		},
	}, r.user.ID); err != nil {
		t.Fatal(err)
	}

	if r.balance() != 900_000 {
		t.Errorf("balance = %d, want 900000", r.balance())
	}
	revenue, _ := r.store.AccountBalanceSat(ctx, store.AccountHouseRevenue)
	if revenue != 100_000 {
		t.Errorf("house revenue = %d, want 100000", revenue)
	}
	r.assertLedgerSound()
}

func TestParlayPaysOnlyWhenEveryLegLands(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	firstEvent, firstSel, firstPart := r.match("First", 2000, 3400, 4000)
	secondEvent, secondSel, secondPart := r.match("Second", 1500, 3400, 6000)

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{
			{SelectionID: firstSel[0]},
			{SelectionID: secondSel[0]},
		},
		StakeSat: 100_000,
	}); err != nil {
		t.Fatal(err)
	}

	// The first leg lands; the bet must stay open until the second is decided.
	if _, err := r.betting.SettleEvent(ctx, firstEvent, Result{
		Participants: []ParticipantResult{
			{ParticipantID: firstPart[0], Score: 1, HasScore: true},
			{ParticipantID: firstPart[1], Score: 0, HasScore: true},
		},
	}, r.user.ID); err != nil {
		t.Fatal(err)
	}
	if r.balance() != 900_000 {
		t.Errorf("balance = %d after one leg, want the bet still open at 900000", r.balance())
	}

	if _, err := r.betting.SettleEvent(ctx, secondEvent, Result{
		Participants: []ParticipantResult{
			{ParticipantID: secondPart[0], Score: 2, HasScore: true},
			{ParticipantID: secondPart[1], Score: 1, HasScore: true},
		},
	}, r.user.ID); err != nil {
		t.Fatal(err)
	}
	// 2.00 * 1.50 = 3.00 on 100,000 is a 300,000 payout.
	if r.balance() != 1_200_000 {
		t.Errorf("balance = %d, want 1200000", r.balance())
	}
	r.assertLedgerSound()
}

func TestParlayLegsFromTheSameEventAreRefused(t *testing.T) {
	r := newRig(t)
	r.fund(1_000_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	_, err := r.betting.Place(context.Background(), r.user, Slip{
		Legs: []SlipLeg{
			{SelectionID: selections[0]},
			{SelectionID: selections[1]}, // the draw in the same match
		},
		StakeSat: 100_000,
	})
	if !errors.Is(err, ErrCorrelatedLegs) {
		t.Errorf("got %v, want ErrCorrelatedLegs", err)
	}
	if r.balance() != 1_000_000 {
		t.Errorf("a refused bet moved money: balance = %d", r.balance())
	}
	r.assertLedgerSound()
}

func TestPriceMoveIsRefusedUnlessAccepted(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	// The trader shortens the price after the slip was rendered.
	if err := r.store.UpdateOdds(ctx, selections[0], 1600, "steam"); err != nil {
		t.Fatal(err)
	}

	_, err := r.betting.Place(ctx, r.user, Slip{
		Legs:     []SlipLeg{{SelectionID: selections[0], ExpectedOddsMilli: 2000}},
		StakeSat: 100_000,
	})
	if !errors.Is(err, ErrOddsChanged) {
		t.Fatalf("got %v, want ErrOddsChanged", err)
	}

	placed, err := r.betting.Place(ctx, r.user, Slip{
		Legs:             []SlipLeg{{SelectionID: selections[0], ExpectedOddsMilli: 2000}},
		StakeSat:         100_000,
		AcceptOddsChange: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The bet is struck at the live price, never the stale one.
	if placed.OddsMilli != 1600 {
		t.Errorf("struck at %d, want the live price 1600", placed.OddsMilli)
	}
	if !placed.PriceMoved {
		t.Error("the bet should be marked as struck at a moved price")
	}
	r.assertLedgerSound()
}

func TestCannotBetMoreThanTheBalance(t *testing.T) {
	r := newRig(t)
	r.fund(50_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	_, err := r.betting.Place(context.Background(), r.user, Slip{
		Legs:     []SlipLeg{{SelectionID: selections[0]}},
		StakeSat: 100_000,
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("got %v, want ErrInsufficientFunds", err)
	}
	if r.balance() != 50_000 {
		t.Errorf("balance = %d, want 50000", r.balance())
	}
	r.assertLedgerSound()
}

func TestStakeAndPayoutLimits(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(500_000_000)
	_, selections, _ := r.match("Test", 9000, 3400, 4000)

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 10,
	}); !errors.Is(err, ErrStakeOutOfRange) {
		t.Errorf("tiny stake: got %v, want ErrStakeOutOfRange", err)
	}
	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 400_000_000,
	}); !errors.Is(err, ErrStakeOutOfRange) {
		t.Errorf("huge stake: got %v, want ErrStakeOutOfRange", err)
	}

	// Inside the stake limit but over the payout ceiling: 40,000,000 at 9.00
	// returns 360,000,000, which is more than the book will write.
	r.cfg.MaxPayoutSat = 300_000_000
	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 40_000_000,
	}); !errors.Is(err, ErrPayoutTooLarge) {
		t.Errorf("got %v, want ErrPayoutTooLarge", err)
	}
	r.assertLedgerSound()
}

func TestBetOnAStartedEventIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	r.now = r.now.Add(3 * time.Hour) // kick-off has passed

	_, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 100_000,
	})
	if !errors.Is(err, ErrSelectionClosed) {
		t.Errorf("got %v, want ErrSelectionClosed", err)
	}
	r.assertLedgerSound()
}

func TestBetOnASuspendedMarketIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	detail, err := r.store.GetSelectionDetail(ctx, selections[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetMarketStatus(ctx, detail.MarketID, store.MarketSuspended); err != nil {
		t.Fatal(err)
	}

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 100_000,
	}); !errors.Is(err, ErrSelectionClosed) {
		t.Errorf("got %v, want ErrSelectionClosed", err)
	}
	r.assertLedgerSound()
}

func TestUnverifiedCustomerCannotBet(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.store.SetKYCStatus(ctx, r.user.ID, store.KYCNone); err != nil {
		t.Fatal(err)
	}
	user, err := r.store.GetUser(ctx, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.fund(1_000_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	_, err = r.betting.Place(ctx, user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 100_000,
	})
	refusal, ok := compliance.AsRefusal(err)
	if !ok || refusal.Code != compliance.CodeKYCRequired {
		t.Errorf("got %v, want a KYC refusal", err)
	}
	r.assertLedgerSound()
}

func TestSelfExcludedCustomerCannotBet(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	_, selections, _ := r.match("Test", 2000, 3400, 4000)

	comp := compliance.New(r.store, r.cfg).WithClock(func() time.Time { return r.now })
	if err := comp.SelfExclude(ctx, r.user.ID, 0, "done"); err != nil {
		t.Fatal(err)
	}

	_, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 100_000,
	})
	refusal, ok := compliance.AsRefusal(err)
	if !ok || refusal.Code != compliance.CodeSelfExcluded {
		t.Errorf("got %v, want a self-exclusion refusal", err)
	}
	r.assertLedgerSound()
}

func TestAbandonedEventReturnsEveryStake(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	eventID, selections, participants := r.match("Test", 2500, 3400, 4000)

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.betting.SettleEvent(ctx, eventID, Result{
		Abandoned: true,
		Participants: []ParticipantResult{
			{ParticipantID: participants[0], Score: 1, HasScore: true},
			{ParticipantID: participants[1], Score: 0, HasScore: true},
		},
	}, r.user.ID); err != nil {
		t.Fatal(err)
	}
	if r.balance() != 1_000_000 {
		t.Errorf("balance = %d, want the full stake returned", r.balance())
	}
	r.assertLedgerSound()
}

func TestSettlingTwiceDoesNotPayTwice(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	eventID, selections, participants := r.match("Test", 2500, 3400, 4000)

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: selections[0]}}, StakeSat: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	result := Result{Participants: []ParticipantResult{
		{ParticipantID: participants[0], Score: 2, HasScore: true},
		{ParticipantID: participants[1], Score: 0, HasScore: true},
	}}
	for i := 0; i < 3; i++ {
		if _, err := r.betting.SettleEvent(ctx, eventID, result, r.user.ID); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	if r.balance() != 1_150_000 {
		t.Errorf("balance = %d after three settlement runs, want 1150000", r.balance())
	}
	r.assertLedgerSound()
}

func TestManualMarketsAreReportedNotGuessed(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	eventID, _, participants := r.match("Test", 2500, 3400, 4000)

	// Add a market no score line can settle.
	marketID, err := r.store.CreateMarket(ctx, store.NewMarket{
		EventID: eventID, Kind: string(catalog.KindFirstScorer), Title: "First Goalscorer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AddSelection(ctx, store.Selection{
		MarketID: marketID, Name: "A Striker", OddsMilli: 5000,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := r.betting.SettleEvent(ctx, eventID, Result{
		Participants: []ParticipantResult{
			{ParticipantID: participants[0], Score: 2, HasScore: true},
			{ParticipantID: participants[1], Score: 0, HasScore: true},
		},
	}, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MarketsManual) != 1 || report.MarketsManual[0] != marketID {
		t.Errorf("manual markets = %v, want [%d]", report.MarketsManual, marketID)
	}
	// The event stays 'finished' rather than 'settled' while a market is
	// outstanding, so it cannot be quietly forgotten.
	event, err := r.store.GetEvent(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if event.Status != store.EventFinished {
		t.Errorf("event status = %s, want finished while a market is ungraded", event.Status)
	}
	r.assertLedgerSound()
}

func TestManualSettlementPaysTheNamedWinner(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	eventID, _, _ := r.match("Test", 2500, 3400, 4000)

	marketID, err := r.store.CreateMarket(ctx, store.NewMarket{
		EventID: eventID, Kind: string(catalog.KindFirstScorer), Title: "First Goalscorer",
	})
	if err != nil {
		t.Fatal(err)
	}
	winner, err := r.store.AddSelection(ctx, store.Selection{
		MarketID: marketID, Name: "A Striker", OutcomeCode: "striker", OddsMilli: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	loser, err := r.store.AddSelection(ctx, store.Selection{
		MarketID: marketID, Name: "A Defender", OutcomeCode: "defender", OddsMilli: 12000,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.betting.Place(ctx, r.user, Slip{
		Legs: []SlipLeg{{SelectionID: winner}}, StakeSat: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.betting.SettleMarketManually(ctx, marketID,
		[]int64{winner}, nil, r.user.ID); err != nil {
		t.Fatal(err)
	}
	if r.balance() != 1_400_000 {
		t.Errorf("balance = %d, want 1400000 (100000 at 5.00)", r.balance())
	}

	// Naming a selection from another market is a mistake, not a no-op.
	if _, err := r.betting.SettleMarketManually(ctx, marketID, []int64{loser + 9999}, nil, r.user.ID); err == nil {
		t.Error("settling with a selection outside the market should fail")
	}
	r.assertLedgerSound()
}

func TestQuotePricesWithoutStriking(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	_, firstSel, _ := r.match("First", 2000, 3400, 4000)
	_, secondSel, _ := r.match("Second", 1500, 3400, 6000)

	quote, err := r.betting.Quote(ctx, Slip{
		Legs: []SlipLeg{
			{SelectionID: firstSel[0], ExpectedOddsMilli: 2000},
			{SelectionID: secondSel[0], ExpectedOddsMilli: 2000}, // stale
		},
		StakeSat: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if quote.Kind != store.BetParlay {
		t.Errorf("kind = %s, want parlay", quote.Kind)
	}
	if quote.OddsMilli != 3000 {
		t.Errorf("combined odds = %d, want 3000", quote.OddsMilli)
	}
	if quote.PotentialPayoutSat != 300_000 {
		t.Errorf("payout = %d, want 300000", quote.PotentialPayoutSat)
	}
	if !quote.PriceMoved {
		t.Error("the quote should flag the stale leg")
	}
	if r.balance() != 1_000_000 {
		t.Error("quoting must not move money")
	}
}
