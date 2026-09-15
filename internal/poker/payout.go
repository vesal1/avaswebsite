package poker

import (
	"fmt"
	"sort"
)

// Pot is a main or side pot with the seats entitled to contest it.
type Pot struct {
	AmountSat int64
	// Eligible are the seat indexes that can win this pot: everybody who
	// contributed to it and has not folded.
	Eligible []int
	// Contributors are every seat whose chips are in this pot, folded or not.
	// Needed to return an uncalled bet: if nobody is eligible to win a pot,
	// the money goes back to whoever put it in.
	Contributors []int
	// Level is the per-player contribution cap that created this pot, kept for
	// the hand history.
	Level int64
}

// Award is one seat's share of one pot.
type Award struct {
	Seat      int
	PlayerID  int64
	AmountSat int64
	// PotIndex identifies which pot the share came from.
	PotIndex int
	// Description is the hand that won it, or why it was awarded uncontested.
	Description string
	// Value is the ranked hand, zero when the pot was not contested.
	Value HandValue
}

// Result is the full outcome of a hand.
type Result struct {
	Pots []Pot
	// Awards are the payouts, ready to be applied to stacks and the ledger.
	Awards []Award
	// RakeSat is what the house took.
	RakeSat int64
	// RakedFrom names the pot the rake came out of, for the hand history.
	Showdown bool
	// Shown lists the seats whose cards were exposed at showdown.
	Shown []int
}

// TotalAwardedSat is what went back to players.
func (r Result) TotalAwardedSat() int64 {
	var total int64
	for _, award := range r.Awards {
		total += award.AmountSat
	}
	return total
}

// BuildPots splits the money committed into a main pot and any side pots.
//
// The standard algorithm: walk the distinct total contributions upward, and at
// each level take that slice from everybody who reached it. A player who is
// all in for less than the others can only win the part of the pot they
// actually matched, which is the entire reason side pots exist.
//
// Folded players' chips stay in the pot, but they are not eligible to win it.
func (h *Hand) BuildPots() []Pot {
	levels := make(map[int64]bool)
	for _, seat := range h.Seats {
		if seat != nil && seat.TotalCommittedSat > 0 {
			levels[seat.TotalCommittedSat] = true
		}
	}
	if len(levels) == 0 {
		return nil
	}

	var pots []Pot
	var previous int64
	for _, level := range sortedInts(levels) {
		var amount int64
		var eligible, contributors []int
		for i, seat := range h.Seats {
			if seat == nil || seat.TotalCommittedSat <= previous {
				continue
			}
			// Everybody who reached this level contributes the slice, folded
			// or not: their chips are already in the middle.
			contribution := level - previous
			if seat.TotalCommittedSat < level {
				contribution = seat.TotalCommittedSat - previous
			}
			amount += contribution
			contributors = append(contributors, i)
			if seat.InHand() && seat.TotalCommittedSat >= level {
				eligible = append(eligible, i)
			}
		}
		if amount > 0 {
			pots = append(pots, Pot{
				AmountSat: amount, Eligible: eligible,
				Contributors: contributors, Level: level,
			})
		}
		previous = level
	}

	// Merge adjacent pots with identical eligibility: they are one pot that
	// happened to be built in two slices, and showing them separately would
	// only confuse the hand history.
	merged := make([]Pot, 0, len(pots))
	for _, pot := range pots {
		if len(merged) > 0 &&
			sameSeats(merged[len(merged)-1].Eligible, pot.Eligible) &&
			sameSeats(merged[len(merged)-1].Contributors, pot.Contributors) {
			merged[len(merged)-1].AmountSat += pot.AmountSat
			merged[len(merged)-1].Level = pot.Level
			continue
		}
		merged = append(merged, pot)
	}
	return merged
}

func sameSeats(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RakeFor computes the house's take on a pot.
//
// Two protections for the player are built in and are not configurable away by
// accident: the rake is capped, and no rake is taken from a hand that ended
// before the flop. "No flop, no drop" is the standard fair-dealing rule, and
// it removes any incentive to churn hands that never get played.
func (h *Hand) RakeFor(potSat int64) int64 {
	if h.Rules.RakeBps <= 0 || potSat <= 0 {
		return 0
	}
	if h.Rules.NoFlopNoDrop && len(h.Board) == 0 {
		return 0
	}
	// A hand nobody contested beyond the blinds is not a raked pot either.
	if len(h.liveSeats()) <= 1 && len(h.Board) == 0 {
		return 0
	}

	rake := potSat * h.Rules.RakeBps / 10_000
	if h.Rules.RakeCapSat > 0 && rake > h.Rules.RakeCapSat {
		rake = h.Rules.RakeCapSat
	}
	if rake > potSat {
		rake = potSat
	}
	return rake
}

// Settle works out who wins what. It does not move any chips: the caller
// applies the awards, so the same computation can be shown to a player before
// it is committed.
func (h *Hand) Settle() (Result, error) {
	if h.Stage < StageShowdown {
		return Result{}, fmt.Errorf("poker: the hand is still on the %s", h.Stage)
	}

	result := Result{Pots: h.BuildPots()}
	live := h.liveSeats()

	// Rake comes off the top of the whole pot, then pots are rebuilt from what
	// is left in proportion. Taking it from a single pot would over-rake a
	// short stack who could not contest the side pot.
	total := h.PotSat()
	result.RakeSat = h.RakeFor(total)
	h.RakeSat = result.RakeSat
	if result.RakeSat > 0 {
		h.logf("rake %d taken from a pot of %d", result.RakeSat, total)
	}
	remaining := total - result.RakeSat
	scalePots(result.Pots, total, remaining)

	// Uncontested: everybody else folded, so the last player standing takes it
	// without showing. Folded chips stay in the pot, which is the rule.
	if len(live) == 1 {
		seat := h.Seats[live[0]]
		var awarded int64
		for index, pot := range result.Pots {
			if pot.AmountSat == 0 {
				continue
			}
			result.Awards = append(result.Awards, Award{
				Seat: live[0], PlayerID: seat.PlayerID, AmountSat: pot.AmountSat,
				PotIndex: index, Description: "uncontested",
			})
			awarded += pot.AmountSat
		}
		h.logf("%s wins %d uncontested", seat.Name, awarded)
		h.Stage = StageComplete
		if awarded+result.RakeSat != total {
			return Result{}, fmt.Errorf(
				"poker: hand %d does not balance: pot %d, awarded %d, rake %d",
				h.ID, total, awarded, result.RakeSat)
		}
		return result, nil
	}

	// Showdown. Rank every live hand once.
	result.Showdown = true
	ranked := make(map[int]Evaluated, len(live))
	for _, index := range live {
		seat := h.Seats[index]
		combined := append(append([]Card{}, seat.Cards...), h.Board...)
		evaluated, err := Evaluate(combined)
		if err != nil {
			return Result{}, fmt.Errorf("poker: ranking seat %d: %w", index, err)
		}
		ranked[index] = evaluated
		result.Shown = append(result.Shown, index)
		h.logf("%s shows %s: %s", seat.Name, FormatCards(seat.Cards), evaluated.Describe())
	}

	for potIndex, pot := range result.Pots {
		if pot.AmountSat == 0 {
			continue
		}
		// Nobody can win this pot, which means it is an uncalled bet: a player
		// put in more than anybody was able to match. It goes back to whoever
		// contributed it rather than being quietly kept.
		if len(pot.Eligible) == 0 {
			result.Awards = append(result.Awards, refund(h, pot, potIndex)...)
			continue
		}
		winners := bestAmong(pot.Eligible, ranked)
		share := pot.AmountSat / int64(len(winners))
		remainder := pot.AmountSat - share*int64(len(winners))

		// Odd chips go to the first winner clockwise from the button, the
		// convention everywhere. Somebody has to get them and a fixed rule
		// beats an arbitrary one.
		for order, index := range h.clockwiseFromButton(winners) {
			amount := share
			if int64(order) < remainder {
				amount++
			}
			if amount == 0 {
				continue
			}
			result.Awards = append(result.Awards, Award{
				Seat: index, PlayerID: h.Seats[index].PlayerID, AmountSat: amount,
				PotIndex: potIndex, Description: ranked[index].Describe(),
				Value: ranked[index].Value,
			})
			h.logf("%s wins %d from pot %d with %s",
				h.Seats[index].Name, amount, potIndex+1, ranked[index].Describe())
		}
	}

	h.Stage = StageComplete

	// Nothing may be lost between the pot and the players. This is checked
	// here rather than trusted, because a hand that quietly keeps chips is
	// indistinguishable from a hand that pays out correctly until somebody
	// audits the table months later.
	if awarded := result.TotalAwardedSat(); awarded+result.RakeSat != total {
		return Result{}, fmt.Errorf(
			"poker: hand %d does not balance: pot %d, awarded %d, rake %d",
			h.ID, total, awarded, result.RakeSat)
	}
	return result, nil
}

// refund returns an uncalled pot to the seats that paid into it, in proportion
// to what each put in.
func refund(h *Hand, pot Pot, potIndex int) []Award {
	var contributedTotal int64
	shares := make(map[int]int64, len(pot.Contributors))
	for _, index := range pot.Contributors {
		seat := h.Seats[index]
		if seat == nil {
			continue
		}
		share := seat.TotalCommittedSat
		shares[index] = share
		contributedTotal += share
	}
	if contributedTotal == 0 {
		return nil
	}

	awards := make([]Award, 0, len(shares))
	var handed int64
	ordered := h.clockwiseFromButton(pot.Contributors)
	for _, index := range ordered {
		amount := pot.AmountSat * shares[index] / contributedTotal
		if amount <= 0 {
			continue
		}
		handed += amount
		awards = append(awards, Award{
			Seat: index, PlayerID: h.Seats[index].PlayerID, AmountSat: amount,
			PotIndex: potIndex, Description: "uncalled bet returned",
		})
		h.logf("%s has %d returned, uncalled", h.Seats[index].Name, amount)
	}
	// Any rounding remainder goes to the largest contributor, so the pot
	// always distributes exactly.
	if remainder := pot.AmountSat - handed; remainder > 0 && len(awards) > 0 {
		best := 0
		for i := range awards {
			if shares[awards[i].Seat] > shares[awards[best].Seat] {
				best = i
			}
		}
		awards[best].AmountSat += remainder
	}
	return awards
}

// scalePots reduces each pot proportionally after rake, keeping the totals
// exact: any rounding remainder is taken off the largest pot so the awards sum
// to precisely what is left.
func scalePots(pots []Pot, total, remaining int64) {
	if total <= 0 || total == remaining {
		return
	}
	var distributed int64
	for i := range pots {
		pots[i].AmountSat = pots[i].AmountSat * remaining / total
		distributed += pots[i].AmountSat
	}
	shortfall := remaining - distributed
	if shortfall == 0 || len(pots) == 0 {
		return
	}
	largest := 0
	for i := range pots {
		if pots[i].AmountSat > pots[largest].AmountSat {
			largest = i
		}
	}
	pots[largest].AmountSat += shortfall
}

// bestAmong returns every seat tied for the best hand.
func bestAmong(eligible []int, ranked map[int]Evaluated) []int {
	var best HandValue
	for _, index := range eligible {
		if evaluated, ok := ranked[index]; ok && evaluated.Value > best {
			best = evaluated.Value
		}
	}
	var winners []int
	for _, index := range eligible {
		if evaluated, ok := ranked[index]; ok && evaluated.Value == best {
			winners = append(winners, index)
		}
	}
	sort.Ints(winners)
	return winners
}

// clockwiseFromButton orders seats starting immediately left of the button.
func (h *Hand) clockwiseFromButton(seats []int) []int {
	ordered := make([]int, 0, len(seats))
	for step := 1; step <= len(h.Seats); step++ {
		index := (h.Button + step) % len(h.Seats)
		for _, candidate := range seats {
			if candidate == index {
				ordered = append(ordered, index)
			}
		}
	}
	// Any seat not matched (should not happen) is appended so no winner is
	// silently dropped from a payout.
	for _, candidate := range seats {
		if !containsInt(ordered, candidate) {
			ordered = append(ordered, candidate)
		}
	}
	return ordered
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ApplyAwards credits the winners' stacks and empties the pot.
//
// Clearing the committed amounts is part of paying out, not bookkeeping
// tidiness: those chips have left the middle and are now in somebody's stack,
// and leaving them recorded would count them in two places at once.
//
// Separate from Settle so a caller can record and display the result before
// any chips move.
func (h *Hand) ApplyAwards(result Result) {
	for _, award := range result.Awards {
		if seat := h.Seats[award.Seat]; seat != nil {
			seat.StackSat += award.AmountSat
		}
	}
	for _, seat := range h.Seats {
		if seat != nil {
			seat.CommittedSat = 0
			seat.TotalCommittedSat = 0
		}
	}
}
