package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Player limits
// ---------------------------------------------------------------------------

// SetPlayerLimit records a limit. A tightening takes effect immediately; a
// loosening is written with a future effective_from so the old, tighter limit
// keeps applying until the cooling-off period has passed.
func (s *Store) SetPlayerLimit(ctx context.Context, limit PlayerLimit) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO player_limits (user_id, kind, amount, effective_from, requested_at)
		 VALUES (?, ?, ?, ?, ?)`,
		limit.UserID, limit.Kind, limit.Amount,
		Timestamp(limit.EffectiveFrom), Timestamp(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: set player limit: %w", err)
	}
	return res.LastInsertId()
}

// EffectiveLimit returns the limit of a kind that binds right now: the most
// recently effective one among those already in force. Returns ok=false when
// the customer has set no limit of that kind.
//
// Deliberately the newest in force rather than the smallest. A tightening is
// written effective immediately and so becomes the newest at once; a
// loosening is written with a future effective_from, so the older tighter
// limit stays newest-in-force until the cooling-off period elapses. Taking
// the minimum instead would mean a customer could never raise a limit again,
// which is not what a cooling-off period is.
func (s *Store) EffectiveLimit(ctx context.Context, userID int64, kind string, now time.Time) (int64, bool, error) {
	var amount sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT amount FROM player_limits
		 WHERE user_id = ? AND kind = ? AND effective_from <= ? AND revoked_at IS NULL
		 ORDER BY effective_from DESC, id DESC LIMIT 1`,
		userID, kind, Timestamp(now)).Scan(&amount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: effective limit: %w", err)
	}
	if !amount.Valid {
		return 0, false, nil
	}
	return amount.Int64, true, nil
}

// ListPlayerLimits returns a customer's limits, including pending increases so
// the account page can show what changes and when.
func (s *Store) ListPlayerLimits(ctx context.Context, userID int64) ([]PlayerLimit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, kind, amount, effective_from, requested_at, COALESCE(revoked_at, '')
		 FROM player_limits WHERE user_id = ? AND revoked_at IS NULL
		 ORDER BY kind, effective_from`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list limits: %w", err)
	}
	defer rows.Close()
	var out []PlayerLimit
	for rows.Next() {
		var limit PlayerLimit
		var effective, requested, revoked string
		if err := rows.Scan(&limit.ID, &limit.UserID, &limit.Kind, &limit.Amount,
			&effective, &requested, &revoked); err != nil {
			return nil, err
		}
		if limit.EffectiveFrom, err = ParseTimestamp(effective); err != nil {
			return nil, err
		}
		if limit.RequestedAt, err = ParseTimestamp(requested); err != nil {
			return nil, err
		}
		if limit.RevokedAt, err = ParseTimestamp(revoked); err != nil {
			return nil, err
		}
		out = append(out, limit)
	}
	return out, rows.Err()
}

// RevokePendingLimitIncrease cancels a not-yet-effective loosening. A customer
// changing their mind before it lands must be able to take it back.
func (s *Store) RevokePendingLimitIncrease(ctx context.Context, userID, limitID int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE player_limits SET revoked_at = ?
		 WHERE id = ? AND user_id = ? AND effective_from > ? AND revoked_at IS NULL`,
		Timestamp(now), limitID, userID, Timestamp(now))
	if err != nil {
		return fmt.Errorf("store: revoke limit: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: limit %d is not a pending increase", ErrConflict, limitID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Exclusions
// ---------------------------------------------------------------------------

// CreateExclusion records a cool-off or self-exclusion.
func (s *Store) CreateExclusion(ctx context.Context, ex Exclusion) (int64, error) {
	var endsAt any
	if !ex.Permanent && !ex.EndsAt.IsZero() {
		endsAt = Timestamp(ex.EndsAt)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO exclusions (user_id, kind, starts_at, ends_at, created_at, reason)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ex.UserID, ex.Kind, Timestamp(ex.StartsAt), endsAt, Timestamp(s.Now()), ex.Reason)
	if err != nil {
		return 0, fmt.Errorf("store: create exclusion: %w", err)
	}
	return res.LastInsertId()
}

// ActiveExclusion returns the exclusion currently barring play, if any.
func (s *Store) ActiveExclusion(ctx context.Context, userID int64, now time.Time) (Exclusion, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, kind, starts_at, COALESCE(ends_at, ''), created_at, reason
		 FROM exclusions WHERE user_id = ? AND starts_at <= ?
		 ORDER BY id DESC`, userID, Timestamp(now))
	if err != nil {
		return Exclusion{}, false, fmt.Errorf("store: active exclusion: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ex Exclusion
		var starts, ends, created string
		if err := rows.Scan(&ex.ID, &ex.UserID, &ex.Kind, &starts, &ends, &created, &ex.Reason); err != nil {
			return Exclusion{}, false, err
		}
		if ex.StartsAt, err = ParseTimestamp(starts); err != nil {
			return Exclusion{}, false, err
		}
		if ends == "" {
			ex.Permanent = true
		} else if ex.EndsAt, err = ParseTimestamp(ends); err != nil {
			return Exclusion{}, false, err
		}
		if ex.CreatedAt, err = ParseTimestamp(created); err != nil {
			return Exclusion{}, false, err
		}
		if ex.Active(now) {
			return ex, true, nil
		}
	}
	return Exclusion{}, false, rows.Err()
}

// ---------------------------------------------------------------------------
// Compliance flags
// ---------------------------------------------------------------------------

// RaiseFlag records something a compliance officer must look at.
func (s *Store) RaiseFlag(ctx context.Context, flag ComplianceFlag) (int64, error) {
	severity := flag.Severity
	if severity == "" {
		severity = "info"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO compliance_flags (user_id, kind, severity, detail, raised_at)
		 VALUES (?, ?, ?, ?, ?)`,
		flag.UserID, flag.Kind, severity, flag.Detail, Timestamp(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: raise flag: %w", err)
	}
	return res.LastInsertId()
}

// RaiseFlagTx raises a flag inside an open transaction, so a flag raised by a
// money movement commits or rolls back with it.
func RaiseFlagTx(ctx context.Context, tx *sql.Tx, flag ComplianceFlag, at string) error {
	severity := flag.Severity
	if severity == "" {
		severity = "info"
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO compliance_flags (user_id, kind, severity, detail, raised_at)
		 VALUES (?, ?, ?, ?, ?)`, flag.UserID, flag.Kind, severity, flag.Detail, at)
	if err != nil {
		return fmt.Errorf("store: raise flag: %w", err)
	}
	return nil
}

// OpenFlags lists unresolved compliance flags, most serious first.
func (s *Store) OpenFlags(ctx context.Context, limit int) ([]ComplianceFlag, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, kind, severity, detail, raised_at, COALESCE(resolved_at, ''),
		        COALESCE(resolved_by, 0), resolution
		 FROM compliance_flags WHERE resolved_at IS NULL
		 ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
		          raised_at LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: open flags: %w", err)
	}
	defer rows.Close()
	return scanFlags(rows)
}

// FlagsForUser lists every flag raised against one account.
func (s *Store) FlagsForUser(ctx context.Context, userID int64) ([]ComplianceFlag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, kind, severity, detail, raised_at, COALESCE(resolved_at, ''),
		        COALESCE(resolved_by, 0), resolution
		 FROM compliance_flags WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: flags for user: %w", err)
	}
	defer rows.Close()
	return scanFlags(rows)
}

func scanFlags(rows *sql.Rows) ([]ComplianceFlag, error) {
	var out []ComplianceFlag
	for rows.Next() {
		var flag ComplianceFlag
		var raised, resolved string
		if err := rows.Scan(&flag.ID, &flag.UserID, &flag.Kind, &flag.Severity, &flag.Detail,
			&raised, &resolved, &flag.ResolvedBy, &flag.Resolution); err != nil {
			return nil, err
		}
		var err error
		if flag.RaisedAt, err = ParseTimestamp(raised); err != nil {
			return nil, err
		}
		if flag.ResolvedAt, err = ParseTimestamp(resolved); err != nil {
			return nil, err
		}
		out = append(out, flag)
	}
	return out, rows.Err()
}

// ResolveFlag closes a compliance flag with a written resolution.
func (s *Store) ResolveFlag(ctx context.Context, flagID, reviewerID int64, resolution string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE compliance_flags SET resolved_at = ?, resolved_by = ?, resolution = ?
		 WHERE id = ? AND resolved_at IS NULL`,
		Timestamp(s.Now()), reviewerID, resolution, flagID)
	if err != nil {
		return fmt.Errorf("store: resolve flag: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: flag %d is already resolved", ErrConflict, flagID)
	}
	return nil
}

// HasOpenCriticalFlag reports whether an account is blocked by an unresolved
// critical flag. Withdrawals do not leave while one stands.
func (s *Store) HasOpenCriticalFlag(ctx context.Context, userID int64) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM compliance_flags
		 WHERE user_id = ? AND severity = 'critical' AND resolved_at IS NULL`, userID).Scan(&count)
	return count > 0, err
}

// ---------------------------------------------------------------------------
// KYC
// ---------------------------------------------------------------------------

// SubmitKYCDocument records a reference to a document held elsewhere.
func (s *Store) SubmitKYCDocument(ctx context.Context, doc KYCDocument) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO kyc_documents (user_id, kind, reference, submitted_at) VALUES (?, ?, ?, ?)`,
		doc.UserID, doc.Kind, doc.Reference, Timestamp(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: submit kyc document: %w", err)
	}
	return res.LastInsertId()
}

// KYCDocumentsForUser lists a customer's submitted documents.
func (s *Store) KYCDocumentsForUser(ctx context.Context, userID int64) ([]KYCDocument, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, kind, reference, status, submitted_at, COALESCE(reviewed_at, ''),
		        COALESCE(reviewed_by, 0), note
		 FROM kyc_documents WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: kyc documents: %w", err)
	}
	defer rows.Close()
	var out []KYCDocument
	for rows.Next() {
		var doc KYCDocument
		var submitted, reviewed string
		if err := rows.Scan(&doc.ID, &doc.UserID, &doc.Kind, &doc.Reference, &doc.Status,
			&submitted, &reviewed, &doc.ReviewedBy, &doc.Note); err != nil {
			return nil, err
		}
		var err error
		if doc.SubmittedAt, err = ParseTimestamp(submitted); err != nil {
			return nil, err
		}
		if doc.ReviewedAt, err = ParseTimestamp(reviewed); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// ReviewKYCDocument records an officer's decision on a document.
func (s *Store) ReviewKYCDocument(ctx context.Context, docID, reviewerID int64, status, note string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE kyc_documents SET status = ?, reviewed_at = ?, reviewed_by = ?, note = ? WHERE id = ?`,
		status, Timestamp(s.Now()), reviewerID, note, docID)
	return err
}

// PendingKYCUsers lists accounts with documents awaiting review.
func (s *Store) PendingKYCUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users
		 WHERE id IN (SELECT DISTINCT user_id FROM kyc_documents WHERE status = 'pending')
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: pending kyc users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, user)
	}
	return out, rows.Err()
}
