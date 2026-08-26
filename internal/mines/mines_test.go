package mines

import (
	"errors"
	"math/big"
	"testing"

	"github.com/vesal1/avaswebsite/internal/fair"
)

func seedFor(t *testing.T, nonce int64) *fair.Seed {
	t.Helper()
	seed, err := fair.RestoreSeed("00112233445566778899aabbccddeeff", "mines-test", nonce)
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

// TestLadderIsExact recomputes every ladder step with independent big-rational
// arithmetic and checks the published integers match the floor of the true
// value — never above it, never more than one grid unit below it.
func TestLadderIsExact(t *testing.T) {
	for _, mineCount := range MineOptions {
		ladder, err := Ladder(mineCount)
		if err != nil {
			t.Fatal(err)
		}
		if ladder[0] != 10_000 {
			t.Errorf("%d mines: the untouched board must be worth exactly the stake", mineCount)
		}
		safe := Cells - mineCount
		for k := 1; k < len(ladder); k++ {
			// fair = C(25,k)/C(safe,k); paid = 0.97 * fair.
			fairValue := new(big.Rat).SetInt64(1)
			for i := 0; i < k; i++ {
				fairValue.Mul(fairValue, big.NewRat(int64(Cells-i), int64(safe-i)))
			}
			paid := new(big.Rat).Mul(fairValue, big.NewRat(HouseFactorBps, 10_000))
			paid.Mul(paid, big.NewRat(10_000, 1))
			floor := new(big.Int).Div(paid.Num(), paid.Denom())

			if got := big.NewInt(ladder[k]); got.Cmp(floor) != 0 {
				t.Errorf("%d mines, %d reveals: ladder %d, exact floor %s", mineCount, k, ladder[k], floor)
			}
			if ladder[k] > CapX10000 {
				t.Errorf("%d mines, %d reveals: %d exceeds the cap", mineCount, k, ladder[k])
			}
			if ladder[k] <= ladder[k-1] {
				t.Errorf("%d mines: the ladder must climb, got %d then %d", mineCount, ladder[k-1], ladder[k])
			}
		}
	}
}

// TestEveryStepPaysThePublishedRate is the honesty check: no cash-out point,
// early or late, returns less than 96.9% — the floor rounding costs at most a
// tenth of a percent and the published figure is the worst step, not the best.
func TestEveryStepPaysThePublishedRate(t *testing.T) {
	for _, mineCount := range MineOptions {
		worst, err := WorstStepRTPBps(mineCount)
		if err != nil {
			t.Fatal(err)
		}
		if worst < 9_690 || worst > HouseFactorBps {
			t.Errorf("%d mines: worst step returns %d bps, want within [9690, %d]", mineCount, worst, HouseFactorBps)
		}
		for k := 1; k <= MaxReveals(mineCount); k++ {
			rtp, err := EffectiveRTPBps(mineCount, k)
			if err != nil {
				t.Fatal(err)
			}
			if rtp < worst {
				t.Errorf("%d mines, step %d: %d bps under the published worst %d", mineCount, k, rtp, worst)
			}
		}
	}
}

func TestLayoutIsDeterministicAndFair(t *testing.T) {
	first, err := Layout(seedFor(t, 1), 5)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Layout(seedFor(t, 1), 5)
	for i := range first {
		if first[i] != second[i] {
			t.Fatal("the same seed produced two different boards")
		}
	}
	other, _ := Layout(seedFor(t, 2), 5)
	same := true
	for i := range first {
		if first[i] != other[i] {
			same = false
		}
	}
	if same {
		t.Error("changing the nonce did not change the board")
	}

	// Every cell must be reachable as a mine: count over many nonces.
	counts := make([]int, Cells)
	const trials = 4000
	for nonce := int64(0); nonce < trials; nonce++ {
		layout, _ := Layout(seedFor(t, nonce), 5)
		for _, cell := range layout {
			counts[cell]++
		}
	}
	expected := float64(trials*5) / Cells
	for cell, count := range counts {
		if diff := float64(count) - expected; diff > expected/3 || diff < -expected/3 {
			t.Errorf("cell %d was a mine %d times, expected about %.0f", cell, count, expected)
		}
	}
}

func TestRevealAndCashOut(t *testing.T) {
	round, err := New(seedFor(t, 7), 3)
	if err != nil {
		t.Fatal(err)
	}
	mines := round.Mines()
	isMine := map[int]bool{}
	for _, cell := range mines {
		isMine[cell] = true
	}

	// Reveal three safe tiles, then cash out.
	revealed := 0
	for cell := 0; cell < Cells && revealed < 3; cell++ {
		if isMine[cell] {
			continue
		}
		safe, over, err := round.Reveal(cell)
		if err != nil || !safe || over {
			t.Fatalf("revealing safe cell %d: safe=%v over=%v err=%v", cell, safe, over, err)
		}
		revealed++
	}
	ladder, _ := Ladder(3)
	if got := round.MultiplierX10000(); got != ladder[3] {
		t.Errorf("multiplier after 3 reveals = %d, want %d", got, ladder[3])
	}
	if err := round.CashOut(); err != nil {
		t.Fatal(err)
	}
	if !round.State.Over || !round.State.CashedOut {
		t.Error("cash out did not settle the round")
	}
	if got := round.PayoutSat(10_000); got != 10_000*ladder[3]/10_000 {
		t.Errorf("payout = %d", got)
	}
	// No acting on a settled board.
	if _, _, err := round.Reveal(24); !errors.Is(err, ErrOver) {
		t.Errorf("revealing after settlement: %v", err)
	}
}

func TestHittingAMineLosesEverything(t *testing.T) {
	round, _ := New(seedFor(t, 9), 10)
	mine := round.Mines()[0]
	safe, over, err := round.Reveal(mine)
	if err != nil || safe || !over {
		t.Fatalf("revealing a mine: safe=%v over=%v err=%v", safe, over, err)
	}
	if round.PayoutSat(40_000) != 0 {
		t.Error("a hit mine must pay nothing")
	}
	if round.State.Hit != mine {
		t.Errorf("hit = %d, want %d", round.State.Hit, mine)
	}
}

func TestGuards(t *testing.T) {
	round, _ := New(seedFor(t, 11), 3)
	if err := round.CashOut(); !errors.Is(err, ErrNoReveals) {
		t.Errorf("cash out with nothing revealed: %v", err)
	}
	if _, _, err := round.Reveal(-1); !errors.Is(err, ErrBadCell) {
		t.Errorf("reveal(-1): %v", err)
	}
	if _, _, err := round.Reveal(Cells); !errors.Is(err, ErrBadCell) {
		t.Errorf("reveal(25): %v", err)
	}
	// Find a safe cell, reveal it twice.
	isMine := map[int]bool{}
	for _, cell := range round.Mines() {
		isMine[cell] = true
	}
	target := -1
	for cell := 0; cell < Cells; cell++ {
		if !isMine[cell] {
			target = cell
			break
		}
	}
	if _, _, err := round.Reveal(target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := round.Reveal(target); !errors.Is(err, ErrRevealed) {
		t.Errorf("double reveal: %v", err)
	}
	if _, err := New(seedFor(t, 1), 7); !errors.Is(err, ErrBadMines) {
		t.Errorf("7 mines is not offered: %v", err)
	}
}

// TestFullClearSettlesAtTheCap: with 3 mines the whole board is 22 reveals at
// 2,231x, inside the 2,500x cap, so a full clear pays; with 10 mines the
// ladder stops early and the round settles itself at the top step.
func TestFullClearSettlesAtTheCap(t *testing.T) {
	if MaxReveals(3) != Cells-3 {
		t.Errorf("3 mines should allow a full clear, ladder stops at %d", MaxReveals(3))
	}
	top := MaxReveals(10)
	if top >= Cells-10 {
		t.Errorf("10 mines at 2,500x cap should stop before a full clear, got %d steps", top)
	}

	round, _ := New(seedFor(t, 13), 10)
	isMine := map[int]bool{}
	for _, cell := range round.Mines() {
		isMine[cell] = true
	}
	steps := 0
	for cell := 0; cell < Cells && steps < top; cell++ {
		if isMine[cell] {
			continue
		}
		steps++
		_, over, err := round.Reveal(cell)
		if err != nil {
			t.Fatal(err)
		}
		if steps == top && !over {
			t.Error("reaching the top of the ladder must settle the round by itself")
		}
		if steps < top && over {
			t.Fatalf("round ended early at step %d", steps)
		}
	}
	if round.State.CashedOut {
		t.Error("an automatic settlement is not a cash out")
	}
	ladder, _ := Ladder(10)
	if got := round.PayoutSat(10_000); got != 10_000*ladder[top]/10_000 {
		t.Errorf("cap payout = %d", got)
	}
}

// TestResumeReproducesTheBoard: a server restart mid-round must hand back the
// exact same board.
func TestResumeReproducesTheBoard(t *testing.T) {
	round, _ := New(seedFor(t, 17), 5)
	isMine := map[int]bool{}
	for _, cell := range round.Mines() {
		isMine[cell] = true
	}
	for cell := 0; cell < Cells; cell++ {
		if !isMine[cell] {
			round.Reveal(cell)
			break
		}
	}

	resumed, err := Resume(seedFor(t, 17), round.State)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Reveals() != 1 {
		t.Errorf("resumed with %d reveals, want 1", resumed.Reveals())
	}
	a, b := round.Mines(), resumed.Mines()
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("resume produced a different board")
		}
	}
}

// TestPlayedReturnConvergesOnTheLadder plays a fixed always-cash-out-at-k
// policy over many rounds and checks the realised return lands on the
// effective RTP for that step. This exercises Reveal/PayoutSat end to end
// rather than trusting the ladder arithmetic alone.
func TestPlayedReturnConvergesOnTheLadder(t *testing.T) {
	const (
		mineCount = 5
		stopAt    = 4
		rounds    = 200_000
		stake     = int64(10_000)
	)
	want, err := EffectiveRTPBps(mineCount, stopAt)
	if err != nil {
		t.Fatal(err)
	}

	var staked, returned int64
	for nonce := int64(0); nonce < rounds; nonce++ {
		round, err := New(seedFor(t, nonce), mineCount)
		if err != nil {
			t.Fatal(err)
		}
		staked += stake
		// Reveal the first stopAt cells in a fixed order; the layout is
		// uniform so the choice of cells cannot matter.
		survived := true
		reveals := 0
		for cell := 0; cell < Cells && reveals < stopAt; cell++ {
			safe, _, err := round.Reveal(cell)
			if err != nil {
				t.Fatal(err)
			}
			if !safe {
				survived = false
				break
			}
			reveals++
		}
		if survived {
			if err := round.CashOut(); err != nil {
				t.Fatal(err)
			}
			returned += round.PayoutSat(stake)
		}
	}

	got := returned * 10_000 / staked
	if diff := got - want; diff > 120 || diff < -120 {
		t.Errorf("played return %d bps over %d rounds, ladder says %d", got, rounds, want)
	}
}
