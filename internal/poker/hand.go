package poker

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Stage is where a hand has reached.
type Stage int

// Stages of a hand.
const (
	StagePreFlop Stage = iota
	StageFlop
	StageTurn
	StageRiver
	StageShowdown
	StageComplete
)

var stageNames = map[Stage]string{
	StagePreFlop: "pre-flop", StageFlop: "flop", StageTurn: "turn",
	StageRiver: "river", StageShowdown: "showdown", StageComplete: "complete",
}

// String names the stage.
func (s Stage) String() string {
	if name, ok := stageNames[s]; ok {
		return name
	}
	return "unknown"
}

// Seat statuses within a hand.
const (
	SeatEmpty      = "empty"
	SeatSittingOut = "sitting_out"
	SeatActive     = "active"
	SeatFolded     = "folded"
	SeatAllIn      = "all_in"
)

// Action is something a player can do.
type Action string

// Actions.
const (
	ActionFold  Action = "fold"
	ActionCheck Action = "check"
	ActionCall  Action = "call"
	ActionBet   Action = "bet"
	ActionRaise Action = "raise"
)

// Errors callers branch on.
var (
	ErrNotYourTurn      = errors.New("poker: it is not that seat's turn")
	ErrIllegalAction    = errors.New("poker: that action is not available")
	ErrBelowMinRaise    = errors.New("poker: raise is below the minimum")
	ErrNotEnoughChips   = errors.New("poker: not enough chips behind")
	ErrHandComplete     = errors.New("poker: the hand is over")
	ErrNotEnoughPlayers = errors.New("poker: a hand needs at least two players")
)

// Seat is one player's position in a hand.
type Seat struct {
	Position int
	PlayerID int64
	Name     string
	// StackSat is what is left behind, in satoshi.
	StackSat int64
	// CommittedSat is what this seat has put in during the current betting
	// round; TotalCommittedSat is across the whole hand, which is what side
	// pots are built from.
	CommittedSat      int64
	TotalCommittedSat int64
	Cards             []Card
	Status            string
	// HasActed is reset whenever a raise reopens the action.
	HasActed bool
	// SatOutAt marks a seat that folded by timing out, for the table to act on.
	TimedOut bool
}

// InHand reports whether the seat can still win the pot.
func (s *Seat) InHand() bool { return s.Status == SeatActive || s.Status == SeatAllIn }

// CanAct reports whether the seat still has decisions to make.
func (s *Seat) CanAct() bool { return s.Status == SeatActive && s.StackSat > 0 }

// Rules are a table's betting rules.
type Rules struct {
	SmallBlindSat int64
	BigBlindSat   int64
	// AnteSat is posted by everybody before the deal, zero for most cash games.
	AnteSat int64
	// RakeBps is the share of the pot the house takes, in basis points.
	RakeBps int64
	// RakeCapSat caps the rake however big the pot gets.
	RakeCapSat int64
	// NoFlopNoDrop means no rake is taken from a hand that ends before the
	// flop. It is the standard fair-dealing rule and is on by default.
	NoFlopNoDrop bool
}

// DefaultRules returns sane cash-game rules for a given big blind.
func DefaultRules(bigBlindSat int64) Rules {
	return Rules{
		SmallBlindSat: bigBlindSat / 2,
		BigBlindSat:   bigBlindSat,
		RakeBps:       250, // 2.5%
		RakeCapSat:    bigBlindSat * 3,
		NoFlopNoDrop:  true,
	}
}

// Hand is one deal, from blinds to payout.
type Hand struct {
	ID      int64
	TableID int64
	Rules   Rules

	// Shuffle is the committed deck for this hand.
	Shuffle *Shuffle
	Nonce   int64

	Seats  []*Seat
	Button int

	Stage Stage
	Board []Card

	// CurrentBet is the amount each active seat must have committed this round
	// to stay in. MinRaiseSat is the smallest legal raise increment.
	CurrentBet  int64
	MinRaiseSat int64
	ToAct       int

	// RakeSat is filled in at payout.
	RakeSat int64
	// Log is a human-readable hand history.
	Log []string

	deck  []Card
	dealt int
	// lastAggressor is the seat that made the last bet or raise, used to decide
	// who shows first at showdown.
	lastAggressor int
}

// NewHand deals a hand to the seated players.
//
// seats must be in table order. Any seat that is not active with chips behind
// is skipped, which is how sitting-out players and busted stacks are handled.
func NewHand(rules Rules, seats []*Seat, button int, shuffle *Shuffle, nonce int64) (*Hand, error) {
	playable := 0
	for _, seat := range seats {
		if seat != nil && seat.Status == SeatActive && seat.StackSat > 0 {
			playable++
		}
	}
	if playable < 2 {
		return nil, ErrNotEnoughPlayers
	}
	if rules.BigBlindSat <= 0 {
		return nil, fmt.Errorf("poker: the big blind must be positive")
	}

	hand := &Hand{
		Rules: rules, Shuffle: shuffle, Nonce: nonce,
		Seats: seats, Button: button,
		Stage: StagePreFlop, MinRaiseSat: rules.BigBlindSat,
		deck:          shuffle.Deck(),
		lastAggressor: -1,
	}

	// Reset per-hand state so a seat reused from the previous hand is clean.
	for _, seat := range hand.Seats {
		if seat == nil {
			continue
		}
		seat.CommittedSat = 0
		seat.TotalCommittedSat = 0
		seat.Cards = nil
		seat.HasActed = false
		seat.TimedOut = false
		if seat.Status == SeatActive && seat.StackSat <= 0 {
			seat.Status = SeatSittingOut
		}
	}

	hand.logf("hand %d begins, deck committed as %s", nonce, shuffle.Commitment)
	hand.postAntesAndBlinds()
	hand.dealHoleCards()
	hand.ToAct = hand.firstToActPreFlop()
	return hand, nil
}

func (h *Hand) logf(format string, args ...any) {
	h.Log = append(h.Log, fmt.Sprintf(format, args...))
}

// occupied returns the seat indexes that are in the hand, in table order.
func (h *Hand) occupied() []int {
	var out []int
	for i, seat := range h.Seats {
		if seat != nil && (seat.Status == SeatActive || seat.Status == SeatAllIn) {
			out = append(out, i)
		}
	}
	return out
}

// nextOccupied walks clockwise from `from`, skipping seats out of the hand.
func (h *Hand) nextOccupied(from int) int {
	for step := 1; step <= len(h.Seats); step++ {
		index := (from + step) % len(h.Seats)
		seat := h.Seats[index]
		if seat != nil && seat.InHand() {
			return index
		}
	}
	return -1
}

// nextActive walks clockwise to the next seat that can still act.
func (h *Hand) nextActive(from int) int {
	for step := 1; step <= len(h.Seats); step++ {
		index := (from + step) % len(h.Seats)
		if seat := h.Seats[index]; seat != nil && seat.CanAct() {
			return index
		}
	}
	return -1
}

func (h *Hand) postAntesAndBlinds() {
	live := h.occupied()

	if h.Rules.AnteSat > 0 {
		for _, index := range live {
			h.commit(index, h.Rules.AnteSat, "ante")
		}
		// Antes do not count as a bet to call.
		for _, index := range live {
			h.Seats[index].CommittedSat = 0
		}
	}

	small, big := h.blindSeats()
	if small >= 0 {
		h.commit(small, h.Rules.SmallBlindSat, "small blind")
	}
	if big >= 0 {
		h.commit(big, h.Rules.BigBlindSat, "big blind")
	}
	h.CurrentBet = h.Rules.BigBlindSat
	h.MinRaiseSat = h.Rules.BigBlindSat
}

// blindSeats returns the small and big blind seat indexes.
//
// Heads-up reverses the usual arrangement: the button posts the small blind
// and acts first pre-flop, then last on every later street. Getting this wrong
// is the classic two-handed poker bug.
func (h *Hand) blindSeats() (small, big int) {
	live := h.occupied()
	if len(live) == 2 {
		return h.Button, h.nextOccupied(h.Button)
	}
	small = h.nextOccupied(h.Button)
	big = h.nextOccupied(small)
	return small, big
}

func (h *Hand) firstToActPreFlop() int {
	live := h.occupied()
	_, big := h.blindSeats()
	if len(live) == 2 {
		// Heads-up: the small blind, which is the button, acts first.
		return h.Button
	}
	return h.nextActive(big)
}

// commit moves chips from a stack into the pot, capped at the stack.
func (h *Hand) commit(index int, amount int64, reason string) int64 {
	seat := h.Seats[index]
	if amount > seat.StackSat {
		amount = seat.StackSat
	}
	if amount <= 0 {
		return 0
	}
	seat.StackSat -= amount
	seat.CommittedSat += amount
	seat.TotalCommittedSat += amount
	if seat.StackSat == 0 {
		seat.Status = SeatAllIn
		h.logf("%s is all in for %d (%s)", seat.Name, seat.TotalCommittedSat, reason)
	} else {
		h.logf("%s posts %d (%s)", seat.Name, amount, reason)
	}
	return amount
}

func (h *Hand) dealHoleCards() {
	// Two rounds of one card each, starting left of the button, exactly as a
	// live dealer would. It changes nothing statistically but keeps the deal
	// reproducible against a hand history.
	for round := 0; round < 2; round++ {
		index := h.nextOccupied(h.Button)
		for range h.occupied() {
			seat := h.Seats[index]
			seat.Cards = append(seat.Cards, h.draw())
			index = h.nextOccupied(index)
		}
	}
}

func (h *Hand) draw() Card {
	card := h.deck[h.dealt]
	h.dealt++
	return card
}

// PotSat is everything committed to the pot so far.
func (h *Hand) PotSat() int64 {
	var total int64
	for _, seat := range h.Seats {
		if seat != nil {
			total += seat.TotalCommittedSat
		}
	}
	return total
}

// CallAmountSat is what the seat must add to call.
func (h *Hand) CallAmountSat(index int) int64 {
	seat := h.Seats[index]
	if seat == nil {
		return 0
	}
	owed := h.CurrentBet - seat.CommittedSat
	if owed < 0 {
		return 0
	}
	if owed > seat.StackSat {
		return seat.StackSat
	}
	return owed
}

// LegalActions lists what a seat may do right now, for the interface to render
// and for the server to validate against.
func (h *Hand) LegalActions(index int) []Action {
	if h.Stage >= StageShowdown || index != h.ToAct {
		return nil
	}
	seat := h.Seats[index]
	if seat == nil || !seat.CanAct() {
		return nil
	}

	var actions []Action
	actions = append(actions, ActionFold)
	if h.CallAmountSat(index) == 0 {
		actions = append(actions, ActionCheck)
	} else {
		actions = append(actions, ActionCall)
	}
	// A player with chips behind can always put more in, even if it is less
	// than a full raise; that is an all-in, not an illegal bet.
	if seat.StackSat > h.CallAmountSat(index) {
		if h.CurrentBet == 0 {
			actions = append(actions, ActionBet)
		} else {
			actions = append(actions, ActionRaise)
		}
	}
	return actions
}

// MinRaiseToSat is the smallest total commitment a raise may bring a seat to.
func (h *Hand) MinRaiseToSat(index int) int64 {
	minimum := h.CurrentBet + h.MinRaiseSat
	seat := h.Seats[index]
	if seat == nil {
		return minimum
	}
	if maximum := seat.CommittedSat + seat.StackSat; minimum > maximum {
		// Short stack: the only raise available is all in.
		return maximum
	}
	return minimum
}

// MaxRaiseToSat is the largest total commitment available, i.e. all in.
func (h *Hand) MaxRaiseToSat(index int) int64 {
	seat := h.Seats[index]
	if seat == nil {
		return 0
	}
	return seat.CommittedSat + seat.StackSat
}

// Act applies a player's decision.
//
// amountSat is the total the seat is raising *to* for a bet or raise, matching
// how a table reads: "raise to 400", not "raise by 250". Every other action
// ignores it.
func (h *Hand) Act(index int, action Action, amountSat int64) error {
	if h.Stage >= StageShowdown {
		return ErrHandComplete
	}
	if index != h.ToAct {
		return fmt.Errorf("%w: seat %d, expected seat %d", ErrNotYourTurn, index, h.ToAct)
	}
	seat := h.Seats[index]
	if seat == nil || !seat.CanAct() {
		return ErrIllegalAction
	}
	if !containsAction(h.LegalActions(index), action) {
		return fmt.Errorf("%w: %s", ErrIllegalAction, action)
	}

	switch action {
	case ActionFold:
		seat.Status = SeatFolded
		seat.HasActed = true
		h.logf("%s folds", seat.Name)

	case ActionCheck:
		seat.HasActed = true
		h.logf("%s checks", seat.Name)

	case ActionCall:
		amount := h.CallAmountSat(index)
		h.commit(index, amount, "call")
		seat.HasActed = true

	case ActionBet, ActionRaise:
		if err := h.applyRaise(index, amountSat); err != nil {
			return err
		}
	}

	h.advance()
	return nil
}

func (h *Hand) applyRaise(index int, raiseTo int64) error {
	seat := h.Seats[index]
	maximum := h.MaxRaiseToSat(index)
	if raiseTo > maximum {
		return fmt.Errorf("%w: raising to %d needs %d but only %d is behind",
			ErrNotEnoughChips, raiseTo, raiseTo-seat.CommittedSat, seat.StackSat)
	}

	minimum := h.MinRaiseToSat(index)
	isAllIn := raiseTo == maximum
	// An all-in for less than a full raise is legal; anything else below the
	// minimum is not.
	if raiseTo < minimum && !isAllIn {
		return fmt.Errorf("%w: raise to at least %d", ErrBelowMinRaise, minimum)
	}

	increment := raiseTo - h.CurrentBet
	h.commit(index, raiseTo-seat.CommittedSat, string(ActionRaise))

	if raiseTo > h.CurrentBet {
		// A short all-in that does not reach a full raise does not reopen the
		// betting for players who have already acted, and does not raise the
		// minimum for those who have not.
		if increment >= h.MinRaiseSat {
			h.MinRaiseSat = increment
			h.reopenAction(index)
		}
		h.CurrentBet = raiseTo
		h.lastAggressor = index
	}
	seat.HasActed = true
	return nil
}

// reopenAction clears HasActed for everybody else, because a full raise gives
// them a fresh decision.
func (h *Hand) reopenAction(raiser int) {
	for i, seat := range h.Seats {
		if seat == nil || i == raiser {
			continue
		}
		if seat.Status == SeatActive {
			seat.HasActed = false
		}
	}
}

// bettingRoundComplete reports whether everybody has had their say.
func (h *Hand) bettingRoundComplete() bool {
	var canStillAct int
	for _, seat := range h.Seats {
		if seat == nil || seat.Status != SeatActive {
			continue
		}
		canStillAct++
		if !seat.HasActed {
			return false
		}
		if seat.CommittedSat < h.CurrentBet && seat.StackSat > 0 {
			return false
		}
	}
	// Nobody left who can act: the round is over by default.
	return canStillAct == 0 || true
}

// advance moves the hand on if the current betting round has finished.
func (h *Hand) advance() {
	// One player left with everybody else folded: the hand ends immediately
	// and there is no showdown.
	if len(h.liveSeats()) <= 1 {
		h.Stage = StageShowdown
		return
	}

	if !h.bettingRoundComplete() {
		if next := h.nextActive(h.ToAct); next >= 0 {
			h.ToAct = next
			return
		}
	}

	// Round finished. If at most one player can still act, run the remaining
	// board out without further betting.
	h.nextStage()
}

// liveSeats returns seats that have not folded.
func (h *Hand) liveSeats() []int {
	var out []int
	for i, seat := range h.Seats {
		if seat != nil && seat.InHand() {
			out = append(out, i)
		}
	}
	return out
}

func (h *Hand) actionableSeats() int {
	var count int
	for _, seat := range h.Seats {
		if seat != nil && seat.CanAct() {
			count++
		}
	}
	return count
}

func (h *Hand) nextStage() {
	// Reset per-round state.
	for _, seat := range h.Seats {
		if seat == nil {
			continue
		}
		seat.CommittedSat = 0
		seat.HasActed = false
	}
	h.CurrentBet = 0
	h.MinRaiseSat = h.Rules.BigBlindSat
	h.lastAggressor = -1

	switch h.Stage {
	case StagePreFlop:
		h.Stage = StageFlop
		h.burn()
		h.Board = append(h.Board, h.draw(), h.draw(), h.draw())
		h.logf("flop: %s", FormatCards(h.Board))
	case StageFlop:
		h.Stage = StageTurn
		h.burn()
		h.Board = append(h.Board, h.draw())
		h.logf("turn: %s", h.Board[3])
	case StageTurn:
		h.Stage = StageRiver
		h.burn()
		h.Board = append(h.Board, h.draw())
		h.logf("river: %s", h.Board[4])
	case StageRiver:
		h.Stage = StageShowdown
		return
	default:
		h.Stage = StageShowdown
		return
	}

	// If fewer than two players can still bet, deal the rest of the board with
	// no more action rather than asking a lone player to act into a dry pot.
	if h.actionableSeats() < 2 {
		h.nextStage()
		return
	}
	if next := h.nextActive(h.Button); next >= 0 {
		h.ToAct = next
	}
}

// burn discards a card before each community street, as a live dealer does.
func (h *Hand) burn() { h.dealt++ }

// Complete reports whether the hand has reached showdown or ended early.
func (h *Hand) Complete() bool { return h.Stage >= StageShowdown }

// BoardSoFar returns the community cards dealt so far.
func (h *Hand) BoardSoFar() []Card { return h.Board }

func containsAction(actions []Action, want Action) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

// Describe renders the hand history as text.
func (h *Hand) Describe() string { return strings.Join(h.Log, "\n") }

// sortedInts is a small helper used by the pot builder.
func sortedInts(values map[int64]bool) []int64 {
	out := make([]int64, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
