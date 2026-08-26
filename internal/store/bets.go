package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// InsertBetTx writes a bet and its legs inside an open transaction.
func InsertBetTx(ctx context.Context, tx *Tx, at string, bet Bet) (int64, error) {
	accepted := 0
	if bet.AcceptedOddsChange {
		accepted = 1
	}
	id, err := tx.InsertID(ctx,
		`INSERT INTO bets (user_id, kind, stake_sat, odds_milli, potential_payout_sat,
		                   status, placed_at, ip, accepted_odds_change)
		 VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		bet.UserID, bet.Kind, bet.StakeSat, bet.OddsMilli, bet.PotentialPayoutSat,
		at, bet.IP, accepted)
	if err != nil {
		return 0, fmt.Errorf("store: insert bet: %w", err)
	}
	betID := id
	if err != nil {
		return 0, err
	}
	for _, leg := range bet.Legs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO bet_legs (bet_id, selection_id, odds_milli, event_name, market_title,
			                       selection_name, sport_key, starts_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			betID, leg.SelectionID, leg.OddsMilli, leg.EventName, leg.MarketTitle,
			leg.SelectionName, leg.SportKey, Timestamp(leg.StartsAt)); err != nil {
			return 0, fmt.Errorf("store: insert bet leg: %w", err)
		}
	}
	return betID, nil
}

const betColumns = `id, user_id, kind, stake_sat, odds_milli, potential_payout_sat, payout_sat,
                    status, placed_at, COALESCE(settled_at, ''), ip, accepted_odds_change`

func scanBet(row interface{ Scan(...any) error }) (Bet, error) {
	var b Bet
	var placedAt, settledAt string
	var accepted int
	err := row.Scan(&b.ID, &b.UserID, &b.Kind, &b.StakeSat, &b.OddsMilli, &b.PotentialPayoutSat,
		&b.PayoutSat, &b.Status, &placedAt, &settledAt, &b.IP, &accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return Bet{}, ErrNotFound
	}
	if err != nil {
		return Bet{}, fmt.Errorf("store: scan bet: %w", err)
	}
	b.AcceptedOddsChange = accepted == 1
	if b.PlacedAt, err = ParseTimestamp(placedAt); err != nil {
		return Bet{}, err
	}
	if b.SettledAt, err = ParseTimestamp(settledAt); err != nil {
		return Bet{}, err
	}
	return b, nil
}

// GetBet loads one bet with its legs.
func (s *Store) GetBet(ctx context.Context, betID int64) (Bet, error) {
	bet, err := scanBet(s.db.QueryRowContext(ctx, `SELECT `+betColumns+` FROM bets WHERE id = ?`, betID))
	if err != nil {
		return Bet{}, err
	}
	bet.Legs, err = s.legsForBets(ctx, bet.ID)
	return bet, err
}

// ListBetsForUser returns a customer's bets, newest first. Pass statuses to
// filter, or none for everything.
func (s *Store) ListBetsForUser(ctx context.Context, userID int64, limit int, statuses ...string) ([]Bet, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + betColumns + ` FROM bets WHERE user_id = ?`
	args := []any{userID}
	if len(statuses) > 0 {
		query += " AND status IN (" + placeholders(len(statuses)) + ")"
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list bets: %w", err)
	}
	defer rows.Close()

	var bets []Bet
	var ids []int64
	for rows.Next() {
		bet, err := scanBet(rows)
		if err != nil {
			return nil, err
		}
		bets = append(bets, bet)
		ids = append(ids, bet.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(bets) == 0 {
		return nil, nil
	}
	legs, err := s.legsForBets(ctx, ids...)
	if err != nil {
		return nil, err
	}
	byBet := make(map[int64][]BetLeg, len(bets))
	for _, leg := range legs {
		byBet[leg.BetID] = append(byBet[leg.BetID], leg)
	}
	for i := range bets {
		bets[i].Legs = byBet[bets[i].ID]
	}
	return bets, nil
}

func (s *Store) legsForBets(ctx context.Context, betIDs ...int64) ([]BetLeg, error) {
	if len(betIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(betIDs))
	for _, id := range betIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, bet_id, selection_id, odds_milli, status, event_name, market_title,
		        selection_name, sport_key, starts_at
		 FROM bet_legs WHERE bet_id IN (`+placeholders(len(betIDs))+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bet legs: %w", err)
	}
	defer rows.Close()
	var out []BetLeg
	for rows.Next() {
		var leg BetLeg
		var startsAt string
		if err := rows.Scan(&leg.ID, &leg.BetID, &leg.SelectionID, &leg.OddsMilli, &leg.Status,
			&leg.EventName, &leg.MarketTitle, &leg.SelectionName, &leg.SportKey, &startsAt); err != nil {
			return nil, err
		}
		if leg.StartsAt, err = ParseTimestamp(startsAt); err != nil {
			return nil, err
		}
		out = append(out, leg)
	}
	return out, rows.Err()
}

// OpenBetIDsForSelection returns the ids of open bets with a leg on a
// selection. Settlement walks these rather than every open bet.
func OpenBetIDsForSelectionTx(ctx context.Context, tx *Tx, selectionID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT b.id FROM bets b
		 JOIN bet_legs l ON l.bet_id = b.id
		 WHERE l.selection_id = ? AND b.status = 'open'`, selectionID)
	if err != nil {
		return nil, fmt.Errorf("store: open bets for selection: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetBetTx loads a bet with its legs inside an open transaction.
func GetBetTx(ctx context.Context, tx *Tx, betID int64) (Bet, error) {
	bet, err := scanBet(tx.QueryRowContext(ctx, `SELECT `+betColumns+` FROM bets WHERE id = ?`, betID))
	if err != nil {
		return Bet{}, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, bet_id, selection_id, odds_milli, status, event_name, market_title,
		        selection_name, sport_key, starts_at
		 FROM bet_legs WHERE bet_id = ? ORDER BY id`, betID)
	if err != nil {
		return Bet{}, fmt.Errorf("store: bet legs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var leg BetLeg
		var startsAt string
		if err := rows.Scan(&leg.ID, &leg.BetID, &leg.SelectionID, &leg.OddsMilli, &leg.Status,
			&leg.EventName, &leg.MarketTitle, &leg.SelectionName, &leg.SportKey, &startsAt); err != nil {
			return Bet{}, err
		}
		if leg.StartsAt, err = ParseTimestamp(startsAt); err != nil {
			return Bet{}, err
		}
		bet.Legs = append(bet.Legs, leg)
	}
	return bet, rows.Err()
}

// SetLegStatusTx grades one leg of every open bet riding on a selection.
func SetLegStatusTx(ctx context.Context, tx *Tx, selectionID int64, status string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE bet_legs SET status = ? WHERE selection_id = ? AND status = 'open'`,
		status, selectionID)
	if err != nil {
		return fmt.Errorf("store: grade legs: %w", err)
	}
	return nil
}

// SettleBetTx records a bet's final outcome and payout.
func SettleBetTx(ctx context.Context, tx *Tx, betID int64, status string, payoutSat int64, at string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE bets SET status = ?, payout_sat = ?, settled_at = ? WHERE id = ? AND status = 'open'`,
		status, payoutSat, at, betID)
	if err != nil {
		return fmt.Errorf("store: settle bet: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: bet %d is already settled", ErrConflict, betID)
	}
	return nil
}

// LiabilityForSelection is the book's exposure if a selection wins: the total
// that would be paid out across every open bet containing it.
//
// A parlay's full potential payout counts, because the whole payout becomes
// due only if this leg wins along with the others. It is a deliberate
// overstatement of true exposure and the safe direction to err in.
func (s *Store) LiabilityForSelection(ctx context.Context, selectionID int64) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(b.potential_payout_sat), 0) FROM bets b
		 JOIN bet_legs l ON l.bet_id = b.id
		 WHERE l.selection_id = ? AND b.status = 'open'`, selectionID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: liability: %w", err)
	}
	return derefInt64(total), nil
}

// LiabilityForSelectionTx is LiabilityForSelection inside an open transaction,
// so a stake check and the bet it admits cannot race each other.
func LiabilityForSelectionTx(ctx context.Context, tx *Tx, selectionID int64) (int64, error) {
	var total sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(b.potential_payout_sat), 0) FROM bets b
		 JOIN bet_legs l ON l.bet_id = b.id
		 WHERE l.selection_id = ? AND b.status = 'open'`, selectionID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: liability: %w", err)
	}
	return derefInt64(total), nil
}

// StakedSince totals a customer's stakes since a moment, for stake limits.
func (s *Store) StakedSince(ctx context.Context, userID int64, since string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(stake_sat), 0) FROM bets WHERE user_id = ? AND placed_at >= ?`,
		userID, since).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: staked since: %w", err)
	}
	return derefInt64(total), nil
}

// NetLossSince is stakes less returns over a window, for loss limits. A
// negative result means the customer is ahead.
func (s *Store) NetLossSince(ctx context.Context, userID int64, since string) (int64, error) {
	var staked, returned sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(stake_sat), 0), COALESCE(SUM(payout_sat), 0)
		 FROM bets WHERE user_id = ? AND placed_at >= ?`, userID, since).Scan(&staked, &returned)
	if err != nil {
		return 0, fmt.Errorf("store: net loss: %w", err)
	}
	return derefInt64(staked) - derefInt64(returned), nil
}

// OpenBetCount is how many unsettled bets a customer holds.
func (s *Store) OpenBetCount(ctx context.Context, userID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bets WHERE user_id = ? AND status = 'open'`, userID).Scan(&count)
	return count, err
}

// PendingSelectionIDs returns selections that still have open bets against
// them, oldest event first. The settlement screen works from this list.
func (s *Store) PendingSelectionIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT l.selection_id FROM bet_legs l
		 JOIN bets b ON b.id = l.bet_id
		 WHERE b.status = 'open' AND l.status = 'open'`)
	if err != nil {
		return nil, fmt.Errorf("store: pending selections: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
