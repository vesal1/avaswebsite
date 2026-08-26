// Package blackjack is the casino's blackjack table: a single deck dealt
// fresh from a committed, verifiable shuffle every round, played under rules
// printed in full on the page.
//
// This is the skill game on the floor. The randomness is provably fair, the
// same as everywhere else in the house — but here the player's decisions
// genuinely change the return. The strategy card that maximises it is printed
// on the game page, and the published return is what that card earns. Casinos
// usually leave the card out and profit from misplay; this one hands it over
// and prices the game on the assumption you use it.
//
// The rules, chosen once and stated everywhere:
//
//   - One deck, freshly shuffled every round from the committed seed.
//   - Dealer stands on all 17s.
//   - Blackjack pays 3:2. Always. A 6:5 table is a 1.4% pay cut dressed as a
//     side rule, and this house does not run one.
//   - Double on any first two cards. No double after a split.
//   - Split any pair once. Split aces receive one card each.
//   - Dealer peeks for blackjack with an ace or a ten showing, so a double
//     or split is never lost to a dealer natural.
//   - No insurance. It is a separate bet returning about 92% dressed up as
//     protection, and the house declines to sell it.
package blackjack

import "fmt"

// Card is an index into a standard deck: suit*13 + rank, rank 0 = deuce
// through 12 = ace.
type Card int

// Value is the card's blackjack value, counting the ace as 1. Soft totals are
// the hand's business, not the card's.
func (c Card) Value() int {
	rank := int(c) % 13
	switch {
	case rank == 12: // ace
		return 1
	case rank >= 8: // ten, jack, queen, king
		return 10
	default:
		return rank + 2
	}
}

// IsAce reports the one rank whose value can float.
func (c Card) IsAce() bool { return int(c)%13 == 12 }

var rankGlyphs = [...]string{"2", "3", "4", "5", "6", "7", "8", "9", "10", "J", "Q", "K", "A"}
var suitGlyphs = [...]string{"♠", "♥", "♦", "♣"}

// Rank and Suit are the display faces.
func (c Card) Rank() string { return rankGlyphs[int(c)%13] }
func (c Card) Suit() string { return suitGlyphs[int(c)/13] }

// Red reports whether the suit prints red.
func (c Card) Red() bool { suit := int(c) / 13; return suit == 1 || suit == 2 }

// String is the compact form used in logs and verification output: "A♠".
func (c Card) String() string { return c.Rank() + c.Suit() }

// Total is a hand's best value and whether it is soft (an ace counted as 11).
func Total(cards []Card) (total int, soft bool) {
	sum, aces := 0, 0
	for _, card := range cards {
		sum += card.Value()
		if card.IsAce() {
			aces++
		}
	}
	if aces > 0 && sum+10 <= 21 {
		return sum + 10, true
	}
	return sum, false
}

// IsBlackjack is a natural: two cards worth 21 on the first deal.
func IsBlackjack(cards []Card) bool {
	if len(cards) != 2 {
		return false
	}
	total, _ := Total(cards)
	return total == 21
}

// FormatCards renders a hand for histories and verification pages.
func FormatCards(cards []Card) string {
	out := ""
	for i, card := range cards {
		if i > 0 {
			out += " "
		}
		out += card.String()
	}
	return out
}

// DeckSize is the one deck the table uses.
const DeckSize = 52

// NewDeck returns the deck in canonical order, ready to shuffle.
func NewDeck() []Card {
	deck := make([]Card, DeckSize)
	for i := range deck {
		deck[i] = Card(i)
	}
	return deck
}

// checkDeck refuses a deck that is not a permutation of 52 distinct cards. A
// duplicated card in blackjack is not a display bug, it is a mis-shuffle that
// invalidates the round.
func checkDeck(deck []Card) error {
	if len(deck) != DeckSize {
		return fmt.Errorf("blackjack: deck has %d cards", len(deck))
	}
	var seen [DeckSize]bool
	for _, card := range deck {
		if card < 0 || int(card) >= DeckSize || seen[card] {
			return fmt.Errorf("blackjack: deck is not a permutation, %v repeats or is out of range", card)
		}
		seen[card] = true
	}
	return nil
}
