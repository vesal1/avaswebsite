// Package auth handles credentials, sessions and CSRF tokens.
//
// Passwords are hashed with Argon2id. Session cookies carry a random token
// whose SHA-256 hash is what the database stores, so a leaked database dump
// cannot be replayed as a live session.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are the interactive-use figures from the Argon2
// RFC, raised to 64 MiB of memory: expensive enough to make offline cracking
// of a stolen hash costly, cheap enough to run on every login.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	saltLen             = 16
)

// Errors callers branch on.
var (
	ErrBadCredentials = errors.New("auth: email or password is incorrect")
	ErrWeakPassword   = errors.New("auth: password is too weak")
	ErrBadHash        = errors.New("auth: stored password hash is unreadable")
)

// MinPasswordLength is the shortest password accepted. Length is the only
// requirement: composition rules push people toward predictable patterns.
const MinPasswordLength = 12

// HashPassword derives an Argon2id hash in the standard encoded form, which
// carries its own parameters so they can be raised later without invalidating
// existing credentials.
func HashPassword(password string) (string, error) {
	if err := CheckPasswordStrength(password); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against an encoded hash in constant time.
func VerifyPassword(password, encoded string) error {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrBadCredentials
	}
	return nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, ErrBadHash
	}
	var params argonParams
	var threads int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &threads); err != nil {
		return argonParams{}, nil, nil, ErrBadHash
	}
	params.threads = uint8(threads)
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, ErrBadHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, ErrBadHash
	}
	return params, salt, key, nil
}

// CheckPasswordStrength rejects passwords that are too short or obviously
// guessable.
func CheckPasswordStrength(password string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, MinPasswordLength)
	}
	if len(password) > 1024 {
		// Argon2 cost is bounded by the parameters, not the input, but an
		// unbounded field is still a needless thing to accept.
		return fmt.Errorf("%w: password is too long", ErrWeakPassword)
	}
	// A banned word only matters when it is most of the password: "football"
	// and "football1234" are both guessable, while a long passphrase that
	// happens to mention football is not.
	lower := strings.ToLower(password)
	for _, banned := range commonPasswords {
		if !strings.Contains(lower, banned) {
			continue
		}
		if len(strings.Replace(lower, banned, "", 1)) < MinPasswordLength-4 {
			return fmt.Errorf("%w: that password is too common", ErrWeakPassword)
		}
	}
	if isSingleRepeatedRune(password) {
		return fmt.Errorf("%w: use more than one character", ErrWeakPassword)
	}
	return nil
}

// commonPasswords is a short deny list of the patterns that dominate breach
// corpora. It is not a substitute for a real breached-password check, which
// belongs behind an external service before launch.
var commonPasswords = []string{
	"password", "123456789", "qwerty", "letmein", "welcome", "admin",
	"iloveyou", "monkey", "dragon", "football", "baseball", "sunshine",
	"princess", "changeme", "passw0rd", "trustno1",
}

func isSingleRepeatedRune(s string) bool {
	if s == "" {
		return true
	}
	first := rune(s[0])
	for _, r := range s {
		if r != first {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Session tokens
// ---------------------------------------------------------------------------

// NewToken returns a fresh 256-bit random token, URL-safe, for use as a
// session cookie value.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken is the one-way function applied before a token is stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// CSRFToken derives a per-session CSRF token from the session token and a
// server secret. It needs no storage of its own and is stable for the life of
// the session, so it can be embedded in every form the session renders.
func CSRFToken(secret, sessionToken string) string {
	sum := sha256.Sum256([]byte("avas-csrf|" + secret + "|" + sessionToken))
	return hex.EncodeToString(sum[:])
}

// CheckCSRF compares a submitted token against the expected one in constant
// time.
func CheckCSRF(secret, sessionToken, submitted string) bool {
	if submitted == "" {
		return false
	}
	want := CSRFToken(secret, sessionToken)
	return subtle.ConstantTimeCompare([]byte(want), []byte(submitted)) == 1
}

// ---------------------------------------------------------------------------
// Login throttling
// ---------------------------------------------------------------------------

// ThrottleWindow is how far back failed attempts are counted.
const ThrottleWindow = 15 * time.Minute

// MaxFailedAttempts is how many failures are tolerated in the window before
// sign-in is refused.
const MaxFailedAttempts = 8

// LockoutRemaining reports how long a locked-out account must wait. A zero
// duration means sign-in may proceed.
func LockoutRemaining(failures int, lastFailure, now time.Time) time.Duration {
	if failures < MaxFailedAttempts || lastFailure.IsZero() {
		return 0
	}
	unlockAt := lastFailure.Add(ThrottleWindow)
	if now.After(unlockAt) {
		return 0
	}
	return unlockAt.Sub(now)
}

// ---------------------------------------------------------------------------
// Age
// ---------------------------------------------------------------------------

// ParseDateOfBirth reads a YYYY-MM-DD date.
func ParseDateOfBirth(value string) (time.Time, error) {
	dob, err := time.Parse("2006-01-02", strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("auth: %q is not a date in YYYY-MM-DD form", value)
	}
	return dob, nil
}

// AgeAt returns a person's age in whole years at a moment.
//
// The comparison is on calendar month and day rather than day-of-year: a leap
// day between birth and now shifts day-of-year by one, which would report
// someone as 17 on their eighteenth birthday and refuse a lawful registration.
func AgeAt(dob, at time.Time) int {
	dob, at = dob.UTC(), at.UTC()
	years := at.Year() - dob.Year()
	birthMonth, birthDay := dob.Month(), dob.Day()
	if at.Month() < birthMonth || (at.Month() == birthMonth && at.Day() < birthDay) {
		years--
	}
	if years < 0 {
		return 0
	}
	return years
}

// FormatDuration renders a lockout or cool-off remaining time for a customer.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0 minutes"
	}
	if d < time.Minute {
		return "less than a minute"
	}
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + " minutes"
	}
	if d < 24*time.Hour {
		return strconv.Itoa(int(d.Hours())) + " hours"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days"
}
