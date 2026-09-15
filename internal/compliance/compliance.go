// Package compliance holds the rules that decide whether a customer may
// register, deposit, bet or withdraw.
//
// These are not product preferences. Geo-restriction, age verification,
// identity checks, self-exclusion and deposit limits are conditions of holding
// a betting licence, and the AML thresholds are statutory. They are gathered
// here, ahead of the money-moving code, so that no path to a wager or a payout
// can quietly skip one.
package compliance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/auth"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Code identifies why an action was refused, so the caller can choose the
// right page to show without matching on message text.
type Code string

const (
	CodeGeoBlocked    Code = "geo_blocked"
	CodeUnderage      Code = "underage"
	CodeAccountClosed Code = "account_closed"
	CodeSuspended     Code = "suspended"
	CodeSelfExcluded  Code = "self_excluded"
	CodeCoolOff       Code = "cool_off"
	CodeKYCRequired   Code = "kyc_required"
	CodeLimitReached  Code = "limit_reached"
	CodeManualReview  Code = "manual_review"
	CodeFlagged       Code = "flagged"
	CodeInvalidInput  Code = "invalid_input"
	CodeBonusActive   Code = "bonus_active"
)

// Refusal is a compliance rejection carrying a customer-facing explanation.
// The message is written to be shown as-is: a customer refused for a
// regulatory reason is entitled to know which one.
type Refusal struct {
	Code    Code
	Message string
	// RetryAfter is set when the refusal is temporary.
	RetryAfter time.Duration
}

func (r *Refusal) Error() string { return string(r.Code) + ": " + r.Message }

func refuse(code Code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsRefusal extracts a Refusal from an error, if it is one.
func AsRefusal(err error) (*Refusal, bool) {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// Service applies the compliance rules.
type Service struct {
	store *store.Store
	cfg   *config.Config
	now   func() time.Time
}

// New builds a compliance service.
func New(s *store.Store, cfg *config.Config) *Service {
	return &Service{store: s, cfg: cfg, now: func() time.Time { return s.Now() }}
}

// WithClock replaces the service clock. Test-only.
func (s *Service) WithClock(clock func() time.Time) *Service {
	s.now = clock
	return s
}

// ---------------------------------------------------------------------------
// Jurisdiction
// ---------------------------------------------------------------------------

// CountryAllowed reports whether the book is licensed to serve a country.
//
// It fails closed: an empty allowlist permits nobody, and an unknown or absent
// country is refused rather than waved through. Serving an unlicensed
// jurisdiction is the failure that costs the licence, so the default has to be
// "no".
func (s *Service) CountryAllowed(country string) error {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" || country == "XX" {
		return refuse(CodeGeoBlocked,
			"We could not confirm which country you are betting from, so we cannot accept your bets.")
	}
	for _, blocked := range s.cfg.BlockedCountries {
		if country == blocked {
			return refuse(CodeGeoBlocked, "%s is not a jurisdiction we are licensed to accept bets from.", country)
		}
	}
	if len(s.cfg.AllowedCountries) == 0 {
		return refuse(CodeGeoBlocked,
			"This sportsbook is not currently licensed in any jurisdiction. No bets can be accepted.")
	}
	for _, allowed := range s.cfg.AllowedCountries {
		if country == allowed {
			return nil
		}
	}
	return refuse(CodeGeoBlocked, "%s is not a jurisdiction we are licensed to accept bets from.", country)
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// CheckRegistration validates a prospective customer's country and age.
func (s *Service) CheckRegistration(country, dateOfBirth string) error {
	if err := s.CountryAllowed(country); err != nil {
		return err
	}
	dob, err := auth.ParseDateOfBirth(dateOfBirth)
	if err != nil {
		return refuse(CodeInvalidInput, "Enter your date of birth as YYYY-MM-DD.")
	}
	now := s.now()
	if dob.After(now) {
		return refuse(CodeInvalidInput, "That date of birth is in the future.")
	}
	age := auth.AgeAt(dob, now)
	if age > 120 {
		return refuse(CodeInvalidInput, "Check the date of birth you entered.")
	}
	if age < s.cfg.MinimumAge {
		return refuse(CodeUnderage,
			"You must be at least %d to open an account.", s.cfg.MinimumAge)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Account state
// ---------------------------------------------------------------------------

// CheckAccountUsable rejects accounts that may not act at all: closed,
// suspended, or inside a self-exclusion or cool-off period.
//
// It is the first check in every money path, including sign-in, because a
// self-excluded customer must not simply be prevented from betting; they must
// not be able to reach a bet slip.
func (s *Service) CheckAccountUsable(ctx context.Context, user store.User) error {
	switch user.Status {
	case store.UserClosed:
		return refuse(CodeAccountClosed, "This account has been closed.")
	case store.UserSuspended:
		return refuse(CodeSuspended,
			"This account is suspended. Contact %s if you believe this is a mistake.", s.cfg.SupportMail)
	}

	exclusion, active, err := s.store.ActiveExclusion(ctx, user.ID, s.now())
	if err != nil {
		return fmt.Errorf("compliance: check exclusion: %w", err)
	}
	if !active {
		return nil
	}
	if exclusion.Kind == store.ExclusionSelfExclusion {
		if exclusion.Permanent {
			return refuse(CodeSelfExcluded,
				"You have permanently self-excluded from this sportsbook. This cannot be reversed.")
		}
		return &Refusal{
			Code: CodeSelfExcluded,
			Message: fmt.Sprintf("You are self-excluded until %s.",
				exclusion.EndsAt.Format("2 January 2006")),
			RetryAfter: exclusion.EndsAt.Sub(s.now()),
		}
	}
	return &Refusal{
		Code:       CodeCoolOff,
		Message:    fmt.Sprintf("You are taking a break until %s.", exclusion.EndsAt.Format("2 January 2006 15:04")),
		RetryAfter: exclusion.EndsAt.Sub(s.now()),
	}
}

// ---------------------------------------------------------------------------
// Betting
// ---------------------------------------------------------------------------

// CheckCanBet decides whether a customer may place a wager of a given stake.
func (s *Service) CheckCanBet(ctx context.Context, user store.User, stakeSat int64) error {
	if err := s.CheckAccountUsable(ctx, user); err != nil {
		return err
	}
	if err := s.CountryAllowed(user.Country); err != nil {
		return err
	}
	if s.cfg.KYCRequiredToBet && user.KYCStatus != store.KYCVerified {
		return refuse(CodeKYCRequired,
			"Verify your identity before placing a bet. You can start verification from your account page.")
	}

	// Daily stake limit.
	if limit, ok, err := s.store.EffectiveLimit(ctx, user.ID, store.LimitStakeDaily, s.now()); err != nil {
		return fmt.Errorf("compliance: stake limit: %w", err)
	} else if ok {
		staked, err := s.store.StakedSince(ctx, user.ID, store.Timestamp(startOfDay(s.now())))
		if err != nil {
			return fmt.Errorf("compliance: staked today: %w", err)
		}
		if staked+stakeSat > limit {
			return refuse(CodeLimitReached,
				"That bet would take you past the daily stake limit you set of %s. You have %s left today.",
				money.FormatBTC(limit)+" BTC", money.FormatBTC(max64(0, limit-staked))+" BTC")
		}
	}

	// Loss limits. A stake is treated as a potential loss in full, which is
	// the only reading that keeps the limit meaningful before the result.
	for _, window := range []struct {
		kind  string
		since time.Time
		label string
	}{
		{store.LimitLossDaily, startOfDay(s.now()), "daily"},
		{store.LimitLossWeekly, startOfWeek(s.now()), "weekly"},
	} {
		limit, ok, err := s.store.EffectiveLimit(ctx, user.ID, window.kind, s.now())
		if err != nil {
			return fmt.Errorf("compliance: loss limit: %w", err)
		}
		if !ok {
			continue
		}
		lost, err := s.store.NetLossSince(ctx, user.ID, store.Timestamp(window.since))
		if err != nil {
			return fmt.Errorf("compliance: net loss: %w", err)
		}
		if lost+stakeSat > limit {
			return refuse(CodeLimitReached,
				"That bet would take you past the %s loss limit you set of %s BTC.",
				window.label, money.FormatBTC(limit))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Deposits
// ---------------------------------------------------------------------------

// CheckDeposit decides whether an inbound deposit may be credited.
//
// A deposit that breaches a self-imposed limit is not silently kept: it is
// refused so the funds can be returned, because crediting it would defeat the
// limit the customer set.
func (s *Service) CheckDeposit(ctx context.Context, user store.User, amountSat int64) error {
	if err := s.CheckAccountUsable(ctx, user); err != nil {
		return err
	}
	if amountSat < s.cfg.MinDepositSat {
		return refuse(CodeInvalidInput,
			"The minimum deposit is %s BTC.", money.FormatBTC(s.cfg.MinDepositSat))
	}
	for _, window := range []struct {
		kind  string
		since time.Time
		label string
	}{
		{store.LimitDepositDaily, startOfDay(s.now()), "daily"},
		{store.LimitDepositWeekly, startOfWeek(s.now()), "weekly"},
		{store.LimitDepositMonthly, startOfMonth(s.now()), "monthly"},
	} {
		limit, ok, err := s.store.EffectiveLimit(ctx, user.ID, window.kind, s.now())
		if err != nil {
			return fmt.Errorf("compliance: deposit limit: %w", err)
		}
		if !ok {
			continue
		}
		deposited, err := s.store.DepositedSince(ctx, user.ID, window.since)
		if err != nil {
			return fmt.Errorf("compliance: deposited since: %w", err)
		}
		if deposited+amountSat > limit {
			return refuse(CodeLimitReached,
				"This deposit would take you past the %s deposit limit you set of %s BTC.",
				window.label, money.FormatBTC(limit))
		}
	}
	return nil
}

// ScreenDeposit raises the flags a credited deposit warrants. It never blocks
// the credit; blocking is CheckDeposit's job. Returns the flags to raise.
func (s *Service) ScreenDeposit(user store.User, amountSat int64) []store.ComplianceFlag {
	var flags []store.ComplianceFlag
	if s.cfg.AMLReportingSat > 0 && amountSat >= s.cfg.AMLReportingSat {
		flags = append(flags, store.ComplianceFlag{
			UserID:   user.ID,
			Kind:     "large_deposit",
			Severity: "warning",
			Detail: fmt.Sprintf("Deposit of %s BTC is at or above the %s BTC reporting threshold.",
				money.FormatBTC(amountSat), money.FormatBTC(s.cfg.AMLReportingSat)),
		})
	}
	if user.KYCStatus != store.KYCVerified && amountSat >= s.cfg.KYCThresholdSat && s.cfg.KYCThresholdSat > 0 {
		flags = append(flags, store.ComplianceFlag{
			UserID:   user.ID,
			Kind:     "unverified_deposit",
			Severity: "warning",
			Detail:   "Deposit received on an account that has not completed identity verification.",
		})
	}
	return flags
}

// ---------------------------------------------------------------------------
// Withdrawals
// ---------------------------------------------------------------------------

// WithdrawalDecision says whether a withdrawal may be released automatically
// or has to wait for a human.
type WithdrawalDecision struct {
	// NeedsReview is true when a compliance officer must release the payout.
	NeedsReview bool
	// Reason explains a review requirement, for the officer's queue.
	Reason string
	// Flags are compliance flags to raise alongside the request.
	Flags []store.ComplianceFlag
}

// CheckWithdrawal decides whether a payout may be requested at all, and
// whether it can be released without a human.
func (s *Service) CheckWithdrawal(ctx context.Context, user store.User, amountSat int64) (WithdrawalDecision, error) {
	var decision WithdrawalDecision

	// A self-excluded customer must still be able to take their money out;
	// withdrawing is how they leave. Only a closed or suspended account is
	// stopped here.
	switch user.Status {
	case store.UserClosed:
		return decision, refuse(CodeAccountClosed, "This account has been closed.")
	case store.UserSuspended:
		return decision, refuse(CodeSuspended,
			"Withdrawals are paused while this account is suspended. Contact %s.", s.cfg.SupportMail)
	}

	if amountSat < s.cfg.MinWithdrawalSat {
		return decision, refuse(CodeInvalidInput,
			"The minimum withdrawal is %s BTC.", money.FormatBTC(s.cfg.MinWithdrawalSat))
	}

	// Identity verification is a precondition of paying money out, regardless
	// of whether it was required to bet.
	if user.KYCStatus != store.KYCVerified {
		return decision, refuse(CodeKYCRequired,
			"Complete identity verification before withdrawing. This is required before we can send funds.")
	}

	flagged, err := s.store.HasOpenCriticalFlag(ctx, user.ID)
	if err != nil {
		return decision, fmt.Errorf("compliance: check flags: %w", err)
	}
	if flagged {
		return decision, refuse(CodeFlagged,
			"This withdrawal is on hold pending a review of your account. Contact %s.", s.cfg.SupportMail)
	}

	// A live bonus with unmet wagering stands between the customer and a
	// withdrawal. They are told plainly what it would cost to forfeit it
	// rather than being left to work out why the button does nothing.
	grants, err := s.store.BonusGrantsForUser(ctx, user.ID, store.GrantActive)
	if err != nil {
		return decision, fmt.Errorf("compliance: check bonuses: %w", err)
	}
	if len(grants) > 0 {
		var remaining int64
		for _, grant := range grants {
			remaining += grant.RemainingWageringSat()
		}
		bonusBalance, err := s.store.BonusBalanceSat(ctx, user.ID)
		if err != nil {
			return decision, fmt.Errorf("compliance: bonus balance: %w", err)
		}
		return decision, refuse(CodeBonusActive,
			"You have an active bonus with %s BTC of wagering left. Finish the wagering, "+
				"or forfeit the bonus from your account page to withdraw now. "+
				"Forfeiting would give up %s BTC of bonus funds; your own money is not affected.",
			money.FormatBTC(remaining), money.FormatBTC(bonusBalance))
	}

	if s.cfg.ManualReviewSat > 0 && amountSat >= s.cfg.ManualReviewSat {
		decision.NeedsReview = true
		decision.Reason = fmt.Sprintf("Amount %s BTC is at or above the %s BTC manual review threshold.",
			money.FormatBTC(amountSat), money.FormatBTC(s.cfg.ManualReviewSat))
	}
	if s.cfg.AMLReportingSat > 0 && amountSat >= s.cfg.AMLReportingSat {
		decision.Flags = append(decision.Flags, store.ComplianceFlag{
			UserID:   user.ID,
			Kind:     "large_withdrawal",
			Severity: "warning",
			Detail: fmt.Sprintf("Withdrawal of %s BTC is at or above the reporting threshold.",
				money.FormatBTC(amountSat)),
		})
	}
	return decision, nil
}

// ---------------------------------------------------------------------------
// Customer-set protections
// ---------------------------------------------------------------------------

// LimitCoolOff is how long a loosening of a limit is held before it applies.
const LimitCoolOff = 24 * time.Hour

// SetLimit records a customer's limit. Tightening applies immediately;
// loosening waits out the cooling-off period, so a limit cannot be raised in
// the moment it starts to bite.
func (s *Service) SetLimit(ctx context.Context, userID int64, kind string, amount int64) (time.Time, error) {
	if amount < 0 {
		return time.Time{}, refuse(CodeInvalidInput, "A limit cannot be negative.")
	}
	now := s.now()
	effectiveFrom := now

	current, ok, err := s.store.EffectiveLimit(ctx, userID, kind, now)
	if err != nil {
		return time.Time{}, fmt.Errorf("compliance: current limit: %w", err)
	}
	if ok && amount > current {
		effectiveFrom = now.Add(LimitCoolOff)
	}

	if _, err := s.store.SetPlayerLimit(ctx, store.PlayerLimit{
		UserID: userID, Kind: kind, Amount: amount, EffectiveFrom: effectiveFrom,
	}); err != nil {
		return time.Time{}, err
	}
	return effectiveFrom, nil
}

// SelfExclude bars a customer from playing. A zero duration is permanent.
//
// Every live session is destroyed as part of the same call: an exclusion that
// leaves an open session behind is not an exclusion.
func (s *Service) SelfExclude(ctx context.Context, userID int64, duration time.Duration, reason string) error {
	now := s.now()
	exclusion := store.Exclusion{
		UserID:    userID,
		Kind:      store.ExclusionSelfExclusion,
		StartsAt:  now,
		Permanent: duration <= 0,
		Reason:    reason,
	}
	if !exclusion.Permanent {
		exclusion.EndsAt = now.Add(duration)
	}
	if _, err := s.store.CreateExclusion(ctx, exclusion); err != nil {
		return err
	}
	if err := s.store.DeleteUserSessions(ctx, userID); err != nil {
		return fmt.Errorf("compliance: end sessions: %w", err)
	}
	return s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: userID, SubjectUserID: userID, Action: "self_exclude",
		Detail: describeDuration(duration),
	})
}

// TakeABreak starts a short cool-off period.
func (s *Service) TakeABreak(ctx context.Context, userID int64, duration time.Duration) error {
	if duration <= 0 {
		return refuse(CodeInvalidInput, "Choose how long you want to take a break for.")
	}
	now := s.now()
	if _, err := s.store.CreateExclusion(ctx, store.Exclusion{
		UserID: userID, Kind: store.ExclusionCoolOff, StartsAt: now, EndsAt: now.Add(duration),
	}); err != nil {
		return err
	}
	if err := s.store.DeleteUserSessions(ctx, userID); err != nil {
		return fmt.Errorf("compliance: end sessions: %w", err)
	}
	return s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: userID, SubjectUserID: userID, Action: "cool_off",
		Detail: describeDuration(duration),
	})
}

// RealityCheckDue reports whether a session has been running long enough that
// the customer should be shown how long they have been playing.
func (s *Service) RealityCheckDue(session store.Session) bool {
	if s.cfg.RealityCheckMin <= 0 {
		return false
	}
	return !s.now().Before(session.RealityCheckAt)
}

// NextRealityCheck is when the following check falls due.
func (s *Service) NextRealityCheck() time.Time {
	return s.now().Add(time.Duration(s.cfg.RealityCheckMin) * time.Minute)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func startOfWeek(t time.Time) time.Time {
	day := startOfDay(t)
	// ISO weeks start on Monday; Go's Sunday is 0.
	offset := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -offset)
}

func startOfMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func describeDuration(d time.Duration) string {
	if d <= 0 {
		return "permanent"
	}
	return auth.FormatDuration(d)
}
