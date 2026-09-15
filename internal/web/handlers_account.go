package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/auth"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// limitForm describes one limit control on the account page.
type limitForm struct {
	Kind        string
	Label       string
	Help        string
	InSatoshi   bool
	Current     int64
	HasCurrent  bool
	PendingFrom time.Time
	Pending     int64
	HasPending  bool
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, r.URL.Query().Get("flash"), "")
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, flash, message string) {
	ctx := r.Context()
	user, _ := CurrentUser(ctx)
	now := s.now()

	limits, err := s.store.ListPlayerLimits(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	forms := buildLimitForms(limits, now)

	documents, err := s.store.KYCDocumentsForUser(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	openBets, err := s.store.OpenBetCount(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	exclusion, excluded, err := s.store.ActiveExclusion(ctx, user.ID, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	history, err := s.store.AuditForUser(ctx, user.ID, 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	bonusSummary, err := s.bonus.Summary(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if flash == "" && r.URL.Query().Get("welcome") != "" {
		flash = "Welcome. Verify your identity and add funds to start betting."
	}

	s.render(w, r, status, "account.html", map[string]any{
		"Title":     "My account",
		"Flash":     flash,
		"Error":     message,
		"Limits":    forms,
		"Bonus":     bonusSummary,
		"Documents": documents,
		"OpenBets":  openBets,
		"Exclusion": exclusion,
		"Excluded":  excluded,
		"History":   history,
		"CoolOff":   compliance.LimitCoolOff,
		"MinAge":    s.cfg.MinimumAge,
		"Formats":   []string{"decimal", "american", "fractional"},
	})
}

func buildLimitForms(limits []store.PlayerLimit, now time.Time) []limitForm {
	specs := []limitForm{
		{Kind: store.LimitDepositDaily, Label: "Daily deposit limit", InSatoshi: true,
			Help: "The most you can deposit in a day."},
		{Kind: store.LimitDepositWeekly, Label: "Weekly deposit limit", InSatoshi: true,
			Help: "The most you can deposit in a week, counted from Monday."},
		{Kind: store.LimitDepositMonthly, Label: "Monthly deposit limit", InSatoshi: true,
			Help: "The most you can deposit in a calendar month."},
		{Kind: store.LimitStakeDaily, Label: "Daily stake limit", InSatoshi: true,
			Help: "The most you can stake in a day, win or lose."},
		{Kind: store.LimitLossDaily, Label: "Daily loss limit", InSatoshi: true,
			Help: "The most you can be down in a day before we stop taking bets."},
		{Kind: store.LimitLossWeekly, Label: "Weekly loss limit", InSatoshi: true,
			Help: "The most you can be down in a week."},
	}

	// The newest limit already in force binds; anything dated ahead is a
	// pending increase, shown separately so the customer can see it coming.
	for i := range specs {
		for _, limit := range limits {
			if limit.Kind != specs[i].Kind {
				continue
			}
			if !limit.EffectiveFrom.After(now) {
				if !specs[i].HasCurrent || !limit.EffectiveFrom.Before(specs[i].PendingFrom) {
					specs[i].Current, specs[i].HasCurrent = limit.Amount, true
				}
			} else {
				specs[i].Pending, specs[i].HasPending = limit.Amount, true
				specs[i].PendingFrom = limit.EffectiveFrom
			}
		}
	}
	return specs
}

func (s *Server) handleOddsFormat(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	format := r.FormValue("format")
	switch format {
	case "decimal", "american", "fractional":
	default:
		s.renderAccount(w, r, http.StatusBadRequest, "", "Pick one of the supported odds formats.")
		return
	}
	if err := s.store.SetOddsFormat(r.Context(), user.ID, format); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?flash=Odds+will+now+be+shown+in+"+format+"+format.", http.StatusSeeOther)
}

func (s *Server) handleSetLimit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())

	kind := r.FormValue("kind")
	switch kind {
	case store.LimitDepositDaily, store.LimitDepositWeekly, store.LimitDepositMonthly,
		store.LimitStakeDaily, store.LimitLossDaily, store.LimitLossWeekly:
	default:
		s.renderAccount(w, r, http.StatusBadRequest, "", "That is not a limit we support.")
		return
	}

	amountSat, err := money.ParseBTC(r.FormValue("amount"))
	if err != nil || amountSat < 0 {
		s.renderAccount(w, r, http.StatusBadRequest, "",
			"Enter the limit in BTC, for example 0.01000000.")
		return
	}

	effectiveFrom, err := s.compliance.SetLimit(r.Context(), user.ID, kind, amountSat)
	if err != nil {
		if refusal, ok := compliance.AsRefusal(err); ok {
			s.renderAccount(w, r, http.StatusBadRequest, "", refusal.Message)
			return
		}
		s.serverError(w, r, err)
		return
	}

	flash := "Limit updated and in force now."
	if effectiveFrom.After(s.now()) {
		flash = "Because this raises a limit, it takes effect on " +
			effectiveFrom.UTC().Format("2 Jan 2006 at 15:04") +
			" UTC. Your current limit applies until then."
	}
	if err := s.store.Audit(r.Context(), store.AuditEntry{
		ActorUserID: user.ID, SubjectUserID: user.ID, Action: "limit_set",
		Detail: kind + " = " + money.FormatBTC(amountSat) + " BTC", IP: clientIP(r),
	}); err != nil {
		s.log.Error("audit limit", "error", err)
	}
	http.Redirect(w, r, "/account?flash="+urlEscape(flash), http.StatusSeeOther)
}

func (s *Server) handleTakeABreak(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())

	var duration time.Duration
	switch r.FormValue("duration") {
	case "24h":
		duration = 24 * time.Hour
	case "7d":
		duration = 7 * 24 * time.Hour
	case "30d":
		duration = 30 * 24 * time.Hour
	default:
		s.renderAccount(w, r, http.StatusBadRequest, "", "Choose how long you want to take a break for.")
		return
	}

	if err := s.compliance.TakeABreak(r.Context(), user.ID, duration); err != nil {
		s.serverError(w, r, err)
		return
	}
	// The session was destroyed as part of the break, so the cookie goes too.
	clearSessionCookie(w, s.cfg.IsProduction())
	s.render(w, r, http.StatusOK, "break.html", map[string]any{
		"Title":    "Your break has started",
		"Duration": auth.FormatDuration(duration),
		"Wide":     true,
	})
}

func (s *Server) handleSelfExclude(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())

	// Self-exclusion is irreversible, so it takes an explicit typed
	// confirmation rather than a single click.
	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), "SELF-EXCLUDE") {
		s.renderAccount(w, r, http.StatusBadRequest, "",
			"To self-exclude, type SELF-EXCLUDE in the confirmation box. "+
				"This cannot be undone.")
		return
	}

	var duration time.Duration // zero means permanent
	switch r.FormValue("duration") {
	case "6m":
		duration = 183 * 24 * time.Hour
	case "1y":
		duration = 365 * 24 * time.Hour
	case "5y":
		duration = 5 * 365 * 24 * time.Hour
	case "permanent":
		duration = 0
	default:
		s.renderAccount(w, r, http.StatusBadRequest, "", "Choose a self-exclusion period.")
		return
	}

	if err := s.compliance.SelfExclude(r.Context(), user.ID, duration, r.FormValue("reason")); err != nil {
		s.serverError(w, r, err)
		return
	}
	clearSessionCookie(w, s.cfg.IsProduction())
	s.render(w, r, http.StatusOK, "excluded.html", map[string]any{
		"Title":     "You are now self-excluded",
		"Permanent": duration == 0,
		"Duration":  auth.FormatDuration(duration),
		"Wide":      true,
	})
}

func (s *Server) handleSubmitKYC(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	user, _ := CurrentUser(ctx)

	kind := r.FormValue("kind")
	switch kind {
	case "identity", "address", "source_of_funds":
	default:
		s.renderAccount(w, r, http.StatusBadRequest, "", "Choose which document you are providing.")
		return
	}
	reference := strings.TrimSpace(r.FormValue("reference"))
	if reference == "" {
		s.renderAccount(w, r, http.StatusBadRequest, "",
			"Enter the reference your document provider gave you.")
		return
	}

	// The document itself is never stored here, only a reference to it in the
	// verification provider's vault: identity documents do not belong in a
	// betting database.
	if _, err := s.store.SubmitKYCDocument(ctx, store.KYCDocument{
		UserID: user.ID, Kind: kind, Reference: reference,
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	if user.KYCStatus == store.KYCNone || user.KYCStatus == store.KYCRejected {
		if err := s.store.SetKYCStatus(ctx, user.ID, store.KYCPending); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	http.Redirect(w, r, "/account?flash="+urlEscape(
		"Document submitted. We will review it and let you know."), http.StatusSeeOther)
}

func (s *Server) handleRealityCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := CurrentUser(ctx)
	session, _ := CurrentSession(ctx)

	since := s.now().Sub(session.CreatedAt)
	staked, err := s.store.StakedSince(ctx, user.ID, store.Timestamp(session.CreatedAt))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	net, err := s.store.NetLossSince(ctx, user.ID, store.Timestamp(session.CreatedAt))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "reality-check.html", map[string]any{
		"Title":   "How long have you been playing?",
		"Elapsed": auth.FormatDuration(since),
		"Staked":  staked,
		"Net":     net,
		"Wide":    true,
	})
}

func (s *Server) handleRealityCheckAck(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	session, ok := CurrentSession(r.Context())
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	if r.FormValue("action") == "stop" {
		if err := s.store.DeleteSession(r.Context(), session.TokenHash); err != nil {
			s.log.Error("delete session", "error", err)
		}
		clearSessionCookie(w, s.cfg.IsProduction())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.store.TouchSession(r.Context(), session.TokenHash,
		s.now(), s.compliance.NextRealityCheck()); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteByte('+')
		case r < 128 && (r == '-' || r == '.' || r == '_' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')):
			b.WriteRune(r)
		default:
			for _, encoded := range []byte(string(r)) {
				b.WriteString("%")
				const hex = "0123456789ABCDEF"
				b.WriteByte(hex[encoded>>4])
				b.WriteByte(hex[encoded&0x0f])
			}
		}
	}
	return b.String()
}

var _ = errors.Is
