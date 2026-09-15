package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/poker"
	"github.com/vesal1/avaswebsite/internal/pokerhouse"
	"github.com/vesal1/avaswebsite/internal/store"
)

func (s *Server) handlePokerLobby(w http.ResponseWriter, r *http.Request) {
	tables := s.poker.Lobby()

	var seatedAt int64
	if user, ok := CurrentUser(r.Context()); ok {
		if id, seated := s.poker.SeatedAt(user.ID); seated {
			seatedAt = id
		}
	}

	s.render(w, r, http.StatusOK, "poker.html", map[string]any{
		"Title":    "Poker",
		"Tables":   tables,
		"SeatedAt": seatedAt,
		"Flash":    r.URL.Query().Get("flash"),
		"Error":    r.URL.Query().Get("error"),
	})
}

func (s *Server) handlePokerTable(w http.ResponseWriter, r *http.Request) {
	tableID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such table.")
		return
	}
	table, ok := s.poker.Table(tableID)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "That table is not open.")
		return
	}

	var viewerID int64
	if user, ok := CurrentUser(r.Context()); ok {
		viewerID = user.ID
	}
	hands, err := s.store.PokerHandsForTable(r.Context(), tableID, 10)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "poker-table.html", map[string]any{
		"Title": table.Config.Name,
		"Table": table.View(viewerID),
		"Hands": hands,
		"Flash": r.URL.Query().Get("flash"),
		"Error": r.URL.Query().Get("error"),
		"Wide":  true,
	})
}

// handlePokerStream is the live feed for a table.
//
// Server-sent events rather than a WebSocket: the traffic is almost entirely
// one-way, SSE reconnects by itself, it needs no extra dependency and it
// survives proxies that mangle upgrades. Player actions go back as ordinary
// form posts, which keeps them inside the same CSRF protection as everything
// else on the site.
func (s *Server) handlePokerStream(w http.ResponseWriter, r *http.Request) {
	tableID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "no such table", http.StatusNotFound)
		return
	}
	table, ok := s.poker.Table(tableID)
	if !ok {
		http.Error(w, "that table is not open", http.StatusNotFound)
		return
	}
	// ResponseController reaches through any middleware wrapping, so this
	// keeps working if another wrapper is added later.
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}

	var viewerID int64
	if user, ok := CurrentUser(r.Context()); ok {
		viewerID = user.ID
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	updates, unsubscribe := table.Subscribe()
	defer unsubscribe()

	// A seated player also receives WebRTC handshake messages addressed to
	// their seat, on the same stream.
	var signals <-chan pokerhouse.Signal
	view := table.View(viewerID)
	if view.YourSeat >= 0 {
		mailbox, leave := s.signals.Join(tableID, view.YourSeat)
		defer leave()
		signals = mailbox
	}

	send := func(event string, payload any) bool {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded); err != nil {
			return false
		}
		return controller.Flush() == nil
	}

	if !send("state", view) {
		return
	}

	// A heartbeat keeps intermediaries from closing an idle connection and
	// lets the client notice a dead one.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	// The action clock ticks down on screen, so the view is refreshed each
	// second while a hand is live even if nothing else changed.
	clock := time.NewTicker(time.Second)
	defer clock.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-updates:
			if !open {
				return
			}
			if !send("state", table.View(viewerID)) {
				return
			}
		case signal, open := <-signals:
			if !open {
				signals = nil
				continue
			}
			if !send("signal", signal) {
				return
			}
		case <-clock.C:
			if table.HandInProgress() {
				if !send("state", table.View(viewerID)) {
					return
				}
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *Server) pokerTableFor(w http.ResponseWriter, r *http.Request) (*poker.Table, int64, bool) {
	tableID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such table.")
		return nil, 0, false
	}
	table, ok := s.poker.Table(tableID)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "That table is not open.")
		return nil, 0, false
	}
	return table, tableID, true
}

func (s *Server) handlePokerSit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	_, tableID, ok := s.pokerTableFor(w, r)
	if !ok {
		return
	}
	user, _ := CurrentUser(r.Context())
	path := "/poker/tables/" + strconv.FormatInt(tableID, 10)

	seatNumber, err := strconv.Atoi(r.FormValue("seat"))
	if err != nil {
		s.redirectAdmin(w, r, path, "", "Pick a seat.")
		return
	}
	buyInSat, err := money.ParseBTC(r.FormValue("buy_in"))
	if err != nil {
		s.redirectAdmin(w, r, path, "", "Enter a buy-in in BTC, for example 0.00200000.")
		return
	}

	if err := s.poker.Sit(r.Context(), user, tableID, seatNumber, buyInSat); err != nil {
		if refusal, ok := compliance.AsRefusal(err); ok {
			s.redirectAdmin(w, r, path, "", refusal.Message)
			return
		}
		s.redirectAdmin(w, r, path, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, path, "Seated. Good luck.", "")
}

func (s *Server) handlePokerLeave(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	_, tableID, ok := s.pokerTableFor(w, r)
	if !ok {
		return
	}
	user, _ := CurrentUser(r.Context())
	path := "/poker/tables/" + strconv.FormatInt(tableID, 10)

	returned, err := s.poker.Leave(r.Context(), tableID, user.ID)
	if err != nil {
		s.redirectAdmin(w, r, path, "", trimErrPrefix(err.Error()))
		return
	}
	if returned == 0 {
		s.redirectAdmin(w, r, path,
			"You will be stood up when this hand finishes; your chips are still in play.", "")
		return
	}
	s.redirectAdmin(w, r, "/poker",
		money.FormatBTC(returned)+" BTC returned to your balance.", "")
}

func (s *Server) handlePokerAct(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "could not verify that request"})
		return
	}
	table, tableID, ok := s.pokerTableFor(w, r)
	if !ok {
		return
	}
	_ = tableID
	user, _ := CurrentUser(r.Context())

	action := poker.Action(strings.TrimSpace(r.FormValue("action")))
	var amountSat int64
	if raw := strings.TrimSpace(r.FormValue("amount")); raw != "" {
		// The interface sends satoshi directly, since the slider works in
		// chips rather than BTC.
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that amount could not be read"})
			return
		}
		amountSat = parsed
	}

	if err := table.Act(r.Context(), user.ID, action, amountSat); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, poker.ErrNotYourTurn) || errors.Is(err, poker.ErrHandComplete) {
			status = http.StatusConflict
		}
		s.writeJSON(w, status, map[string]string{"error": trimErrPrefix(err.Error())})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePokerSeed(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	table, tableID, ok := s.pokerTableFor(w, r)
	if !ok {
		return
	}
	user, _ := CurrentUser(r.Context())
	path := "/poker/tables/" + strconv.FormatInt(tableID, 10)

	if err := table.SetClientSeed(user.ID, strings.TrimSpace(r.FormValue("seed"))); err != nil {
		if errors.Is(err, poker.ErrHandInProgress) {
			s.redirectAdmin(w, r, path, "",
				"You cannot change your seed during a hand: the deck for this hand is already committed.")
			return
		}
		s.redirectAdmin(w, r, path, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, path, "Your seed will be used from the next hand.", "")
}

func (s *Server) handlePokerVideoConsent(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "could not verify that request"})
		return
	}
	table, _, ok := s.pokerTableFor(w, r)
	if !ok {
		return
	}
	user, _ := CurrentUser(r.Context())

	consent := r.FormValue("consent") == "1"
	if consent && !table.Config.VideoEnabled {
		s.writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "camera and microphone are not available at this table"})
		return
	}
	if err := table.SetVideoConsent(user.ID, consent); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": trimErrPrefix(err.Error())})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"consent": consent})
}

// handlePokerSignal relays one WebRTC handshake message to another seat.
func (s *Server) handlePokerSignal(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "could not verify that request"})
		return
	}
	table, tableID, ok := s.pokerTableFor(w, r)
	if !ok {
		return
	}
	user, _ := CurrentUser(r.Context())

	view := table.View(user.ID)
	if view.YourSeat < 0 {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "you are not seated at this table"})
		return
	}
	// Only a seat that has consented may negotiate media at all, so a player
	// cannot open a peer connection to somebody by pretending to share.
	if !view.Seats[view.YourSeat].VideoConsent {
		s.writeJSON(w, http.StatusForbidden,
			map[string]string{"error": "turn your camera on before connecting to other players"})
		return
	}

	to, err := strconv.Atoi(r.FormValue("to"))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no recipient"})
		return
	}
	// The recipient must be sharing too. Signalling at a seat that has not
	// consented would be a way to probe whether somebody is there.
	sharing := false
	for _, seat := range view.VideoPeers {
		if seat == to {
			sharing = true
		}
	}
	if !sharing {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "that seat is not sharing"})
		return
	}

	kind := r.FormValue("kind")
	switch kind {
	case "offer", "answer", "ice", "bye":
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown signal"})
		return
	}
	payload := r.FormValue("payload")
	// A session description is a few kilobytes; anything much larger is not a
	// handshake.
	if len(payload) > 64*1024 {
		s.writeJSON(w, http.StatusRequestEntityTooLarge,
			map[string]string{"error": "that signal is too large"})
		return
	}

	if err := s.signals.Send(tableID, view.YourSeat, pokerhouse.Signal{
		To: to, Kind: kind, Payload: payload,
	}); err != nil {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "that peer is not keeping up"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// ---------------------------------------------------------------------------
// Hand histories and verification
// ---------------------------------------------------------------------------

func (s *Server) handlePokerHand(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	handID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such hand.")
		return
	}
	hand, err := s.store.GetPokerHand(ctx, handID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such hand.")
		return
	}

	var viewerID int64
	if user, ok := CurrentUser(ctx); ok {
		viewerID = user.ID
	}
	players, err := s.store.PokerHandPlayers(ctx, handID, viewerID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	actions, err := s.store.PokerActionsForHand(ctx, handID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Re-derive the deck from the published seed, so the page shows the same
	// check a player would run themselves rather than asserting it passed.
	var deck []string
	var verifyErr string
	if hand.Verifiable() {
		cards, err := poker.Verify(hand.Commitment, hand.ServerSeed, hand.ClientSeed, hand.HandNumber)
		if err != nil {
			verifyErr = err.Error()
		} else {
			for _, card := range cards {
				deck = append(deck, card.String())
			}
		}
	}

	s.render(w, r, http.StatusOK, "poker-hand.html", map[string]any{
		"Title":     fmt.Sprintf("Hand %d", hand.HandNumber),
		"Hand":      hand,
		"Players":   players,
		"Actions":   actions,
		"Deck":      deck,
		"VerifyErr": verifyErr,
		"Wide":      true,
	})
}

func (s *Server) handlePokerVerifyForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "poker-verify.html", map[string]any{
		"Title": "Verify a hand",
	})
}

// handlePokerVerify recomputes a deck from a commitment and a revealed seed.
//
// It takes whatever the player pastes in and does the check in front of them.
// The same computation is described on the page so it can be reimplemented
// independently: a fairness proof that only works on our server is not a
// fairness proof.
func (s *Server) handlePokerVerify(w http.ResponseWriter, r *http.Request) {
	commitment := strings.TrimSpace(r.FormValue("commitment"))
	serverSeed := strings.TrimSpace(r.FormValue("server_seed"))
	clientSeed := strings.TrimSpace(r.FormValue("client_seed"))
	nonce, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("nonce")), 10, 64)

	data := map[string]any{
		"Title":      "Verify a hand",
		"Commitment": commitment,
		"ServerSeed": serverSeed,
		"ClientSeed": clientSeed,
		"Nonce":      nonce,
	}

	cards, err := poker.Verify(commitment, serverSeed, clientSeed, nonce)
	if err != nil {
		data["Error"] = trimErrPrefix(err.Error())
		s.render(w, r, http.StatusOK, "poker-verify.html", data)
		return
	}
	deck := make([]string, 0, len(cards))
	for _, card := range cards {
		deck = append(deck, card.String())
	}
	data["Deck"] = deck
	data["Verified"] = true
	s.render(w, r, http.StatusOK, "poker-verify.html", data)
}

func (s *Server) handlePokerMyHands(w http.ResponseWriter, r *http.Request) {
	user, _ := CurrentUser(r.Context())
	hands, err := s.store.PokerHandsForUser(r.Context(), user.ID, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "poker-hands.html", map[string]any{
		"Title": "My poker hands",
		"Hands": hands,
	})
}

var _ = store.PokerTable{}

// ---------------------------------------------------------------------------
// Back office
// ---------------------------------------------------------------------------

func (s *Server) handleAdminPoker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tables, err := s.store.ListPokerTables(ctx, false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Chips at each table must equal the ledger balance held for it. A
	// mismatch means chips have been created or lost, which is the one thing
	// at a poker table that must never happen quietly.
	type row struct {
		Table     store.PokerTable
		ChipsSat  int64
		LedgerSat int64
		Balanced  bool
		Live      bool
	}
	rows := make([]row, 0, len(tables))
	for _, table := range tables {
		entry := row{Table: table}
		if chips, ledger, err := s.poker.ReconcileTable(ctx, table.ID); err == nil {
			entry.ChipsSat, entry.LedgerSat = chips, ledger
			entry.Balanced = chips == ledger
			entry.Live = true
		} else {
			// A closed table is not loaded, so only its ledger balance exists.
			ledger, ledgerErr := s.store.PokerTableBalanceSat(ctx, table.ID)
			if ledgerErr != nil {
				s.serverError(w, r, ledgerErr)
				return
			}
			entry.LedgerSat = ledger
			entry.Balanced = ledger == 0
		}
		rows = append(rows, entry)
	}

	rake, err := s.store.AccountBalanceSat(ctx, store.AccountHouseRake)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "admin-poker.html", map[string]any{
		"Title":   "Poker tables",
		"Flash":   r.URL.Query().Get("flash"),
		"Error":   r.URL.Query().Get("error"),
		"Rows":    rows,
		"RakeSat": rake,
		"Wide":    true,
	})
}

func (s *Server) handleAdminCreatePokerTable(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	ctx := r.Context()

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.redirectAdmin(w, r, "/admin/poker", "", "Give the table a name.")
		return
	}
	bigBlind, err := money.ParseBTC(r.FormValue("big_blind"))
	if err != nil || bigBlind <= 0 {
		s.redirectAdmin(w, r, "/admin/poker", "", "Enter a big blind in BTC, for example 0.00000200.")
		return
	}
	seats, _ := strconv.Atoi(r.FormValue("max_seats"))
	if seats < 2 || seats > 9 {
		seats = 6
	}
	rakeBps, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("rake_bps")), 10, 64)
	if rakeBps < 0 || rakeBps > 1000 {
		s.redirectAdmin(w, r, "/admin/poker", "",
			"Rake must be between 0 and 1000 basis points. Anything higher is not a rake, it is a toll.")
		return
	}

	if _, err := s.store.CreatePokerTable(ctx, store.PokerTable{
		Name: name, SmallBlindSat: bigBlind / 2, BigBlindSat: bigBlind,
		MinBuyInSat: bigBlind * 20, MaxBuyInSat: bigBlind * 100,
		MaxSeats: seats, RakeBps: rakeBps, RakeCapSat: bigBlind * 3,
		NoFlopNoDrop: true, ActionSeconds: 30,
		VideoEnabled: r.FormValue("video") != "", Active: true,
	}); err != nil {
		s.redirectAdmin(w, r, "/admin/poker", "", trimErrPrefix(err.Error()))
		return
	}
	if err := s.poker.Load(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectAdmin(w, r, "/admin/poker", "Table created and open.", "")
}

func (s *Server) handleAdminTogglePokerTable(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	tableID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such table.")
		return
	}
	active := r.FormValue("active") == "1"

	// Closing a table with players still sitting would strand their chips, so
	// it is refused until the seats are empty.
	if !active {
		if table, ok := s.poker.Table(tableID); ok && len(table.SeatedUsers()) > 0 {
			s.redirectAdmin(w, r, "/admin/poker", "",
				"There are still players seated. They have to cash out before the table can close.")
			return
		}
	}
	if err := s.store.SetPokerTableActive(r.Context(), tableID, active); err != nil {
		s.serverError(w, r, err)
		return
	}
	if active {
		if err := s.poker.Load(r.Context()); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	state := "closed"
	if active {
		state = "open"
	}
	s.redirectAdmin(w, r, "/admin/poker", "Table is now "+state+".", "")
}
