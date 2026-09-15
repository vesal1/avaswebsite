package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/vesal1/avaswebsite/internal/bonus"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
	"github.com/vesal1/avaswebsite/internal/treasury"
)

// ---------------------------------------------------------------------------
// Customer-facing
// ---------------------------------------------------------------------------

func (s *Server) handlePromotions(w http.ResponseWriter, r *http.Request) {
	offers, err := s.store.ListBonusOffers(r.Context(), true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var summary bonus.Summary
	if user, ok := CurrentUser(r.Context()); ok {
		if summary, err = s.bonus.Summary(r.Context(), user.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	s.render(w, r, http.StatusOK, "promotions.html", map[string]any{
		"Title":   "Promotions",
		"Offers":  offers,
		"Summary": summary,
		"Rates": []struct {
			Product string
			Bps     int64
		}{
			{"Slots and casino", bonus.ContributionRateBps("slots")},
			{"Sportsbook", bonus.ContributionRateBps("sportsbook")},
			{"Poker", bonus.ContributionRateBps("poker")},
		},
	})
}

func (s *Server) handleForfeitBonus(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())

	// Forfeiting is irreversible, so it takes a typed confirmation rather than
	// a single click that could be made by accident.
	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), "FORFEIT") {
		s.renderAccount(w, r, http.StatusBadRequest, "",
			"To give up your bonus, type FORFEIT in the confirmation box. "+
				"Your own money is not affected, but the bonus funds are lost.")
		return
	}

	forfeited, err := s.bonus.Forfeit(r.Context(), user.ID, "forfeited by the customer")
	if err != nil {
		if errors.Is(err, bonus.ErrNothingToForfeit) {
			s.renderAccount(w, r, http.StatusBadRequest, "", "You have no active bonus to give up.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?flash="+urlEscape(
		"Bonus forfeited. "+money.FormatBTC(forfeited)+
			" BTC of bonus funds were returned and you can now withdraw."), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Back office: players
// ---------------------------------------------------------------------------

func (s *Server) handleAdminPlayers(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	users, err := s.store.SearchUsers(r.Context(), query, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-players.html", map[string]any{
		"Title":   "Players",
		"Query":   query,
		"Players": users,
		"Wide":    true,
	})
}

func (s *Server) handleAdminPlayer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such player.")
		return
	}
	player, err := s.store.GetUser(ctx, userID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such player.")
		return
	}

	cash, err := s.store.BalanceSat(ctx, userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	summary, err := s.bonus.Summary(ctx, userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	grants, err := s.store.BonusGrantsForUser(ctx, userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ledger, err := s.store.LedgerHistory(ctx, userID, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	bets, err := s.store.ListBetsForUser(ctx, userID, 25)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	flags, err := s.store.FlagsForUser(ctx, userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	audit, err := s.store.AuditForUser(ctx, userID, 50)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	offers, err := s.store.ListBonusOffers(ctx, true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	viewer, _ := CurrentUser(ctx)
	s.render(w, r, http.StatusOK, "admin-player.html", map[string]any{
		"Title":         "Player " + strconv.FormatInt(userID, 10),
		"Flash":         r.URL.Query().Get("flash"),
		"Error":         r.URL.Query().Get("error"),
		"Player":        player,
		"CashSat":       cash,
		"Bonus":         summary,
		"Grants":        grants,
		"Ledger":        ledger,
		"Bets":          bets,
		"Flags":         flags,
		"Audit":         audit,
		"Offers":        offers,
		"TestAllowed":   s.bonus.TestCreditsAllowed(),
		"ApprovalSat":   s.treasury.ApprovalThresholdSat(),
		"CanMoveMoney":  viewer.CanReviewCompliance(),
		"DefaultWagerX": s.cfg.DefaultWageringX100,
		"Wide":          true,
	})
}

func (s *Server) handleAdminGrantBonus(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	staff, _ := CurrentUser(ctx)

	userID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such player.")
		return
	}
	playerPath := "/admin/players/" + strconv.FormatInt(userID, 10)

	// Granting from a stored offer applies that offer's own rules.
	if raw := r.FormValue("offer"); raw != "" && raw != "0" {
		offerID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			s.redirectAdmin(w, r, playerPath, "", "That offer could not be read.")
			return
		}
		deposit, _ := money.ParseBTC(r.FormValue("qualifying_deposit"))
		if _, err := s.bonus.GrantFromOffer(ctx, userID, offerID, staff.ID, deposit); err != nil {
			s.redirectAdmin(w, r, playerPath, "", trimErrPrefix(err.Error()))
			return
		}
		s.redirectAdmin(w, r, playerPath, "Bonus granted from the offer.", "")
		return
	}

	amountSat, err := money.ParseBTC(r.FormValue("amount"))
	if err != nil || amountSat <= 0 {
		s.redirectAdmin(w, r, playerPath, "", "Enter an amount in BTC, for example 0.00500000.")
		return
	}
	kind := r.FormValue("kind")
	switch kind {
	case store.BonusFreeCredit, store.BonusComp, store.BonusTestCredit, store.BonusDepositMatch:
	default:
		s.redirectAdmin(w, r, playerPath, "", "Choose what kind of bonus this is.")
		return
	}

	wagering, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("wagering_x100")), 10, 64)
	if wagering < 0 {
		wagering = 0
	}
	validDays, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("valid_days")))
	minOdds := int64(0)
	if raw := strings.TrimSpace(r.FormValue("min_odds")); raw != "" {
		if parsed, err := money.ParseDecimalOdds(raw); err == nil {
			minOdds = parsed
		}
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if note == "" {
		s.redirectAdmin(w, r, playerPath, "", "Say why this bonus is being granted; it goes on the audit trail.")
		return
	}

	granted, err := s.bonus.Grant(ctx, bonus.GrantRequest{
		UserID: userID, Kind: kind, AmountSat: amountSat, WageringX100: wagering,
		MinOddsMilli: minOdds, ValidDays: validDays, Note: note, GrantedBy: staff.ID,
	})
	if err != nil {
		if errors.Is(err, bonus.ErrTestCreditsRefused) {
			s.redirectAdmin(w, r, playerPath, "",
				"Test credits are disabled on this server. They cannot be granted against real customer money.")
			return
		}
		s.redirectAdmin(w, r, playerPath, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, playerPath,
		"Granted "+money.FormatBTC(granted.AmountSat)+" BTC with "+
			money.FormatBTC(granted.WageringRequiredSat)+" BTC of wagering.", "")
}

func (s *Server) handleAdminCancelBonus(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	staff, _ := CurrentUser(ctx)

	grantID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such bonus.")
		return
	}
	grant, err := s.store.GetBonusGrant(ctx, grantID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such bonus.")
		return
	}
	playerPath := "/admin/players/" + strconv.FormatInt(grant.UserID, 10)

	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.redirectAdmin(w, r, playerPath, "", "Cancelling a bonus needs a reason for the record.")
		return
	}
	if err := s.bonus.Cancel(ctx, grantID, staff.ID, reason); err != nil {
		s.redirectAdmin(w, r, playerPath, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, playerPath, "Bonus cancelled and the funds returned.", "")
}

func (s *Server) handleAdminAdjust(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	staff, _ := CurrentUser(ctx)

	userID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such player.")
		return
	}
	playerPath := "/admin/players/" + strconv.FormatInt(userID, 10)

	amountSat, err := money.ParseBTC(r.FormValue("amount"))
	if err != nil {
		s.redirectAdmin(w, r, playerPath, "", "Enter an amount in BTC, for example 0.00500000.")
		return
	}
	if r.FormValue("direction") == "debit" {
		amountSat = -amountSat
	}
	reason := strings.TrimSpace(r.FormValue("reason"))

	outcome, err := s.treasury.Request(ctx, userID, amountSat, staff.ID, reason)
	if err != nil {
		s.redirectAdmin(w, r, playerPath, "", trimErrPrefix(err.Error()))
		return
	}
	if outcome.NeedsSecond {
		s.redirectAdmin(w, r, playerPath,
			"Adjustment "+strconv.FormatInt(outcome.AdjustmentID, 10)+
				" is above the approval threshold and needs a second person to release it.", "")
		return
	}
	s.redirectAdmin(w, r, playerPath,
		"Adjustment applied. New balance "+money.FormatBTC(outcome.NewBalanceSat)+" BTC.", "")
}

// ---------------------------------------------------------------------------
// Back office: adjustment approvals
// ---------------------------------------------------------------------------

func (s *Server) handleAdminAdjustments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pending, err := s.treasury.Pending(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	recent, err := s.treasury.Recent(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	viewer, _ := CurrentUser(ctx)
	s.render(w, r, http.StatusOK, "admin-adjustments.html", map[string]any{
		"Title":       "Manual adjustments",
		"Flash":       r.URL.Query().Get("flash"),
		"Error":       r.URL.Query().Get("error"),
		"Pending":     pending,
		"Recent":      recent,
		"ViewerID":    viewer.ID,
		"ApprovalSat": s.treasury.ApprovalThresholdSat(),
		"Wide":        true,
	})
}

func (s *Server) handleApproveAdjustment(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	staff, _ := CurrentUser(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such adjustment.")
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if note == "" {
		s.redirectAdmin(w, r, "/admin/adjustments", "", "Say what you checked before approving.")
		return
	}

	balance, err := s.treasury.Approve(r.Context(), id, staff.ID, note)
	if err != nil {
		if errors.Is(err, treasury.ErrSelfApproval) {
			s.redirectAdmin(w, r, "/admin/adjustments", "",
				"You requested this adjustment, so somebody else has to release it.")
			return
		}
		s.redirectAdmin(w, r, "/admin/adjustments", "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, "/admin/adjustments",
		"Adjustment applied. New balance "+money.FormatBTC(balance)+" BTC.", "")
}

func (s *Server) handleRejectAdjustment(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	staff, _ := CurrentUser(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such adjustment.")
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if err := s.treasury.Reject(r.Context(), id, staff.ID, note); err != nil {
		s.redirectAdmin(w, r, "/admin/adjustments", "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, "/admin/adjustments", "Adjustment rejected.", "")
}

// ---------------------------------------------------------------------------
// Back office: offers
// ---------------------------------------------------------------------------

func (s *Server) handleAdminOffers(w http.ResponseWriter, r *http.Request) {
	offers, err := s.store.ListBonusOffers(r.Context(), false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-offers.html", map[string]any{
		"Title":  "Promotional offers",
		"Flash":  r.URL.Query().Get("flash"),
		"Error":  r.URL.Query().Get("error"),
		"Offers": offers,
		"Wide":   true,
	})
}

func (s *Server) handleCreateOffer(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	staff, _ := CurrentUser(ctx)

	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	name := strings.TrimSpace(r.FormValue("name"))
	if code == "" || name == "" {
		s.redirectAdmin(w, r, "/admin/offers", "", "An offer needs a code and a name.")
		return
	}
	kind := r.FormValue("kind")
	switch kind {
	case store.BonusDepositMatch, store.BonusFreeCredit, store.BonusComp:
	default:
		s.redirectAdmin(w, r, "/admin/offers", "", "Choose what kind of offer this is.")
		return
	}

	amount, _ := money.ParseBTC(r.FormValue("amount"))
	maxAmount, _ := money.ParseBTC(r.FormValue("max_amount"))
	minDeposit, _ := money.ParseBTC(r.FormValue("min_deposit"))
	matchBps, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("match_bps")), 10, 64)
	wagering, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("wagering_x100")), 10, 64)
	validDays, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("valid_days")))
	maxPerUser, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("max_per_user")))
	if validDays <= 0 {
		validDays = 30
	}
	if maxPerUser <= 0 {
		maxPerUser = 1
	}
	minOdds := int64(0)
	if raw := strings.TrimSpace(r.FormValue("min_odds")); raw != "" {
		if parsed, err := money.ParseDecimalOdds(raw); err == nil {
			minOdds = parsed
		}
	}

	if _, err := s.store.CreateBonusOffer(ctx, store.BonusOffer{
		Code: code, Name: name, Description: strings.TrimSpace(r.FormValue("description")),
		Kind: kind, AmountSat: amount, MatchBps: matchBps, MaxAmountSat: maxAmount,
		MinDepositSat: minDeposit, WageringX100: wagering, MinOddsMilli: minOdds,
		ValidDays: validDays, MaxPerUser: maxPerUser, Active: true, CreatedBy: staff.ID,
	}); err != nil {
		s.redirectAdmin(w, r, "/admin/offers", "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, "/admin/offers", "Offer created and live.", "")
}

func (s *Server) handleToggleOffer(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such offer.")
		return
	}
	active := r.FormValue("active") == "1"
	if err := s.store.SetBonusOfferActive(r.Context(), id, active); err != nil {
		s.serverError(w, r, err)
		return
	}
	state := "retired"
	if active {
		state = "live"
	}
	s.redirectAdmin(w, r, "/admin/offers", "Offer is now "+state+".", "")
}
