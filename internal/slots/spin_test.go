package slots

import (
	"strings"
	"testing"

	"github.com/vesal1/avaswebsite/internal/fair"
)

func mustSeed(t *testing.T, hex, client string, nonce int64) *fair.Seed {
	t.Helper()
	seed, err := fair.RestoreSeed(hex, client, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

func TestEveryGameValidates(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatalf("the floor did not open: %v", err)
	}
	if len(All()) < 4 {
		t.Fatalf("only %d games on the floor", len(All()))
	}
	for _, game := range All() {
		if game.Blurb == "" || game.Volatility == "" {
			t.Errorf("%s has nothing to tell a player about itself", game.Key)
		}
		if _, ok := MathsFor(game.Key); !ok {
			t.Errorf("%s is on the floor without a published return", game.Key)
		}
	}
}

// TestSpinIsReproducible is the promise made to the player: the seed decides
// the spin, and the same seed always decides it the same way.
func TestSpinIsReproducible(t *testing.T) {
	game, _ := Lookup("bitcoin-bonanza")
	stake := game.StakeLevels[2]

	first, err := Play(game, mustSeed(t, "0011223344", "player", 9), stake)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Play(game, mustSeed(t, "0011223344", "player", 9), stake)
	if err != nil {
		t.Fatal(err)
	}
	if first.TotalWinSat != second.TotalWinSat {
		t.Fatalf("the same seed paid %d then %d", first.TotalWinSat, second.TotalWinSat)
	}
	for reel, stop := range first.Base.Stops {
		if second.Base.Stops[reel] != stop {
			t.Fatalf("reel %d stopped at %d then %d", reel+1, stop, second.Base.Stops[reel])
		}
	}

	// A different client seed is a different spin: the player's contribution
	// has to matter, or committing to the server seed proves nothing useful.
	other, _ := Play(game, mustSeed(t, "0011223344", "someone-else", 9), stake)
	same := true
	for reel, stop := range first.Base.Stops {
		if other.Base.Stops[reel] != stop {
			same = false
		}
	}
	if same {
		t.Error("changing the client seed did not change the reels")
	}
}

// TestStakeScalesExactly is the no-rounding promise. Ten times the stake must
// pay exactly ten times as much, or the advertised return is a ceiling rather
// than a figure.
func TestStakeScalesExactly(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, game := range All() {
		base := game.MinStakeSat()
		for _, stake := range game.StakeLevels {
			if stake%base != 0 {
				continue
			}
			factor := stake / base
			for nonce := int64(0); nonce < 400; nonce++ {
				seed := mustSeed(t, "abcdef", "scaling", nonce)
				small, err := Play(game, seed, base)
				if err != nil {
					t.Fatal(err)
				}
				large, err := Play(game, mustSeed(t, "abcdef", "scaling", nonce), stake)
				if err != nil {
					t.Fatal(err)
				}
				if large.TotalWinSat != small.TotalWinSat*factor {
					t.Fatalf("%s at %d sat paid %d; at %d sat it paid %d, not %d",
						game.Key, base, small.TotalWinSat, stake,
						large.TotalWinSat, small.TotalWinSat*factor)
				}
			}
		}
	}
}

func TestStakeMustBeOnTheLadder(t *testing.T) {
	game, _ := Lookup("aztec-vault")
	if _, err := Play(game, mustSeed(t, "aa", "c", 1), game.MinStakeSat()-1); err == nil {
		t.Error("an off-ladder stake was accepted")
	}
	if _, err := Play(game, mustSeed(t, "aa", "c", 1), 0); err == nil {
		t.Error("a zero stake was accepted")
	}
}

// ---------------------------------------------------------------------------
// Scoring
// ---------------------------------------------------------------------------

// fixed builds a game whose reels are single stops, so a window can be written
// out by hand and scored.
func fixed(t *testing.T, rows int, ways bool, columns ...string) (*Game, *builder) {
	t.Helper()
	b := newBuilder(
		card("A", "Ace", "A"),
		card("B", "Bee", "B"),
		card("C", "Cee", "C"),
		wildFace("W", "Wild", "W"),
		scatterFace("S", "Scatter", "S"),
	)
	game := &Game{
		Key: "fixed", Name: "Fixed", Rows: rows, Symbols: b.symbols,
		PayX100: b.pays(map[string][]int64{
			"A": {0, 0, 200, 1000},
			"B": {0, 0, 100, 500},
			"C": {0, 0, 0, 300},
		}),
		ScatterPayX100: make([]int64, 3*rows+1),
		FreeSpinsFor:   make([]int, 3*rows+1),
	}
	for _, column := range columns {
		game.Reels = append(game.Reels, b.strip(column))
	}
	if ways {
		game.Ways, game.WaysUnits = true, 4
		game.StakeLevels = stakeLadder(4, 1)
	} else {
		for row := 0; row < rows; row++ {
			game.Lines = append(game.Lines, []int{row, row, row})
		}
		game.StakeLevels = stakeLadder(rows, 1)
	}
	game.ScatterPayX100[2] = 500
	if err := game.Validate(); err != nil {
		t.Fatal(err)
	}
	return game, b
}

func TestWildSubstitutesButDoesNotPayAlone(t *testing.T) {
	game, _ := fixed(t, 1, false, "A", "W", "A")
	result := scoreStops(game, []int{0, 0, 0}, 100, 100, false)
	if len(result.Wins) != 1 || result.Wins[0].Count != 3 {
		t.Fatalf("the wild did not complete the line: %+v", result.Wins)
	}
	if result.Wins[0].Symbol != 0 {
		t.Errorf("the win was credited to %s, not the Ace", result.Wins[0].Name)
	}

	// Three wilds pay as the best symbol they stand in for, and never as a
	// wild in their own right.
	all, _ := fixed(t, 1, false, "W", "W", "W")
	best := scoreStops(all, []int{0, 0, 0}, 100, 100, false)
	if len(best.Wins) != 1 || best.Wins[0].Name != "Ace" {
		t.Fatalf("three wilds scored as %+v", best.Wins)
	}
}

func TestScatterIsNotSubstitutedFor(t *testing.T) {
	game, _ := fixed(t, 1, false, "S", "W", "S")
	result := scoreStops(game, []int{0, 0, 0}, 100, 100, false)
	if result.ScatterCount != 2 {
		t.Errorf("counted %d scatters, want 2: a wild must not stand in for one", result.ScatterCount)
	}
}

func TestALinePaysItsBestCombinationOnly(t *testing.T) {
	// Ace pays 10x for three; Bee pays 1x for two. The line shows A A B, so
	// only the two Aces can pay — and exactly once.
	game, _ := fixed(t, 1, false, "A", "A", "B")
	result := scoreStops(game, []int{0, 0, 0}, 100, 100, false)
	if len(result.Wins) != 1 {
		t.Fatalf("one line produced %d wins", len(result.Wins))
	}
	if result.Wins[0].Count != 2 || result.Wins[0].AmountSat != 200 {
		t.Errorf("got %d of a kind for %d sat, want 2 for 200", result.Wins[0].Count, result.Wins[0].AmountSat)
	}
}

func TestWaysCountsEveryCombination(t *testing.T) {
	// Two Aces on reel one, one on reel two, two on reel three: 2 x 1 x 2 = 4
	// ways at 10x the unit bet.
	game, _ := fixed(t, 2, true, "A A", "A B", "A A")
	result := scoreStops(game, []int{0, 0, 0}, 100, 400, false)
	var ace *Win
	for i := range result.Wins {
		if result.Wins[i].Name == "Ace" {
			ace = &result.Wins[i]
		}
	}
	if ace == nil {
		t.Fatal("the Aces did not pay")
	}
	if ace.Ways != 4 || ace.Count != 3 {
		t.Fatalf("got %d ways of %d, want 4 of 3", ace.Ways, ace.Count)
	}
	if ace.AmountSat != 100*1000/100*4 {
		t.Errorf("paid %d sat, want %d", ace.AmountSat, 100*1000/100*4)
	}
}

func TestWaysPaysEverySymbolAtOnce(t *testing.T) {
	game, _ := fixed(t, 2, true, "A B", "A B", "A B")
	result := scoreStops(game, []int{0, 0, 0}, 100, 400, false)
	names := map[string]bool{}
	for _, win := range result.Wins {
		names[win.Name] = true
	}
	if !names["Ace"] || !names["Bee"] {
		t.Errorf("a ways game must pay both runs at once, got %v", names)
	}
}

func TestFreeSpinMultiplierAppliesToLinesNotScatters(t *testing.T) {
	game, _ := fixed(t, 1, false, "A", "A", "A")
	game.FreeMultiplierX100 = 300
	game.FreeSpinsFor[2] = 5
	game.MaxFreeSpins = 10

	base := scoreStops(game, []int{0, 0, 0}, 100, 100, false)
	free := scoreStops(game, []int{0, 0, 0}, 100, 100, true)
	if free.WinSat != base.WinSat*3 {
		t.Errorf("a free spin paid %d, want three times the base %d", free.WinSat, base.WinSat)
	}
}

// ---------------------------------------------------------------------------
// Specification errors
// ---------------------------------------------------------------------------

func TestValidateCatchesMistakes(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*Game)
		want   string
	}{
		{"a stake that would be rounded", func(g *Game) {
			g.StakeLevels = []int64{int64(g.BetUnits())*100 + 1}
		}, "rounded down"},
		{"a payline off the window", func(g *Game) {
			g.Lines[0] = []int{0, 0, 99, 0, 0}
		}, "outside"},
		{"a duplicated payline", func(g *Game) {
			g.Lines[1] = append([]int(nil), g.Lines[0]...)
		}, "twice"},
		{"a paytable that shrinks", func(g *Game) {
			id, _ := lookupCode(g, "T")
			g.PayX100[id] = []int64{0, 0, 0, 5000, 100, 10000}
		}, "but"},
		{"a wild with a paytable", func(g *Game) {
			id, _ := lookupCode(g, "W")
			g.PayX100[id] = []int64{0, 0, 0, 100, 200, 300}
		}, "must not have one"},
		{"an uncapped free-spin round", func(g *Game) {
			g.MaxFreeSpins = 0
		}, "forever"},
		{"a fractional free-spin multiplier", func(g *Game) {
			g.FreeMultiplierX100 = 250
		}, "not a whole number"},
		{"a strip shorter than the window", func(g *Game) {
			g.Reels[0] = g.Reels[0][:2]
		}, "fewer than"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			game := bitcoinBonanza()
			game.Lines = append([][]int(nil), game.Lines...)
			game.Reels = append([][]SymbolID(nil), game.Reels...)
			test.break_(game)
			err := game.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func lookupCode(game *Game, code string) (SymbolID, bool) {
	for id, symbol := range game.Symbols {
		if symbol.Code == code {
			return SymbolID(id), true
		}
	}
	return 0, false
}

// TestGoldenSpin pins one machine's output for a known seed. The verification
// page invites players to replay a spin from its seed; that makes the reel
// strips and the draw order a compatibility surface, and a refactor that moves
// them would quietly invalidate every spin anybody had checked.
func TestGoldenSpin(t *testing.T) {
	game, _ := Lookup("bitcoin-bonanza")
	round, err := Play(game, mustSeed(t, "00112233445566778899aabbccddeeff", "golden", 2), game.StakeLevels[0])
	if err != nil {
		t.Fatal(err)
	}
	const (
		wantStops = "[23 9 26 9 9]"
		wantWin   = int64(2352000)
		wantFree  = 10
	)
	if got := fmtStops(round.Base.Stops); got != wantStops {
		t.Errorf("the reels now stop at %s, they used to stop at %s", got, wantStops)
	}
	if round.FreeSpinsPlayed != wantFree {
		t.Errorf("the round now runs %d free spins, it used to run %d", round.FreeSpinsPlayed, wantFree)
	}
	if round.TotalWinSat != wantWin {
		t.Errorf("the round now pays %d sat, it used to pay %d", round.TotalWinSat, wantWin)
	}
}

func fmtStops(stops []int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, stop := range stops {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(itoa(stop))
	}
	b.WriteByte(']')
	return b.String()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}
