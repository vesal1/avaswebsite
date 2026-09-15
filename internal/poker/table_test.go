package poker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeBank records money crossing the table boundary.
type fakeBank struct {
	mu       sync.Mutex
	accounts map[int64]int64
	table    int64
	rake     int64
	fail     bool
}

func newFakeBank() *fakeBank {
	return &fakeBank{accounts: make(map[int64]int64)}
}

func (b *fakeBank) BuyIn(ctx context.Context, userID, tableID, amountSat int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return errors.New("bank refused")
	}
	b.accounts[userID] -= amountSat
	b.table += amountSat
	return nil
}

func (b *fakeBank) CashOut(ctx context.Context, userID, tableID, amountSat int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.accounts[userID] += amountSat
	b.table -= amountSat
	return nil
}

func (b *fakeBank) TakeRake(ctx context.Context, tableID, handID, amountSat int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.table -= amountSat
	b.rake += amountSat
	return nil
}

// fakeSeats records seat occupancy the way the database would.
type fakeSeats struct {
	mu      sync.Mutex
	stacks  map[int]int64
	vacated map[int]bool
}

func newFakeSeats() *fakeSeats {
	return &fakeSeats{stacks: make(map[int]int64), vacated: make(map[int]bool)}
}

func (f *fakeSeats) Occupy(ctx context.Context, tableID int64, seatNumber int, userID, stackSat int64, clientSeed string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stacks[seatNumber] = stackSat
	delete(f.vacated, seatNumber)
	return nil
}

func (f *fakeSeats) UpdateStack(ctx context.Context, tableID int64, seatNumber int, stackSat int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stacks[seatNumber] = stackSat
	return nil
}

func (f *fakeSeats) Vacate(ctx context.Context, tableID int64, seatNumber int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stacks, seatNumber)
	f.vacated[seatNumber] = true
	return nil
}

type fakeRecorder struct {
	mu       sync.Mutex
	starts   []HandStart
	finishes []HandFinish
	nextID   int64
}

func (r *fakeRecorder) StartHand(ctx context.Context, record HandStart) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.starts = append(r.starts, record)
	return r.nextID, nil
}

func (r *fakeRecorder) FinishHand(ctx context.Context, record HandFinish) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishes = append(r.finishes, record)
	return nil
}

func newTestTable(t *testing.T) (*Table, *fakeBank, *fakeRecorder) {
	t.Helper()
	table, bank, recorder, _ := newTestTableWithSeats(t)
	return table, bank, recorder
}

func newTestTableWithSeats(t *testing.T) (*Table, *fakeBank, *fakeRecorder, *fakeSeats) {
	t.Helper()
	bank, recorder, seats := newFakeBank(), &fakeRecorder{}, newFakeSeats()
	table := NewTable(TableConfig{
		ID: 1, Name: "Test", Rules: DefaultRules(200), MaxSeats: 6,
		MinBuyInSat: 4000, MaxBuyInSat: 40000,
		ActionTimeout: 30 * time.Second, VideoEnabled: true,
	}, bank, recorder, seats)
	return table, bank, recorder, seats
}

// TestSeatsSurviveARestart is the reason seat occupancy is persisted at all.
// Chips at a table are customer money held in the table's ledger account; a
// restart that forgot the seats would strand it there, present in the ledger
// and attached to nobody.
func TestSeatsSurviveARestart(t *testing.T) {
	table, bank, _, seats := newTestTableWithSeats(t)
	ctx := context.Background()

	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.Sit(ctx, 200, "Ben", 3, 15000); err != nil {
		t.Fatal(err)
	}
	if seats.stacks[0] != 20000 || seats.stacks[3] != 15000 {
		t.Fatalf("seats not persisted: %v", seats.stacks)
	}

	// A new process: fresh table, same persisted seats and the same money
	// still sitting in the table's ledger account.
	restarted := NewTable(table.Config, bank, &fakeRecorder{}, seats)
	restarted.Restore([]RestoredSeat{
		{SeatNumber: 0, UserID: 100, Name: "Ana", StackSat: seats.stacks[0]},
		{SeatNumber: 3, UserID: 200, Name: "Ben", StackSat: seats.stacks[3]},
	})

	if stack, seated := restarted.StackOf(100); !seated || stack != 20000 {
		t.Errorf("Ana came back with %d, seated=%v", stack, seated)
	}
	if stack, seated := restarted.StackOf(200); !seated || stack != 15000 {
		t.Errorf("Ben came back with %d, seated=%v", stack, seated)
	}
	if restarted.TotalChipsSat() != bank.table {
		t.Errorf("chips at the table are %d but the ledger holds %d",
			restarted.TotalChipsSat(), bank.table)
	}

	// And they can cash out of the restored table.
	returned, err := restarted.Leave(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if returned != 20000 {
		t.Errorf("cashed out %d, want 20000", returned)
	}
	if !seats.vacated[0] {
		t.Error("the seat was not released in storage")
	}
}

func TestVacatingIsRecordedWhenAPlayerLeaves(t *testing.T) {
	table, _, _, seats := newTestTableWithSeats(t)
	ctx := context.Background()
	if err := table.Sit(ctx, 100, "Ana", 2, 20000); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Leave(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if !seats.vacated[2] {
		t.Error("leaving did not record the seat as vacated")
	}
}

func TestSitAndLeaveMovesMoney(t *testing.T) {
	table, bank, _ := newTestTable(t)
	ctx := context.Background()

	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if bank.accounts[100] != -20000 || bank.table != 20000 {
		t.Errorf("after buy-in account = %d, table = %d", bank.accounts[100], bank.table)
	}
	stack, seated := table.StackOf(100)
	if !seated || stack != 20000 {
		t.Errorf("stack = %d, seated = %v", stack, seated)
	}

	returned, err := table.Leave(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if returned != 20000 {
		t.Errorf("returned %d, want 20000", returned)
	}
	if bank.accounts[100] != 0 || bank.table != 0 {
		t.Errorf("after cash-out account = %d, table = %d", bank.accounts[100], bank.table)
	}
}

func TestSeatIsReleasedWhenTheBankRefuses(t *testing.T) {
	// If the money does not move, the seat must not be held. Otherwise a
	// failed buy-in silently blocks a seat nobody is sitting in.
	table, bank, _ := newTestTable(t)
	bank.fail = true

	if err := table.Sit(context.Background(), 100, "Ana", 0, 20000); err == nil {
		t.Fatal("expected the buy-in to fail")
	}
	if _, seated := table.StackOf(100); seated {
		t.Error("the seat was kept after the buy-in failed")
	}
	// With the bank working again, the seat must be available to somebody else.
	bank.fail = false
	if err := table.Sit(context.Background(), 200, "Ben", 0, 20000); err != nil {
		t.Errorf("seat 0 was not released: %v", err)
	}
}

func TestBuyInRangeIsEnforced(t *testing.T) {
	table, _, _ := newTestTable(t)
	ctx := context.Background()
	if err := table.Sit(ctx, 100, "Ana", 0, 100); !errors.Is(err, ErrBuyInRange) {
		t.Errorf("short buy-in gave %v, want ErrBuyInRange", err)
	}
	if err := table.Sit(ctx, 100, "Ana", 0, 999999); !errors.Is(err, ErrBuyInRange) {
		t.Errorf("large buy-in gave %v, want ErrBuyInRange", err)
	}
}

func TestCannotTakeAnOccupiedSeat(t *testing.T) {
	table, _, _ := newTestTable(t)
	ctx := context.Background()
	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.Sit(ctx, 200, "Ben", 0, 20000); !errors.Is(err, ErrSeatTaken) {
		t.Errorf("got %v, want ErrSeatTaken", err)
	}
	if err := table.Sit(ctx, 100, "Ana", 1, 20000); !errors.Is(err, ErrAlreadySeated) {
		t.Errorf("sitting twice gave %v, want ErrAlreadySeated", err)
	}
}

func TestHandRunsAndMoneyIsConserved(t *testing.T) {
	table, bank, recorder := newTestTable(t)
	ctx := context.Background()

	for i, spec := range []struct {
		id   int64
		name string
	}{{100, "Ana"}, {200, "Ben"}, {300, "Cleo"}} {
		if err := table.Sit(ctx, spec.id, spec.name, i, 20000); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.StartHand(ctx); err != nil {
		t.Fatal(err)
	}
	if !table.HandInProgress() {
		t.Fatal("no hand started")
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("recorded %d hand starts, want 1", len(recorder.starts))
	}
	// The commitment must be on record before anybody could know the deck.
	if recorder.starts[0].Commitment == "" {
		t.Error("the hand was recorded without a deck commitment")
	}

	// Everybody folds to the big blind.
	for guard := 0; table.HandInProgress() && guard < 30; guard++ {
		view := table.View(0)
		seat := -1
		for _, s := range view.Seats {
			if s.IsTurn {
				seat = s.Number
			}
		}
		if seat < 0 {
			break
		}
		userID := table.seats[seat].PlayerID
		if err := table.Act(ctx, userID, ActionFold, 0); err != nil {
			t.Fatalf("fold: %v", err)
		}
	}

	if len(recorder.finishes) != 1 {
		t.Fatalf("recorded %d finishes, want 1", len(recorder.finishes))
	}
	finish := recorder.finishes[0]
	if finish.ServerSeed == "" {
		t.Error("the hand finished without revealing the server seed")
	}
	// The revealed seed must verify against the published commitment.
	if _, err := Verify(recorder.starts[0].Commitment, finish.ServerSeed,
		recorder.starts[0].ClientSeed, 1); err != nil {
		t.Errorf("the published hand does not verify: %v", err)
	}

	// Chips at the table plus rake taken must equal what was bought in.
	if total := table.TotalChipsSat(); total+bank.rake != 60000 {
		t.Errorf("chips %d plus rake %d, want 60000", total, bank.rake)
	}
	if bank.table != table.TotalChipsSat() {
		t.Errorf("the table's ledger balance is %d but the chips total %d",
			bank.table, table.TotalChipsSat())
	}
}

func TestHandHistoryRecordsWhatEachSeatPutIn(t *testing.T) {
	// The payout empties the pot, so committed amounts have to be captured
	// before it. Reading them afterwards records every hand as nobody betting.
	table, _, recorder := newTestTable(t)
	ctx := context.Background()
	for i, spec := range []struct {
		id   int64
		name string
	}{{100, "Ana"}, {200, "Ben"}, {300, "Cleo"}} {
		if err := table.Sit(ctx, spec.id, spec.name, i, 20000); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.StartHand(ctx); err != nil {
		t.Fatal(err)
	}
	for guard := 0; table.HandInProgress() && guard < 40; guard++ {
		view := table.View(0)
		seat := -1
		for _, s := range view.Seats {
			if s.IsTurn {
				seat = s.Number
			}
		}
		if seat < 0 {
			break
		}
		if err := table.Act(ctx, table.seats[seat].PlayerID, ActionFold, 0); err != nil {
			break
		}
	}

	if len(recorder.finishes) != 1 {
		t.Fatalf("recorded %d finishes, want 1", len(recorder.finishes))
	}
	var totalCommitted int64
	for _, player := range recorder.finishes[0].Results {
		totalCommitted += player.CommittedSat
	}
	// The blinds alone are 300, so a total of zero means it was read too late.
	if totalCommitted == 0 {
		t.Error("the hand history records nobody as having put anything in")
	}
	if totalCommitted != recorder.finishes[0].PotSat {
		t.Errorf("seats committed %d but the pot was recorded as %d",
			totalCommitted, recorder.finishes[0].PotSat)
	}
}

func TestViewNeverLeaksAnotherPlayersCards(t *testing.T) {
	// The single most important property of the view layer.
	table, _, _ := newTestTable(t)
	ctx := context.Background()
	for i, spec := range []struct {
		id   int64
		name string
	}{{100, "Ana"}, {200, "Ben"}, {300, "Cleo"}} {
		if err := table.Sit(ctx, spec.id, spec.name, i, 20000); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.StartHand(ctx); err != nil {
		t.Fatal(err)
	}

	view := table.View(100)
	for _, seat := range view.Seats {
		switch {
		case seat.IsYou:
			if len(seat.Cards) != 2 {
				t.Errorf("your own seat shows %d cards, want 2", len(seat.Cards))
			}
		case seat.Occupied:
			if len(seat.Cards) != 0 {
				t.Errorf("seat %d leaked %v to another player", seat.Number, seat.Cards)
			}
			if !seat.HasCards {
				t.Errorf("seat %d should show face-down cards", seat.Number)
			}
		}
	}

	// A spectator sees nobody's cards.
	spectator := table.View(999)
	if spectator.YourSeat != -1 {
		t.Error("a spectator was given a seat")
	}
	for _, seat := range spectator.Seats {
		if len(seat.Cards) != 0 {
			t.Errorf("a spectator saw seat %d's cards", seat.Number)
		}
	}
	if len(spectator.LegalActions) != 0 {
		t.Error("a spectator was offered actions")
	}
}

func TestLeavingDuringAHandIsDeferred(t *testing.T) {
	// Yanking a live stack out mid-hand would be a way to take back a losing
	// bet, so the stand-up waits for the hand to finish.
	table, bank, _ := newTestTable(t)
	ctx := context.Background()
	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.Sit(ctx, 200, "Ben", 1, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.StartHand(ctx); err != nil {
		t.Fatal(err)
	}

	returned, err := table.Leave(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if returned != 0 {
		t.Errorf("leaving mid-hand returned %d immediately, want 0", returned)
	}
	if bank.table != 40000 {
		t.Errorf("chips left the table mid-hand: %d", bank.table)
	}

	// Play the hand out; the departure should then take effect.
	for guard := 0; table.HandInProgress() && guard < 30; guard++ {
		view := table.View(0)
		seat := -1
		for _, s := range view.Seats {
			if s.IsTurn {
				seat = s.Number
			}
		}
		if seat < 0 {
			break
		}
		if err := table.Act(ctx, table.seats[seat].PlayerID, ActionFold, 0); err != nil {
			break
		}
	}
	if _, seated := table.StackOf(100); seated {
		t.Error("the player who asked to leave is still seated after the hand")
	}
}

func TestTimeoutFoldsAndSitsThePlayerOut(t *testing.T) {
	table, _, _ := newTestTable(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	table.WithClock(func() time.Time { return now })
	ctx := context.Background()

	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.Sit(ctx, 200, "Ben", 1, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.StartHand(ctx); err != nil {
		t.Fatal(err)
	}

	// Not yet due.
	if err := table.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !table.HandInProgress() {
		t.Fatal("the hand ended before anybody timed out")
	}

	// Whoever is on the clock is the one who will time out. Heads-up the
	// button posts the small blind and acts first, so it is not necessarily
	// the seat that sat down first.
	var onTheClock int64
	for _, seat := range table.View(0).Seats {
		if seat.IsTurn {
			onTheClock = seat.PlayerID
		}
	}
	if onTheClock == 0 {
		t.Fatal("nobody is on the clock")
	}

	now = now.Add(2 * time.Minute)
	if err := table.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Heads-up, the player on the clock folding pre-flop ends the hand.
	if table.HandInProgress() {
		t.Error("the hand should have ended when the player timed out")
	}
	// They keep their seat and their chips: losing both over one missed
	// decision would be harsher than the problem needs.
	stack, seated := table.StackOf(onTheClock)
	if !seated {
		t.Fatal("a timed-out player lost their seat")
	}
	if stack <= 0 {
		t.Errorf("a timed-out player has %d chips left", stack)
	}
	// But they are sat out, so they stop bleeding blinds while away.
	for _, seat := range table.View(0).Seats {
		if seat.PlayerID == onTheClock && seat.Status == SeatActive {
			t.Error("a timed-out player is still being dealt in")
		}
	}
}

func TestSeedCannotChangeMidHand(t *testing.T) {
	table, _, _ := newTestTable(t)
	ctx := context.Background()
	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.Sit(ctx, 200, "Ben", 1, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.SetClientSeed(100, "my-seed"); err != nil {
		t.Fatal(err)
	}
	if err := table.StartHand(ctx); err != nil {
		t.Fatal(err)
	}
	if err := table.SetClientSeed(100, "changed"); !errors.Is(err, ErrHandInProgress) {
		t.Errorf("got %v, want ErrHandInProgress: a seed must not change after the deck is committed", err)
	}
}

func TestVideoConsentIsOffByDefaultAndClearsOnLeaving(t *testing.T) {
	table, _, _ := newTestTable(t)
	ctx := context.Background()
	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}

	view := table.View(100)
	if view.Seats[0].VideoConsent {
		t.Error("sitting down turned the camera on: consent must be opt-in")
	}
	if len(view.VideoPeers) != 0 {
		t.Error("a seat with no consent was published as a video peer")
	}

	if err := table.SetVideoConsent(100, true); err != nil {
		t.Fatal(err)
	}
	if view = table.View(100); !view.Seats[0].VideoConsent || len(view.VideoPeers) != 1 {
		t.Error("consent was not recorded")
	}

	if _, err := table.Leave(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if table.View(100).Seats[0].VideoConsent {
		t.Error("consent survived leaving the table: it must not be remembered")
	}
}

func TestSubscribersAreNotified(t *testing.T) {
	table, _, _ := newTestTable(t)
	updates, stop := table.Subscribe()
	defer stop()

	if err := table.Sit(context.Background(), 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("no notification after a player sat down")
	}
}

func TestTableNeedsTwoPlayersToDeal(t *testing.T) {
	table, _, recorder := newTestTable(t)
	ctx := context.Background()
	if err := table.Sit(ctx, 100, "Ana", 0, 20000); err != nil {
		t.Fatal(err)
	}
	if err := table.StartHand(ctx); err != nil {
		t.Fatal(err)
	}
	if table.HandInProgress() {
		t.Error("a hand started with one player")
	}
	if len(recorder.starts) != 0 {
		t.Error("a hand was recorded with one player")
	}
}
