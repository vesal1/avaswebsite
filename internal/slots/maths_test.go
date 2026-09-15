package slots

import (
	"math"
	"testing"

	"github.com/vesal1/avaswebsite/internal/fair"
)

// toyLine is a small line game whose every outcome can be listed, so the
// closed-form return can be checked against brute force rather than against
// another derivation of the same algebra.
func toyLine() *Game {
	b := newBuilder(
		card("A", "Ace", "A"),
		card("B", "Bee", "B"),
		card("C", "Cee", "C"),
		wildFace("W", "Wild", "W"),
	)
	return &Game{
		Key: "toy-line", Name: "Toy Line", Rows: 3,
		Symbols: b.symbols,
		Reels: [][]SymbolID{
			b.strip("A B C A B W C B"),
			b.strip("B A C W B C A A"),
			b.strip("C A B B W A C B"),
		},
		Lines: lines([]int{1, 1, 1}, []int{0, 1, 2}, []int{2, 0, 1}),
		PayX100: b.pays(map[string][]int64{
			"A": {0, 0, 200, 5000},
			"B": {0, 0, 100, 2500},
			"C": {0, 0, 0, 1000},
		}),
		StakeLevels: stakeLadder(3, 1, 10),
	}
}

func toyWays() *Game {
	b := newBuilder(
		card("A", "Ace", "A"),
		card("B", "Bee", "B"),
		card("C", "Cee", "C"),
		wildFace("W", "Wild", "W"),
	)
	return &Game{
		Key: "toy-ways", Name: "Toy Ways", Rows: 2,
		Ways: true, WaysUnits: 4,
		Symbols: b.symbols,
		Reels: [][]SymbolID{
			b.strip("A B C A B W C B A"),
			b.strip("B A C W B C A A B"),
			b.strip("C A B B W A C B A"),
		},
		PayX100: b.pays(map[string][]int64{
			"A": {0, 0, 150, 4000},
			"B": {0, 0, 100, 2000},
			"C": {0, 0, 0, 800},
		}),
		StakeLevels: stakeLadder(4, 1, 10),
	}
}

func toyFeature() *Game {
	b := newBuilder(
		card("A", "Ace", "A"),
		card("B", "Bee", "B"),
		wildFace("W", "Wild", "W"),
		scatterFace("S", "Scatter", "S"),
	)
	return &Game{
		Key: "toy-feature", Name: "Toy Feature", Rows: 2,
		Symbols: b.symbols,
		Reels: [][]SymbolID{
			b.strip("A B S A B W S B"),
			b.strip("B A S W B A S B"),
			b.strip("S A B B W A S B"),
		},
		Lines: lines([]int{0, 0, 0}, []int{1, 1, 1}),
		PayX100: b.pays(map[string][]int64{
			"A": {0, 0, 200, 4000},
			"B": {0, 0, 100, 2000},
		}),
		ScatterPayX100:     []int64{0, 0, 200, 1000, 0, 0, 0},
		FreeSpinsFor:       []int{0, 0, 2, 4, 4, 4, 4},
		FreeMultiplierX100: 200,
		MaxFreeSpins:       12,
		StakeLevels:        stakeLadder(2, 1, 10),
	}
}

// exhaustiveReturn walks every combination of reel stops and averages what the
// paying code path actually pays. Nothing is sampled and nothing is derived:
// this is the machine's return by definition.
func exhaustiveReturn(game *Game, free bool) (line, scatter float64, scatterDist []float64) {
	strips := game.Strips(free)
	stake := game.MinStakeSat()
	unit := stake / int64(game.BetUnits())

	combinations := 1
	for _, strip := range strips {
		combinations *= len(strip)
	}
	scatterDist = make([]float64, len(strips)*game.Rows+1)

	stops := make([]int, len(strips))
	var walk func(reel int)
	walk = func(reel int) {
		if reel == len(strips) {
			result := scoreStops(game, append([]int(nil), stops...), unit, stake, free)
			for _, win := range result.Wins {
				if win.Kind == "scatter" {
					scatter += float64(win.AmountSat)
				} else {
					line += float64(win.AmountSat)
				}
			}
			scatterDist[result.ScatterCount]++
			return
		}
		for stop := range strips[reel] {
			stops[reel] = stop
			walk(reel + 1)
		}
	}
	walk(0)

	// Return is measured against the total stake, which is BetUnits unit bets.
	total := float64(combinations) * float64(stake)
	line /= total
	scatter /= total
	for i := range scatterDist {
		scatterDist[i] /= float64(combinations)
	}
	return line, scatter, scatterDist
}

func close(t *testing.T, label string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s = %.8f, want %.8f (difference %.2e)", label, got, want, got-want)
	}
}

// TestLineReturnMatchesBruteForce is the test the published figure rests on.
//
// Analyse works out a payline's return with a walk over symbol combinations,
// on the argument that reels stop independently. This checks that argument the
// only way worth checking it: by playing every possible spin through the same
// function that pays a real player, and comparing the average.
func TestLineReturnMatchesBruteForce(t *testing.T) {
	game := toyLine()
	maths, err := analyse(game)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := exhaustiveReturn(game, false)
	close(t, "line return", maths.BaseLineRTP, line, 1e-9)
}

func TestWaysReturnMatchesBruteForce(t *testing.T) {
	game := toyWays()
	maths, err := analyse(game)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := exhaustiveReturn(game, false)
	close(t, "ways return", maths.BaseLineRTP, line, 1e-9)
}

func TestScatterReturnAndDistributionMatchBruteForce(t *testing.T) {
	game := toyFeature()
	maths, err := analyse(game)
	if err != nil {
		t.Fatal(err)
	}
	line, scatter, distribution := exhaustiveReturn(game, false)

	close(t, "line return", maths.BaseLineRTP, line, 1e-9)
	close(t, "scatter return", maths.BaseScatterRTP, scatter, 1e-9)

	symbol, _ := game.Scatter()
	computed := scatterDistribution(game, game.Reels, symbol)
	trigger := 0.0
	for count, probability := range distribution {
		if count < len(computed) {
			close(t, "scatter distribution", computed[count], probability, 1e-9)
		} else if probability != 0 {
			t.Errorf("brute force found %d scatters, which the model says cannot happen", count)
		}
		if count < len(game.FreeSpinsFor) && game.FreeSpinsFor[count] > 0 {
			trigger += probability
		}
	}
	close(t, "trigger probability", maths.TriggerProbability, trigger, 1e-9)
}

// TestFeatureReturnMatchesPlay checks the recursive part: how long a
// retriggering free-spin round runs, and what it pays, against actually
// playing millions of them.
func TestFeatureReturnMatchesPlay(t *testing.T) {
	game := toyFeature()
	maths, err := analyse(game)
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 400_000
	stake := game.MinStakeSat()
	seed, err := fair.RestoreSeed("f00d", "feature-check", 1)
	if err != nil {
		t.Fatal(err)
	}
	var (
		staked    int64
		returned  int64
		triggered int
		freeSpins int
	)
	for nonce := 0; nonce < rounds; nonce++ {
		seed, _ = fair.RestoreSeed("f00d", "feature-check", int64(nonce))
		round, err := Play(game, seed, stake)
		if err != nil {
			t.Fatal(err)
		}
		staked += stake
		returned += round.TotalWinSat
		if round.Base.FreeSpinsAwarded > 0 {
			triggered++
			freeSpins += round.FreeSpinsPlayed
		}
	}

	played := float64(returned) / float64(staked)
	if relative := math.Abs(played-maths.RTP) / maths.RTP; relative > 0.01 {
		t.Errorf("400k rounds returned %.6f, the model says %.6f (%.2f%% out)",
			played, maths.RTP, relative*100)
	}

	averageSpins := float64(freeSpins) / float64(triggered)
	close(t, "average feature length", averageSpins, maths.ExpectedFreeSpins, 0.05)
}

// TestPublishedReturnMatchesPlay plays every real game long enough to see the
// published figure emerge. Slot variance is large, so the tolerance is wide;
// the exact checks above are what pin the number down, and this is what would
// catch a mismatch between the model and the machine.
func TestPublishedReturnMatchesPlay(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, game := range All() {
		game := game
		t.Run(game.Key, func(t *testing.T) {
			t.Parallel()
			maths, _ := MathsFor(game.Key)
			const rounds = 1_500_000
			stake := game.MinStakeSat()
			var staked, returned int64
			for nonce := 0; nonce < rounds; nonce++ {
				seed, err := fair.RestoreSeed("5eed", "return-check-"+game.Key, int64(nonce))
				if err != nil {
					t.Fatal(err)
				}
				round, err := Play(game, seed, stake)
				if err != nil {
					t.Fatal(err)
				}
				staked += stake
				returned += round.TotalWinSat
			}
			played := float64(returned) / float64(staked)
			t.Logf("%s: %d rounds returned %.4f, published %.4f", game.Key, rounds, played, maths.RTP)
			if math.Abs(played-maths.RTP) > 0.03 {
				t.Errorf("%s returned %.4f over %d spins, published %.4f",
					game.Key, played, rounds, maths.RTP)
			}
		})
	}
}

// TestEveryGameIsPricedInRange keeps a machine from reaching the floor with a
// return nobody looked at. The band is deliberately narrow: a game outside it
// is a mistake in a strip or a paytable, not a design decision.
func TestEveryGameIsPricedInRange(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, game := range All() {
		maths, ok := MathsFor(game.Key)
		if !ok {
			t.Fatalf("%s has no published maths", game.Key)
		}
		if maths.RTP < 0.94 || maths.RTP > 0.97 {
			t.Errorf("%s returns %.2f%%, outside the 94-97%% band", game.Key, maths.RTP*100)
		}
		if maths.HitFrequency <= 0 || maths.HitFrequency > 0.95 {
			t.Errorf("%s hits on %.1f%% of spins", game.Key, maths.HitFrequency*100)
		}
		if game.HasFeature() && maths.TriggerProbability <= 0 {
			t.Errorf("%s has a free-spin round that can never be triggered", game.Key)
		}
		t.Logf("%-16s %6.2f%%  hit %5.2f%%  feature 1 in %.0f, %.1f spins",
			game.Key, maths.RTP*100, maths.HitFrequency*100,
			1/math.Max(maths.TriggerProbability, 1e-12), maths.ExpectedFreeSpins)
	}
}

// TestFeatureAlwaysTerminates makes sure the retrigger cap binds. Without it a
// free-spin round is a random walk that can, in principle, never end.
func TestFeatureAlwaysTerminates(t *testing.T) {
	game := toyFeature()
	// A machine that awards spins on every single spin: only the cap stops it.
	game.FreeSpinsFor = []int{4, 4, 4, 4, 4, 4, 4}
	if err := game.Validate(); err != nil {
		t.Fatal(err)
	}
	for nonce := 0; nonce < 200; nonce++ {
		seed, _ := fair.RestoreSeed("beef", "runaway", int64(nonce))
		round, err := Play(game, seed, game.MinStakeSat())
		if err != nil {
			t.Fatal(err)
		}
		if round.FreeSpinsPlayed > game.MaxFreeSpins {
			t.Fatalf("played %d free spins, cap is %d", round.FreeSpinsPlayed, game.MaxFreeSpins)
		}
	}
}
