package poker

import (
	"fmt"
	"sort"
)

// Category is a poker hand class, ordered worst to best.
type Category int

// Hand categories.
const (
	HighCard Category = iota
	Pair
	TwoPair
	ThreeOfAKind
	Straight
	Flush
	FullHouse
	FourOfAKind
	StraightFlush
)

var categoryNames = map[Category]string{
	HighCard:      "high card",
	Pair:          "a pair",
	TwoPair:       "two pair",
	ThreeOfAKind:  "three of a kind",
	Straight:      "a straight",
	Flush:         "a flush",
	FullHouse:     "a full house",
	FourOfAKind:   "four of a kind",
	StraightFlush: "a straight flush",
}

// String names the category.
func (c Category) String() string {
	if name, ok := categoryNames[c]; ok {
		return name
	}
	return "an unknown hand"
}

// HandValue is a comparable ranking of a five-card hand. Larger is better, and
// two hands are tied exactly when their values are equal, which is what makes
// split pots correct.
//
// The value packs the category and five ordered tiebreak ranks into base-16
// digits, so a single integer comparison decides any showdown.
type HandValue uint32

// Category extracts the hand class.
func (v HandValue) Category() Category { return Category(v >> 20) }

func makeValue(category Category, ranks ...int) HandValue {
	value := HandValue(category) << 20
	for i, rank := range ranks {
		if i >= 5 {
			break
		}
		value |= HandValue(rank) << uint(16-4*i)
	}
	return value
}

// Evaluated is the outcome of ranking a hand.
type Evaluated struct {
	Value HandValue
	// Best is the five cards that make the hand, for display.
	Best []Card
}

// Describe renders a hand in words, as a hand history would.
func (e Evaluated) Describe() string {
	if len(e.Best) == 0 {
		return e.Value.Category().String()
	}
	switch e.Value.Category() {
	case HighCard:
		return fmt.Sprintf("%s high", capitalise(e.Best[0].RankName()))
	case Pair:
		return fmt.Sprintf("a pair of %ss", e.Best[0].RankName())
	case TwoPair:
		return fmt.Sprintf("two pair, %ss and %ss", e.Best[0].RankName(), e.Best[2].RankName())
	case ThreeOfAKind:
		return fmt.Sprintf("three %ss", e.Best[0].RankName())
	case FullHouse:
		return fmt.Sprintf("a full house, %ss full of %ss", e.Best[0].RankName(), e.Best[3].RankName())
	case FourOfAKind:
		return fmt.Sprintf("four %ss", e.Best[0].RankName())
	case Straight, StraightFlush:
		return fmt.Sprintf("%s to the %s", e.Value.Category(), e.Best[0].RankName())
	case Flush:
		return fmt.Sprintf("a flush, %s high", e.Best[0].RankName())
	default:
		return e.Value.Category().String()
	}
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return string(upper(s[0])) + s[1:]
}

// Evaluate ranks the best five-card hand from five, six or seven cards.
func Evaluate(cards []Card) (Evaluated, error) {
	if len(cards) < 5 || len(cards) > 7 {
		return Evaluated{}, fmt.Errorf("poker: cannot rank %d cards", len(cards))
	}
	seen := make(map[Card]bool, len(cards))
	for _, card := range cards {
		if card >= DeckSize {
			return Evaluated{}, fmt.Errorf("poker: %d is not a card", card)
		}
		if seen[card] {
			return Evaluated{}, fmt.Errorf("poker: %s appears twice", card)
		}
		seen[card] = true
	}

	// Sort high to low once; every branch below relies on that order.
	sorted := make([]Card, len(cards))
	copy(sorted, cards)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rank() > sorted[j].Rank() })

	bySuit := make([][]Card, 4)
	rankCount := make([]int, 13)
	for _, card := range sorted {
		bySuit[card.Suit()] = append(bySuit[card.Suit()], card)
		rankCount[card.Rank()]++
	}

	// Straight flush, then flush: both need five of one suit.
	for _, suited := range bySuit {
		if len(suited) < 5 {
			continue
		}
		if run := straightIn(suited); len(run) == 5 {
			return Evaluated{Value: makeValue(StraightFlush, run[0].Rank()), Best: run}, nil
		}
		best := suited[:5]
		return Evaluated{
			Value: makeValue(Flush, best[0].Rank(), best[1].Rank(), best[2].Rank(),
				best[3].Rank(), best[4].Rank()),
			Best: best,
		}, nil
	}

	// Group ranks by how many of each there are, strongest group first and
	// higher rank breaking ties within a group size.
	type group struct {
		rank  int
		count int
	}
	var groups []group
	for rank := 12; rank >= 0; rank-- {
		if rankCount[rank] > 0 {
			groups = append(groups, group{rank: rank, count: rankCount[rank]})
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].count > groups[j].count })

	cardsOfRank := func(rank, n int) []Card {
		var out []Card
		for _, card := range sorted {
			if card.Rank() == rank && len(out) < n {
				out = append(out, card)
			}
		}
		return out
	}

	switch {
	case groups[0].count == 4:
		quad := cardsOfRank(groups[0].rank, 4)
		kicker := firstExcluding(sorted, groups[0].rank, 1)
		best := append(quad, kicker...)
		return Evaluated{
			Value: makeValue(FourOfAKind, groups[0].rank, best[4].Rank()),
			Best:  best,
		}, nil

	case groups[0].count == 3 && len(groups) > 1 && groups[1].count >= 2:
		trips := cardsOfRank(groups[0].rank, 3)
		pair := cardsOfRank(groups[1].rank, 2)
		return Evaluated{
			Value: makeValue(FullHouse, groups[0].rank, groups[1].rank),
			Best:  append(trips, pair...),
		}, nil
	}

	// A straight can beat trips and below, so it is checked before them but
	// after the hands that outrank it.
	if run := straightIn(sorted); len(run) == 5 {
		return Evaluated{Value: makeValue(Straight, run[0].Rank()), Best: run}, nil
	}

	switch {
	case groups[0].count == 3:
		trips := cardsOfRank(groups[0].rank, 3)
		kickers := firstExcluding(sorted, groups[0].rank, 2)
		best := append(trips, kickers...)
		return Evaluated{
			Value: makeValue(ThreeOfAKind, groups[0].rank, best[3].Rank(), best[4].Rank()),
			Best:  best,
		}, nil

	case groups[0].count == 2 && len(groups) > 1 && groups[1].count == 2:
		high := cardsOfRank(groups[0].rank, 2)
		low := cardsOfRank(groups[1].rank, 2)
		kicker := firstExcluding(sorted, groups[0].rank, 1, groups[1].rank)
		best := append(append(high, low...), kicker...)
		return Evaluated{
			Value: makeValue(TwoPair, groups[0].rank, groups[1].rank, best[4].Rank()),
			Best:  best,
		}, nil

	case groups[0].count == 2:
		pair := cardsOfRank(groups[0].rank, 2)
		kickers := firstExcluding(sorted, groups[0].rank, 3)
		best := append(pair, kickers...)
		return Evaluated{
			Value: makeValue(Pair, groups[0].rank, best[2].Rank(), best[3].Rank(), best[4].Rank()),
			Best:  best,
		}, nil

	default:
		best := sorted[:5]
		return Evaluated{
			Value: makeValue(HighCard, best[0].Rank(), best[1].Rank(), best[2].Rank(),
				best[3].Rank(), best[4].Rank()),
			Best: best,
		}, nil
	}
}

// firstExcluding takes the n highest cards whose rank is not in `skip`.
func firstExcluding(sorted []Card, skip int, n int, alsoSkip ...int) []Card {
	blocked := map[int]bool{skip: true}
	for _, rank := range alsoSkip {
		blocked[rank] = true
	}
	var out []Card
	for _, card := range sorted {
		if blocked[card.Rank()] {
			continue
		}
		out = append(out, card)
		if len(out) == n {
			break
		}
	}
	return out
}

// straightIn returns the highest five-card straight among cards sorted high to
// low, or nil. The wheel (5-4-3-2-A) is handled by treating the ace as low,
// which is the one place a rank is not simply its numeric value.
func straightIn(sorted []Card) []Card {
	byRank := make(map[int]Card, len(sorted))
	for _, card := range sorted {
		if _, seen := byRank[card.Rank()]; !seen {
			byRank[card.Rank()] = card
		}
	}

	for high := 12; high >= 4; high-- {
		run := make([]Card, 0, 5)
		for offset := 0; offset < 5; offset++ {
			card, ok := byRank[high-offset]
			if !ok {
				break
			}
			run = append(run, card)
		}
		if len(run) == 5 {
			return run
		}
	}

	// The wheel: ace, five, four, three, two. It ranks as a five-high straight,
	// the lowest of them all.
	needed := []int{3, 2, 1, 0} // five, four, three, two
	ace, hasAce := byRank[12]
	if !hasAce {
		return nil
	}
	run := make([]Card, 0, 5)
	for _, rank := range needed {
		card, ok := byRank[rank]
		if !ok {
			return nil
		}
		run = append(run, card)
	}
	// Ordered five-high down to the ace, so Best[0] is the five and the hand
	// describes itself correctly as "a straight to the five".
	return append(run, ace)
}

// Compare returns 1 when a beats b, -1 when b beats a, and 0 for a tie.
func Compare(a, b HandValue) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}
