package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Entry is one side of a proposed money movement.
type Entry struct {
	Account   string
	UserID    int64 // required for user_cash, ignored otherwise
	AmountSat int64 // positive credits the account, negative debits it
}

// TxnSpec describes a complete money movement to post.
type TxnSpec struct {
	Kind      string
	Memo      string
	RefType   string
	RefID     int64
	CreatedBy int64
	Entries   []Entry
}

// PostTxn writes a balanced ledger transaction inside an existing database
// transaction and returns its id.
//
// The entries must sum to exactly zero. This is checked here rather than
// trusted from the caller, because an unbalanced ledger cannot be repaired
// after the fact: there is no record of which side was wrong.
func PostTxn(ctx context.Context, tx *Tx, at string, spec TxnSpec) (int64, error) {
	if len(spec.Entries) < 2 {
		return 0, fmt.Errorf("%w: a transaction needs at least two entries", ErrUnbalanced)
	}
	var sum int64
	for _, entry := range spec.Entries {
		if entry.AmountSat == 0 {
			return 0, fmt.Errorf("%w: zero-value entry on %s", ErrUnbalanced, entry.Account)
		}
		if entry.Account == AccountUserCash && entry.UserID == 0 {
			return 0, fmt.Errorf("%w: user_cash entry without a user", ErrUnbalanced)
		}
		sum += entry.AmountSat
	}
	if sum != 0 {
		return 0, fmt.Errorf("%w: entries sum to %d, not 0", ErrUnbalanced, sum)
	}

	id, err := tx.InsertID(ctx,
		`INSERT INTO ledger_txns (kind, at, memo, ref_type, ref_id, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		spec.Kind, at, spec.Memo, spec.RefType, nullInt64(spec.RefID), nullInt64(spec.CreatedBy))
	if err != nil {
		return 0, fmt.Errorf("store: insert ledger txn: %w", err)
	}
	txnID := id
	if err != nil {
		return 0, fmt.Errorf("store: ledger txn id: %w", err)
	}

	for _, entry := range spec.Entries {
		userID := entry.UserID
		if entry.Account != AccountUserCash {
			// Non-user accounts stay unattributed; carrying a stray user id
			// would corrupt the per-user balance sum.
			userID = 0
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (txn_id, account, user_id, amount_sat, at)
			 VALUES (?, ?, ?, ?, ?)`,
			txnID, entry.Account, nullInt64(userID), entry.AmountSat, at); err != nil {
			return 0, fmt.Errorf("store: insert ledger entry: %w", err)
		}
	}
	return txnID, nil
}

// BalanceSatTx returns a customer's withdrawable balance inside a transaction.
// The balance is a sum over the ledger, never a stored column, so it cannot
// drift away from the entries that explain it.
func BalanceSatTx(ctx context.Context, tx *Tx, userID int64) (int64, error) {
	var balance sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat), 0) FROM ledger_entries
		 WHERE account = ? AND user_id = ?`, AccountUserCash, userID).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("store: balance for user %d: %w", userID, err)
	}
	return derefInt64(balance), nil
}

// BalanceSat returns a customer's withdrawable balance.
func (s *Store) BalanceSat(ctx context.Context, userID int64) (int64, error) {
	var balance sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat), 0) FROM ledger_entries
		 WHERE account = ? AND user_id = ?`, AccountUserCash, userID).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("store: balance for user %d: %w", userID, err)
	}
	return derefInt64(balance), nil
}

// AccountBalanceSat returns the balance of one of the book's own accounts.
func (s *Store) AccountBalanceSat(ctx context.Context, account string) (int64, error) {
	var balance sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat), 0) FROM ledger_entries WHERE account = ?`,
		account).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("store: balance for %s: %w", account, err)
	}
	return derefInt64(balance), nil
}

// LedgerTotal is the sum of every entry in the ledger. It must always be zero.
func (s *Store) LedgerTotal(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_sat), 0) FROM ledger_entries`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: ledger total: %w", err)
	}
	return derefInt64(total), nil
}

// LedgerProblem describes a broken ledger invariant.
type LedgerProblem struct {
	Kind   string
	UserID int64
	TxnID  int64
	Amount int64
}

func (p LedgerProblem) String() string {
	switch p.Kind {
	case "unbalanced_txn":
		return fmt.Sprintf("transaction %d is out by %d sat", p.TxnID, p.Amount)
	case "negative_balance":
		return fmt.Sprintf("user %d has a negative balance of %d sat", p.UserID, p.Amount)
	case "ledger_total":
		return fmt.Sprintf("the whole ledger is out by %d sat", p.Amount)
	default:
		return fmt.Sprintf("%s: %d", p.Kind, p.Amount)
	}
}

// CheckLedger verifies the invariants that must hold at all times: the ledger
// sums to zero, every individual transaction sums to zero, and no customer
// balance has gone negative. It is cheap enough to run on a schedule and is
// exercised by the test suite after every money-moving operation.
func (s *Store) CheckLedger(ctx context.Context) ([]LedgerProblem, error) {
	var problems []LedgerProblem

	total, err := s.LedgerTotal(ctx)
	if err != nil {
		return nil, err
	}
	if total != 0 {
		problems = append(problems, LedgerProblem{Kind: "ledger_total", Amount: total})
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT txn_id, SUM(amount_sat) FROM ledger_entries
		 GROUP BY txn_id HAVING SUM(amount_sat) <> 0`)
	if err != nil {
		return nil, fmt.Errorf("store: check transactions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var problem LedgerProblem
		problem.Kind = "unbalanced_txn"
		if err := rows.Scan(&problem.TxnID, &problem.Amount); err != nil {
			return nil, err
		}
		problems = append(problems, problem)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// HAVING repeats the aggregate rather than referring to the select alias:
	// PostgreSQL does not resolve output column names in HAVING, even though
	// SQLite happily does.
	negatives, err := s.db.QueryContext(ctx,
		`SELECT user_id, SUM(amount_sat) FROM ledger_entries
		 WHERE account = ? AND user_id IS NOT NULL
		 GROUP BY user_id HAVING SUM(amount_sat) < 0`, AccountUserCash)
	if err != nil {
		return nil, fmt.Errorf("store: check balances: %w", err)
	}
	defer negatives.Close()
	for negatives.Next() {
		var problem LedgerProblem
		problem.Kind = "negative_balance"
		if err := negatives.Scan(&problem.UserID, &problem.Amount); err != nil {
			return nil, err
		}
		problems = append(problems, problem)
	}
	return problems, negatives.Err()
}

// LedgerHistory returns a customer's money movements, newest first.
func (s *Store) LedgerHistory(ctx context.Context, userID int64, limit int) ([]LedgerTxn, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.kind, t.at, t.memo, t.ref_type, COALESCE(t.ref_id, 0), e.amount_sat
		 FROM ledger_entries e
		 JOIN ledger_txns t ON t.id = e.txn_id
		 WHERE e.account = ? AND e.user_id = ?
		 ORDER BY e.id DESC LIMIT ?`, AccountUserCash, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: ledger history: %w", err)
	}
	defer rows.Close()

	var out []LedgerTxn
	for rows.Next() {
		var txn LedgerTxn
		var at string
		var amount int64
		if err := rows.Scan(&txn.ID, &txn.Kind, &at, &txn.Memo, &txn.RefType, &txn.RefID, &amount); err != nil {
			return nil, err
		}
		if txn.At, err = ParseTimestamp(at); err != nil {
			return nil, err
		}
		txn.Entries = []LedgerEntry{{
			TxnID: txn.ID, Account: AccountUserCash, UserID: userID,
			AmountSat: amount, At: txn.At,
		}}
		out = append(out, txn)
	}
	return out, rows.Err()
}

// SumUserEntriesSince totals a customer's ledger movement of the given
// transaction kinds since a moment. Used for deposit and loss limits.
func (s *Store) SumUserEntriesSince(ctx context.Context, userID int64, since string, kinds ...string) (int64, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	query := `SELECT COALESCE(SUM(e.amount_sat), 0) FROM ledger_entries e
	          JOIN ledger_txns t ON t.id = e.txn_id
	          WHERE e.account = ? AND e.user_id = ? AND e.at >= ? AND t.kind IN (`
	args := []any{AccountUserCash, userID, since}
	for i, kind := range kinds {
		if i > 0 {
			query += ", "
		}
		query += "?"
		args = append(args, kind)
	}
	query += ")"

	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: sum entries: %w", err)
	}
	return derefInt64(total), nil
}
