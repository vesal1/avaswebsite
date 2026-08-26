package slots

import (
	"fmt"

	"github.com/vesal1/avaswebsite/internal/fair"
)

// Position is one cell of the visible window.
type Position struct {
	Reel int `json:"reel"`
	Row  int `json:"row"`
}

// Win is a single scoring combination.
type Win struct {
	// Kind is "line", "ways" or "scatter".
	Kind string `json:"kind"`
	// Line is the 1-based payline number, or 0 for ways and scatter wins.
	Line   int      `json:"line,omitempty"`
	Symbol SymbolID `json:"symbol"`
	Name   string   `json:"name"`
	Count  int      `json:"count"`
	// Ways is how many distinct combinations made this win. Always 1 on a
	// payline.
	Ways      int        `json:"ways"`
	Positions []Position `json:"positions"`
	AmountSat int64      `json:"amount_sat"`
}

// Spin is one turn of the reels: the stops, what they showed, and what it paid.
type Spin struct {
	Free           bool         `json:"free"`
	MultiplierX100 int64        `json:"multiplier_x100"`
	Stops          []int        `json:"stops"`
	Window         [][]SymbolID `json:"window"`
	Wins           []Win        `json:"wins"`
	WinSat         int64        `json:"win_sat"`
	ScatterCount   int          `json:"scatter_count"`
	// FreeSpinsAwarded is what the scatters on this spin bought, after the
	// round's cap has been applied.
	FreeSpinsAwarded int `json:"free_spins_awarded"`
}

// Round is a paid spin and everything the feature it triggered went on to do.
// One stake, one seed, one settlement.
type Round struct {
	GameKey    string `json:"game"`
	StakeSat   int64  `json:"stake_sat"`
	UnitBetSat int64  `json:"unit_bet_sat"`
	Commitment string `json:"commitment"`
	ClientSeed string `json:"client_seed"`
	Nonce      int64  `json:"nonce"`

	Base Spin   `json:"base"`
	Free []Spin `json:"free,omitempty"`

	FreeSpinsPlayed int   `json:"free_spins_played"`
	TotalWinSat     int64 `json:"total_win_sat"`
}

// Won reports whether the round returned anything at all.
func (r *Round) Won() bool { return r.TotalWinSat > 0 }

// NetSat is the round's result from the player's point of view.
func (r *Round) NetSat() int64 { return r.TotalWinSat - r.StakeSat }

// Spins returns every spin of the round in order, base first.
func (r *Round) Spins() []Spin {
	out := make([]Spin, 0, 1+len(r.Free))
	out = append(out, r.Base)
	return append(out, r.Free...)
}

// Play resolves one paid spin, including any free spins it wins.
//
// Everything is drawn from a single keystream in a fixed order — base reels
// first, then each free spin's reels — so the whole round replays from the one
// seed the house publishes afterwards.
func Play(game *Game, seed *fair.Seed, stakeSat int64) (*Round, error) {
	if !game.AcceptsStake(stakeSat) {
		return nil, fmt.Errorf("slots: %s does not take a stake of %d sat", game.Key, stakeSat)
	}
	unit := stakeSat / int64(game.BetUnits())
	stream := seed.Stream()

	round := &Round{
		GameKey:    game.Key,
		StakeSat:   stakeSat,
		UnitBetSat: unit,
		Commitment: seed.Commitment,
		ClientSeed: seed.ClientSeed,
		Nonce:      seed.Nonce,
	}

	round.Base = spin(game, stream, unit, stakeSat, false)
	round.TotalWinSat = round.Base.WinSat

	awarded := 0
	if game.HasFeature() && round.Base.ScatterCount < len(game.FreeSpinsFor) {
		awarded = capSpins(game, 0, game.FreeSpinsFor[round.Base.ScatterCount])
		round.Base.FreeSpinsAwarded = awarded
	}

	// Retriggers add to the same pool and count against the same cap, so the
	// round always ends.
	remaining := awarded
	for remaining > 0 {
		remaining--
		free := spin(game, stream, unit, stakeSat, true)
		if game.HasFeature() && free.ScatterCount < len(game.FreeSpinsFor) {
			extra := capSpins(game, awarded, game.FreeSpinsFor[free.ScatterCount])
			free.FreeSpinsAwarded = extra
			awarded += extra
			remaining += extra
		}
		round.Free = append(round.Free, free)
		round.TotalWinSat += free.WinSat
	}
	round.FreeSpinsPlayed = len(round.Free)
	return round, nil
}

// capSpins trims an award to what is left under the round's ceiling.
func capSpins(game *Game, awarded, want int) int {
	if want <= 0 {
		return 0
	}
	if left := game.MaxFreeSpins - awarded; want > left {
		if left < 0 {
			return 0
		}
		return left
	}
	return want
}

// spin turns the reels once and scores the result.
func spin(game *Game, stream *fair.Stream, unit, stake int64, free bool) Spin {
	strips := game.Strips(free)
	stops := make([]int, len(strips))
	for reel, strip := range strips {
		stops[reel] = stream.BoundedIndex(len(strip))
	}
	return scoreStops(game, stops, unit, stake, free)
}

// scoreStops resolves a spin from a known set of reel positions. Splitting it
// out from the draw is what lets the return-to-player calculation and its
// tests walk every possible outcome through exactly the code that pays a real
// player.
func scoreStops(game *Game, stops []int, unit, stake int64, free bool) Spin {
	strips := game.Strips(free)
	multiplier := int64(100)
	if free {
		multiplier = game.FreeMultiplierX100
	}

	result := Spin{
		Free:           free,
		MultiplierX100: multiplier,
		Stops:          stops,
		Window:         make([][]SymbolID, len(strips)),
	}
	for reel, strip := range strips {
		cells := make([]SymbolID, game.Rows)
		for row := 0; row < game.Rows; row++ {
			cells[row] = strip[(stops[reel]+row)%len(strip)]
		}
		result.Window[reel] = cells
	}

	if game.Ways {
		result.Wins = waysWins(game, result.Window, unit, multiplier)
	} else {
		result.Wins = lineWins(game, result.Window, unit, multiplier)
	}
	if scatter, ok := game.Scatter(); ok {
		count, positions := scatters(game, result.Window, scatter)
		result.ScatterCount = count
		if count < len(game.ScatterPayX100) && game.ScatterPayX100[count] > 0 {
			// Scatter wins are a multiple of the total stake and are not
			// touched by the free-spin multiplier.
			result.Wins = append(result.Wins, Win{
				Kind:      "scatter",
				Symbol:    scatter,
				Name:      game.Symbols[scatter].Name,
				Count:     count,
				Ways:      1,
				Positions: positions,
				AmountSat: stake * game.ScatterPayX100[count] / 100,
			})
		}
	}
	for _, win := range result.Wins {
		result.WinSat += win.AmountSat
	}
	return result
}

// lineWins scores every payline. A line pays its single best combination, the
// way a real machine does: three of a kind and a longer run of a lower symbol
// are not both paid on the same line.
func lineWins(game *Game, window [][]SymbolID, unit, multiplier int64) []Win {
	wild, hasWild := game.Wild()
	var wins []Win

	for index, line := range game.Lines {
		var (
			bestPay    int64
			bestSymbol SymbolID
			bestCount  int
		)
		for _, symbol := range sortedKeys(game.PayX100) {
			table := game.PayX100[symbol]
			count := 0
			for reel := range window {
				cell := window[reel][line[reel]]
				if cell != symbol && !(hasWild && cell == wild) {
					break
				}
				count++
			}
			if count == 0 {
				continue
			}
			// Longer runs cannot pay less (Validate enforces it), so the
			// longest run of this symbol is also its best.
			if pay := table[count]; pay > bestPay {
				bestPay, bestSymbol, bestCount = pay, symbol, count
			}
		}
		if bestPay == 0 {
			continue
		}
		positions := make([]Position, 0, bestCount)
		for reel := 0; reel < bestCount; reel++ {
			positions = append(positions, Position{Reel: reel, Row: line[reel]})
		}
		wins = append(wins, Win{
			Kind:      "line",
			Line:      index + 1,
			Symbol:    bestSymbol,
			Name:      game.Symbols[bestSymbol].Name,
			Count:     bestCount,
			Ways:      1,
			Positions: positions,
			AmountSat: unit * bestPay / 100 * multiplier / 100,
		})
	}
	return wins
}

// waysWins scores adjacent-ways play: a symbol pays once for the run of reels
// it appears on from the left, multiplied by how many ways that run can be
// made. Every symbol pays, not just the best.
func waysWins(game *Game, window [][]SymbolID, unit, multiplier int64) []Win {
	wild, hasWild := game.Wild()
	var wins []Win

	for _, symbol := range sortedKeys(game.PayX100) {
		table := game.PayX100[symbol]
		ways, count := 1, 0
		for reel := range window {
			matches := 0
			for _, cell := range window[reel] {
				if cell == symbol || (hasWild && cell == wild) {
					matches++
				}
			}
			if matches == 0 {
				break
			}
			ways *= matches
			count++
		}
		if count == 0 || table[count] == 0 {
			continue
		}
		var positions []Position
		for reel := 0; reel < count; reel++ {
			for row, cell := range window[reel] {
				if cell == symbol || (hasWild && cell == wild) {
					positions = append(positions, Position{Reel: reel, Row: row})
				}
			}
		}
		wins = append(wins, Win{
			Kind:      "ways",
			Symbol:    symbol,
			Name:      game.Symbols[symbol].Name,
			Count:     count,
			Ways:      ways,
			Positions: positions,
			AmountSat: unit * table[count] / 100 * int64(ways) * multiplier / 100,
		})
	}
	return wins
}

// scatters counts the scatter symbols anywhere in the window.
func scatters(game *Game, window [][]SymbolID, scatter SymbolID) (int, []Position) {
	var positions []Position
	for reel := range window {
		for row, cell := range window[reel] {
			if cell == scatter {
				positions = append(positions, Position{Reel: reel, Row: row})
			}
		}
	}
	return len(positions), positions
}
