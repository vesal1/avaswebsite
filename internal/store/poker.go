package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PokerTable is a configured cash table.
type PokerTable struct {
	ID            int64
	Name          string
	Variant       string
	SmallBlindSat int64
	BigBlindSat   int64
	AnteSat       int64
	MinBuyInSat   int64
	MaxBuyInSat   int64
	MaxSeats      int
	RakeBps       int64
	RakeCapSat    int64
	NoFlopNoDrop  bool
	ActionSeconds int
	VideoEnabled  bool
	Active        bool
	CreatedAt     time.Time
}

// PokerHandSummary is a row in a hand history listing.
type PokerHandSummary struct {
	ID         int64
	TableID    int64
	HandNumber int64
	ButtonSeat int
	Commitment string
	ClientSeed string
	ServerSeed string
	Board      string
	PotSat     int64
	RakeSat    int64
	Stage      string
	StartedAt  time.Time
	FinishedAt time.Time
	History    string
}

// Verifiable reports whether the seed has been published, which is what lets a
// player check the deck for themselves.
func (h PokerHandSummary) Verifiable() bool { return h.ServerSeed != "" }

// PokerHandPlayer is one seat's record within a hand.
type PokerHandPlayer struct {
	HandID        int64
	UserID        int64
	SeatNumber    int
	StartingStack int64
	CommittedSat  int64
	WonSat        int64
	HoleCards     string
	Shown         bool
	Result        string
}

// CreatePokerTable adds a table.
func (s *Store) CreatePokerTable(ctx context.Context, table PokerTable) (int64, error) {
	noFlop, video, active := 0, 0, 0
	if table.NoFlopNoDrop {
		noFlop = 1
	}
	if table.VideoEnabled {
		video = 1
	}
	if table.Active {
		active = 1
	}
	return s.db.InsertID(ctx,
		`INSERT INTO poker_tables (name, variant, small_blind_sat, big_blind_sat, ante_sat,
		                           min_buy_in_sat, max_buy_in_sat, max_seats, rake_bps,
		                           rake_cap_sat, no_flop_no_drop, action_seconds,
		                           video_enabled, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		table.Name, "holdem", table.SmallBlindSat, table.BigBlindSat, table.AnteSat,
		table.MinBuyInSat, table.MaxBuyInSat, table.MaxSeats, table.RakeBps,
		table.RakeCapSat, noFlop, table.ActionSeconds, video, active, Timestamp(s.Now()))
}

const pokerTableColumns = `id, name, variant, small_blind_sat, big_blind_sat, ante_sat,
                           min_buy_in_sat, max_buy_in_sat, max_seats, rake_bps, rake_cap_sat,
                           no_flop_no_drop, action_seconds, video_enabled, active, created_at`

func scanPokerTable(row scanner) (PokerTable, error) {
	var table PokerTable
	var noFlop, video, active int
	var createdAt string
	err := row.Scan(&table.ID, &table.Name, &table.Variant, &table.SmallBlindSat,
		&table.BigBlindSat, &table.AnteSat, &table.MinBuyInSat, &table.MaxBuyInSat,
		&table.MaxSeats, &table.RakeBps, &table.RakeCapSat, &noFlop, &table.ActionSeconds,
		&video, &active, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PokerTable{}, ErrNotFound
	}
	if err != nil {
		return PokerTable{}, fmt.Errorf("store: scan poker table: %w", err)
	}
	table.NoFlopNoDrop, table.VideoEnabled, table.Active = noFlop == 1, video == 1, active == 1
	table.CreatedAt, err = ParseTimestamp(createdAt)
	return table, err
}

// GetPokerTable loads one table.
func (s *Store) GetPokerTable(ctx context.Context, id int64) (PokerTable, error) {
	return scanPokerTable(s.db.QueryRowContext(ctx,
		`SELECT `+pokerTableColumns+` FROM poker_tables WHERE id = ?`, id))
}

// ListPokerTables returns tables for the lobby.
func (s *Store) ListPokerTables(ctx context.Context, onlyActive bool) ([]PokerTable, error) {
	query := `SELECT ` + pokerTableColumns + ` FROM poker_tables`
	if onlyActive {
		query += ` WHERE active = 1`
	}
	query += ` ORDER BY big_blind_sat, id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list poker tables: %w", err)
	}
	defer rows.Close()
	var out []PokerTable
	for rows.Next() {
		table, err := scanPokerTable(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, table)
	}
	return out, rows.Err()
}

// SetPokerTableActive opens or closes a table.
func (s *Store) SetPokerTableActive(ctx context.Context, id int64, active bool) error {
	flag := 0
	if active {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE poker_tables SET active = ? WHERE id = ?`, flag, id)
	return err
}

// ---------------------------------------------------------------------------
// Seats
//
// Seat occupancy is persisted, not just held in memory, because the chips at a
// table are real customer money sitting in the table's ledger account. If a
// restart forgot who was sitting where, that money would be stranded: present
// in the ledger, attached to nobody.
// ---------------------------------------------------------------------------

// LivePokerSeat is an occupied seat.
type LivePokerSeat struct {
	TableID     int64
	SeatNumber  int
	UserID      int64
	StackSat    int64
	Status      string
	ClientSeed  string
	DisplayName string
}

// OccupyPokerSeat records a player taking a seat.
func (s *Store) OccupyPokerSeat(ctx context.Context, tableID int64, seatNumber int, userID, stackSat int64, clientSeed string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO poker_seats (table_id, seat_number, user_id, stack_sat, status, client_seed, joined_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tableID, seatNumber, userID, stackSat, "active", clientSeed, Timestamp(s.Now()))
	if err != nil {
		return fmt.Errorf("store: occupy poker seat: %w", err)
	}
	return nil
}

// UpdatePokerSeatStack writes a seat's stack after a hand.
func (s *Store) UpdatePokerSeatStack(ctx context.Context, tableID int64, seatNumber int, stackSat int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE poker_seats SET stack_sat = ?
		 WHERE table_id = ? AND seat_number = ? AND left_at IS NULL`,
		stackSat, tableID, seatNumber)
	if err != nil {
		return fmt.Errorf("store: update poker seat: %w", err)
	}
	return nil
}

// VacatePokerSeat closes a seat occupancy.
func (s *Store) VacatePokerSeat(ctx context.Context, tableID int64, seatNumber int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE poker_seats SET status = 'left', stack_sat = 0, left_at = ?
		 WHERE table_id = ? AND seat_number = ? AND left_at IS NULL`,
		Timestamp(s.Now()), tableID, seatNumber)
	if err != nil {
		return fmt.Errorf("store: vacate poker seat: %w", err)
	}
	return nil
}

// LivePokerSeats returns the seats still occupied at a table.
func (s *Store) LivePokerSeats(ctx context.Context, tableID int64) ([]LivePokerSeat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.table_id, s.seat_number, s.user_id, s.stack_sat, s.status,
		        s.client_seed, u.display_name
		 FROM poker_seats s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.table_id = ? AND s.left_at IS NULL
		 ORDER BY s.seat_number`, tableID)
	if err != nil {
		return nil, fmt.Errorf("store: live poker seats: %w", err)
	}
	defer rows.Close()
	var out []LivePokerSeat
	for rows.Next() {
		var seat LivePokerSeat
		if err := rows.Scan(&seat.TableID, &seat.SeatNumber, &seat.UserID,
			&seat.StackSat, &seat.Status, &seat.ClientSeed, &seat.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Hands
// ---------------------------------------------------------------------------

// StartPokerHand records a hand and its deck commitment before the deal.
func (s *Store) StartPokerHand(ctx context.Context, hand PokerHandSummary, players []PokerHandPlayer) (int64, error) {
	var handID int64
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		handID, err = tx.InsertID(ctx,
			`INSERT INTO poker_hands (table_id, hand_number, button_seat, commitment,
			                          client_seed, stage, started_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			hand.TableID, hand.HandNumber, hand.ButtonSeat, hand.Commitment,
			hand.ClientSeed, "pre-flop", Timestamp(s.Now()))
		if err != nil {
			return fmt.Errorf("store: start poker hand: %w", err)
		}
		for _, player := range players {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO poker_hand_players (hand_id, user_id, seat_number,
				                                 starting_stack_sat, hole_cards)
				 VALUES (?, ?, ?, ?, ?)`,
				handID, player.UserID, player.SeatNumber,
				player.StartingStack, player.HoleCards); err != nil {
				return fmt.Errorf("store: record hand player: %w", err)
			}
		}
		return nil
	})
	return handID, err
}

// FinishPokerHand writes the result, including the revealed seed that makes
// the hand checkable.
func (s *Store) FinishPokerHand(ctx context.Context, hand PokerHandSummary,
	players []PokerHandPlayer, actions []PokerAction) error {

	return s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE poker_hands
			 SET server_seed = ?, board = ?, pot_sat = ?, rake_sat = ?, stage = ?,
			     finished_at = ?, history = ?
			 WHERE id = ?`,
			hand.ServerSeed, hand.Board, hand.PotSat, hand.RakeSat, "complete",
			Timestamp(s.Now()), hand.History, hand.ID); err != nil {
			return fmt.Errorf("store: finish poker hand: %w", err)
		}
		for _, player := range players {
			shown := 0
			if player.Shown {
				shown = 1
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE poker_hand_players
				 SET committed_sat = ?, won_sat = ?, shown = ?, result = ?
				 WHERE hand_id = ? AND seat_number = ?`,
				player.CommittedSat, player.WonSat, shown, player.Result,
				hand.ID, player.SeatNumber); err != nil {
				return fmt.Errorf("store: record hand result: %w", err)
			}
		}
		for _, action := range actions {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO poker_actions (hand_id, sequence, seat_number, stage, action, amount_sat, at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				hand.ID, action.Sequence, action.SeatNumber, action.Stage,
				action.Action, action.AmountSat, Timestamp(s.Now())); err != nil {
				return fmt.Errorf("store: record action: %w", err)
			}
		}
		return nil
	})
}

// PokerAction is one recorded decision.
type PokerAction struct {
	Sequence   int
	SeatNumber int
	Stage      string
	Action     string
	AmountSat  int64
}

const pokerHandColumns = `id, table_id, hand_number, button_seat, commitment, client_seed,
                          server_seed, board, pot_sat, rake_sat, stage, started_at,
                          COALESCE(finished_at, ''), history`

func scanPokerHand(row scanner) (PokerHandSummary, error) {
	var hand PokerHandSummary
	var startedAt, finishedAt string
	err := row.Scan(&hand.ID, &hand.TableID, &hand.HandNumber, &hand.ButtonSeat,
		&hand.Commitment, &hand.ClientSeed, &hand.ServerSeed, &hand.Board,
		&hand.PotSat, &hand.RakeSat, &hand.Stage, &startedAt, &finishedAt, &hand.History)
	if errors.Is(err, sql.ErrNoRows) {
		return PokerHandSummary{}, ErrNotFound
	}
	if err != nil {
		return PokerHandSummary{}, fmt.Errorf("store: scan poker hand: %w", err)
	}
	if hand.StartedAt, err = ParseTimestamp(startedAt); err != nil {
		return PokerHandSummary{}, err
	}
	hand.FinishedAt, err = ParseTimestamp(finishedAt)
	return hand, err
}

// GetPokerHand loads a hand for the verification page.
func (s *Store) GetPokerHand(ctx context.Context, id int64) (PokerHandSummary, error) {
	return scanPokerHand(s.db.QueryRowContext(ctx,
		`SELECT `+pokerHandColumns+` FROM poker_hands WHERE id = ?`, id))
}

// PokerHandsForTable lists recent hands at a table.
func (s *Store) PokerHandsForTable(ctx context.Context, tableID int64, limit int) ([]PokerHandSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pokerHandColumns+` FROM poker_hands
		 WHERE table_id = ? ORDER BY hand_number DESC LIMIT ?`, tableID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: poker hands: %w", err)
	}
	defer rows.Close()
	var out []PokerHandSummary
	for rows.Next() {
		hand, err := scanPokerHand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hand)
	}
	return out, rows.Err()
}

// PokerHandsForUser lists a player's recent hands.
func (s *Store) PokerHandsForUser(ctx context.Context, userID int64, limit int) ([]PokerHandSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+strings.ReplaceAll(pokerHandColumns, "id,", "h.id,")+`
		 FROM poker_hands h
		 JOIN poker_hand_players p ON p.hand_id = h.id
		 WHERE p.user_id = ? ORDER BY h.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: poker hands for user: %w", err)
	}
	defer rows.Close()
	var out []PokerHandSummary
	for rows.Next() {
		hand, err := scanPokerHand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hand)
	}
	return out, rows.Err()
}

// PokerHandPlayers returns the seats in a hand.
//
// Hole cards are returned only for seats that were shown at showdown, or for
// the viewer's own seat. A folded hand's cards are nobody else's business,
// including after the fact.
func (s *Store) PokerHandPlayers(ctx context.Context, handID, viewerID int64) ([]PokerHandPlayer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hand_id, user_id, seat_number, starting_stack_sat, committed_sat,
		        won_sat, hole_cards, shown, result
		 FROM poker_hand_players WHERE hand_id = ? ORDER BY seat_number`, handID)
	if err != nil {
		return nil, fmt.Errorf("store: poker hand players: %w", err)
	}
	defer rows.Close()

	var out []PokerHandPlayer
	for rows.Next() {
		var player PokerHandPlayer
		var shown int
		if err := rows.Scan(&player.HandID, &player.UserID, &player.SeatNumber,
			&player.StartingStack, &player.CommittedSat, &player.WonSat,
			&player.HoleCards, &shown, &player.Result); err != nil {
			return nil, err
		}
		player.Shown = shown == 1
		if !player.Shown && player.UserID != viewerID {
			player.HoleCards = ""
		}
		out = append(out, player)
	}
	return out, rows.Err()
}

// PokerActionsForHand returns the decisions in order.
func (s *Store) PokerActionsForHand(ctx context.Context, handID int64) ([]PokerAction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sequence, seat_number, stage, action, amount_sat
		 FROM poker_actions WHERE hand_id = ? ORDER BY sequence`, handID)
	if err != nil {
		return nil, fmt.Errorf("store: poker actions: %w", err)
	}
	defer rows.Close()
	var out []PokerAction
	for rows.Next() {
		var action PokerAction
		if err := rows.Scan(&action.Sequence, &action.SeatNumber, &action.Stage,
			&action.Action, &action.AmountSat); err != nil {
			return nil, err
		}
		out = append(out, action)
	}
	return out, rows.Err()
}

// PokerTableBalanceSat is the ledger balance held for a table. It must equal
// the chips in play at that table.
func (s *Store) PokerTableBalanceSat(ctx context.Context, tableID int64) (int64, error) {
	var balance sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(e.amount_sat), 0)
		 FROM ledger_entries e
		 JOIN ledger_txns t ON t.id = e.txn_id
		 WHERE e.account = ? AND t.ref_type = 'poker_table' AND t.ref_id = ?`,
		AccountPokerTable, tableID).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("store: poker table balance: %w", err)
	}
	return derefInt64(balance), nil
}
