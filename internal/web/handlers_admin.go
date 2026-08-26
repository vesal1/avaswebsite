package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/betting"
	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	accounts := map[string]int64{}
	for _, account := range []string{
		store.AccountUserCash, store.AccountBetEscrow, store.AccountHouseRevenue,
		store.AccountHouseFees, store.AccountWithdrawalSuspense, store.AccountExternalBitcoin,
	} {
		balance, err := s.store.AccountBalanceSat(ctx, account)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		accounts[account] = balance
	}

	problems, err := s.store.CheckLedger(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	messages := make([]string, 0, len(problems))
	for _, problem := range problems {
		messages = append(messages, problem.String())
	}

	pendingWithdrawals, err := s.store.WithdrawalsByStatus(ctx,
		store.WithdrawalRequested, store.WithdrawalReview)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	flags, err := s.store.OpenFlags(ctx, 25)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	awaitingSettlement, err := s.store.ListEvents(ctx, store.EventFilter{
		Statuses: []string{store.EventFinished, store.EventLive},
		Limit:    50,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var hotWallet int64
	walletErr := ""
	if balance, err := s.wallet.Provider().SpendableSat(ctx); err != nil {
		walletErr = err.Error()
	} else {
		hotWallet = balance
	}

	s.render(w, r, http.StatusOK, "admin.html", map[string]any{
		"Title":          "Back office",
		"Flash":          r.URL.Query().Get("flash"),
		"Accounts":       accounts,
		"LedgerProblems": messages,
		"Withdrawals":    pendingWithdrawals,
		"Flags":          flags,
		"Awaiting":       awaitingSettlement,
		"HotWallet":      hotWallet,
		"WalletErr":      walletErr,
		"Provider":       s.wallet.Provider().Name(),
		"Network":        s.wallet.Provider().Network(),
		"Wide":           true,
	})
}

func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	events, err := s.store.ListEvents(ctx, store.EventFilter{Limit: 200})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-events.html", map[string]any{
		"Title":  "Events",
		"Flash":  r.URL.Query().Get("flash"),
		"Error":  r.URL.Query().Get("error"),
		"Events": events,
		"Sports": catalog.All(),
		"Wide":   true,
	})
}

// handleAdminCreateEvent builds an event, its participants and a starting set
// of markets priced from the trader's own probability weights.
func (s *Server) handleAdminCreateEvent(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	user, _ := CurrentUser(ctx)

	sport, ok := catalog.Lookup(r.FormValue("sport"))
	if !ok {
		s.redirectAdmin(w, r, "/admin/events", "", "Pick a sport from the list.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.redirectAdmin(w, r, "/admin/events", "", "Give the event a name.")
		return
	}
	startsAt, err := time.Parse("2006-01-02T15:04", r.FormValue("starts_at"))
	if err != nil {
		s.redirectAdmin(w, r, "/admin/events", "", "Enter a valid start time.")
		return
	}

	homeName := strings.TrimSpace(r.FormValue("home"))
	awayName := strings.TrimSpace(r.FormValue("away"))
	if homeName == "" || awayName == "" {
		s.redirectAdmin(w, r, "/admin/events", "", "Name both sides.")
		return
	}

	// Weights are the trader's view of each outcome's chance. The pricing
	// layer turns them into odds carrying the configured margin, so a trader
	// never types a price that quietly gives the book away.
	weights := []int64{
		parseWeight(r.FormValue("weight_home"), 40),
		parseWeight(r.FormValue("weight_draw"), 25),
		parseWeight(r.FormValue("weight_away"), 35),
	}
	marginBps := parseWeight(r.FormValue("margin_bps"), s.cfg.DefaultMarginBps)

	threeWay := sport.HasDraw && sport.Supports(catalog.KindMoneyline3)
	if !threeWay {
		weights = []int64{weights[0], weights[2]}
	}
	prices, err := money.OddsFromWeights(weights, marginBps)
	if err != nil {
		s.redirectAdmin(w, r, "/admin/events", "", "Those weights could not be priced: "+err.Error())
		return
	}

	competitionID := int64(0)
	if competition := strings.TrimSpace(r.FormValue("competition")); competition != "" {
		competitionID, err = s.store.UpsertCompetition(ctx, sport.Key, competition, "")
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	eventID, err := s.store.CreateEvent(ctx, store.NewEvent{
		SportKey: sport.Key, CompetitionID: competitionID, Name: name,
		Format: string(catalog.FormatMatch), StartsAt: startsAt,
		Venue: strings.TrimSpace(r.FormValue("venue")),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	homeID, err := s.store.AddParticipant(ctx, store.Participant{
		EventID: eventID, Name: homeName, HomeAway: "home", SortOrder: 0,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	awayID, err := s.store.AddParticipant(ctx, store.Participant{
		EventID: eventID, Name: awayName, HomeAway: "away", SortOrder: 1,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	kind := catalog.KindMoneyline3
	if !threeWay {
		kind = catalog.KindMoneyline2
	}
	marketID, err := s.store.CreateMarket(ctx, store.NewMarket{
		EventID: eventID, Kind: string(kind), Title: sport.MarketTitle(kind), MarginBps: marginBps,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	outcomes := []struct {
		code string
		name string
		pid  int64
	}{
		{"home", homeName, homeID},
		{"draw", "Draw", 0},
		{"away", awayName, awayID},
	}
	if !threeWay {
		outcomes = []struct {
			code string
			name string
			pid  int64
		}{{"home", homeName, homeID}, {"away", awayName, awayID}}
	}
	for i, outcome := range outcomes {
		if _, err := s.store.AddSelection(ctx, store.Selection{
			MarketID: marketID, ParticipantID: outcome.pid, Name: outcome.name,
			OutcomeCode: outcome.code, OddsMilli: prices[i], SortOrder: i,
		}); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	if err := s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: user.ID, Action: "event_created",
		Detail: name + " (" + sport.Key + ")", IP: clientIP(r),
	}); err != nil {
		s.log.Error("audit event", "error", err)
	}
	http.Redirect(w, r, "/admin/events/"+strconv.FormatInt(eventID, 10), http.StatusSeeOther)
}

func parseWeight(raw string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (s *Server) handleAdminEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	eventID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such event.")
		return
	}
	event, err := s.store.GetEvent(ctx, eventID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such event.")
		return
	}
	participants, err := s.store.ParticipantsForEvent(ctx, eventID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	markets, err := s.store.MarketsForEvent(ctx, eventID, false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// The book's exposure on each price, so a trader can see where the risk is
	// before moving a line.
	liability := make(map[int64]int64)
	for _, market := range markets {
		for _, selection := range market.Selections {
			exposure, err := s.store.LiabilityForSelection(ctx, selection.ID)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			liability[selection.ID] = exposure
		}
	}

	sport, _ := catalog.Lookup(event.SportKey)
	s.render(w, r, http.StatusOK, "admin-event.html", map[string]any{
		"Title":        "Trade: " + event.Name,
		"Flash":        r.URL.Query().Get("flash"),
		"Error":        r.URL.Query().Get("error"),
		"Event":        event,
		"Sport":        sport,
		"Participants": participants,
		"Markets":      markets,
		"Liability":    liability,
		"Wide":         true,
	})
}

func (s *Server) handleAdminUpdateOdds(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	eventPath := "/admin/events/" + r.PathValue("id")

	selectionID, err := strconv.ParseInt(r.FormValue("selection"), 10, 64)
	if err != nil {
		s.redirectAdmin(w, r, eventPath, "", "That selection could not be read.")
		return
	}
	oddsMilli, err := money.ParseDecimalOdds(r.FormValue("odds"))
	if err != nil {
		s.redirectAdmin(w, r, eventPath, "", "Enter decimal odds, for example 2.50.")
		return
	}
	if err := s.store.UpdateOdds(ctx, selectionID, oddsMilli, strings.TrimSpace(r.FormValue("reason"))); err != nil {
		s.redirectAdmin(w, r, eventPath, "", "That price could not be moved: "+trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, eventPath, "Price updated to "+money.FormatDecimalOdds(oddsMilli)+".", "")
}

func (s *Server) handleAdminEventStatus(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	eventPath := "/admin/events/" + r.PathValue("id")
	eventID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such event.")
		return
	}

	status := r.FormValue("status")
	switch status {
	case store.EventScheduled, store.EventLive, store.EventSuspended,
		store.EventFinished, store.EventCancelled, store.EventPostponed:
	default:
		s.redirectAdmin(w, r, eventPath, "", "That is not a status an event can take.")
		return
	}
	if err := s.store.SetEventStatus(r.Context(), eventID, status); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectAdmin(w, r, eventPath, "Event is now "+status+".", "")
}

// handleAdminSettle posts a result and settles everything it can grade.
func (s *Server) handleAdminSettle(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	user, _ := CurrentUser(ctx)
	eventPath := "/admin/events/" + r.PathValue("id")

	eventID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such event.")
		return
	}
	participants, err := s.store.ParticipantsForEvent(ctx, eventID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	result := betting.Result{
		EventID:   eventID,
		Abandoned: r.FormValue("abandoned") != "",
	}
	for _, participant := range participants {
		outcome := betting.ParticipantResult{ParticipantID: participant.ID}
		key := strconv.FormatInt(participant.ID, 10)
		if raw := strings.TrimSpace(r.FormValue("score_" + key)); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				s.redirectAdmin(w, r, eventPath, "",
					"Score for "+participant.Name+" is not a whole number.")
				return
			}
			outcome.Score, outcome.HasScore = value, true
		}
		if raw := strings.TrimSpace(r.FormValue("position_" + key)); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				s.redirectAdmin(w, r, eventPath, "",
					"Finishing position for "+participant.Name+" is not valid.")
				return
			}
			outcome.FinishPosition = value
		}
		outcome.Withdrawn = r.FormValue("withdrawn_"+key) != ""
		result.Participants = append(result.Participants, outcome)
	}

	report, err := s.betting.SettleEvent(ctx, eventID, result, user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	message := strconv.Itoa(report.MarketsSettled) + " markets settled, " +
		strconv.Itoa(report.BetsSettled) + " bets paid " +
		money.FormatBTC(report.PaidOutSat) + " BTC."
	if len(report.MarketsManual) > 0 {
		message += " " + strconv.Itoa(len(report.MarketsManual)) +
			" markets need grading by hand."
	}
	s.redirectAdmin(w, r, eventPath, message, strings.Join(report.Problems, "; "))
}

func (s *Server) handleAdminSettleMarket(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	user, _ := CurrentUser(ctx)

	marketID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such market.")
		return
	}
	market, err := s.store.GetMarket(ctx, marketID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such market.")
		return
	}
	eventPath := "/admin/events/" + strconv.FormatInt(market.EventID, 10)

	winners := parseIDs(r.Form["winner"])
	voided := parseIDs(r.Form["void"])
	if len(winners) == 0 && len(voided) == 0 {
		s.redirectAdmin(w, r, eventPath, "",
			"Mark at least one selection as the winner, or void the market instead.")
		return
	}

	report, err := s.betting.SettleMarketManually(ctx, marketID, winners, voided, user.ID)
	if err != nil {
		s.redirectAdmin(w, r, eventPath, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, eventPath, "Market settled: "+strconv.Itoa(report.BetsSettled)+
		" bets, "+money.FormatBTC(report.PaidOutSat)+" BTC paid.", "")
}

func (s *Server) handleAdminVoidMarket(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	user, _ := CurrentUser(ctx)

	marketID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such market.")
		return
	}
	market, err := s.store.GetMarket(ctx, marketID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such market.")
		return
	}
	eventPath := "/admin/events/" + strconv.FormatInt(market.EventID, 10)

	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.redirectAdmin(w, r, eventPath, "", "Voiding a market needs a reason for the record.")
		return
	}
	report, err := s.betting.VoidMarket(ctx, marketID, user.ID, reason)
	if err != nil {
		s.redirectAdmin(w, r, eventPath, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, eventPath, "Market voided: "+strconv.Itoa(report.BetsSettled)+
		" bets refunded "+money.FormatBTC(report.PaidOutSat)+" BTC.", "")
}

func parseIDs(values []string) []int64 {
	var out []int64
	for _, raw := range values {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Compliance queues
// ---------------------------------------------------------------------------

func (s *Server) handleAdminWithdrawals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pending, err := s.store.WithdrawalsByStatus(ctx,
		store.WithdrawalRequested, store.WithdrawalReview, store.WithdrawalApproved)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	recent, err := s.store.WithdrawalsByStatus(ctx,
		store.WithdrawalBroadcast, store.WithdrawalConfirmed,
		store.WithdrawalRejected, store.WithdrawalCancelled, store.WithdrawalFailed)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-withdrawals.html", map[string]any{
		"Title":   "Withdrawals",
		"Flash":   r.URL.Query().Get("flash"),
		"Error":   r.URL.Query().Get("error"),
		"Pending": pending,
		"Recent":  recent,
		"Wide":    true,
	})
}

func (s *Server) handleReleaseWithdrawal(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such withdrawal.")
		return
	}
	if err := s.wallet.ReleaseWithdrawal(r.Context(), id, user.ID); err != nil {
		// A failed send is deliberately not retried here: it has already
		// raised a critical flag, and a second attempt could pay twice.
		s.redirectAdmin(w, r, "/admin/withdrawals", "",
			"The payout did not go out: "+trimErrPrefix(err.Error())+
				" A critical flag has been raised. Confirm on-chain before retrying.")
		return
	}
	s.redirectAdmin(w, r, "/admin/withdrawals", "Withdrawal released.", "")
}

func (s *Server) handleRejectWithdrawal(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such withdrawal.")
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.redirectAdmin(w, r, "/admin/withdrawals", "",
			"Rejecting a withdrawal needs a reason: the customer will be told it.")
		return
	}
	if err := s.wallet.RejectWithdrawal(r.Context(), id, user.ID, reason); err != nil {
		s.redirectAdmin(w, r, "/admin/withdrawals", "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, "/admin/withdrawals", "Withdrawal rejected and funds returned.", "")
}

func (s *Server) handleAdminFlags(w http.ResponseWriter, r *http.Request) {
	flags, err := s.store.OpenFlags(r.Context(), 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-flags.html", map[string]any{
		"Title": "Compliance flags",
		"Flash": r.URL.Query().Get("flash"),
		"Error": r.URL.Query().Get("error"),
		"Flags": flags,
		"Wide":  true,
	})
}

func (s *Server) handleResolveFlag(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such flag.")
		return
	}
	resolution := strings.TrimSpace(r.FormValue("resolution"))
	if resolution == "" {
		s.redirectAdmin(w, r, "/admin/flags", "",
			"Closing a flag needs a written resolution for the audit trail.")
		return
	}
	if err := s.store.ResolveFlag(r.Context(), id, user.ID, resolution); err != nil {
		s.redirectAdmin(w, r, "/admin/flags", "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, "/admin/flags", "Flag closed.", "")
}

func (s *Server) handleAdminKYC(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := s.store.PendingKYCUsers(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	type pending struct {
		User      store.User
		Documents []store.KYCDocument
	}
	queue := make([]pending, 0, len(users))
	for _, user := range users {
		documents, err := s.store.KYCDocumentsForUser(ctx, user.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		queue = append(queue, pending{User: user, Documents: documents})
	}
	s.render(w, r, http.StatusOK, "admin-kyc.html", map[string]any{
		"Title": "Identity verification",
		"Flash": r.URL.Query().Get("flash"),
		"Error": r.URL.Query().Get("error"),
		"Queue": queue,
		"Wide":  true,
	})
}

func (s *Server) handleDecideKYC(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()
	officer, _ := CurrentUser(ctx)

	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such document.")
		return
	}
	subjectID, err := strconv.ParseInt(r.FormValue("user"), 10, 64)
	if err != nil {
		s.redirectAdmin(w, r, "/admin/kyc", "", "That decision was missing its account.")
		return
	}

	decision := r.FormValue("decision")
	var documentStatus, accountStatus string
	switch decision {
	case "accept":
		documentStatus, accountStatus = "accepted", store.KYCVerified
	case "reject":
		documentStatus, accountStatus = "rejected", store.KYCRejected
	default:
		s.redirectAdmin(w, r, "/admin/kyc", "", "Accept or reject the document.")
		return
	}

	note := strings.TrimSpace(r.FormValue("note"))
	if err := s.store.ReviewKYCDocument(ctx, documentID, officer.ID, documentStatus, note); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.SetKYCStatus(ctx, subjectID, accountStatus); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: officer.ID, SubjectUserID: subjectID, Action: "kyc_" + decision,
		Detail: note, IP: clientIP(r),
	}); err != nil {
		s.log.Error("audit kyc", "error", err)
	}
	s.redirectAdmin(w, r, "/admin/kyc", "Identity check recorded.", "")
}

// redirectAdmin returns to a back-office page carrying a message.
func (s *Server) redirectAdmin(w http.ResponseWriter, r *http.Request, path, flash, message string) {
	target := path
	switch {
	case message != "":
		target += "?error=" + urlEscape(message)
	case flash != "":
		target += "?flash=" + urlEscape(flash)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
