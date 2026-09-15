package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Competitions
// ---------------------------------------------------------------------------

// UpsertCompetition returns the id of a competition, creating it if needed.
func (s *Store) UpsertCompetition(ctx context.Context, sportKey, name, region string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("store: a competition needs a name")
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM competitions WHERE sport_key = ? AND name = ?`, sportKey, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: lookup competition: %w", err)
	}
	id, err = s.db.InsertID(ctx,
		`INSERT INTO competitions (sport_key, name, region, created_at) VALUES (?, ?, ?, ?)`,
		sportKey, name, region, Timestamp(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: create competition: %w", err)
	}
	return id, nil
}

// CompetitionsForSport lists a sport's competitions.
func (s *Store) CompetitionsForSport(ctx context.Context, sportKey string) ([]Competition, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, sport_key, name, region FROM competitions WHERE sport_key = ? ORDER BY name`,
		sportKey)
	if err != nil {
		return nil, fmt.Errorf("store: competitions: %w", err)
	}
	defer rows.Close()
	var out []Competition
	for rows.Next() {
		var c Competition
		if err := rows.Scan(&c.ID, &c.SportKey, &c.Name, &c.Region); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// NewEvent describes an event to create.
type NewEvent struct {
	SportKey      string
	CompetitionID int64
	Name          string
	Format        string
	StartsAt      time.Time
	Venue         string
}

// CreateEvent inserts an event and returns its id.
func (s *Store) CreateEvent(ctx context.Context, in NewEvent) (int64, error) {
	id, err := s.db.InsertID(ctx,
		`INSERT INTO events (sport_key, competition_id, name, format, starts_at, venue, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.SportKey, nullInt64(in.CompetitionID), in.Name, in.Format,
		Timestamp(in.StartsAt), in.Venue, Timestamp(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: create event: %w", err)
	}
	return id, nil
}

const eventColumns = `e.id, e.sport_key, COALESCE(e.competition_id, 0), COALESCE(c.name, ''),
                      e.name, e.format, e.starts_at, e.status, e.venue, COALESCE(e.settled_at, '')`

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var startsAt, settledAt string
	err := row.Scan(&e.ID, &e.SportKey, &e.CompetitionID, &e.Competition, &e.Name,
		&e.Format, &startsAt, &e.Status, &e.Venue, &settledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("store: scan event: %w", err)
	}
	if e.StartsAt, err = ParseTimestamp(startsAt); err != nil {
		return Event{}, err
	}
	if e.SettledAt, err = ParseTimestamp(settledAt); err != nil {
		return Event{}, err
	}
	return e, nil
}

// GetEvent loads a single event.
func (s *Store) GetEvent(ctx context.Context, id int64) (Event, error) {
	return scanEvent(s.db.QueryRowContext(ctx,
		`SELECT `+eventColumns+` FROM events e
		 LEFT JOIN competitions c ON c.id = e.competition_id WHERE e.id = ?`, id))
}

// EventFilter narrows an event listing.
type EventFilter struct {
	SportKey      string
	CompetitionID int64
	Statuses      []string
	From          time.Time
	To            time.Time
	Limit         int
}

// ListEvents returns events matching the filter, soonest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events e
	          LEFT JOIN competitions c ON c.id = e.competition_id WHERE 1 = 1`
	var args []any
	if f.SportKey != "" {
		query += " AND e.sport_key = ?"
		args = append(args, f.SportKey)
	}
	if f.CompetitionID != 0 {
		query += " AND e.competition_id = ?"
		args = append(args, f.CompetitionID)
	}
	if len(f.Statuses) > 0 {
		query += " AND e.status IN (" + placeholders(len(f.Statuses)) + ")"
		for _, status := range f.Statuses {
			args = append(args, status)
		}
	}
	if !f.From.IsZero() {
		query += " AND e.starts_at >= ?"
		args = append(args, Timestamp(f.From))
	}
	if !f.To.IsZero() {
		query += " AND e.starts_at <= ?"
		args = append(args, Timestamp(f.To))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	query += " ORDER BY e.starts_at ASC, e.id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// SetEventStatus moves an event through its lifecycle.
func (s *Store) SetEventStatus(ctx context.Context, eventID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE events SET status = ? WHERE id = ?`, status, eventID)
	return err
}

// CountEventsBySport returns how many open events each sport currently has,
// so the sport menu can show counts and hide empty sports.
func (s *Store) CountEventsBySport(ctx context.Context, statuses ...string) (map[string]int, error) {
	if len(statuses) == 0 {
		statuses = []string{EventScheduled, EventLive}
	}
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT sport_key, COUNT(*) FROM events WHERE status IN (`+placeholders(len(statuses))+`)
		 GROUP BY sport_key`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: count events: %w", err)
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return nil, err
		}
		counts[key] = count
	}
	return counts, rows.Err()
}

// ---------------------------------------------------------------------------
// Participants
// ---------------------------------------------------------------------------

// AddParticipant adds a competitor to an event.
func (s *Store) AddParticipant(ctx context.Context, p Participant) (int64, error) {
	id, err := s.db.InsertID(ctx,
		`INSERT INTO participants (event_id, name, short_name, nationality, home_away, sort_order)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.EventID, p.Name, p.ShortName, p.Nationality, homeAwayOrNeutral(p.HomeAway), p.SortOrder)
	if err != nil {
		return 0, fmt.Errorf("store: add participant: %w", err)
	}
	return id, nil
}

func homeAwayOrNeutral(value string) string {
	switch value {
	case "home", "away":
		return value
	default:
		return "neutral"
	}
}

// ParticipantsForEvent lists an event's competitors in display order.
func (s *Store) ParticipantsForEvent(ctx context.Context, eventID int64) ([]Participant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, event_id, name, short_name, nationality, home_away, sort_order,
		        score, COALESCE(finish_position, 0), withdrawn
		 FROM participants WHERE event_id = ? ORDER BY sort_order, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("store: participants: %w", err)
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		var p Participant
		var score sql.NullInt64
		var withdrawn int
		if err := rows.Scan(&p.ID, &p.EventID, &p.Name, &p.ShortName, &p.Nationality,
			&p.HomeAway, &p.SortOrder, &score, &p.FinishPosition, &withdrawn); err != nil {
			return nil, err
		}
		p.Score, p.HasScore = score.Int64, score.Valid
		p.Withdrawn = withdrawn == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordResult writes a competitor's final score and finishing position.
func (s *Store) RecordResult(ctx context.Context, tx *Tx, participantID int64, score sql.NullInt64, position int, withdrawn bool) error {
	flag := 0
	if withdrawn {
		flag = 1
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE participants SET score = ?, finish_position = ?, withdrawn = ? WHERE id = ?`,
		score, nullInt64(int64(position)), flag, participantID)
	return err
}

// ---------------------------------------------------------------------------
// Markets and selections
// ---------------------------------------------------------------------------

// NewMarket describes a market to create.
type NewMarket struct {
	EventID   int64
	Kind      string
	Title     string
	LineX100  int64
	HasLine   bool
	Period    string
	SubjectID int64
	MarginBps int64
}

// CreateMarket inserts a market and returns its id.
func (s *Store) CreateMarket(ctx context.Context, in NewMarket) (int64, error) {
	var line any
	if in.HasLine {
		line = in.LineX100
	}
	margin := in.MarginBps
	if margin <= 0 {
		margin = 500
	}
	id, err := s.db.InsertID(ctx,
		`INSERT INTO markets (event_id, kind, title, line_x100, period, subject_participant_id,
		                      margin_bps, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		in.EventID, in.Kind, in.Title, line, in.Period, nullInt64(in.SubjectID),
		margin, Timestamp(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: create market: %w", err)
	}
	return id, nil
}

// AddSelection inserts a selection into a market.
func (s *Store) AddSelection(ctx context.Context, sel Selection) (int64, error) {
	id, err := s.db.InsertID(ctx,
		`INSERT INTO selections (market_id, participant_id, name, outcome_code, odds_milli,
		                         sort_order, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sel.MarketID, nullInt64(sel.ParticipantID), sel.Name, sel.OutcomeCode,
		sel.OddsMilli, sel.SortOrder, Timestamp(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: add selection: %w", err)
	}
	// The opening price is the first row of the market's price history.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO odds_history (selection_id, odds_milli, at, reason) VALUES (?, ?, ?, 'opening')`,
		id, sel.OddsMilli, Timestamp(s.Now())); err != nil {
		return 0, fmt.Errorf("store: record opening price: %w", err)
	}
	return id, nil
}

// MarketsForEvent loads an event's markets with their selections attached.
func (s *Store) MarketsForEvent(ctx context.Context, eventID int64, onlyOpen bool) ([]Market, error) {
	query := `SELECT id, event_id, kind, title, line_x100, period,
	                 COALESCE(subject_participant_id, 0), status, margin_bps, COALESCE(settled_at, '')
	          FROM markets WHERE event_id = ?`
	args := []any{eventID}
	if onlyOpen {
		query += " AND status = ?"
		args = append(args, MarketOpen)
	}
	query += " ORDER BY id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: markets: %w", err)
	}
	defer rows.Close()

	var markets []Market
	index := make(map[int64]int)
	for rows.Next() {
		var m Market
		var line sql.NullInt64
		var settledAt string
		if err := rows.Scan(&m.ID, &m.EventID, &m.Kind, &m.Title, &line, &m.Period,
			&m.SubjectID, &m.Status, &m.MarginBps, &settledAt); err != nil {
			return nil, err
		}
		m.LineX100, m.HasLine = line.Int64, line.Valid
		if m.SettledAt, err = ParseTimestamp(settledAt); err != nil {
			return nil, err
		}
		index[m.ID] = len(markets)
		markets = append(markets, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(markets) == 0 {
		return nil, nil
	}

	selRows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.market_id, COALESCE(s.participant_id, 0), s.name, s.outcome_code,
		        s.odds_milli, s.status, s.sort_order
		 FROM selections s JOIN markets m ON m.id = s.market_id
		 WHERE m.event_id = ? ORDER BY s.market_id, s.sort_order, s.id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("store: selections: %w", err)
	}
	defer selRows.Close()
	for selRows.Next() {
		var sel Selection
		if err := selRows.Scan(&sel.ID, &sel.MarketID, &sel.ParticipantID, &sel.Name,
			&sel.OutcomeCode, &sel.OddsMilli, &sel.Status, &sel.SortOrder); err != nil {
			return nil, err
		}
		if pos, ok := index[sel.MarketID]; ok {
			markets[pos].Selections = append(markets[pos].Selections, sel)
		}
	}
	return markets, selRows.Err()
}

// GetMarket loads a single market with its selections.
func (s *Store) GetMarket(ctx context.Context, marketID int64) (Market, error) {
	var m Market
	var line sql.NullInt64
	var settledAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, event_id, kind, title, line_x100, period, COALESCE(subject_participant_id, 0),
		        status, margin_bps, COALESCE(settled_at, '')
		 FROM markets WHERE id = ?`, marketID).
		Scan(&m.ID, &m.EventID, &m.Kind, &m.Title, &line, &m.Period, &m.SubjectID,
			&m.Status, &m.MarginBps, &settledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Market{}, ErrNotFound
	}
	if err != nil {
		return Market{}, fmt.Errorf("store: get market: %w", err)
	}
	m.LineX100, m.HasLine = line.Int64, line.Valid
	if m.SettledAt, err = ParseTimestamp(settledAt); err != nil {
		return Market{}, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, market_id, COALESCE(participant_id, 0), name, outcome_code, odds_milli,
		        status, sort_order
		 FROM selections WHERE market_id = ? ORDER BY sort_order, id`, marketID)
	if err != nil {
		return Market{}, fmt.Errorf("store: market selections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sel Selection
		if err := rows.Scan(&sel.ID, &sel.MarketID, &sel.ParticipantID, &sel.Name,
			&sel.OutcomeCode, &sel.OddsMilli, &sel.Status, &sel.SortOrder); err != nil {
			return Market{}, err
		}
		m.Selections = append(m.Selections, sel)
	}
	return m, rows.Err()
}

// SelectionDetail is a selection joined to everything a bet slip needs to
// describe and validate it without a second round of queries.
type SelectionDetail struct {
	Selection
	MarketKind   string
	MarketTitle  string
	MarketStatus string
	EventID      int64
	EventName    string
	EventStatus  string
	SportKey     string
	StartsAt     time.Time
}

// GetSelectionDetail loads one selection with its market and event context.
func (s *Store) GetSelectionDetail(ctx context.Context, selectionID int64) (SelectionDetail, error) {
	return s.selectionDetail(ctx, s.db.QueryRowContext(ctx, selectionDetailQuery+` WHERE s.id = ?`, selectionID))
}

// GetSelectionDetailTx is GetSelectionDetail inside an open transaction, so a
// bet's validation and its stake debit see the same snapshot of the odds.
func GetSelectionDetailTx(ctx context.Context, tx *Tx, selectionID int64) (SelectionDetail, error) {
	return scanSelectionDetail(tx.QueryRowContext(ctx, selectionDetailQuery+` WHERE s.id = ?`, selectionID))
}

const selectionDetailQuery = `
	SELECT s.id, s.market_id, COALESCE(s.participant_id, 0), s.name, s.outcome_code,
	       s.odds_milli, s.status, s.sort_order,
	       m.kind, m.title, m.status,
	       e.id, e.name, e.status, e.sport_key, e.starts_at
	FROM selections s
	JOIN markets m ON m.id = s.market_id
	JOIN events  e ON e.id = m.event_id`

func (s *Store) selectionDetail(ctx context.Context, row *sql.Row) (SelectionDetail, error) {
	return scanSelectionDetail(row)
}

func scanSelectionDetail(row interface{ Scan(...any) error }) (SelectionDetail, error) {
	var d SelectionDetail
	var startsAt string
	err := row.Scan(&d.ID, &d.MarketID, &d.ParticipantID, &d.Name, &d.OutcomeCode,
		&d.OddsMilli, &d.Status, &d.SortOrder,
		&d.MarketKind, &d.MarketTitle, &d.MarketStatus,
		&d.EventID, &d.EventName, &d.EventStatus, &d.SportKey, &startsAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SelectionDetail{}, ErrNotFound
	}
	if err != nil {
		return SelectionDetail{}, fmt.Errorf("store: selection detail: %w", err)
	}
	if d.StartsAt, err = ParseTimestamp(startsAt); err != nil {
		return SelectionDetail{}, err
	}
	return d, nil
}

// UpdateOdds moves a price and records the movement in the price history.
func (s *Store) UpdateOdds(ctx context.Context, selectionID, oddsMilli int64, reason string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE selections SET odds_milli = ? WHERE id = ? AND status IN ('open', 'suspended')`,
			oddsMilli, selectionID)
		if err != nil {
			return fmt.Errorf("store: update odds: %w", err)
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			return fmt.Errorf("%w: selection %d is not open", ErrConflict, selectionID)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO odds_history (selection_id, odds_milli, at, reason) VALUES (?, ?, ?, ?)`,
			selectionID, oddsMilli, Timestamp(s.Now()), reason)
		return err
	})
}

// SetMarketStatus suspends, closes or reopens a market.
func (s *Store) SetMarketStatus(ctx context.Context, marketID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE markets SET status = ? WHERE id = ?`, status, marketID)
	return err
}

// SetSelectionStatus suspends or reopens a single price.
func (s *Store) SetSelectionStatus(ctx context.Context, selectionID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE selections SET status = ? WHERE id = ?`, status, selectionID)
	return err
}

// OddsHistory returns a price's movement, oldest first.
func (s *Store) OddsHistory(ctx context.Context, selectionID int64) ([]struct {
	OddsMilli int64
	At        time.Time
	Reason    string
}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT odds_milli, at, reason FROM odds_history WHERE selection_id = ? ORDER BY id`,
		selectionID)
	if err != nil {
		return nil, fmt.Errorf("store: odds history: %w", err)
	}
	defer rows.Close()
	var out []struct {
		OddsMilli int64
		At        time.Time
		Reason    string
	}
	for rows.Next() {
		var point struct {
			OddsMilli int64
			At        time.Time
			Reason    string
		}
		var at string
		if err := rows.Scan(&point.OddsMilli, &at, &point.Reason); err != nil {
			return nil, err
		}
		if point.At, err = ParseTimestamp(at); err != nil {
			return nil, err
		}
		out = append(out, point)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
