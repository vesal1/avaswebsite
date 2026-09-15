package compliance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/store"
)

func newService(t *testing.T, configure func(*config.Config)) (*Service, *store.Store, *time.Time) {
	t.Helper()
	s, err := store.OpenAndMigrate(filepath.Join(t.TempDir(), "compliance.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s.WithClock(func() time.Time { return now })

	cfg := config.Load()
	cfg.AllowedCountries = []string{"GB", "IE"}
	cfg.BlockedCountries = nil
	cfg.MinimumAge = 18
	cfg.KYCRequiredToBet = true
	if configure != nil {
		configure(cfg)
	}
	return New(s, cfg).WithClock(func() time.Time { return now }), s, &now
}

func makeUser(t *testing.T, s *store.Store, country string) store.User {
	t.Helper()
	ctx := context.Background()
	id, err := s.CreateUser(ctx, store.NewUser{
		Email: "punter@example.com", PasswordHash: "x", DisplayName: "Punter",
		DateOfBirth: "1990-01-01", Country: country,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKYCStatus(ctx, id, store.KYCVerified); err != nil {
		t.Fatal(err)
	}
	user, err := s.GetUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func TestCountryAllowlistFailsClosed(t *testing.T) {
	// The single most important default in the whole service: with nothing
	// configured, nobody is served. An allowlist that defaults to "everyone"
	// is how an unlicensed jurisdiction gets served by accident.
	svc, _, _ := newService(t, func(cfg *config.Config) { cfg.AllowedCountries = nil })
	for _, country := range []string{"GB", "US", "FR", ""} {
		if err := svc.CountryAllowed(country); err == nil {
			t.Errorf("CountryAllowed(%q) allowed play with no licensed jurisdictions", country)
		}
	}
}

func TestCountryAllowlist(t *testing.T) {
	svc, _, _ := newService(t, nil)
	for _, country := range []string{"GB", "IE", "gb"} {
		if err := svc.CountryAllowed(country); err != nil {
			t.Errorf("CountryAllowed(%q) = %v, want nil", country, err)
		}
	}
	for _, country := range []string{"US", "FR", "", "XX"} {
		err := svc.CountryAllowed(country)
		refusal, ok := AsRefusal(err)
		if !ok || refusal.Code != CodeGeoBlocked {
			t.Errorf("CountryAllowed(%q) = %v, want a geo refusal", country, err)
		}
	}
}

func TestBlocklistBeatsAllowlist(t *testing.T) {
	// A country can be licensed in general and blocked for a specific reason,
	// such as a regulator instruction. The block has to win.
	svc, _, _ := newService(t, func(cfg *config.Config) {
		cfg.AllowedCountries = []string{"GB", "IE"}
		cfg.BlockedCountries = []string{"IE"}
	})
	if err := svc.CountryAllowed("GB"); err != nil {
		t.Errorf("GB should be allowed: %v", err)
	}
	if err := svc.CountryAllowed("IE"); err == nil {
		t.Error("IE is on the block list and must be refused")
	}
}

func TestRegistrationAge(t *testing.T) {
	svc, _, now := newService(t, nil)

	// Exactly eighteen today.
	eighteenth := now.AddDate(-18, 0, 0).Format("2006-01-02")
	if err := svc.CheckRegistration("GB", eighteenth); err != nil {
		t.Errorf("someone turning 18 today was refused: %v", err)
	}
	// One day short.
	tomorrow := now.AddDate(-18, 0, 1).Format("2006-01-02")
	refusal, ok := AsRefusal(svc.CheckRegistration("GB", tomorrow))
	if !ok || refusal.Code != CodeUnderage {
		t.Errorf("a 17-year-old was not refused, got %v", refusal)
	}
	// Nonsense dates.
	for _, dob := range []string{"", "not-a-date", "01/01/1990", "2200-01-01"} {
		if err := svc.CheckRegistration("GB", dob); err == nil {
			t.Errorf("CheckRegistration with dob %q was accepted", dob)
		}
	}
}

func TestSelfExclusionBlocksPlayAndEndsSessions(t *testing.T) {
	svc, s, now := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	if err := s.CreateSession(ctx, store.Session{
		TokenHash: "abc", UserID: user.ID, CreatedAt: *now,
		ExpiresAt: now.Add(time.Hour), LastSeenAt: *now, RealityCheckAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.SelfExclude(ctx, user.ID, 0, "stopping"); err != nil {
		t.Fatal(err)
	}

	// The live session must be gone: an exclusion that leaves a session open
	// lets the customer keep playing until their cookie expires.
	if _, err := s.GetSession(ctx, "abc"); err == nil {
		t.Error("self-exclusion left a live session behind")
	}

	refusal, ok := AsRefusal(svc.CheckAccountUsable(ctx, user))
	if !ok || refusal.Code != CodeSelfExcluded {
		t.Errorf("got %v, want a self-exclusion refusal", refusal)
	}
}

func TestCoolOffExpiresButSelfExclusionDoesNot(t *testing.T) {
	svc, s, now := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	if err := svc.TakeABreak(ctx, user.ID, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckAccountUsable(ctx, user); err == nil {
		t.Error("a cool-off should block play")
	}

	*now = now.Add(48 * time.Hour)
	if err := svc.CheckAccountUsable(ctx, user); err != nil {
		t.Errorf("the cool-off should have expired: %v", err)
	}
}

func TestLimitLooseningWaitsOutTheCoolingOff(t *testing.T) {
	svc, s, now := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	// A tightening binds immediately.
	effective, err := svc.SetLimit(ctx, user.ID, store.LimitDepositDaily, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if effective.After(*now) {
		t.Error("the first limit should apply at once")
	}

	// A loosening is dated forward.
	effective, err = svc.SetLimit(ctx, user.ID, store.LimitDepositDaily, 5_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Equal(now.Add(LimitCoolOff)) {
		t.Errorf("increase effective from %v, want %v", effective, now.Add(LimitCoolOff))
	}

	// Until it lands, the tighter limit still bites.
	if err := svc.CheckDeposit(ctx, user, 1_000_000); err == nil {
		t.Error("a deposit past the current limit was allowed during the cooling-off period")
	}

	*now = now.Add(LimitCoolOff + time.Minute)
	if err := svc.CheckDeposit(ctx, user, 1_000_000); err != nil {
		t.Errorf("the raised limit should now apply: %v", err)
	}
}

func TestTighteningALimitAppliesAtOnce(t *testing.T) {
	svc, s, _ := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	if _, err := svc.SetLimit(ctx, user.ID, store.LimitDepositDaily, 5_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetLimit(ctx, user.ID, store.LimitDepositDaily, 100_000); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckDeposit(ctx, user, 1_000_000); err == nil {
		t.Error("the tightened limit should bind immediately")
	}
}

func TestBettingRequiresVerifiedIdentity(t *testing.T) {
	svc, s, _ := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	if err := s.SetKYCStatus(ctx, user.ID, store.KYCPending); err != nil {
		t.Fatal(err)
	}
	user, _ = s.GetUser(ctx, user.ID)

	refusal, ok := AsRefusal(svc.CheckCanBet(ctx, user, 100_000))
	if !ok || refusal.Code != CodeKYCRequired {
		t.Errorf("got %v, want a KYC refusal", refusal)
	}
}

func TestWithdrawalStaysOpenToSelfExcludedCustomers(t *testing.T) {
	// Trapping a self-excluded customer's money would give them a reason not
	// to exclude themselves.
	svc, s, _ := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	if err := svc.SelfExclude(ctx, user.ID, 0, "stopping"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckWithdrawal(ctx, user, 1_000_000); err != nil {
		t.Errorf("a self-excluded customer must still be able to withdraw: %v", err)
	}
}

func TestLargeWithdrawalNeedsReview(t *testing.T) {
	svc, s, _ := newService(t, func(cfg *config.Config) {
		cfg.ManualReviewSat = 10_000_000
		cfg.AMLReportingSat = 10_000_000
	})
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	small, err := svc.CheckWithdrawal(ctx, user, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if small.NeedsReview {
		t.Error("a small withdrawal should not need a human")
	}

	large, err := svc.CheckWithdrawal(ctx, user, 50_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !large.NeedsReview {
		t.Error("a withdrawal over the threshold must go to review")
	}
	if len(large.Flags) == 0 {
		t.Error("a reportable withdrawal should raise a flag")
	}
}

func TestCriticalFlagBlocksWithdrawal(t *testing.T) {
	svc, s, _ := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	if _, err := s.RaiseFlag(ctx, store.ComplianceFlag{
		UserID: user.ID, Kind: "sanctions_hit", Severity: "critical",
	}); err != nil {
		t.Fatal(err)
	}
	refusal, ok := AsRefusal(func() error { _, err := svc.CheckWithdrawal(ctx, user, 1_000_000); return err }())
	if !ok || refusal.Code != CodeFlagged {
		t.Errorf("got %v, want a flagged refusal", refusal)
	}
}

func TestStakeLimitCountsTodaysBets(t *testing.T) {
	svc, s, _ := newService(t, nil)
	ctx := context.Background()
	user := makeUser(t, s, "GB")

	if _, err := svc.SetLimit(ctx, user.ID, store.LimitStakeDaily, 100_000); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckCanBet(ctx, user, 50_000); err != nil {
		t.Errorf("a bet inside the limit was refused: %v", err)
	}
	refusal, ok := AsRefusal(svc.CheckCanBet(ctx, user, 150_000))
	if !ok || refusal.Code != CodeLimitReached {
		t.Errorf("got %v, want a limit refusal", refusal)
	}
}

func TestRealityCheckSchedule(t *testing.T) {
	svc, _, now := newService(t, func(cfg *config.Config) { cfg.RealityCheckMin = 60 })

	session := store.Session{RealityCheckAt: now.Add(30 * time.Minute)}
	if svc.RealityCheckDue(session) {
		t.Error("a check due in 30 minutes is not due yet")
	}
	*now = now.Add(45 * time.Minute)
	if !svc.RealityCheckDue(session) {
		t.Error("the check should now be due")
	}
	if next := svc.NextRealityCheck(); !next.Equal(now.Add(time.Hour)) {
		t.Errorf("next check at %v, want %v", next, now.Add(time.Hour))
	}
}
