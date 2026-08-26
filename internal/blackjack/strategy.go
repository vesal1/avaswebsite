package blackjack

import "sync"

// This file computes the strategy card and the game's expected value by
// dynamic programming, not by quoting a book.
//
// The model draws each card independently with full-deck odds — 1/13 per
// rank, 4/13 for the tens. Against a single physical deck that is an
// approximation: the three cards you can see shift the odds of the ones you
// cannot by a fraction of a percent. The approximation is used for the card
// only. The RETURN published on the page comes from playing the real
// single-deck engine millions of rounds under this exact card (see
// rtp_test.go), so the figure describes the game as dealt, and the DP here
// cross-checks it.
//
// The recursion mirrors the rules in round.go move for move: stand on all
// 17s, double any first two, split once, one card to split aces, no double
// after split, peek. Change a rule there without changing it here and the
// tests fail, which is the point of having both.

// cardProb is the chance of drawing each value 1 (ace) through 10.
func cardProb(value int) float64 {
	if value == 10 {
		return 4.0 / 13.0
	}
	return 1.0 / 13.0
}

// handState is a player or dealer hand reduced to what the rules care about:
// the ace-as-1 sum and whether an ace can currently count as 11.
type handState struct {
	sum  int
	aces int
}

func (h handState) add(value int) handState {
	next := handState{sum: h.sum + value, aces: h.aces}
	if value == 1 {
		next.aces++
	}
	return next
}

// total is the best value; soft reports an ace counted as 11.
func (h handState) total() (int, bool) {
	if h.aces > 0 && h.sum+10 <= 21 {
		return h.sum + 10, true
	}
	return h.sum, false
}

// ---------------------------------------------------------------------------
// Dealer
// ---------------------------------------------------------------------------

// dealerDist is the dealer's final outcome: indices 0..4 are totals 17..21,
// index 5 is a bust.
type dealerDist [6]float64

// dealerFinal computes the dealer's distribution for an upcard, conditioned
// on the peek having already ruled out a natural.
func dealerFinal(up int) dealerDist {
	start := handState{}.add(up)

	var run func(h handState, p float64, dist *dealerDist, first bool)
	run = func(h handState, p float64, dist *dealerDist, first bool) {
		total, _ := h.total()
		if total >= 17 {
			if total > 21 {
				dist[5] += p
			} else {
				dist[total-17] += p
			}
			return
		}
		for value := 1; value <= 10; value++ {
			weight := cardProb(value)
			if first {
				// The hole card is drawn knowing the peek found no natural:
				// an ace up cannot hide a ten, a ten cannot hide an ace.
				if up == 1 && value == 10 {
					continue
				}
				if up == 10 && value == 1 {
					continue
				}
			}
			run(h.add(value), p*weight, dist, false)
		}
	}

	var dist dealerDist
	run(start, 1, &dist, true)

	// Renormalise for the excluded natural, so the distribution sums to one.
	sum := 0.0
	for _, p := range dist {
		sum += p
	}
	for i := range dist {
		dist[i] /= sum
	}
	return dist
}

// ---------------------------------------------------------------------------
// Player
// ---------------------------------------------------------------------------

// evStand is the expectation, per unit bet, of standing on a total.
func evStand(total int, dist dealerDist) float64 {
	if total > 21 {
		return -1
	}
	ev := dist[5] // dealer bust
	for dealerTotal := 17; dealerTotal <= 21; dealerTotal++ {
		p := dist[dealerTotal-17]
		switch {
		case total > dealerTotal:
			ev += p
		case total < dealerTotal:
			ev -= p
		}
	}
	return ev
}

// solver memoises the player recursion for one upcard.
type solver struct {
	dist dealerDist
	memo map[handState]float64
}

func newSolver(up int) *solver {
	return &solver{dist: dealerFinal(up), memo: make(map[handState]float64)}
}

// evHitThenPlay is the expectation of taking a card and continuing with
// hit/stand only.
func (s *solver) evHitThenPlay(h handState) float64 {
	ev := 0.0
	for value := 1; value <= 10; value++ {
		next := h.add(value)
		if next.sum > 21 {
			ev -= cardProb(value)
			continue
		}
		ev += cardProb(value) * s.evPlay(next)
	}
	return ev
}

// evPlay is the best of standing and hitting from here on.
func (s *solver) evPlay(h handState) float64 {
	if cached, ok := s.memo[h]; ok {
		return cached
	}
	total, _ := h.total()
	best := evStand(total, s.dist)
	if total < 21 {
		if hit := s.evHitThenPlay(h); hit > best {
			best = hit
		}
	}
	s.memo[h] = best
	return best
}

// evDouble doubles the bet, takes exactly one card, and stands.
func (s *solver) evDouble(h handState) float64 {
	ev := 0.0
	for value := 1; value <= 10; value++ {
		next := h.add(value)
		total, _ := next.total()
		ev += cardProb(value) * evStand(total, s.dist)
	}
	return 2 * ev
}

// evSplit plays one split hand of a pair and doubles it: the hands draw
// independently under this model, so their expectations add.
func (s *solver) evSplit(pairValue int) float64 {
	if pairValue == 1 {
		// Split aces take one card and stand.
		ev := 0.0
		for value := 1; value <= 10; value++ {
			total, _ := handState{}.add(1).add(value).total()
			ev += cardProb(value) * evStand(total, s.dist)
		}
		return 2 * ev
	}
	ev := 0.0
	for value := 1; value <= 10; value++ {
		next := handState{}.add(pairValue).add(value)
		// No double after split, no resplit: the hand plays hit/stand.
		ev += cardProb(value) * s.evPlay(next)
	}
	return 2 * ev
}

// ---------------------------------------------------------------------------
// The card
// ---------------------------------------------------------------------------

// Advice is a cell of the strategy card: the best play, and what to do when
// the table no longer offers it (a double with three cards, say).
type Advice struct {
	Primary  Action `json:"primary"`
	Fallback Action `json:"fallback"`
}

// Strategy is the whole card. Upcard indices run 1 (ace) to 10.
type Strategy struct {
	// Hard[t][up] for hard totals 5..20; Soft[t][up] for soft totals 13..20;
	// Pairs[v][up] reports whether to split a pair of value v.
	Hard  map[int]map[int]Advice `json:"hard"`
	Soft  map[int]map[int]Advice `json:"soft"`
	Pairs map[int]map[int]bool   `json:"pairs"`
}

var (
	strategyOnce sync.Once
	strategyCard Strategy
	solvers      map[int]*solver
	houseEV      float64
)

func compute() {
	solvers = make(map[int]*solver, 10)
	for up := 1; up <= 10; up++ {
		solvers[up] = newSolver(up)
	}

	strategyCard = Strategy{
		Hard:  make(map[int]map[int]Advice),
		Soft:  make(map[int]map[int]Advice),
		Pairs: make(map[int]map[int]bool),
	}

	// Hard totals: a representative composition with no usable ace. The
	// hit/stand/double expectations depend only on the total under this
	// model, so one composition per total is the general answer.
	for total := 5; total <= 20; total++ {
		strategyCard.Hard[total] = make(map[int]Advice)
		for up := 1; up <= 10; up++ {
			s := solvers[up]
			h := handState{sum: total}
			strategyCard.Hard[total][up] = adviseFirstTwo(s, h)
		}
	}
	// Soft totals 13..20: an ace counted as 11 plus 2..9.
	for total := 13; total <= 20; total++ {
		strategyCard.Soft[total] = make(map[int]Advice)
		for up := 1; up <= 10; up++ {
			s := solvers[up]
			h := handState{}.add(1).add(total - 11)
			strategyCard.Soft[total][up] = adviseFirstTwo(s, h)
		}
	}
	// Pairs: split when it beats the best non-split line of the same two
	// cards.
	for pair := 1; pair <= 10; pair++ {
		strategyCard.Pairs[pair] = make(map[int]bool)
		for up := 1; up <= 10; up++ {
			s := solvers[up]
			h := handState{}.add(pair).add(pair)
			flat := adviseFirstTwo(s, h)
			flatEV := evOf(s, h, flat.Primary)
			strategyCard.Pairs[pair][up] = s.evSplit(pair) > flatEV
		}
	}

	houseEV = overallEV()
}

// adviseFirstTwo picks the best of stand, hit and double for a two-card hand,
// and the best of stand and hit as the fallback.
func adviseFirstTwo(s *solver, h handState) Advice {
	total, _ := h.total()
	stand := evStand(total, s.dist)
	hit := s.evHitThenPlay(h)
	double := s.evDouble(h)

	fallback := Stand
	fallbackEV := stand
	if hit > fallbackEV {
		fallback, fallbackEV = Hit, hit
	}
	primary := fallback
	if double > fallbackEV {
		primary = Double
	}
	return Advice{Primary: primary, Fallback: fallback}
}

func evOf(s *solver, h handState, action Action) float64 {
	total, _ := h.total()
	switch action {
	case Stand:
		return evStand(total, s.dist)
	case Hit:
		return s.evHitThenPlay(h)
	case Double:
		return s.evDouble(h)
	}
	return evStand(total, s.dist)
}

// overallEV is the model's expectation per unit staked at the deal, naturals,
// peek and all — the cross-check for the simulated figure.
func overallEV() float64 {
	ev := 0.0
	for c1 := 1; c1 <= 10; c1++ {
		for c2 := 1; c2 <= 10; c2++ {
			for up := 1; up <= 10; up++ {
				weight := cardProb(c1) * cardProb(c2) * cardProb(up)
				ev += weight * dealEV(c1, c2, up)
			}
		}
	}
	return ev
}

func dealEV(c1, c2, up int) float64 {
	// Chance the peek finds a natural under the hole card.
	var dealerBJ float64
	switch up {
	case 1:
		dealerBJ = cardProb(10)
	case 10:
		dealerBJ = cardProb(1)
	}

	h := handState{}.add(c1).add(c2)
	if total, _ := h.total(); total == 21 {
		// A natural pushes the dealer's and beats everything else 3:2.
		return (1 - dealerBJ) * 1.5
	}

	s := solvers[up]
	best := evOf(s, h, adviseFirstTwo(s, h).Primary)
	if c1 == c2 && strategyCard.Pairs[c1][up] {
		best = s.evSplit(c1)
	}
	return dealerBJ*(-1) + (1-dealerBJ)*best
}

// Card returns the strategy card, computing it once.
func CardTable() Strategy {
	strategyOnce.Do(compute)
	return strategyCard
}

// ModelEV is the DP's expectation per unit staked: a small negative number.
func ModelEV() float64 {
	strategyOnce.Do(compute)
	return houseEV
}

// Advise is what the card says for a live hand. It respects what the table
// actually offers: a split only on a first-move pair, a double only on the
// first two cards of an unsplit hand.
func Advise(round *Round) (Action, bool) {
	hand := round.ActiveHand()
	if hand == nil || len(round.State.Dealer) == 0 {
		return "", false
	}
	strategyOnce.Do(compute)
	up := round.State.Dealer[0].Value()

	if round.allows(Split) && hand.Cards[0].Value() == hand.Cards[1].Value() {
		if strategyCard.Pairs[hand.Cards[0].Value()][up] {
			return Split, true
		}
	}

	total, soft := hand.Total()
	var advice Advice
	if soft {
		advice = strategyCard.Soft[total][up]
	} else {
		advice = strategyCard.Hard[total][up]
	}
	if advice.Primary == Double && !round.allows(Double) {
		return advice.Fallback, true
	}
	if advice.Primary == "" {
		return Stand, true
	}
	return advice.Primary, true
}
