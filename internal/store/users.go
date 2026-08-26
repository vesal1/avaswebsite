package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NormaliseEmail folds an address to the canonical form the database stores.
//
// Email addresses are case-insensitive in practice, so they are lower-cased
// once here rather than relying on a collation. SQLite could do it with
// COLLATE NOCASE and PostgreSQL could do it with a functional index, but
// having the two databases enforce identity by different mechanisms is how
// they end up disagreeing about whether two accounts are the same person.
func NormaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// NewUser is the data required to open an account.
type NewUser struct {
	Email        string
	PasswordHash string
	DisplayName  string
	DateOfBirth  string
	Country      string
	Role         string
}

// CreateUser opens an account and returns its id.
func (s *Store) CreateUser(ctx context.Context, in NewUser) (int64, error) {
	role := in.Role
	if role == "" {
		role = RoleCustomer
	}
	id, err := s.db.InsertID(ctx,
		`INSERT INTO users (email, password_hash, display_name, date_of_birth, country, role, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		NormaliseEmail(in.Email), in.PasswordHash, in.DisplayName, in.DateOfBirth,
		strings.ToUpper(in.Country), role, Timestamp(s.Now()))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: an account with that email already exists", ErrConflict)
		}
		return 0, fmt.Errorf("store: create user: %w", err)
	}
	return id, nil
}

const userColumns = `id, email, password_hash, display_name, date_of_birth, country,
                     role, status, kyc_status, odds_format, created_at, COALESCE(last_login_at, '')`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var createdAt, lastLogin string
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.DateOfBirth,
		&u.Country, &u.Role, &u.Status, &u.KYCStatus, &u.OddsFormat, &createdAt, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: scan user: %w", err)
	}
	if u.CreatedAt, err = ParseTimestamp(createdAt); err != nil {
		return User{}, err
	}
	if u.LastLoginAt, err = ParseTimestamp(lastLogin); err != nil {
		return User{}, err
	}
	return u, nil
}

// GetUser loads an account by id.
func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// GetUserByEmail loads an account by email, case-insensitively.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = ?`, NormaliseEmail(email)))
}

// ListUsers returns accounts for the back office, newest first.
func (s *Store) ListUsers(ctx context.Context, limit int) ([]User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
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

// TouchLogin records a successful sign-in.
func (s *Store) TouchLogin(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = ? WHERE id = ?`, Timestamp(s.Now()), userID)
	return err
}

// SetUserStatus suspends, closes or reactivates an account.
func (s *Store) SetUserStatus(ctx context.Context, userID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET status = ? WHERE id = ?`, status, userID)
	return err
}

// SetKYCStatus records the outcome of an identity check.
func (s *Store) SetKYCStatus(ctx context.Context, userID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET kyc_status = ? WHERE id = ?`, status, userID)
	return err
}

// SetOddsFormat stores the customer's preferred price display.
func (s *Store) SetOddsFormat(ctx context.Context, userID int64, format string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET odds_format = ? WHERE id = ?`, format, userID)
	return err
}

// SetPasswordHash replaces a stored credential.
func (s *Store) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

// SetRole changes an account's role. Staff roles are only ever granted here or
// by the bootstrap admin command, never through a customer-facing form.
func (s *Store) SetRole(ctx context.Context, userID int64, role string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, userID)
	return err
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSession stores a new browser session.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at,
		                       reality_check_at, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.UserID, Timestamp(sess.CreatedAt), Timestamp(sess.ExpiresAt),
		Timestamp(sess.LastSeenAt), Timestamp(sess.RealityCheckAt), sess.IP, sess.UserAgent)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// GetSession loads a session by the hash of its cookie value.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	var created, expires, lastSeen, reality string
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, expires_at, last_seen_at, reality_check_at, ip, user_agent
		 FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&sess.TokenHash, &sess.UserID, &created, &expires, &lastSeen, &reality, &sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: get session: %w", err)
	}
	for _, pair := range []struct {
		raw string
		dst *time.Time
	}{{created, &sess.CreatedAt}, {expires, &sess.ExpiresAt},
		{lastSeen, &sess.LastSeenAt}, {reality, &sess.RealityCheckAt}} {
		parsed, err := ParseTimestamp(pair.raw)
		if err != nil {
			return Session{}, err
		}
		*pair.dst = parsed
	}
	return sess, nil
}

// TouchSession records activity and, when the reality check has been shown,
// schedules the next one.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, lastSeen, nextRealityCheck time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, reality_check_at = ? WHERE token_hash = ?`,
		Timestamp(lastSeen), Timestamp(nextRealityCheck), tokenHash)
	return err
}

// DeleteSession signs one browser out.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions signs a customer out everywhere. Used when an account is
// suspended or self-excluded: leaving a live session open would let a
// self-excluded customer keep playing until their cookie expired.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions removes sessions that have timed out.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, Timestamp(s.Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Login throttling
// ---------------------------------------------------------------------------

// RecordLoginAttempt appends to the throttling log.
func (s *Store) RecordLoginAttempt(ctx context.Context, email, ip string, succeeded bool) error {
	flag := 0
	if succeeded {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO login_attempts (email, ip, succeeded, at) VALUES (?, ?, ?, ?)`,
		NormaliseEmail(email), ip, flag, Timestamp(s.Now()))
	return err
}

// FailedLoginsSince counts recent failures for an email address.
func (s *Store) FailedLoginsSince(ctx context.Context, email string, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM login_attempts WHERE email = ? AND succeeded = 0 AND at >= ?`,
		NormaliseEmail(email), Timestamp(since)).Scan(&count)
	return count, err
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// Audit appends to the append-only action log. Audit failures are reported but
// must never block the action being audited from having already happened.
func (s *Store) Audit(ctx context.Context, entry AuditEntry) error {
	at := entry.At
	if at.IsZero() {
		at = s.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor_user_id, subject_user_id, action, detail, ip)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		Timestamp(at), nullInt64(entry.ActorUserID), nullInt64(entry.SubjectUserID),
		entry.Action, entry.Detail, entry.IP)
	return err
}

// AuditForUser returns the audit trail for one account, newest first.
func (s *Store) AuditForUser(ctx context.Context, userID int64, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, COALESCE(actor_user_id, 0), COALESCE(subject_user_id, 0), action, detail, ip
		 FROM audit_log WHERE subject_user_id = ? OR actor_user_id = ?
		 ORDER BY id DESC LIMIT ?`, userID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: audit for user: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var entry AuditEntry
		var at string
		if err := rows.Scan(&entry.ID, &at, &entry.ActorUserID, &entry.SubjectUserID,
			&entry.Action, &entry.Detail, &entry.IP); err != nil {
			return nil, err
		}
		if entry.At, err = ParseTimestamp(at); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}
