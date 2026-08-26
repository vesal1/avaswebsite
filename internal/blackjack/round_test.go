package blackjack

import (
	"encoding/json"
	"errors"
	"testing"
)

// mustCard builds a card from its display form, so test decks read like hands.
func mustCard(t *testing.T, rank string, suit int) Card {
	t.Helper()
	for r, glyph := range rankGlyphs {
		if glyph == rank {
			return Card(suit*13 + r)
		}
	}
	t.Fatalf("no such rank %q", rank)
	return 0
}

// deckStarting is a full legal deck whose top cards are exactly the given
// ones. Ranks repeat across suits, so up to four of a rank can be scripted.
func deckStarting(t *testing.T, ranks ...string) []Card {
	t.Helper()
	used := make(map[Card]bool, len(ranks))
	deck := make([]Card, 0, DeckSize)
	for _, rank := range ranks {
		placed := false
		for suit := 0; suit < 4 && !placed; suit++ {
			card := mustCard(t, rank, suit)
			if !used[card] {
				used[card] = true
				deck = append(deck, card)
				placed = true
			}
		}
		if !placed {
			t.Fatalf("more than four %ss scripted", rank)
		}
	}
	for c := Card(0); c < DeckSize; c++ {
		if !used[c] {
			deck = append(deck, c)
		}
	}
	return deck
}

func TestTotals(t *testing.T) {
	cases := []struct {
		ranks []string
		total int
		soft  bool
	}{
		{[]string{"A", "K"}, 21, true},
		{[]string{"A", "A"}, 12, true},
		{[]string{"A", "A", "9"}, 21, true},
		{[]string{"A", "5"}, 16, true},
		{[]string{"A", "5", "8"}, 14, false},
		{[]string{"10", "9", "3"}, 22, false},
		{[]string{"7", "8"}, 15, false},
	}
	for _, test := range cases {
		hand := make([]Card, len(test.ranks))
		for i, rank := range test.ranks {
			hand[i] = mustCard(t, rank, i%4)
		}
		total, soft := Total(hand)
		if total != test.total || soft != test.soft {
			t.Errorf("%v: total %d soft %v, want %d %v", test.ranks, total, soft, test.total, test.soft)
		}
	}
}

// Deal order: player, dealer up, player, dealer hole.
func TestDealOrderIsPublished(t *testing.T) {
	round, err := New(deckStarting(t, "5", "9", "7", "2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatCards(round.State.Player[0].Cards); got != "5♠ 7♠" {
		t.Errorf("player dealt %s", got)
	}
	if got := FormatCards(round.State.Dealer); got != "9♠ 2♠" {
		t.Errorf("dealer dealt %s", got)
	}
	if round.Settled() {
		t.Error("a 12 against a 9 has decisions left")
	}
}

func TestNaturalPaysThreeToTwo(t *testing.T) {
	round, _ := New(deckStarting(t, "A", "9", "K", "5"))
	if !round.Settled() || !round.State.PlayerBlackjack {
		t.Fatal("an ace and a king is a natural, settled on the spot")
	}
	if got := round.PayoutSat(10_000); got != 25_000 {
		t.Errorf("natural paid %d on 10000, want 25000", got)
	}
	if round.Outcome(10_000) != "won" {
		t.Errorf("outcome %s", round.Outcome(10_000))
	}
}

func TestDealerNaturalEndsAtThePeek(t *testing.T) {
	// Dealer shows an ace, hole card is a ten: the round ends before the
	// player can double into it.
	round, _ := New(deckStarting(t, "8", "A", "7", "K"))
	if !round.Settled() || !round.State.DealerBlackjack {
		t.Fatal("the peek must end the round on a dealer natural")
	}
	if got := round.PayoutSat(10_000); got != 0 {
		t.Errorf("paid %d against a dealer natural", got)
	}

	// Both naturals push.
	both, _ := New(deckStarting(t, "A", "A", "Q", "K"))
	if got := both.PayoutSat(10_000); got != 10_000 {
		t.Errorf("natural against natural paid %d, want the stake back", got)
	}
	if both.Outcome(10_000) != "pushed" {
		t.Errorf("outcome %s", both.Outcome(10_000))
	}
}

func TestHitBustLosesImmediately(t *testing.T) {
	// Player 10+6 v 7, hits into a king: bust, dealer needn't draw.
	round, _ := New(deckStarting(t, "10", "7", "6", "9", "K"))
	if _, err := round.Act(Hit); err != nil {
		t.Fatal(err)
	}
	if !round.Settled() {
		t.Fatal("a bust ends the round")
	}
	if len(round.State.Dealer) != 2 {
		t.Errorf("the dealer drew %d cards against a busted hand", len(round.State.Dealer)-2)
	}
	if round.PayoutSat(10_000) != 0 {
		t.Error("a bust must pay nothing")
	}
}

func TestDoubleTakesOneCardAndDoublesTheBet(t *testing.T) {
	// Player 6+5 v 6: doubles into a 9 for 20; dealer 6+10 draws 10, bust.
	round, _ := New(deckStarting(t, "6", "6", "5", "10", "9", "10"))
	extra, err := round.Act(Double)
	if err != nil {
		t.Fatal(err)
	}
	if extra != 1 {
		t.Errorf("a double commits %d extra units, want 1", extra)
	}
	if !round.Settled() {
		t.Fatal("a double stands after its card")
	}
	if got := round.PayoutSat(10_000); got != 40_000 {
		t.Errorf("won double paid %d, want 40000", got)
	}
	if round.StakedUnits() != 2 {
		t.Errorf("staked units %d", round.StakedUnits())
	}
	// No doubling with three cards.
	three, _ := New(deckStarting(t, "2", "7", "3", "8", "2"))
	if _, err := three.Act(Hit); err != nil {
		t.Fatal(err)
	}
	if _, err := three.Act(Double); !errors.Is(err, ErrBadAction) {
		t.Errorf("double on three cards: %v", err)
	}
}

func TestSplitPlaysTwoHands(t *testing.T) {
	// Player 8+8 v 6. Split: hand one takes a 10 (18, stands via actions),
	// hand two takes a 2 then hits an 8 (20). Dealer 6+9 = 15, draws 4 = 19.
	round, _ := New(deckStarting(t, "8", "6", "8", "9", "10", "2", "8", "4"))
	extra, err := round.Act(Split)
	if err != nil {
		t.Fatal(err)
	}
	if extra != 1 {
		t.Errorf("a split commits %d extra units, want 1", extra)
	}
	if len(round.State.Player) != 2 {
		t.Fatal("a split plays two hands")
	}
	if _, err := round.Act(Stand); err != nil { // hand one at 18
		t.Fatal(err)
	}
	if _, err := round.Act(Hit); err != nil { // hand two 10 -> 18? 8+2=10, hit 8 = 18
		t.Fatal(err)
	}
	if _, err := round.Act(Stand); err != nil {
		t.Fatal(err)
	}
	if !round.Settled() {
		t.Fatal("both hands played, dealer must finish")
	}
	// Dealer 19: hand one 18 loses, hand two 18 loses.
	if got := round.PayoutSat(10_000); got != 0 {
		t.Errorf("paid %d", got)
	}
	if round.StakedUnits() != 2 {
		t.Errorf("staked units %d", round.StakedUnits())
	}
	// No resplitting: the second 8-8 pair cannot split again.
	pair, _ := New(deckStarting(t, "7", "6", "7", "9", "7", "7"))
	if _, err := pair.Act(Split); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.Act(Split); !errors.Is(err, ErrBadAction) {
		t.Errorf("resplit: %v", err)
	}
	// And no doubling a split hand.
	if _, err := pair.Act(Double); !errors.Is(err, ErrBadAction) {
		t.Errorf("double after split: %v", err)
	}
}

func TestSplitAcesTakeOneCardEach(t *testing.T) {
	// A+A v 9: split, one card each, both stand by rule.
	round, _ := New(deckStarting(t, "A", "9", "A", "5", "9", "7"))
	if _, err := round.Act(Split); err != nil {
		t.Fatal(err)
	}
	if !round.Settled() {
		t.Fatal("split aces have no further decisions")
	}
	first, second := round.State.Player[0], round.State.Player[1]
	if len(first.Cards) != 2 || len(second.Cards) != 2 {
		t.Fatalf("split aces drew %d and %d cards", len(first.Cards), len(second.Cards))
	}
	// A+10 after a split is 21, not a natural: dealer 9+5+? plays on.
	if round.State.PlayerBlackjack {
		t.Error("a split 21 must not count as a natural")
	}
}

func TestTwentyOneStandsAutomatically(t *testing.T) {
	// 10+6 hit 5 = 21: nothing a further card improves, hand stands itself.
	round, _ := New(deckStarting(t, "10", "7", "6", "9", "5", "6"))
	if _, err := round.Act(Hit); err != nil {
		t.Fatal(err)
	}
	if !round.Settled() {
		t.Fatal("21 stands and the dealer finishes")
	}
	hand := round.State.Player[0]
	if total, _ := hand.Total(); total != 21 || !hand.Stood {
		t.Errorf("hand %s, stood %v", FormatCards(hand.Cards), hand.Stood)
	}
}

func TestDealerStandsOnSoft17(t *testing.T) {
	// Player stands on 20. Dealer A+6 is soft 17 and must stand under S17.
	round, _ := New(deckStarting(t, "10", "A", "Q", "6"))
	if _, err := round.Act(Stand); err != nil {
		t.Fatal(err)
	}
	if len(round.State.Dealer) != 2 {
		t.Fatalf("dealer drew on soft 17: %s", FormatCards(round.State.Dealer))
	}
	if got := round.PayoutSat(10_000); got != 20_000 {
		t.Errorf("20 against soft 17 paid %d", got)
	}
}

// TestStateRoundTrips: a round survives the trip through JSON and the deck,
// which is how it lives between requests.
func TestStateRoundTrips(t *testing.T) {
	deck := deckStarting(t, "8", "6", "8", "9", "10", "2", "8", "4")
	round, _ := New(deck)
	if _, err := round.Act(Split); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(round.State)
	if err != nil {
		t.Fatal(err)
	}

	var state State
	if err := json.Unmarshal(encoded, &state); err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(deck, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Act(Stand); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Act(Hit); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Act(Stand); err != nil {
		t.Fatal(err)
	}
	if !resumed.Settled() {
		t.Fatal("the resumed round did not finish")
	}
}

// TestReplayFromActions: the verification path — the same deck and the same
// action log must land on the identical settled state.
func TestReplayFromActions(t *testing.T) {
	deck := deckStarting(t, "8", "6", "8", "9", "10", "2", "8", "4")
	played, _ := New(deck)
	for _, action := range []Action{Split, Stand, Hit, Stand} {
		if _, err := played.Act(action); err != nil {
			t.Fatal(err)
		}
	}

	replayed, _ := New(deck)
	for _, action := range played.State.Actions {
		if _, err := replayed.Act(action); err != nil {
			t.Fatalf("replaying %s: %v", action, err)
		}
	}
	if replayed.PayoutSat(10_000) != played.PayoutSat(10_000) {
		t.Error("the replay settled differently from the play")
	}
	a, _ := json.Marshal(played.State)
	b, _ := json.Marshal(replayed.State)
	if string(a) != string(b) {
		t.Errorf("replayed state differs:\n%s\n%s", a, b)
	}
}

func TestRejectsABadDeck(t *testing.T) {
	if _, err := New(make([]Card, DeckSize)); err == nil {
		t.Error("a deck of 52 aces of spades was accepted")
	}
	if _, err := New(NewDeck()[:51]); err == nil {
		t.Error("a 51-card deck was accepted")
	}
}
