package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Bonus kinds.
const (
	BonusDepositMatch = "deposit_match"
	BonusFreeCredit   = "free_credit"
	BonusComp         = "comp"
	BonusTestCredit   = "test_credit"
)

// Bonus grant statuses.
const (
	GrantActive    = "active"
	GrantCompleted = "completed"
	GrantForfeited = "forfeited"
	GrantExpired   = "expired"
	GrantCancelled = "cancelled"
)

// Manual adjustment statuses.
const (
	AdjustmentPending  = "pending"
	AdjustmentApplied  = "applied"
	AdjustmentRejected = "rejected"
)

// BonusOffer is a reusable template a bonus can be granted from.
type BonusOffer struct {
	ID            int64
	Code          string
	Name          string
	Description   string
	Kind          string
	AmountSat     int64
	MatchBps      int64
	MaxAmountSat  int64
	MinDepositSat int64
	// WageringX100 is the multiplier scaled by 100: 500 means the bonus must
	// be staked five times over before it becomes withdrawable cash.
	WageringX100 int64
	MinOddsMilli int64
	ValidDays    int
	MaxPerUser   int
	Active       bool
	CreatedAt    time.Time
	CreatedBy    int64
}

// BonusGrant is a bonus awarded to one customer.
type BonusGrant struct {
	ID                  int64
	UserID              int64
	OfferID             int64
	Code                string
	Kind                string
	AmountSat           int64
	WageringRequiredSat int64
	WageringDoneSat     int64
	MinOddsMilli        int64
	Status              string
	IsTest              bool
	Note                string
	GrantedBy           int64
	GrantedAt           time.Time
	ExpiresAt           time.Time
	ClosedAt            time.Time
	CloseReason         string
	LedgerTxnID         int64
}

// RemainingWageringSat is how much more must be staked before the bonus
// becomes cash.
func (g BonusGrant) RemainingWageringSat() int64 {
	remaining := g.WageringRequiredSat - g.WageringDoneSat
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ProgressBps is wagering progress in basis points, for a progress bar.
func (g BonusGrant) ProgressBps() int64 {
	if g.WageringRequiredSat <= 0 {
		return 10_000
	}
	progress := g.WageringDoneSat * 10_000 / g.WageringRequiredSat
	if progress > 10_000 {
		return 10_000
	}
	return progress
}

// ManualAdjustment is real money moved by a human.
type ManualAdjustment struct {
	ID           int64
	UserID       int64
	AmountSat    int64
	Reason       string
	Status       string
	RequestedBy  int64
	RequestedAt  time.Time
	DecidedBy    int64
	DecidedAt    time.Time
	DecisionNote string
	LedgerTxnID  int64
}

// ---------------------------------------------------------------------------
// Offers
// ---------------------------------------------------------------------------

// CreateBonusOffer stores a new offer template.
func (s *Store) CreateBonusOffer(ctx context.Context, offer BonusOffer) (int64, error) {
	active := 0
	if offer.Active {
		active = 1
	}
	return s.db.InsertID(ctx,
		`INSERT INTO bonus_offers (code, name, description, kind, amount_sat, match_bps,
		                           max_amount_sat, min_deposit_sat, wagering_x100,
		                           min_odds_milli, valid_days, max_per_user, active,
		                           created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		offer.Code, offer.Name, offer.Description, offer.Kind, offer.AmountSat,
		offer.MatchBps, offer.MaxAmountSat, offer.MinDepositSat, offer.WageringX100,
		offer.MinOddsMilli, offer.ValidDays, offer.MaxPerUser, active,
		Timestamp(s.Now()), nullInt64(offer.CreatedBy))
}

const offerColumns = `id, code, name, description, kind, amount_sat, match_bps,
                      max_amount_sat, min_deposit_sat, wagering_x100, min_odds_milli,
                      valid_days, max_per_user, active, created_at, COALESCE(created_by, 0)`

func scanOffer(row scanner) (BonusOffer, error) {
	var offer BonusOffer
	var active int
	var createdAt string
	err := row.Scan(&offer.ID, &offer.Code, &offer.Name, &offer.Description, &offer.Kind,
		&offer.AmountSat, &offer.MatchBps, &offer.MaxAmountSat, &offer.MinDepositSat,
		&offer.WageringX100, &offer.MinOddsMilli, &offer.ValidDays, &offer.MaxPerUser,
		&active, &createdAt, &offer.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return BonusOffer{}, ErrNotFound
	}
	if err != nil {
		return BonusOffer{}, fmt.Errorf("store: scan bonus offer: %w", err)
	}
	offer.Active = active == 1
	offer.CreatedAt, err = ParseTimestamp(createdAt)
	return offer, err
}

// GetBonusOffer loads an offer by id.
func (s *Store) GetBonusOffer(ctx context.Context, id int64) (BonusOffer, error) {
	return scanOffer(s.db.QueryRowContext(ctx,
		`SELECT `+offerColumns+` FROM bonus_offers WHERE id = ?`, id))
}

// GetBonusOfferByCode loads an offer by its promotional code.
func (s *Store) GetBonusOfferByCode(ctx context.Context, code string) (BonusOffer, error) {
	return scanOffer(s.db.QueryRowContext(ctx,
		`SELECT `+offerColumns+` FROM bonus_offers WHERE code = ?`, code))
}

// ListBonusOffers returns offers, optionally only the active ones.
func (s *Store) ListBonusOffers(ctx context.Context, onlyActive bool) ([]BonusOffer, error) {
	query := `SELECT ` + offerColumns + ` FROM bonus_offers`
	if onlyActive {
		query += ` WHERE active = 1`
	}
	query += ` ORDER BY id DESC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list bonus offers: %w", err)
	}
	defer rows.Close()
	var out []BonusOffer
	for rows.Next() {
		offer, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, offer)
	}
	return out, rows.Err()
}

// SetBonusOfferActive enables or retires an offer.
func (s *Store) SetBonusOfferActive(ctx context.Context, id int64, active bool) error {
	flag := 0
	if active {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE bonus_offers SET active = ? WHERE id = ?`, flag, id)
	return err
}

// ---------------------------------------------------------------------------
// Grants
// ---------------------------------------------------------------------------

// InsertBonusGrantTx records a grant inside an open transaction.
func InsertBonusGrantTx(ctx context.Context, tx *Tx, grant BonusGrant, at string) (int64, error) {
	isTest := 0
	if grant.IsTest {
		isTest = 1
	}
	return tx.InsertID(ctx,
		`INSERT INTO bonus_grants (user_id, offer_id, code, kind, amount_sat,
		                           wagering_required_sat, min_odds_milli, status,
		                           is_test, note, granted_by, granted_at, expires_at,
		                           ledger_txn_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		grant.UserID, nullInt64(grant.OfferID), grant.Code, grant.Kind, grant.AmountSat,
		grant.WageringRequiredSat, grant.MinOddsMilli, GrantActive, isTest, grant.Note,
		nullInt64(grant.GrantedBy), at, Timestamp(grant.ExpiresAt), nullInt64(grant.LedgerTxnID))
}

const grantColumns = `id, user_id, COALESCE(offer_id, 0), code, kind, amount_sat,
                      wagering_required_sat, wagering_done_sat, min_odds_milli, status,
                      is_test, note, COALESCE(granted_by, 0), granted_at, expires_at,
                      COALESCE(closed_at, ''), close_reason, COALESCE(ledger_txn_id, 0)`

func scanGrant(row scanner) (BonusGrant, error) {
	var grant BonusGrant
	var isTest int
	var grantedAt, expiresAt, closedAt string
	err := row.Scan(&grant.ID, &grant.UserID, &grant.OfferID, &grant.Code, &grant.Kind,
		&grant.AmountSat, &grant.WageringRequiredSat, &grant.WageringDoneSat,
		&grant.MinOddsMilli, &grant.Status, &isTest, &grant.Note, &grant.GrantedBy,
		&grantedAt, &expiresAt, &closedAt, &grant.CloseReason, &grant.LedgerTxnID)
	if errors.Is(err, sql.ErrNoRows) {
		return BonusGrant{}, ErrNotFound
	}
	if err != nil {
		return BonusGrant{}, fmt.Errorf("store: scan bonus grant: %w", err)
	}
	grant.IsTest = isTest == 1
	for _, pair := range []struct {
		raw string
		dst *time.Time
	}{{grantedAt, &grant.GrantedAt}, {expiresAt, &grant.ExpiresAt}, {closedAt, &grant.ClosedAt}} {
		parsed, err := ParseTimestamp(pair.raw)
		if err != nil {
			return BonusGrant{}, err
		}
		*pair.dst = parsed
	}
	return grant, nil
}

// GetBonusGrant loads one grant.
func (s *Store) GetBonusGrant(ctx context.Context, id int64) (BonusGrant, error) {
	return scanGrant(s.db.QueryRowContext(ctx,
		`SELECT `+grantColumns+` FROM bonus_grants WHERE id = ?`, id))
}

// GetBonusGrantTx loads one grant inside an open transaction.
func GetBonusGrantTx(ctx context.Context, tx *Tx, id int64) (BonusGrant, error) {
	return scanGrant(tx.QueryRowContext(ctx,
		`SELECT `+grantColumns+` FROM bonus_grants WHERE id = ?`, id))
}

// BonusGrantsForUser lists a customer's bonuses, newest first.
func (s *Store) BonusGrantsForUser(ctx context.Context, userID int64, statuses ...string) ([]BonusGrant, error) {
	query := `SELECT ` + grantColumns + ` FROM bonus_grants WHERE user_id = ?`
	args := []any{userID}
	if len(statuses) > 0 {
		query += ` AND status IN (` + placeholders(len(statuses)) + `)`
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += ` ORDER BY id DESC`
	return s.queryGrants(ctx, query, args...)
}

// ActiveBonusGrantsTx lists a customer's live bonuses inside a transaction,
// oldest first so the earliest-granted bonus is consumed and cleared first.
func ActiveBonusGrantsTx(ctx context.Context, tx *Tx, userID int64) ([]BonusGrant, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+grantColumns+` FROM bonus_grants
		 WHERE user_id = ? AND status = ? ORDER BY id`, userID, GrantActive)
	if err != nil {
		return nil, fmt.Errorf("store: active bonus grants: %w", err)
	}
	defer rows.Close()
	var out []BonusGrant
	for rows.Next() {
		grant, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, grant)
	}
	return out, rows.Err()
}

// ExpiredBonusGrants lists live grants whose expiry has passed.
func (s *Store) ExpiredBonusGrants(ctx context.Context, now time.Time) ([]BonusGrant, error) {
	return s.queryGrants(ctx,
		`SELECT `+grantColumns+` FROM bonus_grants
		 WHERE status = ? AND expires_at <= ? ORDER BY id`, GrantActive, Timestamp(now))
}

// CountGrantsFromOffer is how many times a customer has taken an offer.
func (s *Store) CountGrantsFromOffer(ctx context.Context, userID, offerID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bonus_grants WHERE user_id = ? AND offer_id = ?`,
		userID, offerID).Scan(&count)
	return count, err
}

func (s *Store) queryGrants(ctx context.Context, query string, args ...any) ([]BonusGrant, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: bonus grants: %w", err)
	}
	defer rows.Close()
	var out []BonusGrant
	for rows.Next() {
		grant, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, grant)
	}
	return out, rows.Err()
}

// AddWageringTx credits progress against a grant and records what caused it.
//
// refID identifies the stake within its source: a bet id for the sportsbook, a
// hand for poker, a spin for slots. The unique index on
// (grant_id, source, ref_id) is what stops one stake being counted twice if
// settlement is replayed.
func AddWageringTx(ctx context.Context, tx *Tx, grantID, refID, stakeSat, contributionSat int64, source, at string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bonus_wagering (grant_id, ref_id, source, stake_sat, contribution_sat, at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		grantID, refID, source, stakeSat, contributionSat, at); err != nil {
		return fmt.Errorf("store: record wagering: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE bonus_grants SET wagering_done_sat = wagering_done_sat + ?
		 WHERE id = ? AND status = ?`, contributionSat, grantID, GrantActive); err != nil {
		return fmt.Errorf("store: advance wagering: %w", err)
	}
	return nil
}

// CloseBonusGrantTx moves a grant to a terminal state.
func CloseBonusGrantTx(ctx context.Context, tx *Tx, grantID int64, status, reason, at string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE bonus_grants SET status = ?, closed_at = ?, close_reason = ?
		 WHERE id = ? AND status = ?`, status, at, reason, grantID, GrantActive)
	if err != nil {
		return fmt.Errorf("store: close bonus grant: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: bonus grant %d is not active", ErrConflict, grantID)
	}
	return nil
}

// ReduceBonusGrantTx lowers a grant's remaining amount as it is staked, so the
// row always reflects what is left rather than what was awarded.
func ReduceBonusGrantTx(ctx context.Context, tx *Tx, grantID, amountSat int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE bonus_grants SET amount_sat = amount_sat - ?
		 WHERE id = ? AND status = ? AND amount_sat >= ?`,
		amountSat, grantID, GrantActive, amountSat)
	if err != nil {
		return fmt.Errorf("store: reduce bonus grant: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: bonus grant %d cannot give up %d sat", ErrConflict, grantID, amountSat)
	}
	return nil
}

// BonusBalanceSat is a customer's total bonus balance.
func (s *Store) BonusBalanceSat(ctx context.Context, userID int64) (int64, error) {
	var balance sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat), 0) FROM ledger_entries
		 WHERE account = ? AND user_id = ?`, AccountUserBonus, userID).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("store: bonus balance: %w", err)
	}
	return derefInt64(balance), nil
}

// BonusBalanceSatTx is BonusBalanceSat inside an open transaction.
func BonusBalanceSatTx(ctx context.Context, tx *Tx, userID int64) (int64, error) {
	var balance sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat), 0) FROM ledger_entries
		 WHERE account = ? AND user_id = ?`, AccountUserBonus, userID).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("store: bonus balance: %w", err)
	}
	return derefInt64(balance), nil
}

// ---------------------------------------------------------------------------
// Manual adjustments
// ---------------------------------------------------------------------------

// CreateManualAdjustment records a request to move real money by hand.
func (s *Store) CreateManualAdjustment(ctx context.Context, adjustment ManualAdjustment) (int64, error) {
	return s.db.InsertID(ctx,
		`INSERT INTO manual_adjustments (user_id, amount_sat, reason, status, requested_by, requested_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		adjustment.UserID, adjustment.AmountSat, adjustment.Reason,
		AdjustmentPending, nullInt64(adjustment.RequestedBy), Timestamp(s.Now()))
}

const adjustmentColumns = `id, user_id, amount_sat, reason, status,
                           COALESCE(requested_by, 0), requested_at,
                           COALESCE(decided_by, 0), COALESCE(decided_at, ''),
                           decision_note, COALESCE(ledger_txn_id, 0)`

func scanAdjustment(row scanner) (ManualAdjustment, error) {
	var adjustment ManualAdjustment
	var requestedAt, decidedAt string
	err := row.Scan(&adjustment.ID, &adjustment.UserID, &adjustment.AmountSat,
		&adjustment.Reason, &adjustment.Status, &adjustment.RequestedBy, &requestedAt,
		&adjustment.DecidedBy, &decidedAt, &adjustment.DecisionNote, &adjustment.LedgerTxnID)
	if errors.Is(err, sql.ErrNoRows) {
		return ManualAdjustment{}, ErrNotFound
	}
	if err != nil {
		return ManualAdjustment{}, fmt.Errorf("store: scan adjustment: %w", err)
	}
	if adjustment.RequestedAt, err = ParseTimestamp(requestedAt); err != nil {
		return ManualAdjustment{}, err
	}
	adjustment.DecidedAt, err = ParseTimestamp(decidedAt)
	return adjustment, err
}

// GetManualAdjustmentTx loads an adjustment inside an open transaction.
func GetManualAdjustmentTx(ctx context.Context, tx *Tx, id int64) (ManualAdjustment, error) {
	return scanAdjustment(tx.QueryRowContext(ctx,
		`SELECT `+adjustmentColumns+` FROM manual_adjustments WHERE id = ?`, id))
}

// ManualAdjustmentsByStatus powers the approval queue.
func (s *Store) ManualAdjustmentsByStatus(ctx context.Context, statuses ...string) ([]ManualAdjustment, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+adjustmentColumns+` FROM manual_adjustments
		 WHERE status IN (`+placeholders(len(statuses))+`)
		 ORDER BY requested_at DESC LIMIT 200`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: adjustments: %w", err)
	}
	defer rows.Close()
	var out []ManualAdjustment
	for rows.Next() {
		adjustment, err := scanAdjustment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, adjustment)
	}
	return out, rows.Err()
}

// DecideManualAdjustmentTx records an approval or rejection.
func DecideManualAdjustmentTx(ctx context.Context, tx *Tx, id int64, status string, deciderID, ledgerTxnID int64, note, at string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE manual_adjustments
		 SET status = ?, decided_by = ?, decided_at = ?, decision_note = ?, ledger_txn_id = ?
		 WHERE id = ? AND status = ?`,
		status, nullInt64(deciderID), at, note, nullInt64(ledgerTxnID), id, AdjustmentPending)
	if err != nil {
		return fmt.Errorf("store: decide adjustment: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: adjustment %d has already been decided", ErrConflict, id)
	}
	return nil
}
