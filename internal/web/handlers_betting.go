package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/vesal1/avaswebsite/internal/betting"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

func (s *Server) handleBetslip(w http.ResponseWriter, r *http.Request) {
	s.renderBetslip(w, r, http.StatusOK, "", "", r.URL.Query().Get("stake"))
}

// renderBetslip prices the current slip and shows it.
func (s *Server) renderBetslip(w http.ResponseWriter, r *http.Request, status int, flash, message, stakeInput string) {
	ctx := r.Context()
	entries := readSlip(r)

	data := map[string]any{
		"Title":     "Bet slip",
		"Flash":     flash,
		"Error":     message,
		"Stake":     stakeInput,
		"MinStake":  s.cfg.MinStakeSat,
		"MaxStake":  s.cfg.MaxStakeSat,
		"MaxPayout": s.cfg.MaxPayoutSat,
		"MaxLegs":   s.cfg.MaxParlayLegs,
	}

	if len(entries) > 0 {
		stakeSat, _ := money.ParseBTC(stakeInput)
		quote, err := s.betting.Quote(ctx, betting.Slip{
			Legs: slipToLegs(entries), StakeSat: stakeSat,
		})
		switch {
		case err == nil:
			data["Quote"] = quote
		case errors.Is(err, betting.ErrSelectionClosed),
			errors.Is(err, betting.ErrCorrelatedLegs),
			errors.Is(err, betting.ErrTooManyLegs):
			// The slip cannot be priced as it stands. Say why rather than
			// showing an empty slip.
			if data["Error"] == "" {
				data["Error"] = friendlyBetError(err)
			}
		default:
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, r, status, "betslip.html", data)
}

func (s *Server) handleBetslipAdd(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	selectionID, err := strconv.ParseInt(r.FormValue("selection"), 10, 64)
	if err != nil || selectionID <= 0 {
		s.renderError(w, r, http.StatusBadRequest, "That selection could not be read.")
		return
	}
	// The odds come from the form only as the price the customer was shown.
	// They are never used as the price of the bet.
	shownOdds, _ := strconv.ParseInt(r.FormValue("odds"), 10, 64)

	// Confirm the selection is real before putting it on the slip, so a bad id
	// is rejected here rather than surfacing as a confusing pricing error.
	if _, err := s.store.GetSelectionDetail(r.Context(), selectionID); err != nil {
		s.renderError(w, r, http.StatusNotFound, "That selection is no longer available.")
		return
	}

	entries := readSlip(r)
	for _, entry := range entries {
		if entry.SelectionID == selectionID {
			// Tapping a price already on the slip removes it, which is what a
			// highlighted button implies.
			s.writeSlip(w, removeEntry(entries, selectionID))
			http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
			return
		}
	}
	entries = append(entries, slipEntry{SelectionID: selectionID, OddsMilli: shownOdds})
	s.writeSlip(w, entries)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (s *Server) handleBetslipRemove(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	selectionID, _ := strconv.ParseInt(r.FormValue("selection"), 10, 64)
	s.writeSlip(w, removeEntry(readSlip(r), selectionID))
	http.Redirect(w, r, "/betslip", http.StatusSeeOther)
}

func (s *Server) handleBetslipClear(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	s.writeSlip(w, nil)
	http.Redirect(w, r, "/betslip", http.StatusSeeOther)
}

func removeEntry(entries []slipEntry, selectionID int64) []slipEntry {
	out := entries[:0]
	for _, entry := range entries {
		if entry.SelectionID != selectionID {
			out = append(out, entry)
		}
	}
	return out
}

func (s *Server) handleBetslipPlace(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, ok := CurrentUser(r.Context())
	if !ok {
		s.redirectToLogin(w, r)
		return
	}

	stakeInput := r.FormValue("stake")
	stakeSat, err := money.ParseBTC(stakeInput)
	if err != nil {
		s.renderBetslip(w, r, http.StatusBadRequest, "",
			"Enter a stake in BTC, for example 0.00050000.", stakeInput)
		return
	}

	entries := readSlip(r)
	if len(entries) == 0 {
		s.renderBetslip(w, r, http.StatusBadRequest, "", "Your bet slip is empty.", stakeInput)
		return
	}

	placed, err := s.betting.Place(r.Context(), user, betting.Slip{
		Legs:             slipToLegs(entries),
		StakeSat:         stakeSat,
		AcceptOddsChange: r.FormValue("accept_odds") != "",
		IP:               clientIP(r),
	})
	if err != nil {
		if refusal, ok := compliance.AsRefusal(err); ok {
			s.renderBetslip(w, r, http.StatusForbidden, "", refusal.Message, stakeInput)
			return
		}
		if isCustomerBetError(err) {
			s.renderBetslip(w, r, http.StatusBadRequest, "", friendlyBetError(err), stakeInput)
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.writeSlip(w, nil)
	http.Redirect(w, r, "/bets?placed="+strconv.FormatInt(placed.BetID, 10), http.StatusSeeOther)
}

// isCustomerBetError reports whether an error is one the customer can act on,
// as opposed to a fault at our end.
func isCustomerBetError(err error) bool {
	for _, known := range []error{
		betting.ErrEmptySlip, betting.ErrSelectionClosed, betting.ErrOddsChanged,
		betting.ErrStakeOutOfRange, betting.ErrPayoutTooLarge, betting.ErrCorrelatedLegs,
		betting.ErrTooManyLegs, betting.ErrInsufficientFunds, betting.ErrLiabilityExceeded,
		money.ErrMoney,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

// friendlyBetError turns an engine error into something worth reading.
func friendlyBetError(err error) string {
	switch {
	case errors.Is(err, betting.ErrOddsChanged):
		return "The price moved while you were betting. Check the new price and " +
			"place again, or tick “accept price changes”."
	case errors.Is(err, betting.ErrInsufficientFunds):
		return "You do not have enough in your balance for that stake. " +
			"Add funds from your wallet."
	case errors.Is(err, betting.ErrLiabilityExceeded):
		return "We cannot take a bet that size on that selection. Try a smaller stake."
	case errors.Is(err, betting.ErrCorrelatedLegs):
		return "A multiple cannot combine two selections from the same event. " +
			"Remove one of them, or place them as separate singles."
	case errors.Is(err, betting.ErrSelectionClosed):
		return "One of your selections is no longer available. Remove it and try again."
	default:
		// The engine's own messages already carry the specific limits.
		return trimErrPrefix(err.Error())
	}
}

func trimErrPrefix(message string) string {
	for _, prefix := range []string{"betting: ", "money: ", "wallet: ", "store: "} {
		if len(message) > len(prefix) && message[:len(prefix)] == prefix {
			return message[len(prefix):]
		}
	}
	return message
}

func (s *Server) handleBets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := CurrentUser(ctx)

	filter := r.URL.Query().Get("status")
	var statuses []string
	switch filter {
	case "open":
		statuses = []string{store.OutcomeOpen}
	case "settled":
		statuses = []string{store.OutcomeWon, store.OutcomeLost, store.OutcomeVoid,
			store.OutcomeHalfWon, store.OutcomeHalfLost}
	}

	bets, err := s.store.ListBetsForUser(ctx, user.ID, 200, statuses...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var staked, returned, openStake int64
	for _, bet := range bets {
		staked += bet.StakeSat
		returned += bet.PayoutSat
		if bet.Status == store.OutcomeOpen {
			openStake += bet.StakeSat
		}
	}

	flash := ""
	if placed := r.URL.Query().Get("placed"); placed != "" {
		flash = "Bet " + placed + " accepted. Good luck."
	}

	s.render(w, r, http.StatusOK, "bets.html", map[string]any{
		"Title":     "My bets",
		"Bets":      bets,
		"Filter":    filter,
		"Staked":    staked,
		"Returned":  returned,
		"OpenStake": openStake,
		"Net":       returned - staked,
		"Flash":     flash,
	})
}
