package blackjack

import (
	"errors"
	"fmt"
)

// Action is one player decision.
type Action string

const (
	Hit    Action = "hit"
	Stand  Action = "stand"
	Double Action = "double"
	Split  Action = "split"
)

// Errors callers branch on.
var (
	ErrRoundOver = errors.New("blackjack: the round is over")
	ErrBadAction = errors.New("blackjack: that play is not available")
	ErrDeckShort = errors.New("blackjack: the deck ran out, which cannot happen in one round")
)

// Hand is one of the player's hands — two after a split, otherwise one.
type Hand struct {
	Cards []Card `json:"cards"`
	// BetUnits is 1, or 2 after a double. Each unit is the round's base stake.
	BetUnits int  `json:"bet_units"`
	Stood    bool `json:"stood"`
	Busted   bool `json:"busted"`
	// FromSplit disqualifies the hand from doubling, resplitting, and from
	// counting a two-card 21 as a natural.
	FromSplit bool `json:"from_split"`
	// SplitAces marks a hand that took its one permitted card and stood.
	SplitAces bool `json:"split_aces"`
}

// Total is the hand's best value.
func (h *Hand) Total() (int, bool) { return Total(h.Cards) }

// State is a round as stored between requests, including the action log the
// verification page replays.
type State struct {
	Player  []Hand `json:"player"`
	Dealer  []Card `json:"dealer"`
	Active  int    `json:"active"`
	Settled bool   `json:"settled"`
	// DealerBlackjack ends the round at the peek, before any action.
	DealerBlackjack bool     `json:"dealer_blackjack"`
	PlayerBlackjack bool     `json:"player_blackjack"`
	Actions         []Action `json:"actions"`
	// Dealt is how many cards have come off the top of the deck.
	Dealt int `json:"dealt"`
}

// Round is a live round: the fixed deck plus the state so far.
type Round struct {
	State State
	deck  []Card
}

// New deals a round from a shuffled deck. The deck order is the committed
// randomness; everything after this call is the player's decisions.
//
// Deal order is fixed and published: player, dealer up, player, dealer hole,
// then every subsequent card off the top in play order. A verifier replaying
// the round must land on exactly these cards.
func New(deck []Card) (*Round, error) {
	if err := checkDeck(deck); err != nil {
		return nil, err
	}
	round := &Round{deck: deck}
	round.State.Player = []Hand{{BetUnits: 1}}
	hand := &round.State.Player[0]
	hand.Cards = append(hand.Cards, round.draw())
	round.State.Dealer = append(round.State.Dealer, round.draw())
	hand.Cards = append(hand.Cards, round.draw())
	round.State.Dealer = append(round.State.Dealer, round.draw())

	// The peek. With an ace or a ten up, the dealer checks the hole card
	// before anybody stakes more; a natural ends the round on the spot, so a
	// double or split can never be lost to one.
	round.State.PlayerBlackjack = IsBlackjack(hand.Cards)
	if IsBlackjack(round.State.Dealer) {
		round.State.DealerBlackjack = true
		round.State.Settled = true
		round.State.Active = -1
		return round, nil
	}
	if round.State.PlayerBlackjack {
		round.State.Settled = true
		round.State.Active = -1
		return round, nil
	}
	return round, nil
}

// Resume rebuilds a round from the same deck and its stored state.
func Resume(deck []Card, state State) (*Round, error) {
	if err := checkDeck(deck); err != nil {
		return nil, err
	}
	return &Round{State: state, deck: deck}, nil
}

func (r *Round) draw() Card {
	if r.State.Dealt >= len(r.deck) {
		// One round consumes at most a couple of dozen cards; running out
		// means the state is corrupt, and dealing garbage would be worse
		// than stopping.
		panic(ErrDeckShort)
	}
	card := r.deck[r.State.Dealt]
	r.State.Dealt++
	return card
}

// Settled reports whether the round is finished.
func (r *Round) Settled() bool { return r.State.Settled }

// ActiveHand returns the hand awaiting a decision.
func (r *Round) ActiveHand() *Hand {
	if r.State.Settled || r.State.Active < 0 || r.State.Active >= len(r.State.Player) {
		return nil
	}
	return &r.State.Player[r.State.Active]
}

// Available lists the plays open to the active hand, in display order.
func (r *Round) Available() []Action {
	hand := r.ActiveHand()
	if hand == nil {
		return nil
	}
	actions := []Action{Hit, Stand}
	if len(hand.Cards) == 2 && !hand.FromSplit {
		actions = append(actions, Double)
		if len(r.State.Player) == 1 && hand.Cards[0].Value() == hand.Cards[1].Value() {
			actions = append(actions, Split)
		}
	}
	return actions
}

func (r *Round) allows(action Action) bool {
	for _, available := range r.Available() {
		if available == action {
			return true
		}
	}
	return false
}

// Act applies one decision. ExtraUnits reports how many extra base stakes the
// action itself commits — 1 for a double or a split, 0 otherwise — so the
// caller can move the money in the same breath.
func (r *Round) Act(action Action) (extraUnits int, err error) {
	if r.State.Settled {
		return 0, ErrRoundOver
	}
	if !r.allows(action) {
		return 0, fmt.Errorf("%w: %s", ErrBadAction, action)
	}
	hand := r.ActiveHand()
	r.State.Actions = append(r.State.Actions, action)

	switch action {
	case Hit:
		hand.Cards = append(hand.Cards, r.draw())
		total, _ := hand.Total()
		if total > 21 {
			hand.Busted = true
			r.advance()
		} else if total == 21 {
			// Nothing a further card could improve.
			hand.Stood = true
			r.advance()
		}
	case Stand:
		hand.Stood = true
		r.advance()
	case Double:
		extraUnits = 1
		hand.BetUnits = 2
		hand.Cards = append(hand.Cards, r.draw())
		if total, _ := hand.Total(); total > 21 {
			hand.Busted = true
		} else {
			hand.Stood = true
		}
		r.advance()
	case Split:
		extraUnits = 1
		first, second := hand.Cards[0], hand.Cards[1]
		aces := first.IsAce() && second.IsAce()
		r.State.Player = []Hand{
			{Cards: []Card{first}, BetUnits: 1, FromSplit: true, SplitAces: aces},
			{Cards: []Card{second}, BetUnits: 1, FromSplit: true, SplitAces: aces},
		}
		// Each hand takes its next card as play reaches it; split aces take
		// one card each and stand, by rule.
		r.State.Player[0].Cards = append(r.State.Player[0].Cards, r.draw())
		if aces {
			r.State.Player[0].Stood = true
			r.State.Player[1].Cards = append(r.State.Player[1].Cards, r.draw())
			r.State.Player[1].Stood = true
			r.State.Active = len(r.State.Player)
			r.finish()
			return extraUnits, nil
		}
		if total, _ := r.State.Player[0].Total(); total == 21 {
			r.State.Player[0].Stood = true
			r.advance()
		}
	}
	return extraUnits, nil
}

// advance moves play to the next undecided hand, dealing a split hand its
// second card on arrival, and hands over to the dealer when none remain.
func (r *Round) advance() {
	for {
		r.State.Active++
		if r.State.Active >= len(r.State.Player) {
			r.finish()
			return
		}
		hand := &r.State.Player[r.State.Active]
		if hand.Stood || hand.Busted {
			continue
		}
		if len(hand.Cards) == 1 {
			hand.Cards = append(hand.Cards, r.draw())
			if total, _ := hand.Total(); total == 21 {
				hand.Stood = true
				continue
			}
		}
		return
	}
}

// finish plays the dealer and settles.
func (r *Round) finish() {
	r.State.Active = -1

	// The dealer only draws with a live hand to beat. With every hand bust
	// the outcome is already decided, and every extra card dealt is a card a
	// verifier has to account for.
	anyLive := false
	for i := range r.State.Player {
		if !r.State.Player[i].Busted {
			anyLive = true
		}
	}
	if anyLive {
		for {
			total, _ := Total(r.State.Dealer)
			// Stands on all 17s, soft included.
			if total >= 17 {
				break
			}
			r.State.Dealer = append(r.State.Dealer, r.draw())
		}
	}
	r.State.Settled = true
}

// StakedUnits is the round's total commitment in base stakes.
func (r *Round) StakedUnits() int {
	units := 0
	for i := range r.State.Player {
		units += r.State.Player[i].BetUnits
	}
	return units
}

// PayoutSat is what the round returns on a base stake, all hands settled.
// The stake must be even so the 3:2 natural pays exactly.
func (r *Round) PayoutSat(stakeSat int64) int64 {
	if !r.State.Settled {
		return 0
	}
	if r.State.DealerBlackjack {
		// The peek ended it. A player natural pushes; anything else loses.
		if r.State.PlayerBlackjack {
			return stakeSat
		}
		return 0
	}
	if r.State.PlayerBlackjack {
		return stakeSat + stakeSat*3/2
	}

	dealerTotal, _ := Total(r.State.Dealer)
	dealerBust := dealerTotal > 21

	var payout int64
	for i := range r.State.Player {
		hand := &r.State.Player[i]
		bet := stakeSat * int64(hand.BetUnits)
		if hand.Busted {
			continue
		}
		total, _ := hand.Total()
		switch {
		case dealerBust || total > dealerTotal:
			payout += bet * 2
		case total == dealerTotal:
			payout += bet
		}
	}
	return payout
}

// Outcome summarises a settled round for storage: won, lost or pushed against
// the total staked.
func (r *Round) Outcome(stakeSat int64) string {
	total := stakeSat * int64(r.StakedUnits())
	payout := r.PayoutSat(stakeSat)
	switch {
	case payout > total:
		return "won"
	case payout < total:
		return "lost"
	default:
		return "pushed"
	}
}
