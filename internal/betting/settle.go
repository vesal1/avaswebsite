package betting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// SettlementReport summarises a settlement run.
type SettlementReport struct {
	MarketsSettled int
	MarketsManual  []int64
	BetsSettled    int
	PaidOutSat     int64
	Problems       []string
}

// SettleEvent posts a result and settles every market it can grade.
//
// Markets the result cannot decide are listed in MarketsManual rather than
// guessed at; a trader settles those explicitly. Settling a market wrongly is
// worse than not settling it yet, because a paid-out bet cannot be taken back.
func (s *Service) SettleEvent(ctx context.Context, eventID int64, result Result, actorID int64) (SettlementReport, error) {
	var report SettlementReport

	event, err := s.store.GetEvent(ctx, eventID)
	if err != nil {
		return report, err
	}
	sport, ok := catalog.Lookup(event.SportKey)
	if !ok {
		return report, fmt.Errorf("betting: event %d references unknown sport %q", eventID, event.SportKey)
	}
	participants, err := s.store.ParticipantsForEvent(ctx, eventID)
	if err != nil {
		return report, err
	}
	markets, err := s.store.MarketsForEvent(ctx, eventID, false)
	if err != nil {
		return report, err
	}

	// Record the result first so it is visible even if a market fails to grade.
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		for _, outcome := range result.Participants {
			var score sql.NullInt64
			if outcome.HasScore {
				score = sql.NullInt64{Int64: outcome.Score, Valid: true}
			}
			if err := s.store.RecordResult(ctx, tx, outcome.ParticipantID, score,
				outcome.FinishPosition, outcome.Withdrawn); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return report, err
	}

	for _, market := range markets {
		if market.Status == store.MarketSettled || market.Status == store.MarketVoid {
			continue
		}
		outcomes, err := GradeMarket(sport, market, participants, result)
		switch {
		case errors.Is(err, ErrManualGrading):
			report.MarketsManual = append(report.MarketsManual, market.ID)
			continue
		case errors.Is(err, ErrIncompleteResult):
			report.MarketsManual = append(report.MarketsManual, market.ID)
			report.Problems = append(report.Problems, fmt.Sprintf("market %d: %v", market.ID, err))
			continue
		case err != nil:
			report.Problems = append(report.Problems, fmt.Sprintf("market %d: %v", market.ID, err))
			continue
		}

		settled, paid, problems, err := s.applyOutcomes(ctx, market, outcomes, actorID)
		if err != nil {
			return report, err
		}
		report.MarketsSettled++
		report.BetsSettled += settled
		report.PaidOutSat += paid
		report.Problems = append(report.Problems, problems...)
	}

	status := store.EventSettled
	if len(report.MarketsManual) > 0 {
		status = store.EventFinished
	}
	if err := s.store.SetEventStatus(ctx, eventID, status); err != nil {
		return report, err
	}

	return report, s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: actorID, Action: "event_settled",
		Detail: fmt.Sprintf("event %d: %d markets settled, %d bets, %s BTC paid, %d markets left for manual grading",
			eventID, report.MarketsSettled, report.BetsSettled,
			money.FormatBTC(report.PaidOutSat), len(report.MarketsManual)),
	})
}

// SettleMarketManually settles a market a trader has graded by hand, by naming
// the winning selections. Every other selection in the market loses, except
// those explicitly voided.
func (s *Service) SettleMarketManually(ctx context.Context, marketID int64, winners, voided []int64, actorID int64) (SettlementReport, error) {
	var report SettlementReport

	market, err := s.store.GetMarket(ctx, marketID)
	if err != nil {
		return report, err
	}
	if market.Status == store.MarketSettled || market.Status == store.MarketVoid {
		return report, fmt.Errorf("%w: market %d is already settled", store.ErrConflict, marketID)
	}

	winning := make(map[int64]bool, len(winners))
	for _, id := range winners {
		winning[id] = true
	}
	void := make(map[int64]bool, len(voided))
	for _, id := range voided {
		void[id] = true
	}

	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		switch {
		case void[selection.ID]:
			outcomes[selection.ID] = store.OutcomeVoid
		case winning[selection.ID]:
			outcomes[selection.ID] = store.OutcomeWon
		default:
			outcomes[selection.ID] = store.OutcomeLost
		}
	}
	// Naming a selection that is not in this market is a mistake worth
	// catching before any money moves.
	for _, id := range append(append([]int64{}, winners...), voided...) {
		if _, ok := outcomes[id]; !ok {
			return report, fmt.Errorf("betting: selection %d is not in market %d", id, marketID)
		}
	}

	settled, paid, problems, err := s.applyOutcomes(ctx, market, outcomes, actorID)
	if err != nil {
		return report, err
	}
	report.MarketsSettled = 1
	report.BetsSettled = settled
	report.PaidOutSat = paid
	report.Problems = problems

	return report, s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: actorID, Action: "market_settled_manually",
		Detail: fmt.Sprintf("market %d: %d bets settled, %s BTC paid",
			marketID, settled, money.FormatBTC(paid)),
	})
}

// VoidMarket voids every selection in a market and returns the stakes.
func (s *Service) VoidMarket(ctx context.Context, marketID, actorID int64, reason string) (SettlementReport, error) {
	var report SettlementReport
	market, err := s.store.GetMarket(ctx, marketID)
	if err != nil {
		return report, err
	}
	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		outcomes[selection.ID] = store.OutcomeVoid
	}
	settled, paid, problems, err := s.applyOutcomes(ctx, market, outcomes, actorID)
	if err != nil {
		return report, err
	}
	report.MarketsSettled = 1
	report.BetsSettled = settled
	report.PaidOutSat = paid
	report.Problems = problems

	return report, s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: actorID, Action: "market_voided",
		Detail: fmt.Sprintf("market %d: %s", marketID, reason),
	})
}

// creditWagering tells the wagering recorder about bets that have just
// settled. It runs after the settlement transaction has committed, so a
// promotion bookkeeping failure can never roll back a payout that the customer
// has already been shown. Failures are returned to be reported, not to undo.
func (s *Service) creditWagering(ctx context.Context, settled []settledStake) []string {
	if s.wagering == nil || len(settled) == 0 {
		return nil
	}
	var problems []string
	for _, stake := range settled {
		if err := s.wagering.RecordStake(ctx, stake.UserID, stake.BetID,
			stake.StakeSat, stake.OddsMilli, "sportsbook"); err != nil {
			problems = append(problems,
				fmt.Sprintf("wagering not credited for bet %d: %v", stake.BetID, err))
		}
	}
	return problems
}

// settledStake is what the wagering recorder needs about a resolved bet.
type settledStake struct {
	UserID    int64
	BetID     int64
	StakeSat  int64
	OddsMilli int64
}

// applyOutcomes writes selection outcomes, grades every affected bet leg and
// settles the bets whose legs are all decided.
//
// The whole run is one transaction. A partial settlement would leave bets with
// some legs graded and no payout, which is indistinguishable from a stuck bet.
func (s *Service) applyOutcomes(ctx context.Context, market store.Market, outcomes map[int64]string, actorID int64) (int, int64, []string, error) {
	var betsSettled int
	var paidOut int64
	var settled []settledStake

	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		at := store.Timestamp(s.now())
		affected := make(map[int64]bool)

		for selectionID, outcome := range outcomes {
			if _, err := tx.ExecContext(ctx,
				`UPDATE selections SET status = ? WHERE id = ?`, outcome, selectionID); err != nil {
				return fmt.Errorf("betting: grade selection %d: %w", selectionID, err)
			}
			betIDs, err := store.OpenBetIDsForSelectionTx(ctx, tx, selectionID)
			if err != nil {
				return err
			}
			if err := store.SetLegStatusTx(ctx, tx, selectionID, outcome); err != nil {
				return err
			}
			for _, betID := range betIDs {
				affected[betID] = true
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE markets SET status = ?, settled_at = ? WHERE id = ?`,
			store.MarketSettled, at, market.ID); err != nil {
			return fmt.Errorf("betting: close market %d: %w", market.ID, err)
		}

		for betID := range affected {
			done, payout, stake, err := s.settleBetIfComplete(ctx, tx, betID, at, actorID)
			if err != nil {
				return err
			}
			if done {
				betsSettled++
				paidOut += payout
				settled = append(settled, stake)
			}
		}
		return nil
	})
	if err != nil {
		return betsSettled, paidOut, nil, err
	}
	// Outside the transaction: see creditWagering.
	return betsSettled, paidOut, s.creditWagering(ctx, settled), nil
}

// settleBetIfComplete settles a bet once every leg is decided. A bet with an
// undecided leg is left open.
func (s *Service) settleBetIfComplete(ctx context.Context, tx *store.Tx, betID int64, at string, actorID int64) (bool, int64, settledStake, error) {
	var stake settledStake

	bet, err := store.GetBetTx(ctx, tx, betID)
	if err != nil {
		return false, 0, stake, err
	}
	if bet.Status != store.OutcomeOpen {
		return false, 0, stake, nil
	}

	status, payout, err := SettleBet(bet)
	if err != nil {
		return false, 0, stake, err
	}
	if status == "" {
		return false, 0, stake, nil // still has an open leg
	}

	if err := store.SettleBetTx(ctx, tx, betID, status, payout, at); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return false, 0, stake, nil
		}
		return false, 0, stake, err
	}
	stake = settledStake{
		UserID: bet.UserID, BetID: bet.ID,
		StakeSat: bet.StakeSat, OddsMilli: bet.OddsMilli,
	}

	// Release the escrowed stake, pay the customer what they are owed, and
	// book the difference as house revenue. A customer win makes the revenue
	// entry negative, which is the correct sign for a loss to the book.
	entries := []store.Entry{
		{Account: store.AccountBetEscrow, AmountSat: -bet.StakeSat},
	}
	if payout > 0 {
		entries = append(entries, store.Entry{
			Account: store.AccountUserCash, UserID: bet.UserID, AmountSat: payout,
		})
	}
	if margin := bet.StakeSat - payout; margin != 0 {
		entries = append(entries, store.Entry{
			Account: store.AccountHouseRevenue, AmountSat: margin,
		})
	}

	kind := store.TxnBetPayout
	if status == store.OutcomeVoid {
		kind = store.TxnBetVoid
	}
	if _, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
		Kind:      kind,
		Memo:      fmt.Sprintf("Bet %d settled %s", betID, status),
		RefType:   "bet",
		RefID:     betID,
		CreatedBy: actorID,
		Entries:   entries,
	}); err != nil {
		return false, 0, stake, err
	}
	return true, payout, stake, nil
}

// SettleBet computes a bet's final status and payout from its legs.
//
// An empty status means the bet is not yet decided. The function is pure so
// the payout rules can be tested directly, without a database.
func SettleBet(bet store.Bet) (string, int64, error) {
	if len(bet.Legs) == 0 {
		return "", 0, fmt.Errorf("betting: bet %d has no legs", bet.ID)
	}

	effective := make([]int64, 0, len(bet.Legs))
	allVoid := true
	for _, leg := range bet.Legs {
		switch leg.Status {
		case store.OutcomeOpen, store.OutcomeSuspended:
			return "", 0, nil
		case store.OutcomeLost:
			// One losing leg loses the whole bet, whatever the others did.
			return store.OutcomeLost, 0, nil
		case store.OutcomeWon:
			allVoid = false
			effective = append(effective, leg.OddsMilli)
		case store.OutcomeVoid:
			// A void leg drops out of a parlay at evens, leaving the rest of
			// the bet standing.
			effective = append(effective, money.OddsScale)
		case store.OutcomeHalfWon:
			// Half the stake won at the full price, half was returned.
			allVoid = false
			effective = append(effective, (leg.OddsMilli+money.OddsScale)/2)
		case store.OutcomeHalfLost:
			// Half the stake was lost, half returned.
			allVoid = false
			effective = append(effective, money.OddsScale/2)
		default:
			return "", 0, fmt.Errorf("betting: leg %d has unknown status %q", leg.ID, leg.Status)
		}
	}

	if allVoid {
		return store.OutcomeVoid, bet.StakeSat, nil
	}

	payout := bet.StakeSat
	for _, odds := range effective {
		payout = payout * odds / money.OddsScale
	}

	switch {
	case payout == 0:
		return store.OutcomeLost, 0, nil
	case payout > bet.StakeSat:
		return store.OutcomeWon, payout, nil
	case payout == bet.StakeSat:
		return store.OutcomeVoid, payout, nil
	default:
		return store.OutcomeHalfLost, payout, nil
	}
}
