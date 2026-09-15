// Package pokerhouse runs the poker room: it holds the live tables, moves
// money between player accounts and the tables, and writes hand histories.
//
// It is the seam between the pure poker engine, which knows nothing about
// databases or money, and the rest of the sportsbook.
package pokerhouse

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/vesal1/avaswebsite/internal/bonus"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/poker"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Errors callers branch on.
var (
	ErrTableClosed       = errors.New("pokerhouse: that table is not open")
	ErrInsufficientFunds = errors.New("pokerhouse: not enough in your balance for that buy-in")
)

// House holds every live table.
type House struct {
	store      *store.Store
	cfg        *config.Config
	compliance *compliance.Service
	bonus      *bonus.Service

	mu     sync.RWMutex
	tables map[int64]*poker.Table

	now func() time.Time
}

// New builds the poker room.
func New(s *store.Store, cfg *config.Config, comp *compliance.Service, promotions *bonus.Service) *House {
	return &House{
		store: s, cfg: cfg, compliance: comp, bonus: promotions,
		tables: make(map[int64]*poker.Table),
		now:    func() time.Time { return s.Now() },
	}
}

// Load brings every active table into memory.
//
// Tables live in memory because a hand is a short-lived, high-frequency state
// machine and pushing every check and fold through a database would add
// latency for no benefit. Money and hand histories are persisted; the
// in-flight hand is not, which means a restart abandons any hand in progress.
// That is deliberate and is documented in the README: chips are only ever at
// risk inside a hand, and an abandoned hand refunds through the table balance
// reconciliation rather than paying out a guess.
func (h *House) Load(ctx context.Context) error {
	tables, err := h.store.ListPokerTables(ctx, true)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, record := range tables {
		if _, exists := h.tables[record.ID]; exists {
			continue
		}
		table := h.build(record)

		// Bring the seats back. Their chips are already in the table's ledger
		// account, so without this they would be stranded there: real money
		// present in the ledger and attached to nobody.
		seats, err := h.store.LivePokerSeats(ctx, record.ID)
		if err != nil {
			return err
		}
		restored := make([]poker.RestoredSeat, 0, len(seats))
		for _, seat := range seats {
			restored = append(restored, poker.RestoredSeat{
				SeatNumber: seat.SeatNumber, UserID: seat.UserID,
				Name: seat.DisplayName, StackSat: seat.StackSat,
				ClientSeed: seat.ClientSeed,
				SittingOut: seat.Status != "active",
			})
		}
		if len(restored) > 0 {
			table.Restore(restored)
		}
		h.tables[record.ID] = table
	}
	return nil
}

func (h *House) build(record store.PokerTable) *poker.Table {
	rules := poker.Rules{
		SmallBlindSat: record.SmallBlindSat,
		BigBlindSat:   record.BigBlindSat,
		AnteSat:       record.AnteSat,
		RakeBps:       record.RakeBps,
		RakeCapSat:    record.RakeCapSat,
		NoFlopNoDrop:  record.NoFlopNoDrop,
	}
	if rules.RakeCapSat == 0 {
		rules.RakeCapSat = record.BigBlindSat * 3
	}
	config := poker.TableConfig{
		ID: record.ID, Name: record.Name, Rules: rules,
		MaxSeats:      record.MaxSeats,
		MinBuyInSat:   record.MinBuyInSat,
		MaxBuyInSat:   record.MaxBuyInSat,
		ActionTimeout: time.Duration(record.ActionSeconds) * time.Second,
		VideoEnabled:  record.VideoEnabled,
	}
	return poker.NewTable(config,
		&ledgerBank{store: h.store},
		&storeRecorder{store: h.store},
		&seatStore{store: h.store},
	).WithClock(h.now)
}

// Table returns a live table.
func (h *House) Table(id int64) (*poker.Table, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	table, ok := h.tables[id]
	return table, ok
}

// Lobby lists the open tables, cheapest blinds first.
func (h *House) Lobby() []poker.LobbyView {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]poker.LobbyView, 0, len(h.tables))
	for _, table := range h.tables {
		out = append(out, table.Lobby())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BigBlindSat != out[j].BigBlindSat {
			return out[i].BigBlindSat < out[j].BigBlindSat
		}
		return out[i].TableID < out[j].TableID
	})
	return out
}

// SeatedAt reports which table a player is sitting at, if any.
func (h *House) SeatedAt(userID int64) (int64, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, table := range h.tables {
		if _, seated := table.StackOf(userID); seated {
			return id, true
		}
	}
	return 0, false
}

// Sit seats a player, after checking they are allowed to play and can afford
// the buy-in.
func (h *House) Sit(ctx context.Context, user store.User, tableID int64, seatNumber int, buyInSat int64) error {
	table, ok := h.Table(tableID)
	if !ok {
		return ErrTableClosed
	}
	// Poker is gambling, so it runs through exactly the same gates as a bet:
	// jurisdiction, age, identity, self-exclusion and the player's own limits.
	if err := h.compliance.CheckCanBet(ctx, user, buyInSat); err != nil {
		return err
	}

	cash, err := h.store.BalanceSat(ctx, user.ID)
	if err != nil {
		return err
	}
	bonusBalance, err := h.store.BonusBalanceSat(ctx, user.ID)
	if err != nil {
		return err
	}
	if cash+bonusBalance < buyInSat {
		return fmt.Errorf("%w: you have %s BTC and the buy-in is %s BTC",
			ErrInsufficientFunds, money.FormatBTC(cash+bonusBalance), money.FormatBTC(buyInSat))
	}

	return table.Sit(ctx, user.ID, user.DisplayName, seatNumber, buyInSat)
}

// Leave stands a player up.
func (h *House) Leave(ctx context.Context, tableID, userID int64) (int64, error) {
	table, ok := h.Table(tableID)
	if !ok {
		return 0, ErrTableClosed
	}
	return table.Leave(ctx, userID)
}

// Run drives the tables: deals hands when enough players are ready and folds
// anybody who runs out of time.
func (h *House) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.mu.RLock()
			tables := make([]*poker.Table, 0, len(h.tables))
			for _, table := range h.tables {
				tables = append(tables, table)
			}
			h.mu.RUnlock()

			for _, table := range tables {
				if table.HandInProgress() {
					_ = table.Tick(ctx)
					continue
				}
				_ = table.StartHand(ctx)
			}
		}
	}
}

// ReconcileTable compares the chips at a table with its ledger balance. They
// must match exactly; anything else means chips have been created or lost.
func (h *House) ReconcileTable(ctx context.Context, tableID int64) (chips, ledger int64, err error) {
	table, ok := h.Table(tableID)
	if !ok {
		return 0, 0, ErrTableClosed
	}
	ledger, err = h.store.PokerTableBalanceSat(ctx, tableID)
	if err != nil {
		return 0, 0, err
	}
	return table.TotalChipsSat(), ledger, nil
}

// ---------------------------------------------------------------------------
// Bank: money crossing the table boundary
// ---------------------------------------------------------------------------

type ledgerBank struct{ store *store.Store }

// BuyIn moves a player's money from their account into the table.
func (b *ledgerBank) BuyIn(ctx context.Context, userID, tableID, amountSat int64) error {
	return b.store.Tx(ctx, func(tx *store.Tx) error {
		cash, bonusAvailable, err := store.PlayableBalanceTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if cash+bonusAvailable < amountSat {
			return fmt.Errorf("%w: balance is %s BTC, buy-in is %s BTC",
				ErrInsufficientFunds, money.FormatBTC(cash+bonusAvailable), money.FormatBTC(amountSat))
		}
		// Cash first, then bonus, exactly as a sports stake is funded.
		debits, _, err := store.SpendEntries(userID, amountSat, cash, bonusAvailable)
		if err != nil {
			return err
		}
		entries := append(debits, store.Entry{
			Account: store.AccountPokerTable, AmountSat: amountSat,
		})
		_, err = store.PostTxn(ctx, tx, store.Timestamp(b.store.Now()), store.TxnSpec{
			Kind:    store.TxnPokerBuyIn,
			Memo:    fmt.Sprintf("Buy-in at table %d", tableID),
			RefType: "poker_table",
			RefID:   tableID,
			Entries: entries,
		})
		return err
	})
}

// CashOut returns a player's chips to their account.
//
// Chips always come back as cash, never as bonus. Bonus money that has been
// through a poker table has been wagered, and re-locking it would be taking
// back something the player has already earned the use of.
func (b *ledgerBank) CashOut(ctx context.Context, userID, tableID, amountSat int64) error {
	if amountSat <= 0 {
		return nil
	}
	return b.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(b.store.Now()), store.TxnSpec{
			Kind:    store.TxnPokerCashOut,
			Memo:    fmt.Sprintf("Cash out from table %d", tableID),
			RefType: "poker_table",
			RefID:   tableID,
			Entries: []store.Entry{
				{Account: store.AccountPokerTable, AmountSat: -amountSat},
				{Account: store.AccountUserCash, UserID: userID, AmountSat: amountSat},
			},
		})
		return err
	})
}

// TakeRake moves the house's published share out of the table.
func (b *ledgerBank) TakeRake(ctx context.Context, tableID, handID, amountSat int64) error {
	if amountSat <= 0 {
		return nil
	}
	return b.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(b.store.Now()), store.TxnSpec{
			Kind:    store.TxnPokerRake,
			Memo:    fmt.Sprintf("Rake from hand %d", handID),
			RefType: "poker_table",
			RefID:   tableID,
			Entries: []store.Entry{
				{Account: store.AccountPokerTable, AmountSat: -amountSat},
				{Account: store.AccountHouseRake, AmountSat: amountSat},
			},
		})
		return err
	})
}

// ---------------------------------------------------------------------------
// Seats
// ---------------------------------------------------------------------------

type seatStore struct{ store *store.Store }

func (s *seatStore) Occupy(ctx context.Context, tableID int64, seatNumber int, userID, stackSat int64, clientSeed string) error {
	return s.store.OccupyPokerSeat(ctx, tableID, seatNumber, userID, stackSat, clientSeed)
}

func (s *seatStore) UpdateStack(ctx context.Context, tableID int64, seatNumber int, stackSat int64) error {
	return s.store.UpdatePokerSeatStack(ctx, tableID, seatNumber, stackSat)
}

func (s *seatStore) Vacate(ctx context.Context, tableID int64, seatNumber int) error {
	return s.store.VacatePokerSeat(ctx, tableID, seatNumber)
}

// ---------------------------------------------------------------------------
// Recorder: hand histories
// ---------------------------------------------------------------------------

type storeRecorder struct{ store *store.Store }

func (r *storeRecorder) StartHand(ctx context.Context, record poker.HandStart) (int64, error) {
	players := make([]store.PokerHandPlayer, 0, len(record.Players))
	for _, player := range record.Players {
		players = append(players, store.PokerHandPlayer{
			UserID: player.UserID, SeatNumber: player.SeatNumber,
			StartingStack: player.StartingStack, HoleCards: player.HoleCards,
		})
	}
	return r.store.StartPokerHand(ctx, store.PokerHandSummary{
		TableID: record.TableID, HandNumber: record.HandNumber,
		ButtonSeat: record.ButtonSeat, Commitment: record.Commitment,
		ClientSeed: record.ClientSeed,
	}, players)
}

func (r *storeRecorder) FinishHand(ctx context.Context, record poker.HandFinish) error {
	players := make([]store.PokerHandPlayer, 0, len(record.Results))
	for _, player := range record.Results {
		players = append(players, store.PokerHandPlayer{
			UserID: player.UserID, SeatNumber: player.SeatNumber,
			CommittedSat: player.CommittedSat, WonSat: player.WonSat,
			Shown: player.Shown, Result: player.Result,
		})
	}
	actions := make([]store.PokerAction, 0, len(record.Actions))
	for _, action := range record.Actions {
		actions = append(actions, store.PokerAction{
			Sequence: action.Sequence, SeatNumber: action.SeatNumber,
			Stage: action.Stage, Action: action.Action, AmountSat: action.AmountSat,
		})
	}
	return r.store.FinishPokerHand(ctx, store.PokerHandSummary{
		ID: record.HandID, ServerSeed: record.ServerSeed, Board: record.Board,
		PotSat: record.PotSat, RakeSat: record.RakeSat, History: record.History,
	}, players, actions)
}
