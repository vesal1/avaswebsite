package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/vesal1/avaswebsite/internal/blackjack"
	"github.com/vesal1/avaswebsite/internal/bonus"
	"github.com/vesal1/avaswebsite/internal/mines"
	"github.com/vesal1/avaswebsite/internal/store"
)

func TestMinesRoundMovesMoneyOnceAndSettles(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(1_000_000)
	before := r.cash()

	row, err := r.casino.StartMines(ctx, r.user, 3, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Open() {
		t.Fatal("a fresh board is open")
	}
	if r.cash() != before-10_000 {
		t.Errorf("the stake moved %d, want 10000", before-r.cash())
	}

	// A second start while the board is live must refuse, not double-stake.
	if _, err := r.casino.StartMines(ctx, r.user, 3, 10_000); !errors.Is(err, ErrRoundOpen) {
		t.Errorf("second start: %v", err)
	}
	if r.cash() != before-10_000 {
		t.Error("the refused start still took money")
	}

	// Play: reveal safe tiles until three are showing, then cash out. The
	// layout comes from the open round's own engine view.
	_, board, err := r.casino.OpenMines(ctx, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	isMine := map[int]bool{}
	for _, cell := range board.Mines() {
		isMine[cell] = true
	}
	revealed := 0
	for cell := 0; cell < mines.Cells && revealed < 3; cell++ {
		if isMine[cell] {
			continue
		}
		if _, _, err := r.casino.MinesReveal(ctx, r.user, cell); err != nil {
			t.Fatalf("reveal %d: %v", cell, err)
		}
		revealed++
	}
	row, round, err := r.casino.MinesCashOut(ctx, r.user)
	if err != nil {
		t.Fatal(err)
	}
	if row.Open() || row.Status != store.RoundWon {
		t.Fatalf("cashed out round is %s", row.Status)
	}
	ladder, _ := mines.Ladder(3)
	want := 10_000 * ladder[3] / 10_000
	if row.WinSat != want || round.PayoutSat(10_000) != want {
		t.Errorf("paid %d, ladder says %d", row.WinSat, want)
	}
	if r.cash() != before-10_000+want {
		t.Errorf("balance %d", r.cash())
	}
	r.assertLedgerSound()

	// The board is gone: no acting on a settled round.
	if _, _, err := r.casino.MinesReveal(ctx, r.user, 0); !errors.Is(err, store.ErrNoOpenRound) {
		t.Errorf("reveal after settlement: %v", err)
	}
}

func TestMinesHitLosesTheStake(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(100_000)
	before := r.cash()

	if _, err := r.casino.StartMines(ctx, r.user, 10, 10_000); err != nil {
		t.Fatal(err)
	}
	_, board, _ := r.casino.OpenMines(ctx, r.user.ID)
	mine := board.Mines()[0]
	row, _, err := r.casino.MinesReveal(ctx, r.user, mine)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RoundLost || row.WinSat != 0 {
		t.Errorf("hit a mine: status %s, paid %d", row.Status, row.WinSat)
	}
	if r.cash() != before-10_000 {
		t.Errorf("balance %d", r.cash())
	}
	r.assertLedgerSound()
}

func TestRotationRefusesWhileARoundIsOpen(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(100_000)

	if _, err := r.casino.StartMines(ctx, r.user, 3, 10_000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.casino.Rotate(ctx, r.user.ID, ""); !errors.Is(err, store.ErrRoundStillOpen) {
		t.Fatalf("rotation with an open board: %v — publishing the seed would hand over the mine layout", err)
	}

	// Settle the board; rotation opens up again.
	_, board, _ := r.casino.OpenMines(ctx, r.user.ID)
	isMine := map[int]bool{}
	for _, cell := range board.Mines() {
		isMine[cell] = true
	}
	for cell := 0; cell < mines.Cells; cell++ {
		if !isMine[cell] {
			if _, _, err := r.casino.MinesReveal(ctx, r.user, cell); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if _, _, err := r.casino.MinesCashOut(ctx, r.user); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.casino.Rotate(ctx, r.user.ID, ""); err != nil {
		t.Fatalf("rotation after settlement: %v", err)
	}
}

func TestBlackjackRoundFlow(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(10_000_000)

	// Play hands until one settles through player decisions, standing on
	// everything; the maths is the engine's business, this test watches the
	// money.
	var settledThroughPlay bool
	for i := 0; i < 20 && !settledThroughPlay; i++ {
		before := r.cash()
		row, err := r.casino.StartBlackjack(ctx, r.user, 10_000)
		if err != nil {
			t.Fatal(err)
		}
		if r.cash() != before-10_000 {
			t.Fatalf("stake moved %d", before-r.cash())
		}
		if row.Open() {
			row, round, err := r.casino.BlackjackAct(ctx, r.user, blackjack.Stand)
			if err != nil {
				t.Fatal(err)
			}
			for row.Open() {
				row, round, err = r.casino.BlackjackAct(ctx, r.user, blackjack.Stand)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !round.Settled() {
				t.Fatal("row settled but engine still open")
			}
			settledThroughPlay = true
			if r.cash() != before-10_000+row.WinSat {
				t.Errorf("balance %d after winning %d", r.cash(), row.WinSat)
			}
		} else {
			// A natural on the deal: paid in the same transaction.
			if r.cash() != before-10_000+row.WinSat {
				t.Errorf("natural: balance %d, win %d", r.cash(), row.WinSat)
			}
		}
		r.assertLedgerSound()
	}
	if !settledThroughPlay {
		t.Fatal("twenty hands and every one was a natural — the deck is broken")
	}
}

func TestBlackjackDoubleStakesMore(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(10_000_000)

	// Deal until a hand where doubling is offered, then double and check the
	// second stake moved and the settled row carries both.
	for i := 0; i < 40; i++ {
		row, err := r.casino.StartBlackjack(ctx, r.user, 10_000)
		if err != nil {
			t.Fatal(err)
		}
		if !row.Open() {
			continue
		}
		_, round, _, err := r.casino.OpenBlackjack(ctx, r.user.ID)
		if err != nil {
			t.Fatal(err)
		}
		canDouble := false
		for _, action := range round.Available() {
			if action == blackjack.Double {
				canDouble = true
			}
		}
		if !canDouble {
			for row.Open() {
				row, _, err = r.casino.BlackjackAct(ctx, r.user, blackjack.Stand)
				if err != nil {
					t.Fatal(err)
				}
			}
			continue
		}

		before := r.cash()
		row, round, err = r.casino.BlackjackAct(ctx, r.user, blackjack.Double)
		if err != nil {
			t.Fatal(err)
		}
		if row.StakeSat != 20_000 {
			t.Errorf("row stake %d after a double, want 20000", row.StakeSat)
		}
		if round.StakedUnits() != 2 {
			t.Errorf("engine staked units %d", round.StakedUnits())
		}
		if r.cash() != before-10_000+row.WinSat {
			t.Errorf("double: balance %d, win %d", r.cash(), row.WinSat)
		}
		r.assertLedgerSound()
		return
	}
	t.Fatal("forty hands with no double offered — the dealing is broken")
}

func TestRoundsVerifyOnceTheSeedIsPublished(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fund(10_000_000)

	// One settled Mines board.
	if _, err := r.casino.StartMines(ctx, r.user, 5, 10_000); err != nil {
		t.Fatal(err)
	}
	_, board, _ := r.casino.OpenMines(ctx, r.user.ID)
	isMine := map[int]bool{}
	for _, cell := range board.Mines() {
		isMine[cell] = true
	}
	for cell := 0; cell < mines.Cells; cell++ {
		if !isMine[cell] {
			if _, _, err := r.casino.MinesReveal(ctx, r.user, cell); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	minesRow, _, err := r.casino.MinesCashOut(ctx, r.user)
	if err != nil {
		t.Fatal(err)
	}

	// One settled blackjack hand.
	bjRow, err := r.casino.StartBlackjack(ctx, r.user, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	for bjRow.Open() {
		bjRow, _, err = r.casino.BlackjackAct(ctx, r.user, blackjack.Stand)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Sealed: nothing to verify yet, and the page must say so.
	sealed, err := r.casino.VerifyRound(ctx, minesRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Matches {
		t.Error("a round was reported verified while its seed was sealed")
	}

	if _, _, err := r.casino.Rotate(ctx, r.user.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{minesRow.ID, bjRow.ID} {
		verified, err := r.casino.VerifyRound(ctx, id)
		if err != nil {
			t.Fatalf("verify round %d: %v", id, err)
		}
		if !verified.Matches {
			t.Errorf("round %d replayed to a different result than it paid", id)
		}
	}
	r.assertLedgerSound()
}

func TestBlackjackBonusClearsAtTenPercent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	grant, err := r.bonus.Grant(ctx, bonusGrantForStake(r, 500_000))
	if err != nil {
		t.Fatal(err)
	}
	row, err := r.casino.StartBlackjack(ctx, r.user, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	for row.Open() {
		row, _, err = r.casino.BlackjackAct(ctx, r.user, blackjack.Stand)
		if err != nil {
			t.Fatal(err)
		}
	}
	updated, err := r.store.GetBonusGrant(ctx, grant.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A 10,000 sat stake at a 10% rate contributes 1,000.
	if updated.WageringDoneSat != 1_000 {
		t.Errorf("blackjack advanced wagering by %d, want 1000: the tables must not clear bonuses at par",
			updated.WageringDoneSat)
	}
}

// bonusGrantForStake gives the rig's player a stakeable bonus.
func bonusGrantForStake(r *rig, amountSat int64) bonus.GrantRequest {
	return bonus.GrantRequest{
		UserID: r.user.ID, Kind: store.BonusFreeCredit,
		AmountSat: amountSat, WageringX100: 200, ValidDays: 30,
	}
}
