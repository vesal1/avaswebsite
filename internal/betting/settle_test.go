package betting

import (
	"testing"

	"github.com/vesal1/avaswebsite/internal/store"
)

func bet(stake int64, legs ...store.BetLeg) store.Bet {
	return store.Bet{ID: 1, StakeSat: stake, Legs: legs}
}

func leg(odds int64, status string) store.BetLeg {
	return store.BetLeg{ID: 1, OddsMilli: odds, Status: status}
}

func TestSingleBetOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		bet        store.Bet
		wantStatus string
		wantPayout int64
	}{
		{"won", bet(100_000, leg(2500, store.OutcomeWon)), store.OutcomeWon, 250_000},
		{"lost", bet(100_000, leg(2500, store.OutcomeLost)), store.OutcomeLost, 0},
		{"void returns the stake", bet(100_000, leg(2500, store.OutcomeVoid)), store.OutcomeVoid, 100_000},
		// Asian handicap: half the stake wins at 2.50, half comes back.
		// 50,000 * 2.5 + 50,000 = 175,000.
		{"half won", bet(100_000, leg(2500, store.OutcomeHalfWon)), store.OutcomeWon, 175_000},
		// Half lost: half the stake is gone, half returned.
		{"half lost", bet(100_000, leg(2500, store.OutcomeHalfLost)), store.OutcomeHalfLost, 50_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, payout, err := SettleBet(tc.bet)
			if err != nil {
				t.Fatal(err)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if payout != tc.wantPayout {
				t.Errorf("payout = %d, want %d", payout, tc.wantPayout)
			}
		})
	}
}

func TestOpenLegLeavesBetUnsettled(t *testing.T) {
	status, payout, err := SettleBet(bet(100_000,
		leg(2000, store.OutcomeWon),
		leg(1500, store.OutcomeOpen),
	))
	if err != nil {
		t.Fatal(err)
	}
	if status != "" || payout != 0 {
		t.Errorf("a bet with an open leg was settled as %q for %d", status, payout)
	}
}

func TestOneLostLegLosesTheParlay(t *testing.T) {
	// Even with a winner and a void alongside it, one loser ends the bet.
	status, payout, err := SettleBet(bet(100_000,
		leg(2000, store.OutcomeWon),
		leg(3000, store.OutcomeLost),
		leg(1500, store.OutcomeVoid),
	))
	if err != nil {
		t.Fatal(err)
	}
	if status != store.OutcomeLost {
		t.Errorf("status = %q, want lost", status)
	}
	if payout != 0 {
		t.Errorf("payout = %d, want 0", payout)
	}
}

func TestVoidLegDropsOutOfTheParlay(t *testing.T) {
	// A postponed match should not sink the rest of the accumulator: its leg
	// settles at evens and the remaining legs stand.
	status, payout, err := SettleBet(bet(100_000,
		leg(2000, store.OutcomeWon),
		leg(1500, store.OutcomeVoid),
		leg(3000, store.OutcomeWon),
	))
	if err != nil {
		t.Fatal(err)
	}
	if status != store.OutcomeWon {
		t.Errorf("status = %q, want won", status)
	}
	// 2.00 * 1.00 * 3.00 = 6.00
	if payout != 600_000 {
		t.Errorf("payout = %d, want 600000", payout)
	}
}

func TestParlayOfAllVoidLegsReturnsTheStake(t *testing.T) {
	status, payout, err := SettleBet(bet(100_000,
		leg(2000, store.OutcomeVoid),
		leg(3000, store.OutcomeVoid),
	))
	if err != nil {
		t.Fatal(err)
	}
	if status != store.OutcomeVoid {
		t.Errorf("status = %q, want void", status)
	}
	if payout != 100_000 {
		t.Errorf("payout = %d, want the stake back", payout)
	}
}

func TestHalfLostLegInAParlay(t *testing.T) {
	// 2.00 winner combined with a half-lost leg: 100,000 * 2.0 * 0.5 = 100,000,
	// which returns exactly the stake.
	status, payout, err := SettleBet(bet(100_000,
		leg(2000, store.OutcomeWon),
		leg(1900, store.OutcomeHalfLost),
	))
	if err != nil {
		t.Fatal(err)
	}
	if payout != 100_000 {
		t.Errorf("payout = %d, want 100000", payout)
	}
	if status != store.OutcomeVoid {
		t.Errorf("status = %q; a payout equal to the stake is a push", status)
	}
}

func TestPartialLossIsReportedAsHalfLost(t *testing.T) {
	// A single half-lost leg at short odds returns less than the stake.
	status, payout, err := SettleBet(bet(100_000, leg(1500, store.OutcomeHalfLost)))
	if err != nil {
		t.Fatal(err)
	}
	if status != store.OutcomeHalfLost {
		t.Errorf("status = %q, want half_lost", status)
	}
	if payout != 50_000 {
		t.Errorf("payout = %d, want 50000", payout)
	}
}

func TestSettleBetRejectsUnknownLegStatus(t *testing.T) {
	if _, _, err := SettleBet(bet(100_000, leg(2000, "confiscated"))); err == nil {
		t.Error("an unknown leg status should be an error, not a silent payout")
	}
	if _, _, err := SettleBet(bet(100_000)); err == nil {
		t.Error("a bet with no legs should be an error")
	}
}

func TestPayoutNeverExceedsWhatTheOddsPromise(t *testing.T) {
	// Property check across a spread of stakes and prices: the parlay payout
	// must never exceed the product of the leg prices, and rounding must
	// always fall in the book's favour.
	for _, stake := range []int64{1, 7, 999, 100_000, 12_345_678} {
		for _, first := range []int64{1010, 1333, 2500, 7777} {
			for _, second := range []int64{1001, 1500, 3333} {
				_, payout, err := SettleBet(bet(stake,
					leg(first, store.OutcomeWon), leg(second, store.OutcomeWon)))
				if err != nil {
					t.Fatal(err)
				}
				exact := stake * first / 1000 * second / 1000
				if payout > exact {
					t.Errorf("stake %d at %d/%d paid %d, more than %d", stake, first, second, payout, exact)
				}
				if payout < 0 {
					t.Errorf("stake %d at %d/%d paid a negative %d", stake, first, second, payout)
				}
			}
		}
	}
}
