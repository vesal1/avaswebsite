package blackjack

import (
	"math"
	"testing"
)

// splitmix64 is the simulation's deterministic generator. The engine takes a
// deck and does not care where its order came from; the commit-reveal
// generator is for players, and dragging HMAC through twenty million test
// rounds would buy nothing but wall-clock.
type splitmix64 uint64

func (s *splitmix64) next() uint64 {
	*s += 0x9e3779b97f4a7c15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// bounded is an unbiased draw in [0, n), by rejection.
func (s *splitmix64) bounded(n uint64) uint64 {
	limit := (-n) % n // 2^64 mod n, in uint64 arithmetic
	for {
		value := s.next()
		if value >= limit {
			return value % n
		}
	}
}

func (s *splitmix64) shuffledDeck(deck []Card) {
	for i := range deck {
		deck[i] = Card(i)
	}
	for i := len(deck) - 1; i > 0; i-- {
		j := s.bounded(uint64(i + 1))
		deck[i], deck[j] = deck[j], deck[i]
	}
}

// playRounds runs the real engine under the strategy card and returns what
// was staked and what came back, in units of the base stake times two (so a
// 3:2 natural stays integral).
func playRounds(rounds int, rng *splitmix64) (staked, returned int64) {
	const stake = 2 // half-units: a natural pays 5
	deck := make([]Card, DeckSize)
	for i := 0; i < rounds; i++ {
		rng.shuffledDeck(deck)
		round, err := New(deck)
		if err != nil {
			panic(err)
		}
		staked += stake
		for !round.Settled() {
			action, ok := Advise(round)
			if !ok {
				panic("no advice for a live hand")
			}
			extra, err := round.Act(action)
			if err != nil {
				panic(err)
			}
			staked += int64(extra) * stake
		}
		returned += round.PayoutSat(stake)
	}
	return staked, returned
}

// TestPublishedRTPIsReproducible is the full run behind the published figure.
// Fixed seed, fixed round count, no tolerance: the constant either is what
// the engine does or the build fails.
func TestPublishedRTPIsReproducible(t *testing.T) {
	if testing.Short() {
		t.Skip("twenty million rounds")
	}
	generator := splitmix64(0xb1ac_4ac4_5eed_0001)
	staked, returned := playRounds(20_000_000, &generator)
	got := returned * 10_000 / staked
	t.Logf("20M rounds: staked %d, returned %d, RTP %d bps", staked, returned, got)
	if got != PublishedRTPBps {
		t.Errorf("the engine returns %d bps under the card; the published constant says %d", got, PublishedRTPBps)
	}
}

// TestEngineAgreesWithTheModel is the everyday-speed version: half a million
// rounds must land near the published figure, and the gap between the real
// single-deck engine and the infinite-deck DP must be the small, known,
// player-favouring one — not zero, and not large.
func TestEngineAgreesWithTheModel(t *testing.T) {
	generator := splitmix64(0xb1ac_4ac4_5eed_0002)
	staked, returned := playRounds(500_000, &generator)
	rtp := float64(returned) / float64(staked)

	if math.Abs(rtp-float64(PublishedRTPBps)/10_000) > 0.006 {
		t.Errorf("500k rounds returned %.4f, published %.4f", rtp, float64(PublishedRTPBps)/10_000)
	}

	// The DP prices an infinite deck; one real deck runs richer for the
	// player by a known fraction of a percent. Model EV is per initial
	// stake, so compare on that basis.
	initial := int64(500_000 * 2)
	netPerInitial := float64(returned-staked) / float64(initial)
	gap := netPerInitial - ModelEV()
	if gap < -0.002 || gap > 0.012 {
		t.Errorf("single-deck engine nets %.5f per initial stake, model says %.5f: gap %.5f is outside the known band",
			netPerInitial, ModelEV(), gap)
	}
	t.Logf("engine %.5f vs model %.5f per initial stake (gap %.5f)", netPerInitial, ModelEV(), gap)
}
