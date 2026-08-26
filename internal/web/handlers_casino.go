package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/vesal1/avaswebsite/internal/casino"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/slots"
	"github.com/vesal1/avaswebsite/internal/store"
)

// gameCard is a machine as the lobby shows it: what it costs, what it returns
// and how it behaves. The return is the computed figure, not a claim.
type gameCard struct {
	Game  *slots.Game
	Maths slots.Maths
}

func (s *Server) handleCasinoLobby(w http.ResponseWriter, r *http.Request) {
	games := s.casino.Games()
	cards := make([]gameCard, 0, len(games))
	for _, game := range games {
		maths, _ := slots.MathsFor(game.Key)
		cards = append(cards, gameCard{Game: game, Maths: maths})
	}

	data := map[string]any{
		"Title": "Casino",
		"Games": cards,
		"Flash": r.URL.Query().Get("flash"),
		"Error": r.URL.Query().Get("error"),
	}
	if user, ok := CurrentUser(r.Context()); ok {
		if totals, err := s.casino.Totals(r.Context(), user.ID); err == nil {
			data["Totals"] = totals
		}
	}
	s.render(w, r, http.StatusOK, "casino.html", data)
}

func (s *Server) handleCasinoGame(w http.ResponseWriter, r *http.Request) {
	game, maths, err := s.casino.Game(r.PathValue("game"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such game.")
		return
	}

	data := map[string]any{
		"Title":    game.Name,
		"Game":     game,
		"Maths":    maths,
		"Paytable": paytableRows(game),
		"Symbols":  game.Symbols,
		"Flash":    r.URL.Query().Get("flash"),
		"Error":    r.URL.Query().Get("error"),
		"Wide":     true,
	}
	if user, ok := CurrentUser(r.Context()); ok {
		seed, err := s.casino.Seed(r.Context(), user.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		data["Seed"] = seed
		if spins, err := s.casino.History(r.Context(), user.ID, 10); err == nil {
			data["Recent"] = spins
		}
	}
	s.render(w, r, http.StatusOK, "casino-game.html", data)
}

// paytableRow is one symbol's line values, ready for a template.
type paytableRow struct {
	Symbol slots.Symbol
	// Pays is indexed by how many the line shows; entries below the minimum
	// are omitted so the table reads like a paytable rather than a matrix.
	Pays []paytableEntry
	Note string
}

type paytableEntry struct {
	Count int
	X100  int64
}

func paytableRows(game *slots.Game) []paytableRow {
	var rows []paytableRow
	for id, symbol := range game.Symbols {
		table, ok := game.PayX100[slots.SymbolID(id)]
		if !ok {
			note := ""
			switch {
			case symbol.Wild:
				note = "Stands in for every symbol except the scatter. Has no value of its own."
			case symbol.Scatter:
				note = "Pays on the total stake wherever it lands, and starts the free spins."
			case symbol.Blank:
				continue
			}
			rows = append(rows, paytableRow{Symbol: symbol, Note: note})
			continue
		}
		row := paytableRow{Symbol: symbol}
		for count, value := range table {
			if value > 0 {
				row.Pays = append(row.Pays, paytableEntry{Count: count, X100: value})
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// handleCasinoPlay takes a stake and returns the round.
//
// The reply is JSON so the page can animate the reels, but the request is an
// ordinary form post: that keeps it inside the same CSRF protection as every
// other action on the site rather than inventing a second, weaker path for the
// one part of the product that spends money fastest.
func (s *Server) handleCasinoPlay(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "That request could not be verified. Reload the page and try again.",
		})
		return
	}
	user, _ := CurrentUser(r.Context())

	stakeSat, err := money.ParseBTC(r.FormValue("stake"))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "Choose a stake from the list.",
		})
		return
	}

	result, err := s.casino.Spin(r.Context(), user, r.PathValue("game"), stakeSat)
	if err != nil {
		status := http.StatusBadRequest
		message := trimErrPrefix(err.Error())
		if refusal, ok := compliance.AsRefusal(err); ok {
			message = refusal.Message
			status = http.StatusForbidden
		}
		switch {
		case errors.Is(err, casino.ErrUnknownGame):
			status = http.StatusNotFound
		case errors.Is(err, casino.ErrInsufficientFunds):
			status = http.StatusPaymentRequired
		}
		// A spin that settled but could not advance a wagering requirement has
		// still happened: the player must be shown the round, not an error
		// that makes them think their stake vanished.
		if result.SpinID == 0 {
			s.writeJSON(w, status, map[string]any{"error": message})
			return
		}
		s.log.Error("casino spin", "spin", result.SpinID, "error", err)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"spin_id":        result.SpinID,
		"round":          result.Round,
		"stake_sat":      result.StakeSat,
		"win_sat":        result.WinSat,
		"net_sat":        result.NetSat(),
		"from_bonus_sat": result.FromBonusSat,
		"balance_sat":    result.BalanceSat,
		"balance_btc":    money.FormatBTC(result.BalanceSat),
		"win_btc":        money.FormatBTC(result.WinSat),
		"commitment":     result.Seed.Commitment,
		"client_seed":    result.Seed.ClientSeed,
		"nonce":          result.Round.Nonce,
	})
}

func (s *Server) handleCasinoHistory(w http.ResponseWriter, r *http.Request) {
	user, _ := CurrentUser(r.Context())
	spins, err := s.casino.History(r.Context(), user.ID, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	totals, err := s.casino.Totals(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	seed, err := s.casino.Seed(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "casino-history.html", map[string]any{
		"Title":  "My spins",
		"Spins":  spins,
		"Totals": totals,
		"Seed":   seed,
		"Flash":  r.URL.Query().Get("flash"),
		"Error":  r.URL.Query().Get("error"),
	})
}

// handleCasinoSpin shows one round and, once the seed behind it is published,
// replays it independently.
func (s *Server) handleCasinoSpin(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such spin.")
		return
	}
	verified, err := s.casino.Verify(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such spin.")
		return
	}
	user, _ := CurrentUser(r.Context())
	if verified.Spin.UserID != user.ID && !user.IsStaff() {
		// Somebody else's spin is somebody else's business.
		s.renderError(w, r, http.StatusNotFound, "No such spin.")
		return
	}

	problem := ""
	if err != nil {
		problem = trimErrPrefix(err.Error())
	}
	s.render(w, r, http.StatusOK, "casino-spin.html", map[string]any{
		"Title":    fmt.Sprintf("Spin %d", verified.Spin.ID),
		"Verified": verified,
		"Spin":     verified.Spin,
		"Problem":  problem,
	})
}

func (s *Server) handleCasinoSeed(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	back := backTo(r, "/casino")

	if err := s.casino.SetClientSeed(r.Context(), user.ID, r.FormValue("client_seed")); err != nil {
		s.redirectAdmin(w, r, back, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, back, "Your seed is set. It is mixed into every spin from here on.", "")
}

func (s *Server) handleCasinoRotate(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	back := backTo(r, "/casino/history")

	retired, _, err := s.casino.Rotate(r.Context(), user.ID, r.FormValue("client_seed"))
	if err != nil {
		s.redirectAdmin(w, r, back, "", trimErrPrefix(err.Error()))
		return
	}
	s.redirectAdmin(w, r, back, fmt.Sprintf(
		"Seed published: %s. Every spin you took under it can now be checked.",
		retired.PublishedSeed()), "")
}

// handleCasinoFairness explains the scheme and lets anybody check a round from
// its published seed, without needing an account.
func (s *Server) handleCasinoFairness(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "casino-fairness.html", map[string]any{
		"Title": "How the casino games are decided",
		"Games": s.casino.Games(),
	})
}

// backTo picks a safe redirect target from the form, falling back to a known
// page. Only same-site paths are honoured: a redirect target taken from a form
// is otherwise an open redirect.
func backTo(r *http.Request, fallback string) string {
	target := r.FormValue("back")
	if len(target) > 1 && target[0] == '/' && target[1] != '/' {
		return target
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Back office
// ---------------------------------------------------------------------------

// floorRow pairs a machine's published maths with what it has actually done.
type floorRow struct {
	Game   *slots.Game
	Maths  slots.Maths
	Totals store.CasinoGameTotals
}

// GapBps is how far the machine's actual return sits from its published one.
// A large gap over a small number of spins is variance; a large gap over a
// large number is a bug worth stopping the machine for.
func (r floorRow) GapBps() int64 {
	if r.Totals.Spins == 0 {
		return 0
	}
	return r.Totals.ReturnBps() - r.Maths.RTPBps
}

func (s *Server) handleAdminCasino(w http.ResponseWriter, r *http.Request) {
	totals, err := s.store.CasinoTotalsByGame(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	byGame := make(map[string]store.CasinoGameTotals, len(totals))
	for _, row := range totals {
		byGame[row.GameKey] = row
	}

	rows := make([]floorRow, 0, len(s.casino.Games()))
	var staked, returned int64
	for _, game := range s.casino.Games() {
		maths, _ := slots.MathsFor(game.Key)
		row := floorRow{Game: game, Maths: maths, Totals: byGame[game.Key]}
		staked += row.Totals.StakeSat
		returned += row.Totals.WinSat
		rows = append(rows, row)
	}

	wins, err := s.store.BiggestCasinoWins(r.Context(), 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	house, err := s.store.AccountBalanceSat(r.Context(), store.AccountHouseRevenue)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "admin-casino.html", map[string]any{
		"Title":       "Casino floor",
		"Rows":        rows,
		"StakedSat":   staked,
		"ReturnedSat": returned,
		"HoldSat":     staked - returned,
		"HouseSat":    house,
		"BiggestWins": wins,
	})
}
