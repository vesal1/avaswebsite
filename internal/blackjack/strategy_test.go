package blackjack

import (
	"math"
	"testing"
)

func TestDealerDistributionsSumToOne(t *testing.T) {
	for up := 1; up <= 10; up++ {
		dist := dealerFinal(up)
		sum := 0.0
		for _, p := range dist {
			sum += p
		}
		if math.Abs(sum-1) > 1e-12 {
			t.Errorf("upcard %d: distribution sums to %.15f", up, sum)
		}
	}
}

// TestDealerBustRatesMatchTheLiterature pins the recursion against the
// published S17 bust rates by upcard (conditioned on no natural for the ace
// and the ten). A wrong dealer model poisons every number after it.
func TestDealerBustRatesMatchTheLiterature(t *testing.T) {
	want := map[int]float64{
		2: 0.354, 3: 0.374, 4: 0.394, 5: 0.416, 6: 0.423,
		7: 0.262, 8: 0.239, 9: 0.233, 10: 0.214, 1: 0.170,
	}
	for up, expected := range want {
		bust := dealerFinal(up)[5]
		if math.Abs(bust-expected) > 0.02 {
			t.Errorf("upcard %d busts %.3f, literature says about %.3f", up, bust, expected)
		}
	}
}

// TestStrategyCardMatchesBasicStrategy spot-checks the DP against the cells
// of basic strategy that every published card agrees on for these rules
// (S17, double any two, no double after split).
func TestStrategyCardMatchesBasicStrategy(t *testing.T) {
	card := CardTable()

	hard := func(total, up int) Action { return card.Hard[total][up].Primary }
	soft := func(total, up int) Action { return card.Soft[total][up].Primary }

	cases := []struct {
		name string
		got  Action
		want Action
	}{
		{"hard 16 v 10 hits", hard(16, 10), Hit},
		{"hard 16 v 6 stands", hard(16, 6), Stand},
		{"hard 13 v 6 stands", hard(13, 6), Stand},
		{"hard 12 v 2 hits", hard(12, 2), Hit},
		{"hard 12 v 4 stands", hard(12, 4), Stand},
		{"hard 11 v 6 doubles", hard(11, 6), Double},
		{"hard 11 v 10 doubles", hard(11, 10), Double},
		{"hard 10 v 9 doubles", hard(10, 9), Double},
		{"hard 10 v 10 hits", hard(10, 10), Hit},
		{"hard 9 v 3 doubles", hard(9, 3), Double},
		{"hard 9 v 2 hits", hard(9, 2), Hit},
		{"hard 8 v 6 hits", hard(8, 6), Hit},
		{"hard 17 v 10 stands", hard(17, 10), Stand},
		{"soft 18 v 9 hits", soft(18, 9), Hit},
		{"soft 18 v 6 doubles", soft(18, 6), Double},
		{"soft 18 v 2 stands", soft(18, 2), Stand},
		{"soft 19 v 6 stands", soft(19, 6), Stand},
		{"soft 17 v 3 doubles", soft(17, 3), Double},
		// Soft 13 against a 5 is a genuine borderline that flips between
		// deck models, so it is no use as a check. Soft 15 against a 6 is
		// not: every published card doubles it.
		{"soft 15 v 6 doubles", soft(15, 6), Double},
	}
	for _, test := range cases {
		if test.got != test.want {
			t.Errorf("%s: card says %s", test.name, test.got)
		}
	}

	// The pair cells every card agrees on.
	if !card.Pairs[1][10] || !card.Pairs[1][6] {
		t.Error("aces split against everything")
	}
	if !card.Pairs[8][10] || !card.Pairs[8][6] {
		t.Error("eights split against everything")
	}
	if card.Pairs[10][6] {
		t.Error("tens never split: twenty is not a hand to break up")
	}
	if card.Pairs[5][6] {
		t.Error("fives never split: ten is a hand to double, not divide")
	}
	if card.Pairs[9][7] {
		t.Error("nines stand against a seven")
	}
	if !card.Pairs[9][6] {
		t.Error("nines split against a six")
	}

	// The fallback column: a double the table no longer offers falls back to
	// the hit/stand choice.
	if card.Hard[11][6].Fallback != Hit {
		t.Error("hard 11's fallback is a hit")
	}
	if card.Soft[18][6].Fallback != Stand {
		t.Error("soft 18 v 6 falls back to standing")
	}
}

// TestModelEVIsASmallHouseEdge: under this model and card the game keeps a
// fraction of a percent. Far outside that band means the DP or the rules are
// wrong, not that the house got lucky.
func TestModelEVIsASmallHouseEdge(t *testing.T) {
	ev := ModelEV()
	if ev > -0.002 || ev < -0.012 {
		t.Errorf("model EV per unit staked = %.5f, expected a small house edge", ev)
	}
	t.Logf("model EV per initial stake: %.5f (return %.3f%%)", ev, (1+ev)*100)
}

func TestAdviseRespectsTheTable(t *testing.T) {
	// Hard 11 v 6 with two cards: double. After a hit, the double is off the
	// table and the card falls back.
	round, _ := New(deckStarting(t, "6", "6", "5", "10", "2"))
	action, ok := Advise(round)
	if !ok || action != Double {
		t.Fatalf("11 v 6 advises %s", action)
	}
	if _, err := round.Act(Hit); err != nil { // 13 v 6, three cards
		t.Fatal(err)
	}
	if !round.Settled() {
		action, ok = Advise(round)
		if !ok || action != Stand {
			t.Errorf("13 v 6 with three cards advises %s", action)
		}
	}

	// 8,8 v 10: the card says split even here.
	pair, _ := New(deckStarting(t, "8", "10", "8", "5"))
	if action, _ := Advise(pair); action != Split {
		t.Errorf("8,8 v 10 advises %s, the card says split", action)
	}
}
