package poker

import (
	"errors"
	"math/rand"
	"testing"
)

func seat(position int, name string, stack int64) *Seat {
	return &Seat{
		Position: position, PlayerID: int64(position + 1), Name: name,
		StackSat: stack, Status: SeatActive,
	}
}

func newTestHand(t *testing.T, stacks []int64, button int, rules Rules) *Hand {
	t.Helper()
	seats := make([]*Seat, len(stacks))
	names := []string{"Ana", "Ben", "Cleo", "Dev", "Eve", "Finn"}
	for i, stack := range stacks {
		seats[i] = seat(i, names[i%len(names)], stack)
	}
	shuffle, err := NewShuffle("test", int64(button))
	if err != nil {
		t.Fatal(err)
	}
	hand, err := NewHand(rules, seats, button, shuffle, int64(button))
	if err != nil {
		t.Fatal(err)
	}
	return hand
}

// chipsInPlay is stacks plus whatever is still in the middle. It must never
// change during a hand except by the rake, which is the single most important
// invariant in the whole engine: chips cannot be created or destroyed at a
// poker table.
func chipsInPlay(h *Hand) int64 {
	var total int64
	for _, s := range h.Seats {
		if s != nil {
			total += s.StackSat + s.TotalCommittedSat
		}
	}
	return total
}

func TestBlindsArePosted(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000}, 0, DefaultRules(200))
	// Three-handed: button 0, small blind 1, big blind 2.
	if h.Seats[1].TotalCommittedSat != 100 {
		t.Errorf("small blind committed %d, want 100", h.Seats[1].TotalCommittedSat)
	}
	if h.Seats[2].TotalCommittedSat != 200 {
		t.Errorf("big blind committed %d, want 200", h.Seats[2].TotalCommittedSat)
	}
	if h.ToAct != 0 {
		t.Errorf("first to act is seat %d, want the button at 0", h.ToAct)
	}
	if h.PotSat() != 300 {
		t.Errorf("pot = %d, want 300", h.PotSat())
	}
}

func TestHeadsUpButtonPostsSmallBlindAndActsFirst(t *testing.T) {
	// The classic two-handed bug: heads-up, the button is the small blind and
	// acts first pre-flop, then last on every later street.
	h := newTestHand(t, []int64{10000, 10000}, 0, DefaultRules(200))
	if h.Seats[0].TotalCommittedSat != 100 {
		t.Errorf("button committed %d, want the small blind of 100", h.Seats[0].TotalCommittedSat)
	}
	if h.Seats[1].TotalCommittedSat != 200 {
		t.Errorf("other seat committed %d, want the big blind of 200", h.Seats[1].TotalCommittedSat)
	}
	if h.ToAct != 0 {
		t.Errorf("first to act is seat %d, want the button", h.ToAct)
	}

	// Get to the flop and check the button now acts last.
	if err := h.Act(0, ActionCall, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Act(1, ActionCheck, 0); err != nil {
		t.Fatal(err)
	}
	if h.Stage != StageFlop {
		t.Fatalf("stage = %v, want the flop", h.Stage)
	}
	if h.ToAct != 1 {
		t.Errorf("post-flop first to act is seat %d, want the big blind at 1", h.ToAct)
	}
}

func TestEveryoneGetsTwoCards(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000, 10000}, 0, DefaultRules(200))
	seen := make(map[Card]bool)
	for i, s := range h.Seats {
		if len(s.Cards) != 2 {
			t.Errorf("seat %d has %d cards, want 2", i, len(s.Cards))
		}
		for _, card := range s.Cards {
			if seen[card] {
				t.Errorf("%s was dealt twice", card)
			}
			seen[card] = true
		}
	}
}

func TestFoldingEverybodyElseWinsUncontested(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000}, 0, DefaultRules(200))
	before := chipsInPlay(h)

	if err := h.Act(0, ActionFold, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Act(1, ActionFold, 0); err != nil {
		t.Fatal(err)
	}
	if !h.Complete() {
		t.Fatal("the hand should be over once everybody folds")
	}

	result, err := h.Settle()
	if err != nil {
		t.Fatal(err)
	}
	// No flop, no drop: a hand that ended pre-flop is never raked.
	if result.RakeSat != 0 {
		t.Errorf("rake = %d on a hand that never saw a flop, want 0", result.RakeSat)
	}
	if result.TotalAwardedSat() != 300 {
		t.Errorf("awarded %d, want the whole 300 pot", result.TotalAwardedSat())
	}
	h.ApplyAwards(result)
	if after := chipsInPlay(h); after != before {
		t.Errorf("chips went from %d to %d", before, after)
	}
	// The big blind gets its own 200 back plus the small blind's 100.
	if h.Seats[2].StackSat != 10100 {
		t.Errorf("winner's stack = %d, want 10100", h.Seats[2].StackSat)
	}
}

func TestCallingStationReachesShowdown(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000}, 0, DefaultRules(200))
	before := chipsInPlay(h)

	// Pre-flop.
	mustAct(t, h, 0, ActionCall, 0)
	mustAct(t, h, 1, ActionCheck, 0)
	// Flop, turn, river: everybody checks.
	for h.Stage < StageShowdown {
		mustAct(t, h, h.ToAct, ActionCheck, 0)
		if h.ToAct == 0 || h.ToAct == 1 {
			if h.Stage < StageShowdown && h.LegalActions(h.ToAct) != nil {
				mustAct(t, h, h.ToAct, ActionCheck, 0)
			}
		}
	}
	if len(h.Board) != 5 {
		t.Fatalf("board has %d cards, want 5", len(h.Board))
	}

	result, err := h.Settle()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Showdown {
		t.Error("expected a showdown")
	}
	h.ApplyAwards(result)
	if after := chipsInPlay(h); after != before-result.RakeSat {
		t.Errorf("chips = %d, want %d after %d rake", after, before-result.RakeSat, result.RakeSat)
	}
}

func TestMinimumRaiseIsEnforced(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000}, 0, DefaultRules(200))
	// The big blind is 200, so the minimum raise is to 400.
	if got := h.MinRaiseToSat(0); got != 400 {
		t.Errorf("minimum raise to %d, want 400", got)
	}
	if err := h.Act(0, ActionRaise, 300); !errors.Is(err, ErrBelowMinRaise) {
		t.Errorf("raising to 300 gave %v, want ErrBelowMinRaise", err)
	}
	if err := h.Act(0, ActionRaise, 400); err != nil {
		t.Fatalf("a legal minimum raise was refused: %v", err)
	}
	// After a raise to 400 the next minimum is 600.
	if got := h.MinRaiseToSat(1); got != 600 {
		t.Errorf("next minimum raise to %d, want 600", got)
	}
}

func TestARaiseReopensTheAction(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000}, 0, DefaultRules(200))
	mustAct(t, h, 0, ActionCall, 0)    // button calls 200
	mustAct(t, h, 1, ActionCall, 0)    // small blind completes
	mustAct(t, h, 2, ActionRaise, 600) // big blind raises

	// The button, who already called, must get another turn.
	if h.ToAct != 0 {
		t.Errorf("action is on seat %d, want it back on the button", h.ToAct)
	}
	if h.Stage != StagePreFlop {
		t.Errorf("stage = %v, want still pre-flop", h.Stage)
	}
}

func TestActingOutOfTurnIsRefused(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000}, 0, DefaultRules(200))
	if err := h.Act(1, ActionCall, 0); !errors.Is(err, ErrNotYourTurn) {
		t.Errorf("got %v, want ErrNotYourTurn", err)
	}
}

func TestCheckingAFacingBetIsRefused(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000}, 0, DefaultRules(200))
	if err := h.Act(0, ActionCheck, 0); !errors.Is(err, ErrIllegalAction) {
		t.Errorf("checking into the big blind gave %v, want ErrIllegalAction", err)
	}
}

func TestRaisingMoreThanTheStackIsRefused(t *testing.T) {
	h := newTestHand(t, []int64{1000, 10000, 10000}, 0, DefaultRules(200))
	if err := h.Act(0, ActionRaise, 5000); !errors.Is(err, ErrNotEnoughChips) {
		t.Errorf("got %v, want ErrNotEnoughChips", err)
	}
}

func TestShortAllInIsLegalBelowTheMinimumRaise(t *testing.T) {
	// A player with less than a full raise behind can still put it all in.
	h := newTestHand(t, []int64{300, 10000, 10000}, 0, DefaultRules(200))
	if err := h.Act(0, ActionRaise, 300); err != nil {
		t.Fatalf("a short all-in was refused: %v", err)
	}
	if h.Seats[0].Status != SeatAllIn {
		t.Errorf("seat status = %s, want all in", h.Seats[0].Status)
	}
}

func TestSidePotIsBuiltForAShortStack(t *testing.T) {
	// Ana can only cover 1000; Ben and Cleo play for more on top.
	h := newTestHand(t, []int64{1000, 10000, 10000}, 0, DefaultRules(200))
	before := chipsInPlay(h)

	mustAct(t, h, 0, ActionRaise, 1000) // Ana all in for 1000
	mustAct(t, h, 1, ActionRaise, 3000) // Ben raises to 3000
	mustAct(t, h, 2, ActionCall, 0)     // Cleo calls 3000

	pots := h.BuildPots()
	if len(pots) != 2 {
		t.Fatalf("built %d pots, want a main and a side pot: %#v", len(pots), pots)
	}
	// Main pot: 1000 from each of three players.
	if pots[0].AmountSat != 3000 {
		t.Errorf("main pot = %d, want 3000", pots[0].AmountSat)
	}
	if len(pots[0].Eligible) != 3 {
		t.Errorf("main pot has %d eligible seats, want 3", len(pots[0].Eligible))
	}
	// Side pot: 2000 more from each of the two deeper players.
	if pots[1].AmountSat != 4000 {
		t.Errorf("side pot = %d, want 4000", pots[1].AmountSat)
	}
	if len(pots[1].Eligible) != 2 {
		t.Errorf("side pot has %d eligible seats, want 2", len(pots[1].Eligible))
	}
	if containsInt(pots[1].Eligible, 0) {
		t.Error("the short stack is eligible for a side pot they did not pay into")
	}

	var potTotal int64
	for _, pot := range pots {
		potTotal += pot.AmountSat
	}
	if potTotal != h.PotSat() {
		t.Errorf("pots sum to %d but %d was committed", potTotal, h.PotSat())
	}
	if chipsInPlay(h) != before {
		t.Error("building pots changed the chips in play")
	}
}

func TestShortStackCannotWinMoreThanTheyMatched(t *testing.T) {
	// The whole point of a side pot: an all-in for 1000 wins at most 1000 from
	// each opponent, however big the pot grows behind them.
	h := newTestHand(t, []int64{1000, 10000, 10000}, 0, DefaultRules(200))
	h.Rules.RakeBps = 0

	mustAct(t, h, 0, ActionRaise, 1000)
	mustAct(t, h, 1, ActionRaise, 3000)
	mustAct(t, h, 2, ActionCall, 0)

	runOutBoard(t, h)

	// Rig the showdown: give the short stack the nuts and the others nothing.
	h.Board = cards(t, "2c", "7d", "9h", "Js", "4s")
	h.Seats[0].Cards = cards(t, "Ac", "Ad")
	h.Seats[1].Cards = cards(t, "Kc", "Kd")
	h.Seats[2].Cards = cards(t, "Qc", "Qd")

	result, err := h.Settle()
	if err != nil {
		t.Fatal(err)
	}
	var shortStackWon int64
	for _, award := range result.Awards {
		if award.Seat == 0 {
			shortStackWon += award.AmountSat
		}
	}
	if shortStackWon != 3000 {
		t.Errorf("the short stack won %d, want exactly the 3000 main pot", shortStackWon)
	}
	// The side pot goes to the best of the two who paid for it.
	var benWon int64
	for _, award := range result.Awards {
		if award.Seat == 1 {
			benWon += award.AmountSat
		}
	}
	if benWon != 4000 {
		t.Errorf("the side pot paid %d to the kings, want 4000", benWon)
	}
}

func TestSplitPotDividesEvenly(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000}, 0, DefaultRules(200))
	h.Rules.RakeBps = 0

	mustAct(t, h, 0, ActionCall, 0)
	mustAct(t, h, 1, ActionCheck, 0)
	runOutBoard(t, h)

	// Both play the board.
	h.Board = cards(t, "Ah", "Kh", "Qh", "Jh", "Th")
	h.Seats[0].Cards = cards(t, "2c", "3d")
	h.Seats[1].Cards = cards(t, "4c", "5d")

	result, err := h.Settle()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Awards) != 2 {
		t.Fatalf("got %d awards, want a two-way split", len(result.Awards))
	}
	if result.Awards[0].AmountSat != result.Awards[1].AmountSat {
		t.Errorf("split was %d and %d, want equal shares",
			result.Awards[0].AmountSat, result.Awards[1].AmountSat)
	}
	if result.TotalAwardedSat() != h.PotSat() {
		t.Errorf("awarded %d of a %d pot", result.TotalAwardedSat(), h.PotSat())
	}
}

func TestOddChipGoesLeftOfTheButton(t *testing.T) {
	h := newTestHand(t, []int64{10000, 10000, 10000}, 0, DefaultRules(200))
	h.Rules.RakeBps = 0

	// Build an odd pot: 101 from each of three players is 303, split three
	// ways among two winners leaves an odd chip.
	for _, s := range h.Seats {
		s.TotalCommittedSat = 0
		s.CommittedSat = 0
	}
	h.Seats[0].TotalCommittedSat = 101
	h.Seats[1].TotalCommittedSat = 101
	h.Seats[2].TotalCommittedSat = 101
	h.Stage = StageShowdown
	h.Board = cards(t, "Ah", "Kh", "Qh", "Jh", "Th")
	h.Seats[0].Cards = cards(t, "2c", "3d")
	h.Seats[1].Cards = cards(t, "4c", "5d")
	h.Seats[2].Cards = cards(t, "6c", "7d")

	result, err := h.Settle()
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalAwardedSat() != 303 {
		t.Fatalf("awarded %d of 303", result.TotalAwardedSat())
	}
	// Seat 1 is immediately left of the button at seat 0, so it takes the odd chip.
	for _, award := range result.Awards {
		if award.Seat == 1 && award.AmountSat != 101 {
			t.Errorf("seat left of the button got %d, want the odd chip at 101", award.AmountSat)
		}
	}
}

func TestRakeIsCappedAndSkippedPreFlop(t *testing.T) {
	rules := DefaultRules(200)
	rules.RakeBps = 500    // 5%
	rules.RakeCapSat = 300 // capped low to make the cap bite

	h := newTestHand(t, []int64{100000, 100000}, 0, rules)
	// Pre-flop only: no rake at all.
	if got := h.RakeFor(50000); got != 0 {
		t.Errorf("rake before the flop = %d, want 0", got)
	}

	h.Board = cards(t, "2c", "7d", "9h")
	if got := h.RakeFor(1000); got != 50 {
		t.Errorf("rake on 1000 = %d, want 50 (5%%)", got)
	}
	if got := h.RakeFor(100000); got != 300 {
		t.Errorf("rake on 100000 = %d, want the 300 cap", got)
	}
}

func TestRakeNeverExceedsThePublishedRate(t *testing.T) {
	// Property check: whatever the pot, the house takes no more than the rate
	// and no more than the cap.
	rules := DefaultRules(200)
	h := newTestHand(t, []int64{100000, 100000}, 0, rules)
	h.Board = cards(t, "2c", "7d", "9h")

	for pot := int64(0); pot < 5_000_000; pot += 9973 {
		rake := h.RakeFor(pot)
		if rake < 0 {
			t.Fatalf("negative rake %d on a pot of %d", rake, pot)
		}
		if rake > pot {
			t.Fatalf("rake %d exceeds the pot of %d", rake, pot)
		}
		if rake > pot*rules.RakeBps/10_000 {
			t.Fatalf("rake %d on %d is above the published %d bps", rake, pot, rules.RakeBps)
		}
		if rake > rules.RakeCapSat {
			t.Fatalf("rake %d exceeds the cap of %d", rake, rules.RakeCapSat)
		}
	}
}

// TestChipsAreConservedAcrossRandomHands is the invariant that matters most:
// across many randomly played hands, chips in play only ever fall by exactly
// the rake. Nothing is created and nothing vanishes.
func TestChipsAreConservedAcrossRandomHands(t *testing.T) {
	source := rand.New(rand.NewSource(20260826))

	for trial := 0; trial < 500; trial++ {
		count := 2 + source.Intn(5)
		stacks := make([]int64, count)
		for i := range stacks {
			stacks[i] = int64(200 * (1 + source.Intn(100)))
		}
		button := source.Intn(count)
		h := newTestHand(t, stacks, button, DefaultRules(200))
		before := chipsInPlay(h)

		for steps := 0; !h.Complete() && steps < 400; steps++ {
			index := h.ToAct
			legal := h.LegalActions(index)
			if len(legal) == 0 {
				break
			}
			action := legal[source.Intn(len(legal))]
			var amount int64
			if action == ActionBet || action == ActionRaise {
				minimum := h.MinRaiseToSat(index)
				maximum := h.MaxRaiseToSat(index)
				amount = minimum
				if maximum > minimum {
					amount = minimum + int64(source.Int63n(maximum-minimum+1))
				}
			}
			if err := h.Act(index, action, amount); err != nil {
				t.Fatalf("trial %d: %s of %d refused: %v", trial, action, amount, err)
			}
		}

		if !h.Complete() {
			t.Fatalf("trial %d: hand did not finish", trial)
		}
		result, err := h.Settle()
		if err != nil {
			t.Fatalf("trial %d: settle: %v", trial, err)
		}
		h.ApplyAwards(result)

		after := chipsInPlay(h)
		if after != before-result.RakeSat {
			t.Fatalf("trial %d: chips went from %d to %d with %d rake (difference %d)",
				trial, before, after, result.RakeSat, before-result.RakeSat-after)
		}
		for i, s := range h.Seats {
			if s.StackSat < 0 {
				t.Fatalf("trial %d: seat %d has a negative stack of %d", trial, i, s.StackSat)
			}
		}
	}
}

func TestHandNeedsTwoPlayers(t *testing.T) {
	seats := []*Seat{seat(0, "Ana", 1000), {Position: 1, Status: SeatEmpty}}
	shuffle, _ := NewShuffle("x", 1)
	if _, err := NewHand(DefaultRules(200), seats, 0, shuffle, 1); !errors.Is(err, ErrNotEnoughPlayers) {
		t.Errorf("got %v, want ErrNotEnoughPlayers", err)
	}
}

func mustAct(t *testing.T, h *Hand, index int, action Action, amount int64) {
	t.Helper()
	if err := h.Act(index, action, amount); err != nil {
		t.Fatalf("seat %d %s %d: %v", index, action, amount, err)
	}
}

// runOutBoard checks the hand down to showdown.
func runOutBoard(t *testing.T, h *Hand) {
	t.Helper()
	for steps := 0; !h.Complete() && steps < 50; steps++ {
		legal := h.LegalActions(h.ToAct)
		if len(legal) == 0 {
			break
		}
		action := ActionCheck
		if !containsAction(legal, ActionCheck) {
			action = ActionCall
		}
		if err := h.Act(h.ToAct, action, 0); err != nil {
			t.Fatalf("running out the board: %v", err)
		}
	}
}
