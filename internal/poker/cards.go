// Package poker implements Texas Hold'em: cards, hand ranking, a verifiable
// shuffle and the betting engine.
//
// Two properties matter more than anything else here, because they are what a
// player has to take on trust at every other online poker room:
//
//  1. The deck is fixed before the deal and can be checked afterwards. The
//     house commits to a shuffle it cannot then change, and publishes the seed
//     when the hand is over so anyone can recompute the exact deck.
//  2. The house's income does not depend on how many hands are played beyond
//     the published rake. There is no mechanism anywhere in this package that
//     can influence which cards come out, and rake is capped and never taken
//     from a hand that ends before the flop.
package poker

import (
	"fmt"
	"strings"
)

// Card is a card in a standard 52-card deck, encoded 0..51.
//
// rank = card / 4, running 0 (two) through 12 (ace).
// suit = card % 4, running clubs, diamonds, hearts, spades.
type Card uint8

// DeckSize is the number of cards in a deck.
const DeckSize = 52

// Rank returns 0 for a two through 12 for an ace.
func (c Card) Rank() int { return int(c) / 4 }

// Suit returns 0..3.
func (c Card) Suit() int { return int(c) % 4 }

const rankLetters = "23456789TJQKA"
const suitLetters = "cdhs"

// String renders a card as "As" or "Td".
func (c Card) String() string {
	if c >= DeckSize {
		return "??"
	}
	return string(rankLetters[c.Rank()]) + string(suitLetters[c.Suit()])
}

// RankName is the long form, for hand descriptions.
func (c Card) RankName() string {
	names := []string{"two", "three", "four", "five", "six", "seven", "eight",
		"nine", "ten", "jack", "queen", "king", "ace"}
	if c.Rank() < 0 || c.Rank() >= len(names) {
		return "?"
	}
	return names[c.Rank()]
}

// ParseCard reads "As", "td" and so on.
func ParseCard(text string) (Card, error) {
	text = strings.TrimSpace(text)
	if len(text) != 2 {
		return 0, fmt.Errorf("poker: %q is not a card", text)
	}
	rank := strings.IndexByte(rankLetters, upper(text[0]))
	suit := strings.IndexByte(suitLetters, lower(text[1]))
	if rank < 0 || suit < 0 {
		return 0, fmt.Errorf("poker: %q is not a card", text)
	}
	return Card(rank*4 + suit), nil
}

func upper(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - 32
	}
	return b
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

// FormatCards renders a hand for logs and hand histories.
func FormatCards(cards []Card) string {
	parts := make([]string, 0, len(cards))
	for _, card := range cards {
		parts = append(parts, card.String())
	}
	return strings.Join(parts, " ")
}

// NewDeck returns an ordered deck. It is never dealt from directly; Shuffle
// produces the playing order.
func NewDeck() []Card {
	deck := make([]Card, DeckSize)
	for i := range deck {
		deck[i] = Card(i)
	}
	return deck
}
