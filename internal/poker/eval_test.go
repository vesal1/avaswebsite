package poker

import (
	"math/rand"
	"testing"
)

func cards(t *testing.T, text ...string) []Card {
	t.Helper()
	out := make([]Card, 0, len(text))
	for _, item := range text {
		card, err := ParseCard(item)
		if err != nil {
			t.Fatalf("ParseCard(%q): %v", item, err)
		}
		out = append(out, card)
	}
	return out
}

func eval(t *testing.T, text ...string) Evaluated {
	t.Helper()
	result, err := Evaluate(cards(t, text...))
	if err != nil {
		t.Fatalf("Evaluate(%v): %v", text, err)
	}
	return result
}

func TestCardRoundTrip(t *testing.T) {
	for i := 0; i < DeckSize; i++ {
		card := Card(i)
		back, err := ParseCard(card.String())
		if err != nil {
			t.Fatalf("ParseCard(%q): %v", card, err)
		}
		if back != card {
			t.Errorf("%s round tripped to %s", card, back)
		}
	}
	if _, err := ParseCard("Xx"); err == nil {
		t.Error("a nonsense card should be rejected")
	}
}

func TestCategories(t *testing.T) {
	cases := []struct {
		name string
		hand []string
		want Category
	}{
		{"high card", []string{"Ah", "Kd", "9c", "7s", "3h"}, HighCard},
		{"pair", []string{"Ah", "Ad", "9c", "7s", "3h"}, Pair},
		{"two pair", []string{"Ah", "Ad", "9c", "9s", "3h"}, TwoPair},
		{"trips", []string{"Ah", "Ad", "Ac", "9s", "3h"}, ThreeOfAKind},
		{"straight", []string{"9h", "8d", "7c", "6s", "5h"}, Straight},
		{"flush", []string{"Ah", "Jh", "9h", "7h", "3h"}, Flush},
		{"full house", []string{"Ah", "Ad", "Ac", "9s", "9h"}, FullHouse},
		{"quads", []string{"Ah", "Ad", "Ac", "As", "9h"}, FourOfAKind},
		{"straight flush", []string{"9h", "8h", "7h", "6h", "5h"}, StraightFlush},
		{"royal flush", []string{"Ah", "Kh", "Qh", "Jh", "Th"}, StraightFlush},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eval(t, tc.hand...)
			if got.Value.Category() != tc.want {
				t.Errorf("category = %v, want %v", got.Value.Category(), tc.want)
			}
			if len(got.Best) != 5 {
				t.Errorf("best hand has %d cards, want 5", len(got.Best))
			}
		})
	}
}

func TestCategoryOrdering(t *testing.T) {
	// Every category must beat every weaker one, whatever the ranks involved.
	ladder := [][]string{
		{"Ah", "Kd", "9c", "7s", "3h"}, // high card, ace high
		{"2h", "2d", "3c", "4s", "5h"}, // lowest pair
		{"2h", "2d", "3c", "3s", "5h"}, // two pair
		{"2h", "2d", "2c", "3s", "5h"}, // trips
		{"6h", "5d", "4c", "3s", "2h"}, // lowest straight ... a wheel is lower still
		{"7h", "5h", "4h", "3h", "2h"}, // flush
		{"2h", "2d", "2c", "3s", "3h"}, // full house
		{"2h", "2d", "2c", "2s", "3h"}, // quads
		{"6h", "5h", "4h", "3h", "2h"}, // straight flush
	}
	for i := 1; i < len(ladder); i++ {
		weaker := eval(t, ladder[i-1]...)
		stronger := eval(t, ladder[i]...)
		if Compare(stronger.Value, weaker.Value) != 1 {
			t.Errorf("%v (%s) did not beat %v (%s)",
				ladder[i], stronger.Describe(), ladder[i-1], weaker.Describe())
		}
	}
}

func TestWheelIsTheLowestStraight(t *testing.T) {
	wheel := eval(t, "Ah", "5d", "4c", "3s", "2h")
	if wheel.Value.Category() != Straight {
		t.Fatalf("A-5-4-3-2 ranked as %v, want a straight", wheel.Value.Category())
	}
	// It must be beaten by six-high, the next straight up.
	sixHigh := eval(t, "6h", "5d", "4c", "3s", "2h")
	if Compare(sixHigh.Value, wheel.Value) != 1 {
		t.Error("six-high straight did not beat the wheel")
	}
	// And the ace must not make it ace-high: that is the classic bug.
	aceHighStraight := eval(t, "Ah", "Kd", "Qc", "Js", "Th")
	if Compare(aceHighStraight.Value, wheel.Value) != 1 {
		t.Error("broadway did not beat the wheel: the ace is being counted high in A-5")
	}
	if wheel.Best[0].Rank() != 3 {
		t.Errorf("the wheel's top card is %s, want the five", wheel.Best[0])
	}
}

func TestSteelWheelIsAStraightFlush(t *testing.T) {
	hand := eval(t, "Ah", "5h", "4h", "3h", "2h")
	if hand.Value.Category() != StraightFlush {
		t.Fatalf("A-5 suited ranked as %v, want a straight flush", hand.Value.Category())
	}
	royal := eval(t, "Ah", "Kh", "Qh", "Jh", "Th")
	if Compare(royal.Value, hand.Value) != 1 {
		t.Error("a royal flush did not beat the steel wheel")
	}
}

func TestKickersDecideTies(t *testing.T) {
	// Same pair, different kicker.
	better := eval(t, "Ah", "Ad", "Kc", "7s", "3h")
	worse := eval(t, "As", "Ac", "Qc", "7d", "3d")
	if Compare(better.Value, worse.Value) != 1 {
		t.Error("a king kicker did not beat a queen kicker")
	}

	// Identical hands of different suits must tie exactly, which is what makes
	// a split pot correct rather than nearly correct.
	left := eval(t, "Ah", "Kh", "Qh", "9h", "3h")
	right := eval(t, "As", "Ks", "Qs", "9s", "3s")
	if Compare(left.Value, right.Value) != 0 {
		t.Error("identical flushes in different suits did not tie: suits must not rank")
	}
}

func TestSevenCardHandsPickTheBestFive(t *testing.T) {
	// Board pairs but the player has a flush; the flush must be found.
	result := eval(t, "Ah", "Kh", "Qh", "7h", "2h", "9s", "9d")
	if result.Value.Category() != Flush {
		t.Errorf("category = %v, want a flush", result.Value.Category())
	}

	// Board has a straight; the player's pair is irrelevant.
	result = eval(t, "9c", "8d", "7h", "6s", "5c", "2h", "2d")
	if result.Value.Category() != Straight {
		t.Errorf("category = %v, want a straight", result.Value.Category())
	}

	// Trips on board plus a pair in hand is a full house, not trips.
	result = eval(t, "9c", "9d", "9h", "4s", "4c", "2h", "3d")
	if result.Value.Category() != FullHouse {
		t.Errorf("category = %v, want a full house", result.Value.Category())
	}

	// Two sets means the higher one plays as the trips of the full house.
	result = eval(t, "9c", "9d", "9h", "4s", "4c", "4d", "3d")
	if result.Value.Category() != FullHouse {
		t.Fatalf("category = %v, want a full house", result.Value.Category())
	}
	if result.Best[0].Rank() != 7 { // nines
		t.Errorf("full house built on %ss, want nines", result.Best[0].RankName())
	}
}

func TestPlayingTheBoard(t *testing.T) {
	// Both players' hole cards are worse than the board, so they must tie.
	board := []string{"Ah", "Kh", "Qh", "Jh", "Th"}
	first := eval(t, append(append([]string{}, board...), "2c", "3d")...)
	second := eval(t, append(append([]string{}, board...), "4c", "5d")...)
	if Compare(first.Value, second.Value) != 0 {
		t.Error("two players playing the same board did not tie")
	}
}

func TestEvaluateRejectsBadInput(t *testing.T) {
	if _, err := Evaluate(cards(t, "Ah", "Kd", "9c", "7s")); err == nil {
		t.Error("four cards should be rejected")
	}
	if _, err := Evaluate(cards(t, "Ah", "Kd", "9c", "7s", "3h", "2h", "4h", "5h")); err == nil {
		t.Error("eight cards should be rejected")
	}
	// A duplicate card means the deck is broken; ranking it would hide that.
	if _, err := Evaluate(cards(t, "Ah", "Ah", "9c", "7s", "3h")); err == nil {
		t.Error("a duplicated card should be rejected")
	}
}

func TestDescriptionsReadCorrectly(t *testing.T) {
	cases := []struct {
		hand []string
		want string
	}{
		{[]string{"Ah", "Ad", "Kc", "7s", "3h"}, "a pair of aces"},
		{[]string{"Ah", "Ad", "Kc", "Ks", "3h"}, "two pair, aces and kings"},
		{[]string{"9c", "9d", "9h", "4s", "4c"}, "a full house, nines full of fours"},
		{[]string{"Ah", "Kd", "9c", "7s", "3h"}, "Ace high"},
		{[]string{"Ah", "5d", "4c", "3s", "2h"}, "a straight to the five"},
	}
	for _, tc := range cases {
		if got := eval(t, tc.hand...).Describe(); got != tc.want {
			t.Errorf("Describe(%v) = %q, want %q", tc.hand, got, tc.want)
		}
	}
}

// TestExhaustiveFiveCardCategories checks the evaluator against an independent
// count. Across every possible five-card hand the number falling into each
// category is a known, published set of figures; matching all nine of them is
// a strong signal the evaluator has no gaps.
func TestExhaustiveFiveCardCategories(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive over 2,598,960 hands")
	}
	counts := make(map[Category]int)
	deck := NewDeck()

	for a := 0; a < 52; a++ {
		for b := a + 1; b < 52; b++ {
			for c := b + 1; c < 52; c++ {
				for d := c + 1; d < 52; d++ {
					for e := d + 1; e < 52; e++ {
						hand := []Card{deck[a], deck[b], deck[c], deck[d], deck[e]}
						result, err := Evaluate(hand)
						if err != nil {
							t.Fatalf("Evaluate(%v): %v", FormatCards(hand), err)
						}
						counts[result.Value.Category()]++
					}
				}
			}
		}
	}

	want := map[Category]int{
		HighCard:      1302540,
		Pair:          1098240,
		TwoPair:       123552,
		ThreeOfAKind:  54912,
		Straight:      10200,
		Flush:         5108,
		FullHouse:     3744,
		FourOfAKind:   624,
		StraightFlush: 40,
	}
	var total int
	for category, expected := range want {
		if counts[category] != expected {
			t.Errorf("%v: counted %d hands, want %d", category, counts[category], expected)
		}
		total += counts[category]
	}
	if total != 2598960 {
		t.Errorf("counted %d hands in total, want 2598960", total)
	}
}

// TestSevenCardIsNeverWorseThanFive is a property check: adding cards to a hand
// can only improve it, so the best five of seven must beat or match any five
// of those seven.
func TestSevenCardIsNeverWorseThanFive(t *testing.T) {
	source := rand.New(rand.NewSource(20260826))
	deck := NewDeck()

	for trial := 0; trial < 3000; trial++ {
		source.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
		seven := append([]Card{}, deck[:7]...)

		best, err := Evaluate(seven)
		if err != nil {
			t.Fatal(err)
		}
		// Every five-card subset must be no better than the reported best.
		for a := 0; a < 7; a++ {
			for b := a + 1; b < 7; b++ {
				subset := make([]Card, 0, 5)
				for i, card := range seven {
					if i != a && i != b {
						subset = append(subset, card)
					}
				}
				five, err := Evaluate(subset)
				if err != nil {
					t.Fatal(err)
				}
				if Compare(five.Value, best.Value) == 1 {
					t.Fatalf("subset %s (%s) beat the reported best %s (%s) of %s",
						FormatCards(subset), five.Describe(),
						FormatCards(best.Best), best.Describe(), FormatCards(seven))
				}
			}
		}
	}
}
