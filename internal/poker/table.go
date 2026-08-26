package poker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Errors a table returns.
var (
	ErrSeatTaken      = errors.New("poker: that seat is taken")
	ErrAlreadySeated  = errors.New("poker: you are already at this table")
	ErrNotSeated      = errors.New("poker: you are not seated at this table")
	ErrBuyInRange     = errors.New("poker: buy-in is outside the table's range")
	ErrHandInProgress = errors.New("poker: you cannot do that during a hand")
	ErrTableFull      = errors.New("poker: the table is full")
)

// Bank moves money between a player's account and the table.
//
// Chips only cross this boundary at buy-in and cash-out. While a hand is
// running, chips move between seats inside the table's own ledger account, so
// a bad beat is not a ledger transaction. Rake is the one exception: it leaves
// the table for the house.
type Bank interface {
	BuyIn(ctx context.Context, userID, tableID, amountSat int64) error
	CashOut(ctx context.Context, userID, tableID, amountSat int64) error
	TakeRake(ctx context.Context, tableID, handID, amountSat int64) error
}

// SeatStore persists who is sitting where and with how much.
//
// It exists because chips at a table are customer money held in the table's
// ledger account. Losing track of the seats across a restart would strand that
// money: present in the ledger, attributable to nobody.
type SeatStore interface {
	Occupy(ctx context.Context, tableID int64, seatNumber int, userID, stackSat int64, clientSeed string) error
	UpdateStack(ctx context.Context, tableID int64, seatNumber int, stackSat int64) error
	Vacate(ctx context.Context, tableID int64, seatNumber int) error
}

// Recorder writes hand histories. A hand nobody can look up afterwards cannot
// be disputed, which is not the same thing as being correct.
type Recorder interface {
	StartHand(ctx context.Context, record HandStart) (int64, error)
	FinishHand(ctx context.Context, record HandFinish) error
}

// HandStart is written before the deal, so the commitment is on record before
// anybody could know what the deck holds.
type HandStart struct {
	TableID    int64
	HandNumber int64
	ButtonSeat int
	Commitment string
	ClientSeed string
	Players    []HandStartPlayer
}

// HandStartPlayer is one seat as the hand begins.
type HandStartPlayer struct {
	UserID        int64
	SeatNumber    int
	StartingStack int64
	HoleCards     string
}

// HandFinish is written when the hand is over, including the revealed seed.
type HandFinish struct {
	HandID     int64
	ServerSeed string
	Board      string
	PotSat     int64
	RakeSat    int64
	History    string
	Actions    []RecordedAction
	Results    []HandResultPlayer
}

// RecordedAction is one decision, in order.
type RecordedAction struct {
	Sequence   int
	SeatNumber int
	Stage      string
	Action     string
	AmountSat  int64
}

// HandResultPlayer is one seat's outcome.
type HandResultPlayer struct {
	UserID       int64
	SeatNumber   int
	CommittedSat int64
	WonSat       int64
	Shown        bool
	Result       string
}

// TableConfig describes a cash table.
type TableConfig struct {
	ID            int64
	Name          string
	Rules         Rules
	MaxSeats      int
	MinBuyInSat   int64
	MaxBuyInSat   int64
	ActionTimeout time.Duration
	VideoEnabled  bool
}

// Table is a running cash table.
//
// One mutex guards everything. A poker table is a small, low-frequency state
// machine — a handful of actions a minute — so the simplest correct
// concurrency model is the right one, and it removes any chance of two
// actions interleaving inside a hand.
type Table struct {
	Config TableConfig

	mu          sync.Mutex
	seats       []*Seat
	seatUsers   map[int64]int // user id to seat number
	clientSeeds map[int]string
	consented   map[int]bool

	hand       *Hand
	handID     int64
	handNumber int64
	button     int
	deadline   time.Time
	actions    []RecordedAction

	bank      Bank
	recorder  Recorder
	seatStore SeatStore
	now       func() time.Time

	subscribers map[int64]chan struct{}
	nextSub     int64

	// pendingLeave holds seats that asked to stand up mid-hand; they are
	// released when the hand ends rather than yanked out of it.
	pendingLeave map[int]bool
	// pendingSitOut holds seats that should stop being dealt in after this
	// hand but keep their seat and chips.
	pendingSitOut map[int]bool
}

// NewTable builds a running table.
func NewTable(config TableConfig, bank Bank, recorder Recorder, seatStore SeatStore) *Table {
	if config.MaxSeats < 2 {
		config.MaxSeats = 6
	}
	if config.ActionTimeout <= 0 {
		config.ActionTimeout = 30 * time.Second
	}
	seats := make([]*Seat, config.MaxSeats)
	for i := range seats {
		seats[i] = &Seat{Position: i, Status: SeatEmpty}
	}
	return &Table{
		Config: config, seats: seats,
		seatUsers:     make(map[int64]int),
		clientSeeds:   make(map[int]string),
		consented:     make(map[int]bool),
		subscribers:   make(map[int64]chan struct{}),
		pendingLeave:  make(map[int]bool),
		pendingSitOut: make(map[int]bool),
		bank:          bank, recorder: recorder, seatStore: seatStore,
		now: time.Now,
	}
}

// RestoredSeat is a seat being brought back after a restart.
type RestoredSeat struct {
	SeatNumber int
	UserID     int64
	Name       string
	StackSat   int64
	ClientSeed string
	SittingOut bool
}

// Restore puts persisted seats back at the table.
//
// No chips move: they are already in the table's ledger account, which is
// exactly why the seats have to come back. Anybody restored with an empty
// stack starts sitting out rather than being dealt into a hand they cannot pay
// the blinds for.
func (t *Table) Restore(seats []RestoredSeat) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, restored := range seats {
		if restored.SeatNumber < 0 || restored.SeatNumber >= len(t.seats) {
			continue
		}
		seat := t.seats[restored.SeatNumber]
		seat.PlayerID = restored.UserID
		seat.Name = restored.Name
		seat.StackSat = restored.StackSat
		seat.Status = SeatActive
		if restored.SittingOut || restored.StackSat <= 0 {
			seat.Status = SeatSittingOut
		}
		t.seatUsers[restored.UserID] = restored.SeatNumber

		seed := restored.ClientSeed
		if seed == "" {
			if generated, err := NewClientSeed(); err == nil {
				seed = generated
			}
		}
		t.clientSeeds[restored.SeatNumber] = seed
	}
	t.notify()
}

// WithClock replaces the table clock. Test-only.
func (t *Table) WithClock(clock func() time.Time) *Table {
	t.now = clock
	return t
}

// Subscribe returns a channel that receives a nudge whenever the table state
// changes, and a function to stop listening.
//
// The channel carries no data on purpose: a listener re-reads its own view, so
// there is no chance of a stale or mis-addressed snapshot leaking one player's
// cards to another.
func (t *Table) Subscribe() (<-chan struct{}, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.nextSub++
	id := t.nextSub
	channel := make(chan struct{}, 1)
	t.subscribers[id] = channel

	return channel, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if existing, ok := t.subscribers[id]; ok {
			delete(t.subscribers, id)
			close(existing)
		}
	}
}

// notify wakes every listener. Must be called with the lock held.
func (t *Table) notify() {
	for _, channel := range t.subscribers {
		select {
		case channel <- struct{}{}:
		default:
			// A listener that has not caught up yet will re-read anyway.
		}
	}
}

// Sit seats a player and moves their buy-in from their account to the table.
func (t *Table) Sit(ctx context.Context, userID int64, name string, seatNumber int, buyInSat int64) error {
	if buyInSat < t.Config.MinBuyInSat || buyInSat > t.Config.MaxBuyInSat {
		return fmt.Errorf("%w: buy in between %d and %d",
			ErrBuyInRange, t.Config.MinBuyInSat, t.Config.MaxBuyInSat)
	}

	t.mu.Lock()
	if _, seated := t.seatUsers[userID]; seated {
		t.mu.Unlock()
		return ErrAlreadySeated
	}
	if seatNumber < 0 || seatNumber >= len(t.seats) {
		t.mu.Unlock()
		return fmt.Errorf("%w: seat %d does not exist", ErrSeatTaken, seatNumber)
	}
	if t.seats[seatNumber].Status != SeatEmpty {
		t.mu.Unlock()
		return ErrSeatTaken
	}
	// Claim the seat before releasing the lock so two players cannot buy into
	// it at once, then hand the money over.
	t.seats[seatNumber].Status = SeatSittingOut
	t.seats[seatNumber].PlayerID = userID
	t.seats[seatNumber].Name = name
	t.seatUsers[userID] = seatNumber
	t.mu.Unlock()

	if err := t.bank.BuyIn(ctx, userID, t.Config.ID, buyInSat); err != nil {
		// Give the seat back: the player has not paid for it.
		t.mu.Lock()
		t.releaseSeat(seatNumber)
		t.mu.Unlock()
		return err
	}

	seed, err := NewClientSeed()
	if err != nil {
		return err
	}

	t.mu.Lock()
	seat := t.seats[seatNumber]
	seat.StackSat = buyInSat
	// A player joining mid-hand waits for the next one rather than being dealt
	// into a hand whose blinds they did not post.
	if t.hand == nil {
		seat.Status = SeatActive
	}
	t.clientSeeds[seatNumber] = seed
	t.notify()
	t.mu.Unlock()

	// Persisted outside the lock. The seat is already claimed in memory and
	// the money has moved, so a failure here is reported rather than unwound:
	// the chips are at the table either way, and the reconciliation check is
	// what would surface a seat that failed to record.
	return t.seatStore.Occupy(ctx, t.Config.ID, seatNumber, userID, buyInSat, seed)
}

func (t *Table) releaseSeat(seatNumber int) {
	seat := t.seats[seatNumber]
	delete(t.seatUsers, seat.PlayerID)
	delete(t.clientSeeds, seatNumber)
	delete(t.consented, seatNumber)
	delete(t.pendingLeave, seatNumber)
	delete(t.pendingSitOut, seatNumber)
	t.seats[seatNumber] = &Seat{Position: seatNumber, Status: SeatEmpty}
}

// Leave stands a player up and returns their chips to their account.
//
// Leaving during a hand is allowed but deferred: the chips are already in the
// pot and the hand plays out. Letting somebody remove a live stack mid-hand
// would be a way to take back a losing bet.
func (t *Table) Leave(ctx context.Context, userID int64) (int64, error) {
	t.mu.Lock()
	seatNumber, seated := t.seatUsers[userID]
	if !seated {
		t.mu.Unlock()
		return 0, ErrNotSeated
	}
	if t.hand != nil && t.seats[seatNumber].InHand() {
		t.pendingLeave[seatNumber] = true
		t.seats[seatNumber].Status = SeatActive // still in the hand
		t.mu.Unlock()
		return 0, nil
	}
	stack := t.seats[seatNumber].StackSat
	t.releaseSeat(seatNumber)
	t.mu.Unlock()

	if stack > 0 {
		if err := t.bank.CashOut(ctx, userID, t.Config.ID, stack); err != nil {
			return 0, err
		}
	}
	if err := t.seatStore.Vacate(ctx, t.Config.ID, seatNumber); err != nil {
		return stack, err
	}

	t.mu.Lock()
	t.notify()
	t.mu.Unlock()
	return stack, nil
}

// SetClientSeed lets a player choose their own contribution to the shuffle.
func (t *Table) SetClientSeed(userID int64, seed string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	seatNumber, seated := t.seatUsers[userID]
	if !seated {
		return ErrNotSeated
	}
	if t.hand != nil {
		// Changing a seed mid-hand would let a player influence a deck that
		// has already been committed to.
		return ErrHandInProgress
	}
	if seed == "" {
		generated, err := NewClientSeed()
		if err != nil {
			return err
		}
		seed = generated
	}
	t.clientSeeds[seatNumber] = seed
	t.notify()
	return nil
}

// SetVideoConsent records whether a seat is sharing camera and microphone.
//
// Consent is per seat and per sitting: it is not remembered, and standing up
// clears it. Nobody's camera is on because of something they agreed to once.
func (t *Table) SetVideoConsent(userID int64, consent bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	seatNumber, seated := t.seatUsers[userID]
	if !seated {
		return ErrNotSeated
	}
	if consent {
		t.consented[seatNumber] = true
	} else {
		delete(t.consented, seatNumber)
	}
	t.notify()
	return nil
}

// SitIn and SitOut move a seat between playing and watching.
func (t *Table) SitIn(userID int64) error  { return t.setPlaying(userID, true) }
func (t *Table) SitOut(userID int64) error { return t.setPlaying(userID, false) }

func (t *Table) setPlaying(userID int64, playing bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	seatNumber, seated := t.seatUsers[userID]
	if !seated {
		return ErrNotSeated
	}
	seat := t.seats[seatNumber]
	if t.hand != nil && seat.InHand() {
		// Sitting out takes effect after the current hand; the player stays in
		// the one they are already in.
		if playing {
			delete(t.pendingSitOut, seatNumber)
		} else {
			t.pendingSitOut[seatNumber] = true
		}
		return nil
	}
	if playing {
		if seat.StackSat <= 0 {
			return fmt.Errorf("%w: rebuy before sitting in", ErrBuyInRange)
		}
		seat.Status = SeatActive
	} else {
		seat.Status = SeatSittingOut
	}
	t.notify()
	return nil
}

// readySeats counts seats able to be dealt in.
func (t *Table) readySeats() int {
	var count int
	for _, seat := range t.seats {
		if seat.Status == SeatActive && seat.StackSat > 0 {
			count++
		}
	}
	return count
}

// StartHand deals the next hand if the table is ready. It is safe to call at
// any time; it does nothing when a hand is already running or too few players
// are seated.
func (t *Table) StartHand(ctx context.Context) error {
	t.mu.Lock()
	if t.hand != nil && !t.hand.Complete() {
		t.mu.Unlock()
		return nil
	}
	if t.readySeats() < 2 {
		t.mu.Unlock()
		return nil
	}

	t.button = t.nextButton()
	t.handNumber++

	// Fold every seated player's seed into one, so neither the house nor any
	// single player decides the deck.
	seeds := make([]string, 0, len(t.seats))
	for i := range t.seats {
		if t.seats[i].Status == SeatActive {
			seeds = append(seeds, t.clientSeeds[i])
		}
	}
	combined := CombineClientSeeds(seeds)

	shuffle, err := NewShuffle(combined, t.handNumber)
	if err != nil {
		t.mu.Unlock()
		return err
	}
	hand, err := NewHand(t.Config.Rules, t.seats, t.button, shuffle, t.handNumber)
	if err != nil {
		t.mu.Unlock()
		if errors.Is(err, ErrNotEnoughPlayers) {
			return nil
		}
		return err
	}
	hand.TableID = t.Config.ID
	t.hand = hand
	t.actions = nil
	t.deadline = t.now().Add(t.Config.ActionTimeout)

	start := HandStart{
		TableID: t.Config.ID, HandNumber: t.handNumber, ButtonSeat: t.button,
		Commitment: shuffle.Commitment, ClientSeed: combined,
	}
	for i, seat := range t.seats {
		if seat.InHand() {
			start.Players = append(start.Players, HandStartPlayer{
				UserID: seat.PlayerID, SeatNumber: i,
				StartingStack: seat.StackSat + seat.TotalCommittedSat,
				HoleCards:     FormatCards(seat.Cards),
			})
		}
	}
	t.notify()
	t.mu.Unlock()

	handID, err := t.recorder.StartHand(ctx, start)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.handID = handID
	t.hand.ID = handID
	t.mu.Unlock()

	// A hand can be decided before anybody acts if only one player has chips.
	return t.finishIfComplete(ctx)
}

// nextButton advances the dealer button to the next seated player.
func (t *Table) nextButton() int {
	for step := 1; step <= len(t.seats); step++ {
		index := (t.button + step) % len(t.seats)
		if t.seats[index].Status == SeatActive && t.seats[index].StackSat > 0 {
			return index
		}
	}
	return t.button
}

// Act applies a player's decision and moves the hand on.
func (t *Table) Act(ctx context.Context, userID int64, action Action, amountSat int64) error {
	t.mu.Lock()
	if t.hand == nil || t.hand.Complete() {
		t.mu.Unlock()
		return ErrHandComplete
	}
	seatNumber, seated := t.seatUsers[userID]
	if !seated {
		t.mu.Unlock()
		return ErrNotSeated
	}
	stage := t.hand.Stage.String()
	if err := t.hand.Act(seatNumber, action, amountSat); err != nil {
		t.mu.Unlock()
		return err
	}
	t.actions = append(t.actions, RecordedAction{
		Sequence: len(t.actions) + 1, SeatNumber: seatNumber,
		Stage: stage, Action: string(action), AmountSat: amountSat,
	})
	t.deadline = t.now().Add(t.Config.ActionTimeout)
	t.notify()
	t.mu.Unlock()

	return t.finishIfComplete(ctx)
}

// Tick folds a player who has run out of time. Call it on a timer.
//
// A timed-out player checks when it is free to do so and folds otherwise,
// which is the standard rule and the one that costs a disconnected player
// least.
func (t *Table) Tick(ctx context.Context) error {
	t.mu.Lock()
	if t.hand == nil || t.hand.Complete() || t.now().Before(t.deadline) {
		t.mu.Unlock()
		return nil
	}
	seatNumber := t.hand.ToAct
	seat := t.hand.Seats[seatNumber]
	action := ActionFold
	if t.hand.CallAmountSat(seatNumber) == 0 {
		action = ActionCheck
	}
	stage := t.hand.Stage.String()
	if err := t.hand.Act(seatNumber, action, 0); err != nil {
		t.mu.Unlock()
		return err
	}
	if seat != nil {
		seat.TimedOut = true
		// Somebody who is not there stops being dealt in, so they do not bleed
		// blinds until their stack is gone. They keep their seat and their
		// chips: taking both away over one missed decision would be a harsher
		// answer than the problem needs, and their money stays reconciled at
		// the table until they come back or cash out.
		t.pendingSitOut[seatNumber] = true
	}
	t.actions = append(t.actions, RecordedAction{
		Sequence: len(t.actions) + 1, SeatNumber: seatNumber,
		Stage: stage, Action: string(action) + " (timed out)",
	})
	t.deadline = t.now().Add(t.Config.ActionTimeout)
	t.notify()
	t.mu.Unlock()

	return t.finishIfComplete(ctx)
}

// finishIfComplete settles a finished hand, pays out and records it.
func (t *Table) finishIfComplete(ctx context.Context) error {
	t.mu.Lock()
	if t.hand == nil || !t.hand.Complete() || t.hand.Stage == StageComplete {
		t.mu.Unlock()
		return nil
	}

	hand := t.hand
	result, err := hand.Settle()
	if err != nil {
		t.mu.Unlock()
		return err
	}

	// What each seat put in has to be read before the payout, which empties
	// the pot. Reading it afterwards records every hand as nobody having bet.
	committed := make(map[int]int64, len(hand.Seats))
	for i, seat := range hand.Seats {
		if seat != nil {
			committed[i] = seat.TotalCommittedSat
		}
	}

	hand.ApplyAwards(result)

	finish := HandFinish{
		HandID:     t.handID,
		ServerSeed: hand.Shuffle.Reveal(),
		Board:      FormatCards(hand.Board),
		PotSat:     hand.PotSat() + result.TotalAwardedSat() + result.RakeSat,
		RakeSat:    result.RakeSat,
		History:    hand.Describe(),
		Actions:    t.actions,
	}
	// PotSat has been zeroed by the payout, so the recorded pot is what was
	// actually distributed plus the rake.
	finish.PotSat = result.TotalAwardedSat() + result.RakeSat

	won := make(map[int]int64, len(result.Awards))
	shown := make(map[int]bool, len(result.Shown))
	description := make(map[int]string, len(result.Awards))
	for _, award := range result.Awards {
		won[award.Seat] += award.AmountSat
		description[award.Seat] = award.Description
	}
	for _, index := range result.Shown {
		shown[index] = true
	}
	for i, seat := range hand.Seats {
		if seat == nil || seat.PlayerID == 0 {
			continue
		}
		finish.Results = append(finish.Results, HandResultPlayer{
			UserID: seat.PlayerID, SeatNumber: i,
			CommittedSat: committed[i], WonSat: won[i],
			Shown: shown[i], Result: description[i],
		})
	}

	rake := result.RakeSat
	handID := t.handID

	// Release anybody who asked to leave or timed out, now the hand is done.
	var departures []departure
	for seatNumber := range t.pendingLeave {
		seat := t.seats[seatNumber]
		if seat.PlayerID == 0 {
			continue
		}
		departures = append(departures, departure{
			userID: seat.PlayerID, stack: seat.StackSat, seat: seatNumber,
		})
		t.releaseSeat(seatNumber)
	}
	t.pendingLeave = make(map[int]bool)

	// Any seat left with nothing must sit out rather than be dealt in with an
	// empty stack.
	for _, seat := range t.seats {
		if seat.Status == SeatActive && seat.StackSat <= 0 {
			seat.Status = SeatSittingOut
		}
		if seat.Status == SeatSittingOut && seat.StackSat > 0 && seat.PlayerID != 0 {
			// A player dealt out because they joined mid-hand can play now.
			seat.Status = SeatActive
		}
	}
	// Anybody who timed out or asked to sit out stops being dealt in, but
	// keeps their seat and their chips.
	for seatNumber := range t.pendingSitOut {
		if seat := t.seats[seatNumber]; seat.PlayerID != 0 {
			seat.Status = SeatSittingOut
		}
	}
	t.pendingSitOut = make(map[int]bool)
	// Persist every stack now the hand is settled, so a restart brings the
	// table back exactly as it stands rather than losing the chips.
	stacks := make(map[int]int64, len(t.seats))
	for i, seat := range t.seats {
		if seat.PlayerID != 0 {
			stacks[i] = seat.StackSat
		}
	}
	t.notify()
	t.mu.Unlock()

	for seatNumber, stack := range stacks {
		if err := t.seatStore.UpdateStack(ctx, t.Config.ID, seatNumber, stack); err != nil {
			return err
		}
	}
	for _, gone := range departures {
		if err := t.seatStore.Vacate(ctx, t.Config.ID, gone.seat); err != nil {
			return err
		}
	}

	if rake > 0 {
		if err := t.bank.TakeRake(ctx, t.Config.ID, handID, rake); err != nil {
			return err
		}
	}
	for _, gone := range departures {
		if gone.stack > 0 {
			if err := t.bank.CashOut(ctx, gone.userID, t.Config.ID, gone.stack); err != nil {
				return err
			}
		}
	}
	return t.recorder.FinishHand(ctx, finish)
}

// departure is a seat released at the end of a hand.
type departure struct {
	userID int64
	stack  int64
	seat   int
}

// HandInProgress reports whether a hand is running.
func (t *Table) HandInProgress() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hand != nil && !t.hand.Complete()
}

// SeatedUsers lists the user ids at the table.
func (t *Table) SeatedUsers() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]int64, 0, len(t.seatUsers))
	for userID := range t.seatUsers {
		out = append(out, userID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// StackOf returns a seated player's current stack.
func (t *Table) StackOf(userID int64) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	seatNumber, seated := t.seatUsers[userID]
	if !seated {
		return 0, false
	}
	return t.seats[seatNumber].StackSat, true
}

// TotalChipsSat is the sum of every stack plus whatever is in the middle. It
// must equal the table's ledger balance; the wallet checks that.
func (t *Table) TotalChipsSat() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var total int64
	for _, seat := range t.seats {
		total += seat.StackSat + seat.TotalCommittedSat
	}
	return total
}
