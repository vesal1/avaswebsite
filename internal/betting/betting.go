// Package betting validates and strikes wagers, and settles them once results
// are posted.
package betting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Errors callers branch on.
var (
	ErrEmptySlip         = errors.New("betting: the bet slip is empty")
	ErrSelectionClosed   = errors.New("betting: that selection is no longer available")
	ErrOddsChanged       = errors.New("betting: the price has changed")
	ErrStakeOutOfRange   = errors.New("betting: stake is outside the accepted range")
	ErrPayoutTooLarge    = errors.New("betting: the potential payout exceeds the maximum")
	ErrCorrelatedLegs    = errors.New("betting: a parlay cannot combine selections from the same event")
	ErrTooManyLegs       = errors.New("betting: too many selections for one bet")
	ErrInsufficientFunds = errors.New("betting: insufficient balance")
	ErrLiabilityExceeded = errors.New("betting: this bet exceeds the book's limit on that selection")
	ErrManualGrading     = errors.New("betting: this market must be graded by a trader")
	ErrIncompleteResult  = errors.New("betting: the posted result does not settle this market")
)

// SlipLeg is one selection on a bet slip.
type SlipLeg struct {
	SelectionID int64
	// ExpectedOddsMilli is the price the customer was shown. A bet is refused
	// when the live price has moved away from it, unless they said they would
	// accept a change.
	ExpectedOddsMilli int64
}

// Slip is a bet as submitted.
type Slip struct {
	Legs             []SlipLeg
	StakeSat         int64
	AcceptOddsChange bool
	IP               string
}

// Placed describes a struck bet.
type Placed struct {
	BetID              int64
	Kind               string
	StakeSat           int64
	OddsMilli          int64
	PotentialPayoutSat int64
	// PriceMoved is set when the bet was struck at a different price from the
	// one shown, which the customer had agreed to accept.
	PriceMoved bool
	// FromBonusSat is how much of the stake came out of promotional funds
	// rather than the customer's own money.
	FromBonusSat int64
}

// WageringRecorder is told when a stake has resolved, so a promotional
// wagering requirement can advance. It is an interface rather than a direct
// dependency on the bonus package: the betting engine should not have to know
// what a promotion is, only that something may want to hear about a stake.
type WageringRecorder interface {
	RecordStake(ctx context.Context, userID, refID, stakeSat, oddsMilli int64, source string) error
}

// Service places and settles bets.
type Service struct {
	store      *store.Store
	compliance *compliance.Service
	cfg        *config.Config
	wagering   WageringRecorder
	now        func() time.Time
}

// New builds a betting service.
func New(s *store.Store, comp *compliance.Service, cfg *config.Config) *Service {
	return &Service{store: s, compliance: comp, cfg: cfg, now: func() time.Time { return s.Now() }}
}

// WithWagering attaches a wagering recorder.
func (s *Service) WithWagering(recorder WageringRecorder) *Service {
	s.wagering = recorder
	return s
}

// Quote is a priced, validated slip that has not been struck.
type Quote struct {
	Kind               string
	StakeSat           int64
	OddsMilli          int64
	PotentialPayoutSat int64
	Legs               []QuoteLeg
	PriceMoved         bool
}

// QuoteLeg describes one leg of a quote for display.
type QuoteLeg struct {
	SelectionID   int64
	SelectionName string
	MarketTitle   string
	EventName     string
	SportKey      string
	OddsMilli     int64
	Moved         bool
	StartsAt      time.Time
}

// Quote prices a slip without striking it, so the bet slip can show a payout
// and warn about a price move before the customer commits.
func (s *Service) Quote(ctx context.Context, slip Slip) (Quote, error) {
	if len(slip.Legs) == 0 {
		return Quote{}, ErrEmptySlip
	}
	if len(slip.Legs) > s.cfg.MaxParlayLegs {
		return Quote{}, fmt.Errorf("%w: %d legs, the maximum is %d",
			ErrTooManyLegs, len(slip.Legs), s.cfg.MaxParlayLegs)
	}

	quote := Quote{Kind: store.BetSingle, StakeSat: slip.StakeSat}
	if len(slip.Legs) > 1 {
		quote.Kind = store.BetParlay
	}

	seenEvents := make(map[int64]bool, len(slip.Legs))
	odds := make([]int64, 0, len(slip.Legs))
	for _, leg := range slip.Legs {
		detail, err := s.store.GetSelectionDetail(ctx, leg.SelectionID)
		if errors.Is(err, store.ErrNotFound) {
			return Quote{}, fmt.Errorf("%w: selection %d", ErrSelectionClosed, leg.SelectionID)
		}
		if err != nil {
			return Quote{}, err
		}
		if err := s.checkTradable(detail); err != nil {
			return Quote{}, err
		}
		// Legs from the same event are correlated: their outcomes are not
		// independent, so the combined price would not reflect the real risk.
		if quote.Kind == store.BetParlay {
			if seenEvents[detail.EventID] {
				return Quote{}, fmt.Errorf("%w: %s", ErrCorrelatedLegs, detail.EventName)
			}
			seenEvents[detail.EventID] = true
		}

		moved := leg.ExpectedOddsMilli != 0 && leg.ExpectedOddsMilli != detail.OddsMilli
		quote.PriceMoved = quote.PriceMoved || moved
		odds = append(odds, detail.OddsMilli)
		quote.Legs = append(quote.Legs, QuoteLeg{
			SelectionID: detail.ID, SelectionName: detail.Name, MarketTitle: detail.MarketTitle,
			EventName: detail.EventName, SportKey: detail.SportKey, OddsMilli: detail.OddsMilli,
			Moved: moved, StartsAt: detail.StartsAt,
		})
	}

	combined, err := money.CombineOdds(odds)
	if err != nil {
		return Quote{}, err
	}
	quote.OddsMilli = combined
	if slip.StakeSat > 0 {
		payout, err := money.Payout(slip.StakeSat, combined)
		if err != nil {
			return Quote{}, err
		}
		quote.PotentialPayoutSat = payout
	}
	return quote, nil
}

func (s *Service) checkTradable(detail store.SelectionDetail) error {
	if detail.Status != store.OutcomeOpen {
		return fmt.Errorf("%w: %s is %s", ErrSelectionClosed, detail.Name, detail.Status)
	}
	if detail.MarketStatus != store.MarketOpen {
		return fmt.Errorf("%w: %s is %s", ErrSelectionClosed, detail.MarketTitle, detail.MarketStatus)
	}
	switch detail.EventStatus {
	case store.EventScheduled:
		if !s.now().Before(detail.StartsAt) {
			return fmt.Errorf("%w: %s has started", ErrSelectionClosed, detail.EventName)
		}
	case store.EventLive:
		// In-play betting stays open; the trader suspends markets as needed.
	default:
		return fmt.Errorf("%w: %s is %s", ErrSelectionClosed, detail.EventName, detail.EventStatus)
	}
	return nil
}

// Place validates and strikes a bet.
//
// Everything that decides whether the bet may be struck is re-read inside the
// database transaction that debits the stake: the odds, the balance and the
// book's exposure. Checking any of them outside would leave a window in which
// two requests each pass a check against the same funds or the same limit.
func (s *Service) Place(ctx context.Context, user store.User, slip Slip) (Placed, error) {
	if len(slip.Legs) == 0 {
		return Placed{}, ErrEmptySlip
	}
	if len(slip.Legs) > s.cfg.MaxParlayLegs {
		return Placed{}, fmt.Errorf("%w: %d legs, the maximum is %d",
			ErrTooManyLegs, len(slip.Legs), s.cfg.MaxParlayLegs)
	}
	if slip.StakeSat < s.cfg.MinStakeSat {
		return Placed{}, fmt.Errorf("%w: the minimum stake is %s BTC",
			ErrStakeOutOfRange, money.FormatBTC(s.cfg.MinStakeSat))
	}
	if slip.StakeSat > s.cfg.MaxStakeSat {
		return Placed{}, fmt.Errorf("%w: the maximum stake is %s BTC",
			ErrStakeOutOfRange, money.FormatBTC(s.cfg.MaxStakeSat))
	}

	if err := s.compliance.CheckCanBet(ctx, user, slip.StakeSat); err != nil {
		return Placed{}, err
	}

	kind := store.BetSingle
	if len(slip.Legs) > 1 {
		kind = store.BetParlay
	}

	var placed Placed
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		seenEvents := make(map[int64]bool, len(slip.Legs))
		seenSelections := make(map[int64]bool, len(slip.Legs))
		odds := make([]int64, 0, len(slip.Legs))
		legs := make([]store.BetLeg, 0, len(slip.Legs))
		priceMoved := false

		for _, leg := range slip.Legs {
			detail, err := store.GetSelectionDetailTx(ctx, tx, leg.SelectionID)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: selection %d", ErrSelectionClosed, leg.SelectionID)
			}
			if err != nil {
				return err
			}
			if err := s.checkTradable(detail); err != nil {
				return err
			}
			if seenSelections[detail.ID] {
				return fmt.Errorf("%w: %s appears twice", ErrCorrelatedLegs, detail.Name)
			}
			seenSelections[detail.ID] = true
			if kind == store.BetParlay {
				if seenEvents[detail.EventID] {
					return fmt.Errorf("%w: %s", ErrCorrelatedLegs, detail.EventName)
				}
				seenEvents[detail.EventID] = true
			}

			if leg.ExpectedOddsMilli != 0 && leg.ExpectedOddsMilli != detail.OddsMilli {
				if !slip.AcceptOddsChange {
					return fmt.Errorf("%w: %s is now %s, was %s", ErrOddsChanged, detail.Name,
						money.FormatDecimalOdds(detail.OddsMilli),
						money.FormatDecimalOdds(leg.ExpectedOddsMilli))
				}
				priceMoved = true
			}

			odds = append(odds, detail.OddsMilli)
			legs = append(legs, store.BetLeg{
				SelectionID: detail.ID, OddsMilli: detail.OddsMilli,
				EventName: detail.EventName, MarketTitle: detail.MarketTitle,
				SelectionName: detail.Name, SportKey: detail.SportKey, StartsAt: detail.StartsAt,
			})
		}

		combined, err := money.CombineOdds(odds)
		if err != nil {
			return err
		}
		payout, err := money.Payout(slip.StakeSat, combined)
		if err != nil {
			return err
		}
		if payout > s.cfg.MaxPayoutSat {
			return fmt.Errorf("%w: this bet would pay %s BTC, the maximum is %s BTC",
				ErrPayoutTooLarge, money.FormatBTC(payout), money.FormatBTC(s.cfg.MaxPayoutSat))
		}

		// The book's exposure to each selection, counted the same way the
		// admin screen reports it.
		if s.cfg.MaxLiabilityPerSel > 0 {
			for _, leg := range legs {
				existing, err := store.LiabilityForSelectionTx(ctx, tx, leg.SelectionID)
				if err != nil {
					return err
				}
				if existing+payout > s.cfg.MaxLiabilityPerSel {
					return fmt.Errorf("%w: %s", ErrLiabilityExceeded, leg.SelectionName)
				}
			}
		}

		// Cash and bonus are both stakeable, and the stake draws on cash
		// first. Only cash is withdrawable, so the two are counted separately
		// rather than added into one "balance" that would let a bonus be
		// treated as money the customer owns.
		cash, bonusAvailable, err := store.PlayableBalanceTx(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		if cash+bonusAvailable < slip.StakeSat {
			return fmt.Errorf("%w: you have %s BTC to stake, and the stake is %s BTC",
				ErrInsufficientFunds, money.FormatBTC(cash+bonusAvailable), money.FormatBTC(slip.StakeSat))
		}
		debits, fromBonus, err := store.SpendEntries(user.ID, slip.StakeSat, cash, bonusAvailable)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInsufficientFunds, err)
		}

		at := store.Timestamp(s.now())
		betID, err := store.InsertBetTx(ctx, tx, at, store.Bet{
			UserID: user.ID, Kind: kind, StakeSat: slip.StakeSat, OddsMilli: combined,
			PotentialPayoutSat: payout, IP: slip.IP, AcceptedOddsChange: slip.AcceptOddsChange,
			Legs: legs,
		})
		if err != nil {
			return err
		}
		entries := append(debits, store.Entry{
			Account: store.AccountBetEscrow, AmountSat: slip.StakeSat,
		})
		if _, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:    store.TxnBetStake,
			Memo:    fmt.Sprintf("Stake on bet %d", betID),
			RefType: "bet",
			RefID:   betID,
			Entries: entries,
		}); err != nil {
			return err
		}

		placed = Placed{
			BetID: betID, Kind: kind, StakeSat: slip.StakeSat, OddsMilli: combined,
			PotentialPayoutSat: payout, PriceMoved: priceMoved, FromBonusSat: fromBonus,
		}
		return nil
	})
	if err != nil {
		return Placed{}, err
	}

	return placed, s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: user.ID, SubjectUserID: user.ID, Action: "bet_placed", IP: slip.IP,
		Detail: fmt.Sprintf("bet %d, %s at %s for %s BTC", placed.BetID, placed.Kind,
			money.FormatDecimalOdds(placed.OddsMilli), money.FormatBTC(placed.StakeSat)),
	})
}
