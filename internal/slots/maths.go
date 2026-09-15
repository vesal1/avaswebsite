package slots

import (
	"fmt"
	"math"

	"github.com/vesal1/avaswebsite/internal/fair"
)

// Maths is a game's theoretical performance, computed from its reel strips and
// paytable rather than measured by playing it.
//
// The return figure is exact. It is not a simulation and not a target: it is
// the sum over every outcome the machine can produce, weighted by how often
// the strips produce it. That is what makes it safe to print on the game's
// page, and it is why Validate refuses stake levels that would round a win
// down — rounding is the one thing that could make the real return differ from
// this number.
type Maths struct {
	// RTP is the fraction of stake returned over the long run, and RTPBps the
	// same figure in basis points for display and for storage.
	RTP    float64
	RTPBps int64

	// The three components add up to RTP.
	BaseLineRTP    float64
	BaseScatterRTP float64
	FeatureRTP     float64

	// TriggerProbability is the chance a paid spin starts a free-spin round,
	// and ExpectedFreeSpins the average length of one when it does.
	TriggerProbability float64
	ExpectedFreeSpins  float64

	// HitFrequency is the share of paid spins that return anything. Unlike
	// RTP this is measured, because whether a spin pays at all depends on the
	// whole window at once and the exact sum runs to billions of terms.
	HitFrequency float64
	HitSamples   int

	// MaxRoundWinX100 is an upper bound on what one round can return, as a
	// multiple of the total stake in hundredths. It is a ceiling for risk
	// limits, not an advertised maximum: no single spin can hit every payline
	// with the top symbol at once.
	MaxRoundWinX100 int64
}

// Percent renders the return as it appears on the game's page.
func (m Maths) Percent() string { return fmt.Sprintf("%.2f%%", m.RTP*100) }

// HouseEdge is the complement of the return.
func (m Maths) HouseEdge() float64 { return 1 - m.RTP }

// Analyse computes a game's exact return and its supporting figures.
func Analyse(game *Game) (Maths, error) {
	maths, err := analyse(game)
	if err != nil {
		return Maths{}, err
	}
	maths.HitFrequency, maths.HitSamples = hitFrequency(game)
	return maths, nil
}

// analyse is the exact part, without the sampled hit frequency. Tuning a
// paytable calls it thousands of times.
func analyse(game *Game) (Maths, error) {
	if err := game.Validate(); err != nil {
		return Maths{}, err
	}

	base := expectedUnitsPerSpin(game, false)
	free := expectedUnitsPerSpin(game, true)
	units := float64(game.BetUnits())

	maths := Maths{
		BaseLineRTP:    base.line / units,
		BaseScatterRTP: base.scatter / units,
	}

	if game.HasFeature() {
		freeReturn := (free.line*float64(game.FreeMultiplierX100)/100 + free.scatter) / units
		spins := newSpinCounter(game, free.scatterDist)
		for count, probability := range base.scatterDist {
			awarded := capSpins(game, 0, freeSpinsFor(game, count))
			if awarded == 0 || probability == 0 {
				continue
			}
			maths.TriggerProbability += probability
			length := spins.expected(awarded, awarded)
			maths.ExpectedFreeSpins += probability * length
			maths.FeatureRTP += probability * length * freeReturn
		}
		if maths.TriggerProbability > 0 {
			maths.ExpectedFreeSpins /= maths.TriggerProbability
		}
	}

	maths.RTP = maths.BaseLineRTP + maths.BaseScatterRTP + maths.FeatureRTP
	maths.RTPBps = int64(math.Round(maths.RTP * 10000))
	maths.MaxRoundWinX100 = maxRoundWinX100(game)
	return maths, nil
}

func freeSpinsFor(game *Game, scatterCount int) int {
	if scatterCount < 0 || scatterCount >= len(game.FreeSpinsFor) {
		return 0
	}
	return game.FreeSpinsFor[scatterCount]
}

// spinReturn is the expected win of one spin, in unit bets, split by source.
type spinReturn struct {
	line    float64
	scatter float64
	// scatterDist[c] is the chance the window shows exactly c scatters.
	scatterDist []float64
}

func expectedUnitsPerSpin(game *Game, free bool) spinReturn {
	strips := game.Strips(free)
	out := spinReturn{scatterDist: []float64{1}}

	if game.Ways {
		out.line = expectedWaysUnits(game, strips)
	} else {
		out.line = float64(len(game.Lines)) * expectedLineUnits(game, strips)
	}

	if scatter, ok := game.Scatter(); ok {
		out.scatterDist = scatterDistribution(game, strips, scatter)
		for count, probability := range out.scatterDist {
			if count < len(game.ScatterPayX100) {
				// Scatter pays a multiple of the total stake, so it is worth
				// that many unit bets.
				out.scatter += probability * float64(game.ScatterPayX100[count]) / 100 * float64(game.BetUnits())
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Payline return
// ---------------------------------------------------------------------------

// expectedLineUnits is the exact expected win of a single payline, in unit
// bets.
//
// Every payline takes exactly one cell from each reel, and the reels stop
// independently, so the symbol a line shows on reel i is distributed by that
// strip's symbol frequencies regardless of what the other reels did. The
// expectation is therefore a walk over symbol combinations rather than over
// the billions of whole-window outcomes — and every payline has the same
// distribution, which is why one number covers them all.
func expectedLineUnits(game *Game, strips [][]SymbolID) float64 {
	wild, hasWild := game.Wild()
	paying := sortedKeys(game.PayX100)

	// Per reel: each distinct symbol and how often the strip shows it.
	type face struct {
		symbol      SymbolID
		probability float64
	}
	faces := make([][]face, len(strips))
	for reel, strip := range strips {
		counts := make(map[SymbolID]int, len(game.Symbols))
		for _, id := range strip {
			counts[id]++
		}
		for id := range game.Symbols {
			if count := counts[SymbolID(id)]; count > 0 {
				faces[reel] = append(faces[reel], face{
					symbol:      SymbolID(id),
					probability: float64(count) / float64(len(strip)),
				})
			}
		}
	}

	total := 0.0
	// alive holds the symbols whose run is still going; best is the largest
	// payout already locked in by a run that has ended.
	var walk func(reel int, probability float64, alive []SymbolID, best int64)
	walk = func(reel int, probability float64, alive []SymbolID, best int64) {
		if reel == len(strips) || len(alive) == 0 {
			// Any symbol still running has reached the far edge.
			for _, symbol := range alive {
				if pay := game.PayX100[symbol][reel]; pay > best {
					best = pay
				}
			}
			total += probability * float64(best) / 100
			return
		}
		for _, f := range faces[reel] {
			next := make([]SymbolID, 0, len(alive))
			ended := best
			for _, symbol := range alive {
				if f.symbol == symbol || (hasWild && f.symbol == wild) {
					next = append(next, symbol)
					continue
				}
				// This symbol's run stopped at the previous reel.
				if pay := game.PayX100[symbol][reel]; pay > ended {
					ended = pay
				}
			}
			walk(reel+1, probability*f.probability, next, ended)
		}
	}
	walk(0, 1, paying, 0)
	return total
}

// ---------------------------------------------------------------------------
// Ways return
// ---------------------------------------------------------------------------

// expectedWaysUnits is the exact expected win of a ways game, in unit bets.
//
// A ways win pays the product of how many matching cells each reel shows. The
// reels are independent, so the expectation of that product is the product of
// the expectations — which turns a combinatorial sum into a handful of
// multiplications. A run of length k also needs reel k+1 to show none of the
// symbol, and that probability comes from walking the strip.
func expectedWaysUnits(game *Game, strips [][]SymbolID) float64 {
	wild, hasWild := game.Wild()
	total := 0.0

	for _, symbol := range sortedKeys(game.PayX100) {
		table := game.PayX100[symbol]
		matches := func(id SymbolID) bool { return id == symbol || (hasWild && id == wild) }

		expected := make([]float64, len(strips))
		absent := make([]float64, len(strips))
		for reel, strip := range strips {
			hits := 0
			for _, id := range strip {
				if matches(id) {
					hits++
				}
			}
			// Each visible row is, on its own, a uniform position on the strip.
			expected[reel] = float64(game.Rows) * float64(hits) / float64(len(strip))
			absent[reel] = emptyWindowProbability(strip, game.Rows, matches)
		}

		running := 1.0
		for length := 1; length <= len(strips); length++ {
			running *= expected[length-1]
			if running == 0 {
				break
			}
			if table[length] == 0 {
				continue
			}
			probability := running
			if length < len(strips) {
				probability *= absent[length]
			}
			total += probability * float64(table[length]) / 100
		}
	}
	return total
}

// emptyWindowProbability is the chance a reel's visible window shows none of a
// symbol, found by trying every stop.
func emptyWindowProbability(strip []SymbolID, rows int, matches func(SymbolID) bool) float64 {
	empty := 0
	for stop := range strip {
		clear := true
		for row := 0; row < rows; row++ {
			if matches(strip[(stop+row)%len(strip)]) {
				clear = false
				break
			}
		}
		if clear {
			empty++
		}
	}
	return float64(empty) / float64(len(strip))
}

// ---------------------------------------------------------------------------
// Scatters and the feature
// ---------------------------------------------------------------------------

// scatterDistribution is the exact distribution of how many scatters the
// window shows, found per reel by trying every stop and then convolving the
// reels together.
func scatterDistribution(game *Game, strips [][]SymbolID, scatter SymbolID) []float64 {
	distribution := []float64{1}
	for _, strip := range strips {
		perReel := make([]float64, game.Rows+1)
		for stop := range strip {
			count := 0
			for row := 0; row < game.Rows; row++ {
				if strip[(stop+row)%len(strip)] == scatter {
					count++
				}
			}
			perReel[count] += 1 / float64(len(strip))
		}
		combined := make([]float64, len(distribution)+game.Rows)
		for have, probability := range distribution {
			if probability == 0 {
				continue
			}
			for extra, chance := range perReel {
				combined[have+extra] += probability * chance
			}
		}
		distribution = combined
	}
	return distribution
}

// spinCounter works out how long a free-spin round runs when retriggers can
// extend it, taking the round's ceiling into account exactly rather than
// assuming the cap never binds.
type spinCounter struct {
	game    *Game
	scatter []float64
	memo    map[[2]int]float64
}

func newSpinCounter(game *Game, freeScatterDist []float64) *spinCounter {
	return &spinCounter{game: game, scatter: freeScatterDist, memo: make(map[[2]int]float64)}
}

// expected returns the average number of spins played given how many are left
// and how many the round has already awarded.
func (s *spinCounter) expected(remaining, awarded int) float64 {
	if remaining <= 0 {
		return 0
	}
	key := [2]int{remaining, awarded}
	if value, ok := s.memo[key]; ok {
		return value
	}
	// Guard against a cycle before recursing; awarded never decreases and
	// remaining only grows when awarded does, so this terminates.
	s.memo[key] = 0

	total := 1.0
	for count, probability := range s.scatter {
		if probability == 0 {
			continue
		}
		extra := capSpins(s.game, awarded, freeSpinsFor(s.game, count))
		total += probability * s.expected(remaining-1+extra, awarded+extra)
	}
	s.memo[key] = total
	return total
}

// ---------------------------------------------------------------------------
// Supporting figures
// ---------------------------------------------------------------------------

// maxRoundWinX100 bounds what one round can return. Every payline is credited
// with its best symbol at full length at once, which no window can actually
// do, so the figure is a ceiling and is used as one.
func maxRoundWinX100(game *Game) int64 {
	best := int64(0)
	for _, symbol := range sortedKeys(game.PayX100) {
		table := game.PayX100[symbol]
		if top := table[len(table)-1]; top > best {
			best = top
		}
	}

	var spin int64
	if game.Ways {
		ways := int64(1)
		for range game.Reels {
			ways *= int64(game.Rows)
		}
		// Every paying symbol at once, each filling the window.
		spin = best * ways * int64(len(game.PayX100))
	} else {
		spin = best * int64(len(game.Lines))
	}
	if len(game.ScatterPayX100) > 0 {
		spin += game.ScatterPayX100[len(game.ScatterPayX100)-1] * int64(game.BetUnits())
	}

	total := spin
	if game.HasFeature() {
		total += int64(game.MaxFreeSpins) * spin * game.FreeMultiplierX100 / 100
	}
	// Convert from unit bets to a multiple of the total stake.
	return total / int64(game.BetUnits())
}

// hitFrequency measures how often a paid spin returns anything.
//
// This one is sampled rather than solved. Whether a spin pays at all depends
// on the whole window, and the reels share cells between paylines, so the
// exact figure is a sum over every combination of stops — tens of billions of
// terms for a five-reel game. The sample is drawn from a fixed seed so the
// number on the page never wanders between builds.
func hitFrequency(game *Game) (float64, int) {
	const samples = 50_000
	hits := 0
	seed, err := fair.RestoreSeed("6869746672657175656e6379", "house-maths", 1)
	if err != nil {
		return 0, 0
	}
	stream := seed.Stream()
	stake := game.MinStakeSat()
	unit := stake / int64(game.BetUnits())
	for i := 0; i < samples; i++ {
		if spin(game, stream, unit, stake, false).WinSat > 0 {
			hits++
		}
	}
	return float64(hits) / float64(samples), samples
}
