package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/vesal1/avaswebsite/internal/blackjack"
	"github.com/vesal1/avaswebsite/internal/casino"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/mines"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// The table games: blackjack and Mines. Pages render the frame; the play
// itself is form posts returning JSON, exactly like a slot spin — same CSRF
// discipline, same ledger, same verification trail.

// ---------------------------------------------------------------------------
// JSON views
// ---------------------------------------------------------------------------

type cardView struct {
	Rank string `json:"rank"`
	Suit string `json:"suit"`
	Red  bool   `json:"red"`
}

func cardViews(cards []blackjack.Card) []cardView {
	out := make([]cardView, len(cards))
	for i, card := range cards {
		out[i] = cardView{Rank: card.Rank(), Suit: card.Suit(), Red: card.Red()}
	}
	return out
}

type blackjackHandView struct {
	Cards    []cardView `json:"cards"`
	Total    int        `json:"total"`
	Soft     bool       `json:"soft"`
	BetUnits int        `json:"bet_units"`
	Busted   bool       `json:"busted"`
	Stood    bool       `json:"stood"`
	Active   bool       `json:"active"`
}

type blackjackView struct {
	Open         bool                `json:"open"`
	RoundID      int64               `json:"round_id,omitempty"`
	Status       string              `json:"status,omitempty"`
	StakeSat     int64               `json:"stake_sat,omitempty"`
	BaseStakeSat int64               `json:"base_stake_sat,omitempty"`
	WinSat       int64               `json:"win_sat"`
	WinBTC       string              `json:"win_btc,omitempty"`
	Player       []blackjackHandView `json:"player,omitempty"`
	Dealer       []cardView          `json:"dealer,omitempty"`
	DealerHole   bool                `json:"dealer_hole_hidden"`
	DealerTotal  int                 `json:"dealer_total,omitempty"`
	Actions      []blackjack.Action  `json:"actions,omitempty"`
	Advice       blackjack.Action    `json:"advice,omitempty"`
	PlayerBJ     bool                `json:"player_blackjack,omitempty"`
	DealerBJ     bool                `json:"dealer_blackjack,omitempty"`
	BalanceBTC   string              `json:"balance_btc,omitempty"`
	Commitment   string              `json:"commitment,omitempty"`
	Nonce        int64               `json:"nonce,omitempty"`
}

func (s *Server) blackjackJSON(r *http.Request, row store.CasinoRound, round *blackjack.Round, baseStake int64) blackjackView {
	view := blackjackView{
		Open:         row.Open(),
		RoundID:      row.ID,
		Status:       row.Status,
		StakeSat:     row.StakeSat,
		BaseStakeSat: baseStake,
		WinSat:       row.WinSat,
		WinBTC:       money.FormatBTC(row.WinSat),
		DealerHole:   row.Open(),
		PlayerBJ:     round.State.PlayerBlackjack,
		DealerBJ:     round.State.DealerBlackjack,
		Commitment:   row.Commitment,
		Nonce:        row.Nonce,
	}
	for i := range round.State.Player {
		hand := &round.State.Player[i]
		total, soft := hand.Total()
		view.Player = append(view.Player, blackjackHandView{
			Cards: cardViews(hand.Cards), Total: total, Soft: soft,
			BetUnits: hand.BetUnits, Busted: hand.Busted, Stood: hand.Stood,
			Active: row.Open() && i == round.State.Active,
		})
	}
	if row.Open() {
		// The hole card stays face down: the view shows the upcard and a
		// marker, and the JSON never carries what the player must not know.
		view.Dealer = cardViews(round.State.Dealer[:1])
	} else {
		view.Dealer = cardViews(round.State.Dealer)
		total, _ := blackjack.Total(round.State.Dealer)
		view.DealerTotal = total
	}
	if row.Open() {
		view.Actions = round.Available()
		if advice, ok := blackjack.Advise(round); ok {
			view.Advice = advice
		}
	}
	if user, ok := CurrentUser(r.Context()); ok {
		if balance, err := s.store.BalanceSat(r.Context(), user.ID); err == nil {
			view.BalanceBTC = money.FormatBTC(balance)
		}
	}
	return view
}

type minesView struct {
	Open           bool    `json:"open"`
	RoundID        int64   `json:"round_id,omitempty"`
	Status         string  `json:"status,omitempty"`
	MineCount      int     `json:"mine_count,omitempty"`
	StakeSat       int64   `json:"stake_sat,omitempty"`
	WinSat         int64   `json:"win_sat"`
	WinBTC         string  `json:"win_btc,omitempty"`
	Revealed       []int   `json:"revealed"`
	Reveals        int     `json:"reveals"`
	MaxReveals     int     `json:"max_reveals,omitempty"`
	MultX10000     int64   `json:"mult_x10000"`
	NextMultX10000 int64   `json:"next_mult_x10000,omitempty"`
	CashOutSat     int64   `json:"cash_out_sat"`
	CashOutBTC     string  `json:"cash_out_btc,omitempty"`
	Hit            int     `json:"hit"`
	Mines          []int   `json:"mines,omitempty"`
	CashedOut      bool    `json:"cashed_out,omitempty"`
	BalanceBTC     string  `json:"balance_btc,omitempty"`
	Commitment     string  `json:"commitment,omitempty"`
	Nonce          int64   `json:"nonce,omitempty"`
	LadderX10000   []int64 `json:"ladder_x10000,omitempty"`
}

func (s *Server) minesJSON(r *http.Request, row store.CasinoRound, round *mines.Round) minesView {
	view := minesView{
		Open: row.Open(), RoundID: row.ID, Status: row.Status,
		MineCount: round.State.MineCount, StakeSat: row.StakeSat,
		WinSat: row.WinSat, WinBTC: money.FormatBTC(row.WinSat),
		Revealed: append([]int{}, round.State.Revealed...),
		Reveals:  round.Reveals(), MaxReveals: mines.MaxReveals(round.State.MineCount),
		MultX10000: round.MultiplierX10000(),
		Hit:        round.State.Hit, CashedOut: round.State.CashedOut,
		Commitment: row.Commitment, Nonce: row.Nonce,
	}
	if view.Revealed == nil {
		view.Revealed = []int{}
	}
	if ladder, err := mines.Ladder(round.State.MineCount); err == nil {
		view.LadderX10000 = ladder
		if next := round.Reveals() + 1; next < len(ladder) {
			view.NextMultX10000 = ladder[next]
		}
	}
	if row.Open() && round.Reveals() > 0 {
		view.CashOutSat = round.PayoutSat(row.StakeSat)
		view.CashOutBTC = money.FormatBTC(view.CashOutSat)
	}
	if !row.Open() {
		// Only a settled board shows where the mines were.
		view.Mines = round.Mines()
	}
	if user, ok := CurrentUser(r.Context()); ok {
		if balance, err := s.store.BalanceSat(r.Context(), user.ID); err == nil {
			view.BalanceBTC = money.FormatBTC(balance)
		}
	}
	return view
}

// tableError turns a service refusal into the JSON the game frames show.
func (s *Server) tableError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	message := trimErrPrefix(err.Error())
	if refusal, ok := compliance.AsRefusal(err); ok {
		message = refusal.Message
		status = http.StatusForbidden
	}
	switch {
	case errors.Is(err, casino.ErrInsufficientFunds):
		status = http.StatusPaymentRequired
	case errors.Is(err, store.ErrNoOpenRound):
		status = http.StatusConflict
		message = "That round has finished. Start another?"
	case errors.Is(err, casino.ErrRoundOpen):
		status = http.StatusConflict
		message = "You already have a round in play — it has been picked back up."
	}
	s.writeJSON(w, status, map[string]any{"error": message})
}

// ---------------------------------------------------------------------------
// Blackjack
// ---------------------------------------------------------------------------

// strategyRow is one line of the printed strategy card.
type strategyRow struct {
	Label string
	Cells []string
}

// strategyUpcards is the column order every printed card uses.
var strategyUpcards = []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 1}

func upcardLabel(value int) string {
	if value == 1 {
		return "A"
	}
	return strconv.Itoa(value)
}

func adviceLetter(advice blackjack.Advice) string {
	switch advice.Primary {
	case blackjack.Double:
		return "D"
	case blackjack.Hit:
		return "H"
	default:
		return "S"
	}
}

// strategyRows renders the DP's card into the three familiar blocks.
func strategyRows() (hard, soft, pairs []strategyRow, upcards []string) {
	card := blackjack.CardTable()
	for _, up := range strategyUpcards {
		upcards = append(upcards, upcardLabel(up))
	}
	for total := 5; total <= 20; total++ {
		row := strategyRow{Label: strconv.Itoa(total)}
		for _, up := range strategyUpcards {
			row.Cells = append(row.Cells, adviceLetter(card.Hard[total][up]))
		}
		hard = append(hard, row)
	}
	for total := 13; total <= 20; total++ {
		row := strategyRow{Label: "A," + strconv.Itoa(total-11)}
		for _, up := range strategyUpcards {
			row.Cells = append(row.Cells, adviceLetter(card.Soft[total][up]))
		}
		soft = append(soft, row)
	}
	for pair := 1; pair <= 10; pair++ {
		label := upcardLabel(pair) + "," + upcardLabel(pair)
		row := strategyRow{Label: label}
		for _, up := range strategyUpcards {
			if card.Pairs[pair][up] {
				row.Cells = append(row.Cells, "P")
				continue
			}
			// The card for the un-split pair: aces read as soft 12, the rest
			// as their hard total.
			if pair == 1 {
				row.Cells = append(row.Cells, "H")
				continue
			}
			row.Cells = append(row.Cells, adviceLetter(card.Hard[pair*2][up]))
		}
		pairs = append(pairs, row)
	}
	return hard, soft, pairs, upcards
}

func (s *Server) handleBlackjackPage(w http.ResponseWriter, r *http.Request) {
	hard, soft, pairs, upcards := strategyRows()
	data := map[string]any{
		"Title":        "Blackjack",
		"RTPBps":       int64(blackjack.PublishedRTPBps),
		"StrategyHard": hard,
		"StrategySoft": soft,
		"StrategyPair": pairs,
		"Upcards":      upcards,
		"StakeLevels":  casino.BlackjackStakeLevels,
		"Wide":         true,
		"Flash":        r.URL.Query().Get("flash"),
		"Error":        r.URL.Query().Get("error"),
	}
	if user, ok := CurrentUser(r.Context()); ok {
		if seed, err := s.casino.Seed(r.Context(), user.ID); err == nil {
			data["Seed"] = seed
		}
	}
	s.render(w, r, http.StatusOK, "casino-blackjack.html", data)
}

func (s *Server) handleBlackjackState(w http.ResponseWriter, r *http.Request) {
	user, _ := CurrentUser(r.Context())
	row, round, baseStake, err := s.casino.OpenBlackjack(r.Context(), user.ID)
	if errors.Is(err, store.ErrNoOpenRound) {
		s.writeJSON(w, http.StatusOK, blackjackView{Open: false})
		return
	}
	if err != nil {
		s.tableError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.blackjackJSON(r, row, round, baseStake))
}

func (s *Server) handleBlackjackDeal(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "That request could not be verified. Reload and try again."})
		return
	}
	user, _ := CurrentUser(r.Context())
	stakeSat, err := money.ParseBTC(r.FormValue("stake"))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Choose a stake from the list."})
		return
	}
	row, err := s.casino.StartBlackjack(r.Context(), user, stakeSat)
	if err != nil {
		s.tableError(w, err)
		return
	}
	// The deal may have settled on a natural; render whatever came back.
	if row.Open() {
		liveRow, round, baseStake, err := s.casino.OpenBlackjack(r.Context(), user.ID)
		if err != nil {
			s.tableError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, s.blackjackJSON(r, liveRow, round, baseStake))
		return
	}
	s.settledBlackjackJSON(w, r, row)
}

func (s *Server) settledBlackjackJSON(w http.ResponseWriter, r *http.Request, row store.CasinoRound) {
	verified, err := s.casino.VerifyRound(r.Context(), row.ID)
	if err != nil && verified.Blackjack == nil {
		// Replay for display only; a sealed seed still renders from state.
		s.writeJSON(w, http.StatusOK, map[string]any{"open": false, "round_id": row.ID})
		return
	}
	round := verified.Blackjack
	if round == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"open": false, "round_id": row.ID})
		return
	}
	s.writeJSON(w, http.StatusOK, s.blackjackJSON(r, row, round, row.StakeSat))
}

func (s *Server) handleBlackjackAct(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "That request could not be verified. Reload and try again."})
		return
	}
	user, _ := CurrentUser(r.Context())
	action := blackjack.Action(r.FormValue("action"))
	switch action {
	case blackjack.Hit, blackjack.Stand, blackjack.Double, blackjack.Split:
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "That is not a play."})
		return
	}
	row, round, err := s.casino.BlackjackAct(r.Context(), user, action)
	if err != nil {
		s.tableError(w, err)
		return
	}
	var state struct {
		BaseStakeSat int64 `json:"base_stake_sat"`
	}
	baseStake := row.StakeSat
	if err := json.Unmarshal([]byte(row.State), &state); err == nil && state.BaseStakeSat > 0 {
		baseStake = state.BaseStakeSat
	}
	s.writeJSON(w, http.StatusOK, s.blackjackJSON(r, row, round, baseStake))
}

// ---------------------------------------------------------------------------
// Mines
// ---------------------------------------------------------------------------

func (s *Server) handleMinesPage(w http.ResponseWriter, r *http.Request) {
	ladders := map[int][]int64{}
	worst := map[int]int64{}
	for _, count := range mines.MineOptions {
		if ladder, err := mines.Ladder(count); err == nil {
			ladders[count] = ladder
		}
		if rtp, err := mines.WorstStepRTPBps(count); err == nil {
			worst[count] = rtp
		}
	}
	laddersJSON, _ := json.Marshal(ladders)
	data := map[string]any{
		"Title":       "Mines",
		"MineOptions": mines.MineOptions,
		"Ladders":     ladders,
		"LaddersJSON": string(laddersJSON),
		"WorstRTP":    worst,
		"StakeLevels": mines.StakeLevels,
		"CapX10000":   int64(mines.CapX10000),
		"Wide":        true,
		"Flash":       r.URL.Query().Get("flash"),
		"Error":       r.URL.Query().Get("error"),
	}
	if user, ok := CurrentUser(r.Context()); ok {
		if seed, err := s.casino.Seed(r.Context(), user.ID); err == nil {
			data["Seed"] = seed
		}
	}
	s.render(w, r, http.StatusOK, "casino-mines.html", data)
}

func (s *Server) handleMinesState(w http.ResponseWriter, r *http.Request) {
	user, _ := CurrentUser(r.Context())
	row, round, err := s.casino.OpenMines(r.Context(), user.ID)
	if errors.Is(err, store.ErrNoOpenRound) {
		s.writeJSON(w, http.StatusOK, minesView{Open: false, Revealed: []int{}, Hit: -1})
		return
	}
	if err != nil {
		s.tableError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.minesJSON(r, row, round))
}

func (s *Server) handleMinesStart(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "That request could not be verified. Reload and try again."})
		return
	}
	user, _ := CurrentUser(r.Context())
	stakeSat, err := money.ParseBTC(r.FormValue("stake"))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Choose a stake from the list."})
		return
	}
	mineCount, err := strconv.Atoi(r.FormValue("mines"))
	if err != nil || !mines.ValidMines(mineCount) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Choose a mine count from the list."})
		return
	}
	if _, err := s.casino.StartMines(r.Context(), user, mineCount, stakeSat); err != nil {
		s.tableError(w, err)
		return
	}
	row, round, err := s.casino.OpenMines(r.Context(), user.ID)
	if err != nil {
		s.tableError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.minesJSON(r, row, round))
}

func (s *Server) handleMinesReveal(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "That request could not be verified. Reload and try again."})
		return
	}
	user, _ := CurrentUser(r.Context())
	cell, err := strconv.Atoi(r.FormValue("cell"))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "That is not a tile."})
		return
	}
	row, round, err := s.casino.MinesReveal(r.Context(), user, cell)
	if err != nil {
		s.tableError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.minesJSON(r, row, round))
}

func (s *Server) handleMinesCashOut(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "That request could not be verified. Reload and try again."})
		return
	}
	user, _ := CurrentUser(r.Context())
	row, round, err := s.casino.MinesCashOut(r.Context(), user)
	if err != nil {
		s.tableError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.minesJSON(r, row, round))
}

// ---------------------------------------------------------------------------
// Round detail and verification
// ---------------------------------------------------------------------------

func (s *Server) handleCasinoRound(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such round.")
		return
	}
	verified, err := s.casino.VerifyRound(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such round.")
		return
	}
	user, _ := CurrentUser(r.Context())
	if verified.Round.UserID != user.ID && !user.IsStaff() {
		s.renderError(w, r, http.StatusNotFound, "No such round.")
		return
	}
	problem := ""
	if err != nil {
		problem = trimErrPrefix(err.Error())
	}
	s.render(w, r, http.StatusOK, "casino-round.html", map[string]any{
		"Title":    titleCase(verified.Round.GameKey) + " round " + strconv.FormatInt(id, 10),
		"Verified": verified,
		"Round":    verified.Round,
		"Problem":  problem,
	})
}
