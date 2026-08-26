package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if err := VerifyPassword("correct horse battery staple", hash); err != nil {
		t.Errorf("the correct password was rejected: %v", err)
	}
	if err := VerifyPassword("Correct horse battery staple", hash); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("a wrong password gave %v, want ErrBadCredentials", err)
	}
}

func TestEachHashUsesAFreshSalt(t *testing.T) {
	first, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two hashes of the same password are identical: the salt is not random")
	}
}

func TestWeakPasswordsAreRejected(t *testing.T) {
	for _, password := range []string{
		"", "short", "password1234", "aaaaaaaaaaaaaaa", "qwertyqwerty",
	} {
		if _, err := HashPassword(password); !errors.Is(err, ErrWeakPassword) {
			t.Errorf("HashPassword(%q) = %v, want ErrWeakPassword", password, err)
		}
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for _, hash := range []string{"", "not-a-hash", "$argon2id$broken", "$bcrypt$v=19$m=1,t=1,p=1$aa$bb"} {
		if err := VerifyPassword("correct horse battery staple", hash); !errors.Is(err, ErrBadHash) {
			t.Errorf("VerifyPassword against %q = %v, want ErrBadHash", hash, err)
		}
	}
}

func TestTokensAreUniqueAndHashed(t *testing.T) {
	first, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two session tokens collided")
	}
	if HashToken(first) == first {
		t.Error("the stored token hash must not equal the cookie value")
	}
	if HashToken(first) != HashToken(first) {
		t.Error("token hashing is not deterministic")
	}
}

func TestCSRFTokenBindsToTheSession(t *testing.T) {
	const secret = "server-secret"
	sessionA, _ := NewToken()
	sessionB, _ := NewToken()

	tokenA := CSRFToken(secret, sessionA)
	if !CheckCSRF(secret, sessionA, tokenA) {
		t.Error("a session's own CSRF token was rejected")
	}
	// A token minted for one session must not work in another, which is what
	// stops an attacker reusing a token harvested from their own account.
	if CheckCSRF(secret, sessionB, tokenA) {
		t.Error("a CSRF token from another session was accepted")
	}
	if CheckCSRF("other-secret", sessionA, tokenA) {
		t.Error("a CSRF token survived a change of server secret")
	}
	if CheckCSRF(secret, sessionA, "") {
		t.Error("an empty CSRF token was accepted")
	}
}

func TestLockout(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if d := LockoutRemaining(3, now.Add(-time.Minute), now); d != 0 {
		t.Errorf("a few failures should not lock out, got %v", d)
	}
	if d := LockoutRemaining(MaxFailedAttempts, now.Add(-time.Minute), now); d <= 0 {
		t.Error("repeated failures should lock the account")
	}
	if d := LockoutRemaining(MaxFailedAttempts, now.Add(-ThrottleWindow-time.Minute), now); d != 0 {
		t.Errorf("the lockout should expire with the window, got %v", d)
	}
}

func TestAgeAt(t *testing.T) {
	dob, err := ParseDateOfBirth("2008-06-15")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   string
		want int
	}{
		{"2026-06-14", 17}, // the day before the birthday
		{"2026-06-15", 18}, // the birthday itself
		{"2026-06-16", 18},
		{"2027-01-01", 18},
	}
	for _, tc := range cases {
		at, _ := time.Parse("2006-01-02", tc.at)
		if got := AgeAt(dob, at); got != tc.want {
			t.Errorf("AgeAt(%s) = %d, want %d", tc.at, got, tc.want)
		}
	}
	if _, err := ParseDateOfBirth("15/06/2008"); err == nil {
		t.Error("a non-ISO date should be rejected")
	}
}
