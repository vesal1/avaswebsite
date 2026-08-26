// Package mines is the casino's Mines game: a 5x5 board, a chosen number of
// mines, and one decision made over and over — reveal another tile, or take
// what the board is currently worth.
//
// A word on what this game is, because the page says the same thing: it is
// not a skill game. The mine layout is fixed by the committed seed before the
// first tile is touched, every safe reveal multiplies the stake by exactly the
// inverse of its survival odds times the house factor, and therefore every
// cash-out point returns the same published percentage. The decision the
// player actually makes is how much variance they want. Sites that imply a
// system or a hot streak here are lying; this one prints the multiplier
// ladder and the maths behind it instead.
package mines

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/vesal1/avaswebsite/internal/fair"
)

const (
	// GridSize is the board edge; Cells the tile count.
	GridSize = 5
	Cells    = GridSize * GridSize

	// HouseFactorBps is the share of the fair multiplier the board pays:
	// 97.00%, applied identically at every step.
	HouseFactorBps = 9_700

	// CapX10000 is the top of every ladder. A run that reaches it settles
	// automatically: there is nothing above it, and the page says so. The cap
	// exists to bound the book's exposure on one round, not to shave a step —
	// every step below it still pays the full published rate.
	CapX10000 = 25_000_000 // 2,500x
)

// MineOptions are the mine counts the board offers. Each has its own printed
// ladder; a free choice of 1-24 would mean 24 ladders nobody reads.
var MineOptions = []int{3, 5, 10}

// ValidMines reports whether a mine count is offered.
func ValidMines(count int) bool {
	for _, option := range MineOptions {
		if option == count {
			return true
		}
	}
	return false
}

// Errors callers branch on.
var (
	ErrBadCell   = errors.New("mines: that tile is not on the board")
	ErrRevealed  = errors.New("mines: that tile is already showing")
	ErrOver      = errors.New("mines: the round is over")
	ErrNoReveals = errors.New("mines: reveal at least one tile before cashing out")
	ErrBadMines  = errors.New("mines: that mine count is not offered")
)

// Ladder is the multiplier table for one mine count: LadderX10000[k] is what
// the stake is worth after k safe reveals, in ten-thousandths. Index 0 is the
// untouched board, worth exactly the stake.
func Ladder(mineCount int) ([]int64, error) {
	if !ValidMines(mineCount) {
		return nil, ErrBadMines
	}
	safe := Cells - mineCount

	// Exact arithmetic until the final division. The fair multiplier after k
	// reveals is C(Cells,k)/C(safe,k); the board pays HouseFactorBps of it,
	// floored onto the x10000 grid. The floor loses at most one part in ten
	// thousand, and EffectiveRTPBps reports the loss rather than hiding it.
	ladder := []int64{10_000}
	numerator := big.NewInt(1)
	denominator := big.NewInt(1)
	for k := 1; k <= safe; k++ {
		numerator.Mul(numerator, big.NewInt(int64(Cells-k+1)))
		denominator.Mul(denominator, big.NewInt(int64(safe-k+1)))

		value := new(big.Int).Mul(numerator, big.NewInt(HouseFactorBps))
		value.Div(value, denominator)
		if !value.IsInt64() || value.Int64() > CapX10000 {
			break
		}
		ladder = append(ladder, value.Int64())
	}
	return ladder, nil
}

// MaxReveals is how many safe reveals the ladder offers before the round
// settles automatically at the cap.
func MaxReveals(mineCount int) int {
	ladder, err := Ladder(mineCount)
	if err != nil {
		return 0
	}
	return len(ladder) - 1
}

// EffectiveRTPBps is the true return of cashing out after k reveals, in basis
// points: the floored ladder value times the exact survival probability. It
// sits just under HouseFactorBps because of the floor, and the page publishes
// the worst step rather than the nominal figure.
func EffectiveRTPBps(mineCount, reveals int) (int64, error) {
	ladder, err := Ladder(mineCount)
	if err != nil {
		return 0, err
	}
	if reveals < 1 || reveals >= len(ladder) {
		return 0, fmt.Errorf("mines: no ladder step %d for %d mines", reveals, mineCount)
	}
	safe := Cells - mineCount
	// P(survive k) = C(safe,k)/C(Cells,k); return = ladder[k] * P.
	numerator := big.NewInt(ladder[reveals])
	denominator := big.NewInt(1)
	for i := 0; i < reveals; i++ {
		numerator.Mul(numerator, big.NewInt(int64(safe-i)))
		denominator.Mul(denominator, big.NewInt(int64(Cells-i)))
	}
	numerator.Mul(numerator, big.NewInt(10_000))
	numerator.Div(numerator, denominator)
	numerator.Div(numerator, big.NewInt(10_000))
	return numerator.Int64(), nil
}

// WorstStepRTPBps is the lowest effective return anywhere on a ladder — the
// number honest enough to publish.
func WorstStepRTPBps(mineCount int) (int64, error) {
	worst := int64(HouseFactorBps)
	for k := 1; k <= MaxReveals(mineCount); k++ {
		rtp, err := EffectiveRTPBps(mineCount, k)
		if err != nil {
			return 0, err
		}
		if rtp < worst {
			worst = rtp
		}
	}
	return worst, nil
}

// Layout derives the mine positions from a round's seed. The first mineCount
// entries of a fair permutation of the board are the mines: fixed before the
// first reveal, checkable after the seed is published.
func Layout(seed *fair.Seed, mineCount int) ([]int, error) {
	if !ValidMines(mineCount) {
		return nil, ErrBadMines
	}
	permutation := seed.Stream().Permutation(Cells)
	mines := make([]int, mineCount)
	copy(mines, permutation[:mineCount])
	return mines, nil
}

// State is a round in play, serialised into the casino_rounds row.
type State struct {
	MineCount int   `json:"mine_count"`
	Revealed  []int `json:"revealed"`
	// Over marks a settled board; Hit the tile that ended a lost one.
	Over bool `json:"over"`
	Hit  int  `json:"hit"`
	// CashedOut distinguishes a taken win from a run that reached the cap.
	CashedOut bool `json:"cashed_out"`
}

// Round is a live board: the fixed layout plus the state so far.
type Round struct {
	State State
	mines map[int]bool
}

// Resume rebuilds a round from its seed and stored state.
func Resume(seed *fair.Seed, state State) (*Round, error) {
	layout, err := Layout(seed, state.MineCount)
	if err != nil {
		return nil, err
	}
	round := &Round{State: state, mines: make(map[int]bool, len(layout))}
	for _, cell := range layout {
		round.mines[cell] = true
	}
	return round, nil
}

// New starts a round.
func New(seed *fair.Seed, mineCount int) (*Round, error) {
	return Resume(seed, State{MineCount: mineCount, Hit: -1})
}

// Reveals is how many safe tiles are showing.
func (r *Round) Reveals() int { return len(r.State.Revealed) }

// MultiplierX10000 is what the board is worth right now.
func (r *Round) MultiplierX10000() int64 {
	ladder, err := Ladder(r.State.MineCount)
	if err != nil || r.Reveals() >= len(ladder) {
		return 10_000
	}
	return ladder[r.Reveals()]
}

// Reveal turns over a tile. It returns whether the tile was safe, and whether
// the round is now over (a mine, or the top of the ladder).
func (r *Round) Reveal(cell int) (safe, over bool, err error) {
	if r.State.Over {
		return false, true, ErrOver
	}
	if cell < 0 || cell >= Cells {
		return false, false, ErrBadCell
	}
	for _, shown := range r.State.Revealed {
		if shown == cell {
			return false, false, ErrRevealed
		}
	}

	if r.mines[cell] {
		r.State.Over = true
		r.State.Hit = cell
		return false, true, nil
	}
	r.State.Revealed = append(r.State.Revealed, cell)
	if r.Reveals() >= MaxReveals(r.State.MineCount) {
		// The ladder has no higher step: the board pays out by itself.
		r.State.Over = true
		return true, true, nil
	}
	return true, false, nil
}

// CashOut ends the round at the current multiplier.
func (r *Round) CashOut() error {
	if r.State.Over {
		return ErrOver
	}
	if r.Reveals() == 0 {
		return ErrNoReveals
	}
	r.State.Over = true
	r.State.CashedOut = true
	return nil
}

// Mines lists the layout — for settlement display and verification only.
// Nothing player-facing may call this while the round is open.
func (r *Round) Mines() []int {
	out := make([]int, 0, len(r.mines))
	for cell := range r.mines {
		out = append(out, cell)
	}
	// Deterministic order for display and tests.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// PayoutSat is what a stake is worth at settlement: the current ladder step
// for a win, nothing for a mine.
func (r *Round) PayoutSat(stakeSat int64) int64 {
	if r.State.Hit >= 0 {
		return 0
	}
	return stakeSat * r.MultiplierX10000() / 10_000
}

// StakeStep is the granularity every stake must divide by, so the ladder's
// x10000 multipliers pay exactly.
const StakeStep = 10_000

// StakeLevels are the stakes the board takes. The top level times the cap is
// the book's worst case on one round: 40,000 sat x 2,500 = 1 BTC, which is
// the sportsbook's payout ceiling too.
var StakeLevels = []int64{10_000, 20_000, 40_000}

// AcceptsStake reports whether a stake is on the ladder.
func AcceptsStake(sat int64) bool {
	for _, level := range StakeLevels {
		if level == sat {
			return true
		}
	}
	return false
}
