// Package slots is the casino's slot machine engine.
//
// A slot game here is a specification, not code: reel strips, paylines, a
// paytable and a feature set. The engine spins it, evaluates it, and — this is
// the part that matters — computes its exact return to player by enumerating
// the maths rather than by simulating it. That figure is published next to the
// game, so a player can see what the machine returns before spending anything
// on it.
//
// Two rules run through the whole package:
//
//   - The outcome comes from internal/fair, the same commit-reveal generator
//     that shuffles the poker decks. The reel stops are fixed by a seed the
//     house commits to before the spin and publishes afterwards. There is no
//     "adjust the odds if the player is winning" lever, because there is
//     nowhere to put one.
//
//   - Nothing is rounded in the player's disfavour. Stakes are restricted to
//     levels that divide exactly into the paytable's hundredths, so a win is
//     never shaved by integer division. The published RTP is therefore the
//     real one, not an approximation.
package slots

import (
	"fmt"
	"sort"
	"strings"
)

// SymbolID indexes a game's symbol table.
type SymbolID int

// Symbol is one face on the reels.
type Symbol struct {
	// Code is the short name used when writing reel strips, so a strip reads
	// like a par sheet rather than a wall of identifiers.
	Code string
	Name string
	// Glyph is what the browser draws.
	Glyph string
	// Wild substitutes for every paying symbol. Wilds here are pure
	// substitutes with no line value of their own: it keeps the paytable
	// honest (no "five wilds pays as wild or as the best symbol, whichever
	// suits") and keeps the RTP arithmetic exact.
	Wild bool
	// Scatter pays on count anywhere in the window and triggers the feature.
	// It is never substituted for by a wild.
	Scatter bool
	// Blank is a stop that shows nothing and pays nothing. Three-reel
	// machines are mostly blanks; without them the only way to space out a
	// symbol is to crowd the strip with another paying one.
	Blank bool
}

// Pays reports whether a symbol has line values of its own.
func (s Symbol) Pays() bool { return !s.Wild && !s.Scatter && !s.Blank }

// Game is a complete slot machine.
type Game struct {
	Key   string
	Name  string
	Blurb string
	// Volatility is a plain-English label, not an input to the maths.
	Volatility string

	Symbols []Symbol
	Rows    int
	// Reels are the strips, one per reel, as they read top to bottom. A stop
	// is a position on the strip; the window shows Rows consecutive stops
	// from there, wrapping at the end.
	Reels [][]SymbolID
	// FreeReels are the strips used during free spins. Nil means the base
	// strips are used throughout.
	FreeReels [][]SymbolID

	// Lines gives, for each payline, the row it occupies on each reel. Empty
	// for a ways game.
	Lines [][]int
	// Ways switches the game to adjacent-ways scoring: every combination of
	// matching symbols on consecutive reels from the left pays, and every
	// symbol pays at once rather than one best win per line.
	Ways bool
	// WaysUnits is the bet size of a ways game in line-bet units, the way a
	// real machine charges 25 coins for 1024 ways.
	WaysUnits int

	// PayX100 is the line paytable: PayX100[symbol][count] is the win as a
	// multiplier of the unit bet, in hundredths. Index 0 and any count below
	// the minimum are zero.
	PayX100 map[SymbolID][]int64
	// ScatterPayX100 is the scatter win by count, as a multiplier of the
	// total bet, in hundredths.
	ScatterPayX100 []int64
	// FreeSpinsFor is the number of free spins awarded by a scatter count.
	FreeSpinsFor []int
	// FreeMultiplierX100 multiplies line wins during free spins. Scatter wins
	// are not multiplied, which is the usual convention and is what the RTP
	// calculation assumes.
	FreeMultiplierX100 int64
	// MaxFreeSpins caps the total spins a feature can award, so a retriggering
	// round always terminates. The cap is included in the published RTP.
	MaxFreeSpins int

	// StakeLevels are the total stakes the game accepts, smallest first.
	StakeLevels []int64
}

// BetUnits is how many unit bets make up one total stake.
func (g *Game) BetUnits() int {
	if g.Ways {
		return g.WaysUnits
	}
	return len(g.Lines)
}

// ReelCount is the number of reels.
func (g *Game) ReelCount() int { return len(g.Reels) }

// Strips returns the strips in use for a spin.
func (g *Game) Strips(free bool) [][]SymbolID {
	if free && g.FreeReels != nil {
		return g.FreeReels
	}
	return g.Reels
}

// HasFeature reports whether the game has a free spin round at all.
func (g *Game) HasFeature() bool {
	for _, spins := range g.FreeSpinsFor {
		if spins > 0 {
			return true
		}
	}
	return false
}

// Wild returns the wild symbol, if the game has one.
func (g *Game) Wild() (SymbolID, bool) {
	for id, symbol := range g.Symbols {
		if symbol.Wild {
			return SymbolID(id), true
		}
	}
	return 0, false
}

// Scatter returns the scatter symbol, if the game has one.
func (g *Game) Scatter() (SymbolID, bool) {
	for id, symbol := range g.Symbols {
		if symbol.Scatter {
			return SymbolID(id), true
		}
	}
	return 0, false
}

// AcceptsStake reports whether a total stake is one the game offers.
func (g *Game) AcceptsStake(sat int64) bool {
	for _, level := range g.StakeLevels {
		if level == sat {
			return true
		}
	}
	return false
}

// MinStakeSat and MaxStakeSat bound the game's betting range.
func (g *Game) MinStakeSat() int64 { return g.StakeLevels[0] }
func (g *Game) MaxStakeSat() int64 { return g.StakeLevels[len(g.StakeLevels)-1] }

// Validate checks a game specification for the mistakes that would otherwise
// show up as a wrong payout or a wrong published RTP.
//
// Every game runs through this at startup. A machine that cannot be described
// correctly should not be on the floor.
func (g *Game) Validate() error {
	if g.Key == "" || g.Name == "" {
		return fmt.Errorf("slots: a game needs a key and a name")
	}
	if g.Rows < 1 {
		return fmt.Errorf("slots: %s has %d rows", g.Key, g.Rows)
	}
	if len(g.Reels) < 3 {
		return fmt.Errorf("slots: %s has %d reels", g.Key, len(g.Reels))
	}
	if len(g.Symbols) < 2 {
		return fmt.Errorf("slots: %s has %d symbols", g.Key, len(g.Symbols))
	}

	wilds, scatters := 0, 0
	codes := make(map[string]bool, len(g.Symbols))
	for _, symbol := range g.Symbols {
		if symbol.Code == "" || symbol.Name == "" {
			return fmt.Errorf("slots: %s has a symbol with no code or name", g.Key)
		}
		if codes[symbol.Code] {
			return fmt.Errorf("slots: %s uses the symbol code %q twice", g.Key, symbol.Code)
		}
		codes[symbol.Code] = true
		if symbol.Wild && symbol.Scatter {
			return fmt.Errorf("slots: %s symbol %q is both wild and scatter", g.Key, symbol.Code)
		}
		if symbol.Blank && (symbol.Wild || symbol.Scatter) {
			return fmt.Errorf("slots: %s symbol %q is a blank and also wild or scatter", g.Key, symbol.Code)
		}
		if symbol.Wild {
			wilds++
		}
		if symbol.Scatter {
			scatters++
		}
	}
	if wilds > 1 || scatters > 1 {
		return fmt.Errorf("slots: %s has %d wilds and %d scatters, at most one of each is supported",
			g.Key, wilds, scatters)
	}

	if err := g.checkStrips(g.Reels, "base"); err != nil {
		return err
	}
	if g.FreeReels != nil {
		if len(g.FreeReels) != len(g.Reels) {
			return fmt.Errorf("slots: %s has %d base reels but %d free-spin reels",
				g.Key, len(g.Reels), len(g.FreeReels))
		}
		if err := g.checkStrips(g.FreeReels, "free-spin"); err != nil {
			return err
		}
	}

	if g.Ways {
		if len(g.Lines) != 0 {
			return fmt.Errorf("slots: %s is a ways game and must not define paylines", g.Key)
		}
		if g.WaysUnits < 1 {
			return fmt.Errorf("slots: %s is a ways game with a bet of %d units", g.Key, g.WaysUnits)
		}
	} else {
		if len(g.Lines) == 0 {
			return fmt.Errorf("slots: %s has no paylines", g.Key)
		}
		seen := make(map[string]bool, len(g.Lines))
		for i, line := range g.Lines {
			if len(line) != len(g.Reels) {
				return fmt.Errorf("slots: %s payline %d covers %d reels, want %d",
					g.Key, i+1, len(line), len(g.Reels))
			}
			for _, row := range line {
				if row < 0 || row >= g.Rows {
					return fmt.Errorf("slots: %s payline %d uses row %d, outside 0..%d",
						g.Key, i+1, row, g.Rows-1)
				}
			}
			// A duplicated payline is paid twice and is almost always a
			// copy-paste slip in the layout rather than a design choice.
			key := fmt.Sprint(line)
			if seen[key] {
				return fmt.Errorf("slots: %s defines payline %v twice", g.Key, line)
			}
			seen[key] = true
		}
	}

	paying := 0
	for id, symbol := range g.Symbols {
		pays, ok := g.PayX100[SymbolID(id)]
		if !symbol.Pays() {
			if ok {
				return fmt.Errorf("slots: %s gives the %s a line paytable; wilds, scatters and blanks must not have one",
					g.Key, symbol.Name)
			}
			continue
		}
		if !ok {
			return fmt.Errorf("slots: %s has no paytable for %s", g.Key, symbol.Name)
		}
		if len(pays) != len(g.Reels)+1 {
			return fmt.Errorf("slots: %s paytable for %s has %d entries, want %d",
				g.Key, symbol.Name, len(pays), len(g.Reels)+1)
		}
		for count, value := range pays {
			if value < 0 {
				return fmt.Errorf("slots: %s pays %d for %d %s", g.Key, value, count, symbol.Name)
			}
			// Paying less for more of the same symbol is either a typo or a
			// trap; either way a player reading the paytable would be misled.
			if count > 0 && value < pays[count-1] {
				return fmt.Errorf("slots: %s pays %d for %d %s but %d for %d",
					g.Key, value, count, symbol.Name, pays[count-1], count-1)
			}
		}
		if pays[len(pays)-1] == 0 {
			return fmt.Errorf("slots: %s never pays for a full line of %s", g.Key, symbol.Name)
		}
		paying++
	}
	if paying == 0 {
		return fmt.Errorf("slots: %s has no paying symbols", g.Key)
	}

	if _, ok := g.Scatter(); ok {
		if len(g.ScatterPayX100) != len(g.Reels)*g.Rows+1 {
			return fmt.Errorf("slots: %s scatter paytable has %d entries, want %d",
				g.Key, len(g.ScatterPayX100), len(g.Reels)*g.Rows+1)
		}
		if len(g.FreeSpinsFor) != len(g.ScatterPayX100) {
			return fmt.Errorf("slots: %s awards free spins for %d counts but pays for %d",
				g.Key, len(g.FreeSpinsFor), len(g.ScatterPayX100))
		}
	} else if len(g.ScatterPayX100) != 0 || len(g.FreeSpinsFor) != 0 {
		return fmt.Errorf("slots: %s has scatter pays but no scatter symbol", g.Key)
	}

	if g.HasFeature() {
		if g.MaxFreeSpins < 1 {
			return fmt.Errorf("slots: %s awards free spins with no cap; a retrigger could run forever", g.Key)
		}
		if g.FreeMultiplierX100 < 100 {
			return fmt.Errorf("slots: %s multiplies free-spin wins by %d/100", g.Key, g.FreeMultiplierX100)
		}
		// A fractional multiplier would reintroduce the rounding that the
		// stake levels were chosen to avoid.
		if g.FreeMultiplierX100%100 != 0 {
			return fmt.Errorf("slots: %s free-spin multiplier %d/100 is not a whole number",
				g.Key, g.FreeMultiplierX100)
		}
	}

	if len(g.StakeLevels) == 0 {
		return fmt.Errorf("slots: %s offers no stakes", g.Key)
	}
	units := int64(g.BetUnits())
	for i, stake := range g.StakeLevels {
		if stake <= 0 {
			return fmt.Errorf("slots: %s offers a stake of %d", g.Key, stake)
		}
		if i > 0 && stake <= g.StakeLevels[i-1] {
			return fmt.Errorf("slots: %s stake levels are not in ascending order", g.Key)
		}
		// This is what keeps the published RTP exact. A unit bet that is not a
		// whole number of hundredths would have every win truncated by integer
		// division, quietly shaving the return below the advertised figure.
		if stake%(units*100) != 0 {
			return fmt.Errorf("slots: %s stake %d is not a multiple of %d, so wins would be rounded down",
				g.Key, stake, units*100)
		}
	}
	return nil
}

func (g *Game) checkStrips(strips [][]SymbolID, label string) error {
	for reel, strip := range strips {
		if len(strip) < g.Rows {
			return fmt.Errorf("slots: %s %s reel %d has %d stops, fewer than the %d visible rows",
				g.Key, label, reel+1, len(strip), g.Rows)
		}
		for _, id := range strip {
			if int(id) < 0 || int(id) >= len(g.Symbols) {
				return fmt.Errorf("slots: %s %s reel %d refers to symbol %d, which does not exist",
					g.Key, label, reel+1, id)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Writing a game down
// ---------------------------------------------------------------------------

// builder assembles a game from short symbol codes, so reel strips can be
// written the way a par sheet writes them.
type builder struct {
	symbols []Symbol
	index   map[string]SymbolID
}

func newBuilder(symbols ...Symbol) *builder {
	b := &builder{symbols: symbols, index: make(map[string]SymbolID, len(symbols))}
	for id, symbol := range symbols {
		b.index[symbol.Code] = SymbolID(id)
	}
	return b
}

// strip turns "A K Q J T" into reel stops. It panics on an unknown code:
// these are compiled-in game definitions, and a typo in one must not start.
func (b *builder) strip(codes string) []SymbolID {
	fields := strings.Fields(codes)
	out := make([]SymbolID, 0, len(fields))
	for _, code := range fields {
		id, ok := b.index[code]
		if !ok {
			panic(fmt.Sprintf("slots: unknown symbol code %q in reel strip", code))
		}
		out = append(out, id)
	}
	return out
}

// pays builds the line paytable from symbol codes.
func (b *builder) pays(table map[string][]int64) map[SymbolID][]int64 {
	out := make(map[SymbolID][]int64, len(table))
	for code, values := range table {
		id, ok := b.index[code]
		if !ok {
			panic(fmt.Sprintf("slots: unknown symbol code %q in paytable", code))
		}
		out[id] = values
	}
	return out
}

// lines lays out paylines from rows written one line per string.
func lines(rows ...[]int) [][]int { return rows }

// sortedKeys keeps map iteration out of anything that affects an outcome.
func sortedKeys(m map[SymbolID][]int64) []SymbolID {
	out := make([]SymbolID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
