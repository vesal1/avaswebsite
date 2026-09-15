// Package bonus awards and tracks promotional money.
//
// The rule the whole package exists to enforce: bonus money is not cash. It
// lives in its own ledger account, it cannot be withdrawn, and it becomes cash
// only by meeting the wagering requirement it was granted with. Anything else
// would let a promotion be cashed out on the day it was awarded.
//
// The customer-facing consequences are deliberately made visible rather than
// buried in terms: a bonus shows its remaining wagering, its expiry, and what
// forfeiting it would cost. Promotions that are hard to understand are how
// disputes start.
package bonus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Errors callers branch on.
var (
	ErrTestCreditsRefused = errors.New("bonus: test credits are not available on this server")
	ErrOfferUnavailable   = errors.New("bonus: that offer is not available")
	ErrAlreadyTaken       = errors.New("bonus: this offer has already been taken")
	ErrNotPermitted       = errors.New("bonus: not permitted")
	ErrNothingToForfeit   = errors.New("bonus: there is no active bonus to forfeit")
)

// Service awards and settles bonuses.
type Service struct {
	store *store.Store
	cfg   *config.Config
	now   func() time.Time
}

// New builds a bonus service.
func New(s *store.Store, cfg *config.Config) *Service {
	return &Service{store: s, cfg: cfg, now: func() time.Time { return s.Now() }}
}

// WithClock replaces the service clock. Test-only.
func (s *Service) WithClock(clock func() time.Time) *Service {
	s.now = clock
	return s
}

// Summary is a customer's promotional position.
type Summary struct {
	BonusBalanceSat int64
	Active          []store.BonusGrant
	// LockedBySat is how much bonus stands between the customer and a
	// withdrawal: they must either finish the wagering or forfeit it.
	LockedBySat int64
	// TotalRemainingWageringSat is what is left to stake across all live
	// bonuses.
	TotalRemainingWageringSat int64
}

// Summary gathers a customer's bonus position.
func (s *Service) Summary(ctx context.Context, userID int64) (Summary, error) {
	balance, err := s.store.BonusBalanceSat(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	active, err := s.store.BonusGrantsForUser(ctx, userID, store.GrantActive)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{BonusBalanceSat: balance, Active: active, LockedBySat: balance}
	for _, grant := range active {
		summary.TotalRemainingWageringSat += grant.RemainingWageringSat()
	}
	return summary, nil
}

// ---------------------------------------------------------------------------
// Granting
// ---------------------------------------------------------------------------

// GrantRequest describes a bonus to award.
type GrantRequest struct {
	UserID int64
	// OfferID awards from a template. Leave zero for an ad-hoc award and fill
	// in the fields below.
	OfferID      int64
	Kind         string
	AmountSat    int64
	WageringX100 int64
	MinOddsMilli int64
	ValidDays    int
	Note         string
	GrantedBy    int64
}

// Grant awards a bonus and credits the customer's bonus balance.
func (s *Service) Grant(ctx context.Context, request GrantRequest) (store.BonusGrant, error) {
	if request.AmountSat <= 0 {
		return store.BonusGrant{}, fmt.Errorf("%w: a bonus must be worth something", ErrNotPermitted)
	}
	kind := request.Kind
	if kind == "" {
		kind = store.BonusComp
	}

	// Test credits are development scaffolding. Letting them exist alongside
	// real customer money would make the ledger a mix of the two, and no
	// amount of tagging makes that safe to reconcile against a bank.
	isTest := kind == store.BonusTestCredit
	if isTest && !s.TestCreditsAllowed() {
		return store.BonusGrant{}, ErrTestCreditsRefused
	}

	validDays := request.ValidDays
	if validDays <= 0 {
		validDays = 30
	}
	wageringRequired := request.AmountSat * request.WageringX100 / 100

	grant := store.BonusGrant{
		UserID:              request.UserID,
		OfferID:             request.OfferID,
		Kind:                kind,
		AmountSat:           request.AmountSat,
		WageringRequiredSat: wageringRequired,
		MinOddsMilli:        request.MinOddsMilli,
		IsTest:              isTest,
		Note:                request.Note,
		GrantedBy:           request.GrantedBy,
		ExpiresAt:           s.now().AddDate(0, 0, validDays),
	}

	var granted store.BonusGrant
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		at := store.Timestamp(s.now())
		txnID, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:      store.TxnBonus,
			Memo:      bonusMemo(kind, request.Note),
			RefType:   "bonus_grant",
			CreatedBy: request.GrantedBy,
			Entries: []store.Entry{
				{Account: store.AccountUserBonus, UserID: request.UserID, AmountSat: request.AmountSat},
				{Account: store.AccountHousePromotions, AmountSat: -request.AmountSat},
			},
		})
		if err != nil {
			return err
		}
		grant.LedgerTxnID = txnID

		grantID, err := store.InsertBonusGrantTx(ctx, tx, grant, at)
		if err != nil {
			return err
		}
		grant.ID = grantID
		grant.GrantedAt = s.now()
		granted = grant

		// A bonus with no wagering requirement is already cash in everything
		// but name, so convert it immediately rather than leaving a
		// permanently unclearable row on the account.
		if wageringRequired == 0 {
			return s.convertTx(ctx, tx, granted, at)
		}
		return nil
	})
	if err != nil {
		return store.BonusGrant{}, err
	}

	if err := s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: request.GrantedBy, SubjectUserID: request.UserID, Action: "bonus_granted",
		Detail: fmt.Sprintf("%s of %s BTC, wagering %s BTC, expires %s",
			kind, money.FormatBTC(request.AmountSat), money.FormatBTC(wageringRequired),
			grant.ExpiresAt.Format("2 Jan 2006")),
	}); err != nil {
		return granted, err
	}
	return granted, nil
}

// GrantFromOffer awards a bonus from a stored template, applying its rules.
func (s *Service) GrantFromOffer(ctx context.Context, userID, offerID, grantedBy, qualifyingDepositSat int64) (store.BonusGrant, error) {
	offer, err := s.store.GetBonusOffer(ctx, offerID)
	if err != nil {
		return store.BonusGrant{}, err
	}
	if !offer.Active {
		return store.BonusGrant{}, ErrOfferUnavailable
	}

	taken, err := s.store.CountGrantsFromOffer(ctx, userID, offer.ID)
	if err != nil {
		return store.BonusGrant{}, err
	}
	if taken >= offer.MaxPerUser {
		return store.BonusGrant{}, ErrAlreadyTaken
	}

	amount := offer.AmountSat
	if offer.MatchBps > 0 {
		if qualifyingDepositSat < offer.MinDepositSat {
			return store.BonusGrant{}, fmt.Errorf("%w: this offer needs a deposit of at least %s BTC",
				ErrOfferUnavailable, money.FormatBTC(offer.MinDepositSat))
		}
		amount += qualifyingDepositSat * offer.MatchBps / 10_000
	}
	if offer.MaxAmountSat > 0 && amount > offer.MaxAmountSat {
		amount = offer.MaxAmountSat
	}
	if amount <= 0 {
		return store.BonusGrant{}, fmt.Errorf("%w: the offer awards nothing on that deposit", ErrOfferUnavailable)
	}

	return s.Grant(ctx, GrantRequest{
		UserID:       userID,
		OfferID:      offer.ID,
		Kind:         offer.Kind,
		AmountSat:    amount,
		WageringX100: offer.WageringX100,
		MinOddsMilli: offer.MinOddsMilli,
		ValidDays:    offer.ValidDays,
		Note:         offer.Name,
		GrantedBy:    grantedBy,
	})
}

// TestCreditsAllowed reports whether this server may mint test money.
func (s *Service) TestCreditsAllowed() bool {
	return !s.cfg.IsProduction() || s.cfg.AllowTestCredits
}

func bonusMemo(kind, note string) string {
	label := strings.ReplaceAll(kind, "_", " ")
	if note == "" {
		return label
	}
	return label + ": " + note
}

// ---------------------------------------------------------------------------
// Wagering
// ---------------------------------------------------------------------------

// ContributionRate returns how much of a stake counts toward wagering, in
// basis points, for a given product.
//
// Sports and poker contribute less than slots because they are lower-margin
// for the house: a bonus that cleared at full rate on near-even-money bets
// would be a standing invitation to arbitrage the promotion rather than play.
// The rates are published on the promotions page, not hidden in terms.
func ContributionRateBps(source string) int64 {
	switch source {
	case "slots", "casino", "mines":
		return 10_000 // 100%
	case "sportsbook":
		return 5_000 // 50%
	case "poker":
		return 2_000 // 20%
	case "blackjack":
		// A 99.9% game clears a wagering requirement nearly for free, which
		// is why every bonus abuser heads straight for the tables. It counts
		// a tenth, and the offer terms say so.
		return 1_000 // 10%
	default:
		return 5_000
	}
}

// RecordStake credits wagering progress for a settled stake and converts any
// bonus that has finished clearing.
//
// refID identifies the stake within its source, and makes the credit
// idempotent: settlement replayed twice cannot advance a wagering requirement
// twice.
func (s *Service) RecordStake(ctx context.Context, userID, refID, stakeSat, oddsMilli int64, source string) error {
	if stakeSat <= 0 {
		return nil
	}
	rate := ContributionRateBps(source)
	contribution := stakeSat * rate / 10_000
	if contribution <= 0 {
		return nil
	}

	return s.store.Tx(ctx, func(tx *store.Tx) error {
		grants, err := store.ActiveBonusGrantsTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		at := store.Timestamp(s.now())

		for _, grant := range grants {
			// A price below the offer's minimum does not clear the bonus. This
			// is the standard guard against clearing a bonus on 1.01 shots,
			// and it is stated on the offer.
			if grant.MinOddsMilli > 0 && oddsMilli > 0 && oddsMilli < grant.MinOddsMilli {
				continue
			}
			if err := store.AddWageringTx(ctx, tx, grant.ID, refID, stakeSat, contribution, source, at); err != nil {
				// A duplicate (grant, source, ref) means this stake has
				// already been counted. That is the guard working, not a
				// failure.
				if isDuplicate(err) {
					continue
				}
				return err
			}

			updated, err := store.GetBonusGrantTx(ctx, tx, grant.ID)
			if err != nil {
				return err
			}
			if updated.WageringDoneSat >= updated.WageringRequiredSat {
				if err := s.convertTx(ctx, tx, updated, at); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// convertTx turns a cleared bonus into withdrawable cash.
//
// The amount converted is capped by the bonus balance actually left, because
// bonus money is one fungible pot that staking draws down. Converting the
// amount originally awarded would mint cash the customer no longer holds and
// push the bonus account negative.
func (s *Service) convertTx(ctx context.Context, tx *store.Tx, grant store.BonusGrant, at string) error {
	balance, err := store.BonusBalanceSatTx(ctx, tx, grant.UserID)
	if err != nil {
		return err
	}
	convertible := grant.AmountSat
	if convertible > balance {
		convertible = balance
	}
	if convertible > 0 {
		if _, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:    store.TxnBonus,
			Memo:    fmt.Sprintf("Bonus %d cleared and converted to cash", grant.ID),
			RefType: "bonus_grant",
			RefID:   grant.ID,
			Entries: []store.Entry{
				{Account: store.AccountUserBonus, UserID: grant.UserID, AmountSat: -convertible},
				{Account: store.AccountUserCash, UserID: grant.UserID, AmountSat: convertible},
			},
		}); err != nil {
			return err
		}
	}
	return store.CloseBonusGrantTx(ctx, tx, grant.ID, store.GrantCompleted, "wagering met", at)
}

func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique") || strings.Contains(message, "duplicate")
}

// ---------------------------------------------------------------------------
// Forfeiting and expiry
// ---------------------------------------------------------------------------

// Forfeit gives up every live bonus, returning the bonus balance to the house.
//
// This is how a customer unlocks a withdrawal without finishing the wagering.
// It is theirs to choose, so it is offered plainly rather than hidden: the
// alternative is a customer whose own money is stuck behind a promotion they
// no longer want.
func (s *Service) Forfeit(ctx context.Context, userID int64, reason string) (int64, error) {
	var forfeited int64
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		grants, err := store.ActiveBonusGrantsTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			return ErrNothingToForfeit
		}
		// The bonus pot is fungible, so the whole remaining balance goes back
		// at once rather than a per-grant amount that may already be spent.
		balance, err := store.BonusBalanceSatTx(ctx, tx, userID)
		if err != nil {
			return err
		}

		at := store.Timestamp(s.now())
		if balance > 0 {
			if _, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
				Kind:      store.TxnBonus,
				Memo:      "Bonus forfeited: " + reason,
				RefType:   "bonus_forfeit",
				CreatedBy: userID,
				Entries: []store.Entry{
					{Account: store.AccountUserBonus, UserID: userID, AmountSat: -balance},
					{Account: store.AccountHousePromotions, AmountSat: balance},
				},
			}); err != nil {
				return err
			}
			forfeited = balance
		}
		for _, grant := range grants {
			if err := store.CloseBonusGrantTx(ctx, tx, grant.ID, store.GrantForfeited, reason, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return forfeited, s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: userID, SubjectUserID: userID, Action: "bonus_forfeited",
		Detail: money.FormatBTC(forfeited) + " BTC: " + reason,
	})
}

// ExpireOverdue closes bonuses whose validity has run out. Runs on a timer.
func (s *Service) ExpireOverdue(ctx context.Context) (int, error) {
	grants, err := s.store.ExpiredBonusGrants(ctx, s.now())
	if err != nil {
		return 0, err
	}
	var closed int
	for _, grant := range grants {
		err := s.store.Tx(ctx, func(tx *store.Tx) error {
			at := store.Timestamp(s.now())
			amount, err := reclaimable(ctx, tx, grant)
			if err != nil {
				return err
			}
			if amount > 0 {
				if _, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
					Kind:    store.TxnBonus,
					Memo:    fmt.Sprintf("Bonus %d expired", grant.ID),
					RefType: "bonus_grant",
					RefID:   grant.ID,
					Entries: []store.Entry{
						{Account: store.AccountUserBonus, UserID: grant.UserID, AmountSat: -amount},
						{Account: store.AccountHousePromotions, AmountSat: amount},
					},
				}); err != nil {
					return err
				}
			}
			return store.CloseBonusGrantTx(ctx, tx, grant.ID, store.GrantExpired, "validity elapsed", at)
		})
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue // closed by something else in the meantime
			}
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// reclaimable is how much of a grant can still be taken back: its awarded
// amount, capped by the bonus balance the customer actually still holds.
func reclaimable(ctx context.Context, tx *store.Tx, grant store.BonusGrant) (int64, error) {
	balance, err := store.BonusBalanceSatTx(ctx, tx, grant.UserID)
	if err != nil {
		return 0, err
	}
	if grant.AmountSat < balance {
		return grant.AmountSat, nil
	}
	if balance < 0 {
		return 0, nil
	}
	return balance, nil
}

// Cancel withdraws a specific grant, for a trader correcting a mistake.
func (s *Service) Cancel(ctx context.Context, grantID, actorID int64, reason string) error {
	grant, err := s.store.GetBonusGrant(ctx, grantID)
	if err != nil {
		return err
	}
	if grant.Status != store.GrantActive {
		return fmt.Errorf("%w: bonus %d is %s", ErrNotPermitted, grantID, grant.Status)
	}
	err = s.store.Tx(ctx, func(tx *store.Tx) error {
		at := store.Timestamp(s.now())
		amount, err := reclaimable(ctx, tx, grant)
		if err != nil {
			return err
		}
		if amount > 0 {
			if _, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
				Kind:      store.TxnBonus,
				Memo:      fmt.Sprintf("Bonus %d cancelled: %s", grant.ID, reason),
				RefType:   "bonus_grant",
				RefID:     grant.ID,
				CreatedBy: actorID,
				Entries: []store.Entry{
					{Account: store.AccountUserBonus, UserID: grant.UserID, AmountSat: -amount},
					{Account: store.AccountHousePromotions, AmountSat: amount},
				},
			}); err != nil {
				return err
			}
		}
		return store.CloseBonusGrantTx(ctx, tx, grant.ID, store.GrantCancelled, reason, at)
	})
	if err != nil {
		return err
	}
	return s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: actorID, SubjectUserID: grant.UserID, Action: "bonus_cancelled",
		Detail: fmt.Sprintf("bonus %d: %s", grant.ID, reason),
	})
}
