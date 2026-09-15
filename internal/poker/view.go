package poker

import "time"

// SeatView is one seat as a particular viewer is allowed to see it.
type SeatView struct {
	Number       int    `json:"seat"`
	Occupied     bool   `json:"occupied"`
	PlayerID     int64  `json:"player_id,omitempty"`
	Name         string `json:"name,omitempty"`
	StackSat     int64  `json:"stack_sat"`
	CommittedSat int64  `json:"committed_sat"`
	Status       string `json:"status"`
	// Cards is populated only for the viewer's own seat, or for a seat shown
	// at showdown. Everybody else's hole cards are face down.
	Cards []string `json:"cards,omitempty"`
	// HasCards says a seat holds cards without saying what they are, so the
	// interface can draw face-down cards.
	HasCards     bool `json:"has_cards"`
	IsButton     bool `json:"is_button"`
	IsTurn       bool `json:"is_turn"`
	IsYou        bool `json:"is_you"`
	VideoConsent bool `json:"video_consent"`
	TimedOut     bool `json:"timed_out"`
}

// PotView is a pot as shown at the table.
type PotView struct {
	AmountSat int64 `json:"amount_sat"`
	Eligible  []int `json:"eligible"`
}

// TableView is everything one viewer may see.
type TableView struct {
	TableID    int64  `json:"table_id"`
	Name       string `json:"name"`
	Variant    string `json:"variant"`
	HandNumber int64  `json:"hand_number"`

	SmallBlindSat int64 `json:"small_blind_sat"`
	BigBlindSat   int64 `json:"big_blind_sat"`
	MinBuyInSat   int64 `json:"min_buy_in_sat"`
	MaxBuyInSat   int64 `json:"max_buy_in_sat"`
	RakeBps       int64 `json:"rake_bps"`
	RakeCapSat    int64 `json:"rake_cap_sat"`
	NoFlopNoDrop  bool  `json:"no_flop_no_drop"`

	Stage      string     `json:"stage"`
	Board      []string   `json:"board"`
	Seats      []SeatView `json:"seats"`
	Pots       []PotView  `json:"pots"`
	PotSat     int64      `json:"pot_sat"`
	CurrentBet int64      `json:"current_bet_sat"`

	// YourSeat is -1 when the viewer is watching rather than playing.
	YourSeat      int      `json:"your_seat"`
	LegalActions  []string `json:"legal_actions,omitempty"`
	CallAmountSat int64    `json:"call_amount_sat"`
	MinRaiseToSat int64    `json:"min_raise_to_sat"`
	MaxRaiseToSat int64    `json:"max_raise_to_sat"`
	SecondsLeft   int      `json:"seconds_left"`

	// Commitment is the deck the house has committed to for this hand. It is
	// shown before the deal so a player can note it down and check it later.
	Commitment string `json:"commitment,omitempty"`
	ClientSeed string `json:"client_seed,omitempty"`
	YourSeed   string `json:"your_seed,omitempty"`

	VideoEnabled bool `json:"video_enabled"`
	// VideoPeers are the seats currently sharing, for the browser to connect
	// to. A seat appears here only while it is consenting.
	VideoPeers []int `json:"video_peers"`

	Log []string `json:"log,omitempty"`
}

// View renders the table from one viewer's point of view.
//
// This is the only place a hole card can escape, so the rule is enforced here
// once rather than in each template: a card is included only if it belongs to
// the viewer, or if the seat was shown at showdown.
func (t *Table) View(viewerID int64) TableView {
	t.mu.Lock()
	defer t.mu.Unlock()

	view := TableView{
		TableID: t.Config.ID, Name: t.Config.Name, Variant: "holdem",
		HandNumber:    t.handNumber,
		SmallBlindSat: t.Config.Rules.SmallBlindSat,
		BigBlindSat:   t.Config.Rules.BigBlindSat,
		MinBuyInSat:   t.Config.MinBuyInSat,
		MaxBuyInSat:   t.Config.MaxBuyInSat,
		RakeBps:       t.Config.Rules.RakeBps,
		RakeCapSat:    t.Config.Rules.RakeCapSat,
		NoFlopNoDrop:  t.Config.Rules.NoFlopNoDrop,
		Stage:         "waiting",
		YourSeat:      -1,
		VideoEnabled:  t.Config.VideoEnabled,
	}

	viewerSeat, seated := t.seatUsers[viewerID]
	if seated {
		view.YourSeat = viewerSeat
		view.YourSeed = t.clientSeeds[viewerSeat]
	}

	hand := t.hand
	showdown := hand != nil && hand.Stage >= StageShowdown
	shown := make(map[int]bool)
	if hand != nil {
		view.Stage = hand.Stage.String()
		view.Commitment = hand.Shuffle.Commitment
		view.ClientSeed = hand.Shuffle.ClientSeed
		view.CurrentBet = hand.CurrentBet
		view.PotSat = hand.PotSat()
		for _, card := range hand.Board {
			view.Board = append(view.Board, card.String())
		}
		for _, pot := range hand.BuildPots() {
			view.Pots = append(view.Pots, PotView{AmountSat: pot.AmountSat, Eligible: pot.Eligible})
		}
		view.Log = hand.Log
		if showdown {
			// Only seats that reached showdown are exposed; a folded hand's
			// cards stay private, exactly as at a live table.
			for _, index := range hand.liveSeats() {
				shown[index] = true
			}
		}
		if remaining := time.Until(t.deadline); remaining > 0 {
			view.SecondsLeft = int(remaining.Seconds())
		}
	}

	for i, seat := range t.seats {
		out := SeatView{
			Number: i, Status: seat.Status,
			StackSat: seat.StackSat, CommittedSat: seat.CommittedSat,
			VideoConsent: t.consented[i],
			TimedOut:     seat.TimedOut,
		}
		if seat.Status != SeatEmpty && seat.PlayerID != 0 {
			out.Occupied = true
			out.PlayerID = seat.PlayerID
			out.Name = seat.Name
			out.IsYou = seat.PlayerID == viewerID
		}
		if hand != nil {
			out.IsButton = i == t.button
			out.IsTurn = !hand.Complete() && i == hand.ToAct
			out.HasCards = len(seat.Cards) > 0 && seat.InHand()
			if len(seat.Cards) > 0 && (out.IsYou || shown[i]) {
				for _, card := range seat.Cards {
					out.Cards = append(out.Cards, card.String())
				}
			}
		}
		if t.Config.VideoEnabled && t.consented[i] && out.Occupied {
			view.VideoPeers = append(view.VideoPeers, i)
		}
		view.Seats = append(view.Seats, out)
	}

	// Action controls, for the viewer's own seat only.
	if seated && hand != nil && !hand.Complete() && hand.ToAct == viewerSeat {
		for _, action := range hand.LegalActions(viewerSeat) {
			view.LegalActions = append(view.LegalActions, string(action))
		}
		view.CallAmountSat = hand.CallAmountSat(viewerSeat)
		view.MinRaiseToSat = hand.MinRaiseToSat(viewerSeat)
		view.MaxRaiseToSat = hand.MaxRaiseToSat(viewerSeat)
	}
	return view
}

// LobbyView is a table as it appears in the lobby.
type LobbyView struct {
	TableID       int64  `json:"table_id"`
	Name          string `json:"name"`
	SmallBlindSat int64  `json:"small_blind_sat"`
	BigBlindSat   int64  `json:"big_blind_sat"`
	MinBuyInSat   int64  `json:"min_buy_in_sat"`
	MaxBuyInSat   int64  `json:"max_buy_in_sat"`
	RakeBps       int64  `json:"rake_bps"`
	RakeCapSat    int64  `json:"rake_cap_sat"`
	NoFlopNoDrop  bool   `json:"no_flop_no_drop"`
	Seated        int    `json:"seated"`
	MaxSeats      int    `json:"max_seats"`
	VideoEnabled  bool   `json:"video_enabled"`
	HandsPlayed   int64  `json:"hands_played"`
}

// Lobby renders the table's lobby row.
func (t *Table) Lobby() LobbyView {
	t.mu.Lock()
	defer t.mu.Unlock()
	var seated int
	for _, seat := range t.seats {
		if seat.Status != SeatEmpty && seat.PlayerID != 0 {
			seated++
		}
	}
	return LobbyView{
		TableID: t.Config.ID, Name: t.Config.Name,
		SmallBlindSat: t.Config.Rules.SmallBlindSat,
		BigBlindSat:   t.Config.Rules.BigBlindSat,
		MinBuyInSat:   t.Config.MinBuyInSat,
		MaxBuyInSat:   t.Config.MaxBuyInSat,
		RakeBps:       t.Config.Rules.RakeBps,
		RakeCapSat:    t.Config.Rules.RakeCapSat,
		NoFlopNoDrop:  t.Config.Rules.NoFlopNoDrop,
		Seated:        seated, MaxSeats: t.Config.MaxSeats,
		VideoEnabled: t.Config.VideoEnabled,
		HandsPlayed:  t.handNumber,
	}
}
