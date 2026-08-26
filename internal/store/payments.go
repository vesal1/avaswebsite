package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Deposit addresses
// ---------------------------------------------------------------------------

// SaveDepositAddress records an address issued to a customer.
func (s *Store) SaveDepositAddress(ctx context.Context, addr DepositAddress) (int64, error) {
	id, err := s.db.InsertID(ctx,
		`INSERT INTO deposit_addresses (user_id, address, provider, provider_ref, network, created_at, active)
		 VALUES (?, ?, ?, ?, ?, ?, 1)`,
		addr.UserID, addr.Address, addr.Provider, addr.ProviderRef, addr.Network, Timestamp(s.Now()))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: address %s is already issued", ErrConflict, addr.Address)
		}
		return 0, fmt.Errorf("store: save deposit address: %w", err)
	}
	return id, nil
}

// ActiveDepositAddress returns the customer's current address, if any.
func (s *Store) ActiveDepositAddress(ctx context.Context, userID int64) (DepositAddress, error) {
	var addr DepositAddress
	var createdAt string
	var active int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, address, provider, provider_ref, network, created_at, active
		 FROM deposit_addresses WHERE user_id = ? AND active = 1
		 ORDER BY id DESC LIMIT 1`, userID).
		Scan(&addr.ID, &addr.UserID, &addr.Address, &addr.Provider, &addr.ProviderRef,
			&addr.Network, &createdAt, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return DepositAddress{}, ErrNotFound
	}
	if err != nil {
		return DepositAddress{}, fmt.Errorf("store: active deposit address: %w", err)
	}
	addr.Active = active == 1
	addr.CreatedAt, err = ParseTimestamp(createdAt)
	return addr, err
}

// UserForDepositAddress maps an inbound payment back to its customer.
func (s *Store) UserForDepositAddress(ctx context.Context, address string) (int64, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM deposit_addresses WHERE address = ?`, address).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}

// ---------------------------------------------------------------------------
// Deposits
// ---------------------------------------------------------------------------

// RecordDeposit inserts a newly seen on-chain payment, or updates the
// confirmation count of one already known.
//
// The (txid, vout) unique constraint is what makes this safe to call from a
// webhook that may fire many times for the same output.
func (s *Store) RecordDeposit(ctx context.Context, d Deposit) (int64, error) {
	var id int64
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, status FROM deposits WHERE txid = ? AND vout = ?`, d.TxID, d.Vout).
		Scan(&id, &status)
	switch {
	case err == nil:
		// Already known. Only the confirmation count may move, and never
		// backwards past a credit that has already been made.
		if status == DepositCredited {
			return id, nil
		}
		newStatus := status
		if d.Confirmations > 0 && status == DepositPending {
			newStatus = DepositConfirmed
		}
		_, err = s.db.ExecContext(ctx,
			`UPDATE deposits SET confirmations = ?, status = ? WHERE id = ?`,
			d.Confirmations, newStatus, id)
		return id, err
	case errors.Is(err, sql.ErrNoRows):
		status := DepositPending
		if d.Confirmations > 0 {
			status = DepositConfirmed
		}
		id, insertErr := s.db.InsertID(ctx,
			`INSERT INTO deposits (user_id, address, txid, vout, amount_sat, confirmations,
			                       status, first_seen_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			d.UserID, d.Address, d.TxID, d.Vout, d.AmountSat, d.Confirmations,
			status, Timestamp(s.Now()))
		if insertErr != nil {
			return 0, fmt.Errorf("store: record deposit: %w", insertErr)
		}
		return id, nil
	default:
		return 0, fmt.Errorf("store: lookup deposit: %w", err)
	}
}

const depositColumns = `id, user_id, address, txid, vout, amount_sat, confirmations, status,
                        first_seen_at, COALESCE(credited_at, ''), COALESCE(ledger_txn_id, 0)`

func scanDeposit(row interface{ Scan(...any) error }) (Deposit, error) {
	var d Deposit
	var firstSeen, credited string
	err := row.Scan(&d.ID, &d.UserID, &d.Address, &d.TxID, &d.Vout, &d.AmountSat,
		&d.Confirmations, &d.Status, &firstSeen, &credited, &d.LedgerTxnID)
	if errors.Is(err, sql.ErrNoRows) {
		return Deposit{}, ErrNotFound
	}
	if err != nil {
		return Deposit{}, fmt.Errorf("store: scan deposit: %w", err)
	}
	if d.FirstSeenAt, err = ParseTimestamp(firstSeen); err != nil {
		return Deposit{}, err
	}
	if d.CreditedAt, err = ParseTimestamp(credited); err != nil {
		return Deposit{}, err
	}
	return d, nil
}

// DepositByID loads a single deposit.
func (s *Store) DepositByID(ctx context.Context, depositID int64) (Deposit, error) {
	return scanDeposit(s.db.QueryRowContext(ctx, `SELECT `+depositColumns+` FROM deposits WHERE id = ?`, depositID))
}

// GetDepositTx loads a deposit inside an open transaction.
func GetDepositTx(ctx context.Context, tx *Tx, depositID int64) (Deposit, error) {
	return scanDeposit(tx.QueryRowContext(ctx, `SELECT `+depositColumns+` FROM deposits WHERE id = ?`, depositID))
}

// DepositsForUser lists a customer's deposits, newest first.
func (s *Store) DepositsForUser(ctx context.Context, userID int64, limit int) ([]Deposit, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+depositColumns+` FROM deposits WHERE user_id = ? ORDER BY id DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: deposits for user: %w", err)
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		d, err := scanDeposit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreditableDeposits returns confirmed deposits that have reached the required
// confirmation depth and have not yet been credited.
func (s *Store) CreditableDeposits(ctx context.Context, minConfirmations int) ([]Deposit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+depositColumns+` FROM deposits
		 WHERE status IN ('pending', 'confirmed') AND confirmations >= ?
		 ORDER BY id`, minConfirmations)
	if err != nil {
		return nil, fmt.Errorf("store: creditable deposits: %w", err)
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		d, err := scanDeposit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDepositCreditedTx finalises a deposit against its ledger transaction.
// The status guard is what stops a concurrent crediting run paying twice.
func MarkDepositCreditedTx(ctx context.Context, tx *Tx, depositID, ledgerTxnID int64, at string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE deposits SET status = 'credited', credited_at = ?, ledger_txn_id = ?
		 WHERE id = ? AND status IN ('pending', 'confirmed')`, at, ledgerTxnID, depositID)
	if err != nil {
		return fmt.Errorf("store: mark deposit credited: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: deposit %d is not awaiting credit", ErrConflict, depositID)
	}
	return nil
}

// SetDepositStatus moves a deposit to a terminal state such as frozen or
// orphaned.
func (s *Store) SetDepositStatus(ctx context.Context, depositID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deposits SET status = ? WHERE id = ?`, status, depositID)
	return err
}

// DepositedSince totals credited deposits over a window, for deposit limits.
func (s *Store) DepositedSince(ctx context.Context, userID int64, since time.Time) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat), 0) FROM deposits
		 WHERE user_id = ? AND status = 'credited' AND credited_at >= ?`,
		userID, Timestamp(since)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: deposited since: %w", err)
	}
	return derefInt64(total), nil
}

// ---------------------------------------------------------------------------
// Withdrawals
// ---------------------------------------------------------------------------

// InsertWithdrawalTx records a withdrawal request inside an open transaction.
func InsertWithdrawalTx(ctx context.Context, tx *Tx, w Withdrawal, at string) (int64, error) {
	id, err := tx.InsertID(ctx,
		`INSERT INTO withdrawals (user_id, address, amount_sat, fee_sat, status, requested_at, ledger_txn_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.UserID, w.Address, w.AmountSat, w.FeeSat, w.Status, at, nullInt64(w.LedgerTxnID))
	if err != nil {
		return 0, fmt.Errorf("store: insert withdrawal: %w", err)
	}
	return id, nil
}

const withdrawalColumns = `id, user_id, address, amount_sat, fee_sat, status, txid, requested_at,
                           COALESCE(decided_at, ''), COALESCE(decided_by, 0), reason,
                           COALESCE(ledger_txn_id, 0)`

func scanWithdrawal(row interface{ Scan(...any) error }) (Withdrawal, error) {
	var w Withdrawal
	var requested, decided string
	err := row.Scan(&w.ID, &w.UserID, &w.Address, &w.AmountSat, &w.FeeSat, &w.Status,
		&w.TxID, &requested, &decided, &w.DecidedBy, &w.Reason, &w.LedgerTxnID)
	if errors.Is(err, sql.ErrNoRows) {
		return Withdrawal{}, ErrNotFound
	}
	if err != nil {
		return Withdrawal{}, fmt.Errorf("store: scan withdrawal: %w", err)
	}
	if w.RequestedAt, err = ParseTimestamp(requested); err != nil {
		return Withdrawal{}, err
	}
	if w.DecidedAt, err = ParseTimestamp(decided); err != nil {
		return Withdrawal{}, err
	}
	return w, nil
}

// GetWithdrawal loads a single withdrawal.
func (s *Store) GetWithdrawal(ctx context.Context, id int64) (Withdrawal, error) {
	return scanWithdrawal(s.db.QueryRowContext(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = ?`, id))
}

// GetWithdrawalTx loads a withdrawal inside an open transaction.
func GetWithdrawalTx(ctx context.Context, tx *Tx, id int64) (Withdrawal, error) {
	return scanWithdrawal(tx.QueryRowContext(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = ?`, id))
}

// WithdrawalsForUser lists a customer's withdrawals, newest first.
func (s *Store) WithdrawalsForUser(ctx context.Context, userID int64, limit int) ([]Withdrawal, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE user_id = ? ORDER BY id DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: withdrawals for user: %w", err)
	}
	defer rows.Close()
	var out []Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WithdrawalsByStatus powers the compliance review queue.
func (s *Store) WithdrawalsByStatus(ctx context.Context, statuses ...string) ([]Withdrawal, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals
		 WHERE status IN (`+placeholders(len(statuses))+`) ORDER BY requested_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: withdrawals by status: %w", err)
	}
	defer rows.Close()
	var out []Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWithdrawalStatusTx moves a withdrawal forward, guarding against a
// transition from a state that no longer permits it.
func SetWithdrawalStatusTx(ctx context.Context, tx *Tx, id int64, from []string, to string, decidedBy int64, reason, at string) error {
	args := []any{to, at, nullInt64(decidedBy), reason, id}
	query := `UPDATE withdrawals SET status = ?, decided_at = ?, decided_by = ?, reason = ?
	          WHERE id = ?`
	if len(from) > 0 {
		query += " AND status IN (" + placeholders(len(from)) + ")"
		for _, status := range from {
			args = append(args, status)
		}
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: set withdrawal status: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: withdrawal %d is not in a state that allows %s", ErrConflict, id, to)
	}
	return nil
}

// SetWithdrawalTxID records the broadcast transaction.
func (s *Store) SetWithdrawalTxID(ctx context.Context, id int64, txid string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE withdrawals SET txid = ? WHERE id = ?`, txid, id)
	return err
}

// PendingWithdrawalSat is the total a customer has in flight. It is already
// debited from their balance, so it is shown separately rather than added back.
func (s *Store) PendingWithdrawalSat(ctx context.Context, userID int64) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat + fee_sat), 0) FROM withdrawals
		 WHERE user_id = ? AND status IN ('requested', 'review', 'approved', 'broadcast')`,
		userID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: pending withdrawals: %w", err)
	}
	return derefInt64(total), nil
}
