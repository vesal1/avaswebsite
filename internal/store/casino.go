package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CasinoSeed is a commit-reveal seed pair for one player's casino play.
//
// The server seed is drawn and committed to once; every spin under the pair
// uses the same secret with a fresh nonce. That ordering is the whole point:
// the outcome of a spin the player has not taken yet was already fixed when
// they were shown the commitment, so nothing about their balance, their
// session or their luck so far can reach it.
type CasinoSeed struct {
	ID         int64
	UserID     int64
	Commitment string
	// ServerSeed is the secret. It is loaded for the engine that needs it and
	// must never be handed to a player while the pair is live; PublishedSeed
	// is the accessor that respects that.
	ServerSeed string
	ClientSeed string
	NextNonce  int64
	Active     bool
	CreatedAt  time.Time
	RetiredAt  time.Time
}

// Revealed reports whether the pair has been retired, and so whether its
// spins can be checked yet.
func (s CasinoSeed) Revealed() bool { return !s.Active }

// PublishedSeed is the server seed as a player may see it: the real thing once
// the pair is retired, and nothing at all before that. Handing out a live seed
// would let the player compute the next spin before deciding whether to take
// it, which is the one thing the commitment exists to prevent.
func (s CasinoSeed) PublishedSeed() string {
	if s.Active {
		return ""
	}
	return s.ServerSeed
}

// CasinoSpin is one settled round.
type CasinoSpin struct {
	ID          int64
	UserID      int64
	SeedID      int64
	GameKey     string
	Commitment  string
	ClientSeed  string
	Nonce       int64
	StakeSat    int64
	WinSat      int64
	FreeSpins   int
	RTPBps      int64
	Result      string
	StakeTxnID  int64
	PayoutTxnID int64
	CreatedAt   time.Time
	// ServerSeed is filled in by lookups that join the seed pair, and stays
	// empty until that pair is retired. A live spin is not checkable yet, and
	// pretending otherwise would mean handing out the secret early.
	ServerSeed string
}

// NetSat is the round's result from the player's side.
func (s CasinoSpin) NetSat() int64 { return s.WinSat - s.StakeSat }

// Verifiable reports whether the seed behind the spin has been published.
func (s CasinoSpin) Verifiable() bool { return s.ServerSeed != "" }

// ErrNoCasinoSeed is returned when a player has no live seed pair.
var ErrNoCasinoSeed = errors.New("store: no live casino seed")

const casinoSeedColumns = `id, user_id, commitment, server_seed, client_seed,
	next_nonce, active, created_at, retired_at`

func scanCasinoSeed(row interface{ Scan(...any) error }) (CasinoSeed, error) {
	var (
		seed    CasinoSeed
		active  int64
		created string
		retired sql.NullString
	)
	if err := row.Scan(&seed.ID, &seed.UserID, &seed.Commitment, &seed.ServerSeed,
		&seed.ClientSeed, &seed.NextNonce, &active, &created, &retired); err != nil {
		return CasinoSeed{}, err
	}
	seed.Active = active == 1
	seed.CreatedAt = mustTimestamp(created)
	if retired.Valid {
		seed.RetiredAt = mustTimestamp(retired.String)
	}
	return seed, nil
}

// LiveCasinoSeed returns a player's current seed pair.
func (s *Store) LiveCasinoSeed(ctx context.Context, userID int64) (CasinoSeed, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+casinoSeedColumns+` FROM casino_seeds
		 WHERE user_id = ? AND active = 1`, userID)
	seed, err := scanCasinoSeed(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CasinoSeed{}, ErrNoCasinoSeed
	}
	if err != nil {
		return CasinoSeed{}, fmt.Errorf("store: live casino seed: %w", err)
	}
	return seed, nil
}

// CreateCasinoSeed commits to a new seed pair for a player.
//
// The unique index on live pairs means two concurrent requests cannot both
// succeed; the loser is told to read the winner's pair rather than quietly
// getting a second one.
func (s *Store) CreateCasinoSeed(ctx context.Context, userID int64, commitment, serverSeed, clientSeed string) (CasinoSeed, error) {
	now := Timestamp(time.Now())
	id, err := s.db.InsertID(ctx,
		`INSERT INTO casino_seeds (user_id, commitment, server_seed, client_seed,
			next_nonce, active, created_at)
		 VALUES (?, ?, ?, ?, 1, 1, ?)`,
		userID, commitment, serverSeed, clientSeed, now)
	if err != nil {
		return CasinoSeed{}, fmt.Errorf("store: create casino seed: %w", err)
	}
	return s.casinoSeedByID(ctx, id)
}

func (s *Store) casinoSeedByID(ctx context.Context, id int64) (CasinoSeed, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+casinoSeedColumns+` FROM casino_seeds WHERE id = ?`, id)
	seed, err := scanCasinoSeed(row)
	if err != nil {
		return CasinoSeed{}, fmt.Errorf("store: casino seed %d: %w", id, err)
	}
	return seed, nil
}

// CasinoSeedByID returns one seed pair.
func (s *Store) CasinoSeedByID(ctx context.Context, id int64) (CasinoSeed, error) {
	return s.casinoSeedByID(ctx, id)
}

// RotateCasinoSeed retires a player's live pair, which publishes its server
// seed, and commits to a replacement in the same breath.
//
// Both halves happen in one transaction. A retirement that published the
// secret without replacing the pair would leave a player able to compute the
// next spin before deciding to take it.
func (s *Store) RotateCasinoSeed(ctx context.Context, userID int64,
	nextCommitment, nextServerSeed, nextClientSeed string) (retired, next CasinoSeed, err error) {

	now := Timestamp(time.Now())
	err = s.Tx(ctx, func(tx *Tx) error {
		var liveID int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM casino_seeds WHERE user_id = ? AND active = 1`, userID).Scan(&liveID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoCasinoSeed
		}
		if err != nil {
			return fmt.Errorf("store: find live casino seed: %w", err)
		}
		// Retiring the pair publishes its secret. With a blackjack hand or a
		// Mines board still open on it, that secret IS the deck order or the
		// mine layout, and the player could read it before deciding their
		// next move. The round must finish first.
		open, err := CountOpenRoundsOnSeedTx(ctx, tx, liveID)
		if err != nil {
			return err
		}
		if open > 0 {
			return ErrRoundStillOpen
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE casino_seeds SET active = 0, retired_at = ?
			 WHERE id = ?`, now, liveID)
		if err != nil {
			return fmt.Errorf("store: retire casino seed: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return ErrNoCasinoSeed
		}
		id, err := tx.InsertID(ctx,
			`INSERT INTO casino_seeds (user_id, commitment, server_seed, client_seed,
				next_nonce, active, created_at)
			 VALUES (?, ?, ?, ?, 1, 1, ?)`,
			userID, nextCommitment, nextServerSeed, nextClientSeed, now)
		if err != nil {
			return fmt.Errorf("store: replace casino seed: %w", err)
		}
		row := tx.QueryRowContext(ctx, `SELECT `+casinoSeedColumns+` FROM casino_seeds WHERE id = ?`, id)
		if next, err = scanCasinoSeed(row); err != nil {
			return fmt.Errorf("store: read replacement casino seed: %w", err)
		}
		row = tx.QueryRowContext(ctx,
			`SELECT `+casinoSeedColumns+` FROM casino_seeds
			 WHERE user_id = ? AND active = 0 ORDER BY id DESC LIMIT 1`, userID)
		if retired, err = scanCasinoSeed(row); err != nil {
			return fmt.Errorf("store: read retired casino seed: %w", err)
		}
		return nil
	})
	return retired, next, err
}

// SetCasinoClientSeed changes the player's contribution to the live pair.
//
// Changing it does not need a rotation: the server seed is unchanged and still
// secret, so the commitment still covers every spin taken under it. The nonce
// keeps counting, so no outcome is repeated.
func (s *Store) SetCasinoClientSeed(ctx context.Context, userID int64, clientSeed string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE casino_seeds SET client_seed = ? WHERE user_id = ? AND active = 1`,
		clientSeed, userID)
	if err != nil {
		return fmt.Errorf("store: set casino client seed: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNoCasinoSeed
	}
	return nil
}

// ClaimCasinoNonce takes the next nonce for a seed pair inside a transaction.
//
// The update is conditional on the value it read, so two spins racing for the
// same nonce cannot both win: the loser gets no row back and the caller
// retries. Two spins on one nonce would be two different outcomes claiming the
// same proof.
func ClaimCasinoNonce(ctx context.Context, tx *Tx, seedID int64) (int64, error) {
	var nonce int64
	err := tx.QueryRowContext(ctx,
		`SELECT next_nonce FROM casino_seeds WHERE id = ? AND active = 1`, seedID).Scan(&nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoCasinoSeed
	}
	if err != nil {
		return 0, fmt.Errorf("store: read casino nonce: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE casino_seeds SET next_nonce = ? WHERE id = ? AND next_nonce = ?`,
		nonce+1, seedID, nonce)
	if err != nil {
		return 0, fmt.Errorf("store: claim casino nonce: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return 0, fmt.Errorf("store: casino nonce %d was taken by another spin", nonce)
	}
	return nonce, nil
}

// RecordCasinoSpin writes a settled round.
func RecordCasinoSpin(ctx context.Context, tx *Tx, spin CasinoSpin) (int64, error) {
	id, err := tx.InsertID(ctx,
		`INSERT INTO casino_spins (user_id, seed_id, game_key, commitment, client_seed,
			nonce, stake_sat, win_sat, free_spins, rtp_bps, result,
			stake_txn_id, payout_txn_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spin.UserID, spin.SeedID, spin.GameKey, spin.Commitment, spin.ClientSeed,
		spin.Nonce, spin.StakeSat, spin.WinSat, spin.FreeSpins, spin.RTPBps, spin.Result,
		nullInt64(spin.StakeTxnID), nullInt64(spin.PayoutTxnID),
		Timestamp(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: record casino spin: %w", err)
	}
	return id, nil
}

const casinoSpinColumns = `s.id, s.user_id, s.seed_id, s.game_key, s.commitment,
	s.client_seed, s.nonce, s.stake_sat, s.win_sat, s.free_spins, s.rtp_bps,
	s.result, s.stake_txn_id, s.payout_txn_id, s.created_at,
	CASE WHEN k.active = 1 THEN '' ELSE k.server_seed END`

func scanCasinoSpin(row interface{ Scan(...any) error }) (CasinoSpin, error) {
	var (
		spin    CasinoSpin
		stakeTx sql.NullInt64
		payTx   sql.NullInt64
		created string
	)
	if err := row.Scan(&spin.ID, &spin.UserID, &spin.SeedID, &spin.GameKey, &spin.Commitment,
		&spin.ClientSeed, &spin.Nonce, &spin.StakeSat, &spin.WinSat, &spin.FreeSpins,
		&spin.RTPBps, &spin.Result, &stakeTx, &payTx, &created, &spin.ServerSeed); err != nil {
		return CasinoSpin{}, err
	}
	spin.StakeTxnID = stakeTx.Int64
	spin.PayoutTxnID = payTx.Int64
	spin.CreatedAt = mustTimestamp(created)
	return spin, nil
}

// CasinoSpinByID returns one round with its seed, if that seed has been
// published.
func (s *Store) CasinoSpinByID(ctx context.Context, id int64) (CasinoSpin, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+casinoSpinColumns+`
		 FROM casino_spins s JOIN casino_seeds k ON k.id = s.seed_id
		 WHERE s.id = ?`, id)
	spin, err := scanCasinoSpin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CasinoSpin{}, ErrNotFound
	}
	if err != nil {
		return CasinoSpin{}, fmt.Errorf("store: casino spin %d: %w", id, err)
	}
	return spin, nil
}

// RecentCasinoSpins lists a player's most recent rounds.
func (s *Store) RecentCasinoSpins(ctx context.Context, userID int64, limit int) ([]CasinoSpin, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+casinoSpinColumns+`
		 FROM casino_spins s JOIN casino_seeds k ON k.id = s.seed_id
		 WHERE s.user_id = ? ORDER BY s.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent casino spins: %w", err)
	}
	defer rows.Close()

	var out []CasinoSpin
	for rows.Next() {
		spin, err := scanCasinoSpin(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan casino spin: %w", err)
		}
		out = append(out, spin)
	}
	return out, rows.Err()
}

// CasinoTotals is a player's lifetime casino figures.
type CasinoTotals struct {
	Spins    int64
	StakeSat int64
	WinSat   int64
}

// NetSat is what the player is up or down.
func (t CasinoTotals) NetSat() int64 { return t.WinSat - t.StakeSat }

// ReturnBps is the return this player has actually seen, which will wander a
// long way from the machine's published figure over any human number of spins.
func (t CasinoTotals) ReturnBps() int64 {
	if t.StakeSat == 0 {
		return 0
	}
	return t.WinSat * 10000 / t.StakeSat
}

// CasinoTotalsFor sums a player's casino play.
func (s *Store) CasinoTotalsFor(ctx context.Context, userID int64) (CasinoTotals, error) {
	var totals CasinoTotals
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(stake_sat), 0), COALESCE(SUM(win_sat), 0)
		 FROM casino_spins WHERE user_id = ?`, userID).
		Scan(&totals.Spins, &totals.StakeSat, &totals.WinSat)
	if err != nil {
		return CasinoTotals{}, fmt.Errorf("store: casino totals: %w", err)
	}
	return totals, nil
}

// mustTimestamp reads a stored timestamp, treating an unreadable one as zero:
// a malformed date in a history row should not stop a player seeing the row.
func mustTimestamp(value string) time.Time {
	parsed, err := ParseTimestamp(value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// CasinoGameTotals is one machine's takings.
type CasinoGameTotals struct {
	GameKey  string
	Spins    int64
	StakeSat int64
	WinSat   int64
	Players  int64
}

// HoldSat is what the house kept.
func (t CasinoGameTotals) HoldSat() int64 { return t.StakeSat - t.WinSat }

// ReturnBps is the return this machine has actually paid so far. Over a small
// number of spins it says nothing about the machine; over a large number it
// should converge on the published figure, and a lasting gap is worth
// investigating rather than celebrating.
func (t CasinoGameTotals) ReturnBps() int64 {
	if t.StakeSat == 0 {
		return 0
	}
	return t.WinSat * 10000 / t.StakeSat
}

// CasinoTotalsByGame sums play across the floor, newest-busiest first.
func (s *Store) CasinoTotalsByGame(ctx context.Context) ([]CasinoGameTotals, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT game_key, COUNT(*), COALESCE(SUM(stake_sat), 0),
		        COALESCE(SUM(win_sat), 0), COUNT(DISTINCT user_id)
		 FROM casino_spins
		 GROUP BY game_key
		 ORDER BY SUM(stake_sat) DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: casino totals by game: %w", err)
	}
	defer rows.Close()

	var out []CasinoGameTotals
	for rows.Next() {
		var totals CasinoGameTotals
		if err := rows.Scan(&totals.GameKey, &totals.Spins, &totals.StakeSat,
			&totals.WinSat, &totals.Players); err != nil {
			return nil, fmt.Errorf("store: scan casino totals: %w", err)
		}
		out = append(out, totals)
	}
	return out, rows.Err()
}

// BiggestCasinoWins lists the largest single rounds, which is what a risk desk
// looks at first.
func (s *Store) BiggestCasinoWins(ctx context.Context, limit int) ([]CasinoSpin, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+casinoSpinColumns+`
		 FROM casino_spins s JOIN casino_seeds k ON k.id = s.seed_id
		 WHERE s.win_sat > 0 ORDER BY s.win_sat DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: biggest casino wins: %w", err)
	}
	defer rows.Close()

	var out []CasinoSpin
	for rows.Next() {
		spin, err := scanCasinoSpin(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan casino spin: %w", err)
		}
		out = append(out, spin)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Stateful rounds: blackjack, Mines, and anything else that spans requests
// ---------------------------------------------------------------------------

// Round status values.
const (
	RoundOpen   = "open"
	RoundWon    = "won"
	RoundLost   = "lost"
	RoundPushed = "pushed"
)

// CasinoRound is one stateful game round: opened with a stake, acted on over
// several requests, settled once.
type CasinoRound struct {
	ID          int64
	UserID      int64
	SeedID      int64
	GameKey     string
	Commitment  string
	ClientSeed  string
	Nonce       int64
	StakeSat    int64
	WinSat      int64
	Status      string
	State       string
	RTPBps      int64
	StakeTxnID  int64
	PayoutTxnID int64
	CreatedAt   time.Time
	SettledAt   time.Time
	// ServerSeed joins from the seed pair and stays empty until that pair is
	// retired, exactly as for spins.
	ServerSeed string
}

// Open reports whether the round is still being played.
func (r CasinoRound) Open() bool { return r.Status == RoundOpen }

// NetSat is the round's result from the player's side.
func (r CasinoRound) NetSat() int64 { return r.WinSat - r.StakeSat }

// Verifiable reports whether the seed behind the round has been published.
func (r CasinoRound) Verifiable() bool { return r.ServerSeed != "" && !r.Open() }

// ErrNoOpenRound is returned when an action arrives for a round that is not
// there — settled, never started, or someone else's.
var ErrNoOpenRound = errors.New("store: no open round")

// ErrRoundStillOpen refuses a seed rotation while a round is being played on
// the pair. Finish or settle the round, then rotate.
var ErrRoundStillOpen = errors.New("store: finish your open round before publishing the seed behind it")

const casinoRoundColumns = `r.id, r.user_id, r.seed_id, r.game_key, r.commitment,
	r.client_seed, r.nonce, r.stake_sat, r.win_sat, r.status, r.state, r.rtp_bps,
	r.stake_txn_id, r.payout_txn_id, r.created_at, r.settled_at,
	CASE WHEN k.active = 1 THEN '' ELSE k.server_seed END`

func scanCasinoRound(row interface{ Scan(...any) error }) (CasinoRound, error) {
	var (
		round   CasinoRound
		stakeTx sql.NullInt64
		payTx   sql.NullInt64
		created string
		settled sql.NullString
	)
	if err := row.Scan(&round.ID, &round.UserID, &round.SeedID, &round.GameKey,
		&round.Commitment, &round.ClientSeed, &round.Nonce, &round.StakeSat,
		&round.WinSat, &round.Status, &round.State, &round.RTPBps,
		&stakeTx, &payTx, &created, &settled, &round.ServerSeed); err != nil {
		return CasinoRound{}, err
	}
	round.StakeTxnID = stakeTx.Int64
	round.PayoutTxnID = payTx.Int64
	round.CreatedAt = mustTimestamp(created)
	if settled.Valid {
		round.SettledAt = mustTimestamp(settled.String)
	}
	return round, nil
}

// ScanCasinoRoundRow reads a round row whose columns match
// casinoRoundColumns, for callers composing their own in-transaction reads.
func ScanCasinoRoundRow(row interface{ Scan(...any) error }) (CasinoRound, error) {
	return scanCasinoRound(row)
}

// InsertCasinoRoundTx opens a round inside the transaction that also claims
// its nonce and posts its stake.
func InsertCasinoRoundTx(ctx context.Context, tx *Tx, at string, round CasinoRound) (int64, error) {
	id, err := tx.InsertID(ctx,
		`INSERT INTO casino_rounds (user_id, seed_id, game_key, commitment, client_seed,
			nonce, stake_sat, win_sat, status, state, rtp_bps, stake_txn_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
		round.UserID, round.SeedID, round.GameKey, round.Commitment, round.ClientSeed,
		round.Nonce, round.StakeSat, RoundOpen, round.State, round.RTPBps,
		nullInt64(round.StakeTxnID), at)
	if err != nil {
		return 0, fmt.Errorf("store: open casino round: %w", err)
	}
	return id, nil
}

// OpenCasinoRoundTx loads a player's live round for one game, for update.
func OpenCasinoRoundTx(ctx context.Context, tx *Tx, userID int64, gameKey string) (CasinoRound, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+casinoRoundColumns+`
		 FROM casino_rounds r JOIN casino_seeds k ON k.id = r.seed_id
		 WHERE r.user_id = ? AND r.game_key = ? AND r.status = ?`,
		userID, gameKey, RoundOpen)
	round, err := scanCasinoRound(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CasinoRound{}, ErrNoOpenRound
	}
	if err != nil {
		return CasinoRound{}, fmt.Errorf("store: open casino round: %w", err)
	}
	return round, nil
}

// OpenCasinoRound is the read-only variant, for rendering the page.
func (s *Store) OpenCasinoRound(ctx context.Context, userID int64, gameKey string) (CasinoRound, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+casinoRoundColumns+`
		 FROM casino_rounds r JOIN casino_seeds k ON k.id = r.seed_id
		 WHERE r.user_id = ? AND r.game_key = ? AND r.status = ?`,
		userID, gameKey, RoundOpen)
	round, err := scanCasinoRound(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CasinoRound{}, ErrNoOpenRound
	}
	if err != nil {
		return CasinoRound{}, fmt.Errorf("store: open casino round: %w", err)
	}
	return round, nil
}

// UpdateCasinoRoundTx writes a round's new state after an action.
//
// The update is guarded on the round still being open, so two racing requests
// cannot both act: the loser sees no row and the caller reports a conflict
// rather than double-applying a move.
func UpdateCasinoRoundTx(ctx context.Context, tx *Tx, roundID int64, state string, extraStakeSat int64) error {
	result, err := tx.ExecContext(ctx,
		`UPDATE casino_rounds SET state = ?, stake_sat = stake_sat + ?
		 WHERE id = ? AND status = ?`, state, extraStakeSat, roundID, RoundOpen)
	if err != nil {
		return fmt.Errorf("store: update casino round: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNoOpenRound
	}
	return nil
}

// SettleCasinoRoundTx closes a round with its outcome.
func SettleCasinoRoundTx(ctx context.Context, tx *Tx, at string, roundID int64,
	status string, state string, winSat, payoutTxnID int64) error {
	if status == RoundOpen {
		return fmt.Errorf("store: a round cannot settle to open")
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE casino_rounds SET status = ?, state = ?, win_sat = ?,
			payout_txn_id = ?, settled_at = ?
		 WHERE id = ? AND status = ?`,
		status, state, winSat, nullInt64(payoutTxnID), at, roundID, RoundOpen)
	if err != nil {
		return fmt.Errorf("store: settle casino round: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNoOpenRound
	}
	return nil
}

// CasinoRoundByID returns one round with its seed, blanked while live.
func (s *Store) CasinoRoundByID(ctx context.Context, id int64) (CasinoRound, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+casinoRoundColumns+`
		 FROM casino_rounds r JOIN casino_seeds k ON k.id = r.seed_id
		 WHERE r.id = ?`, id)
	round, err := scanCasinoRound(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CasinoRound{}, ErrNotFound
	}
	if err != nil {
		return CasinoRound{}, fmt.Errorf("store: casino round %d: %w", id, err)
	}
	return round, nil
}

// RecentCasinoRounds lists a player's rounds, newest first.
func (s *Store) RecentCasinoRounds(ctx context.Context, userID int64, limit int) ([]CasinoRound, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+casinoRoundColumns+`
		 FROM casino_rounds r JOIN casino_seeds k ON k.id = r.seed_id
		 WHERE r.user_id = ? ORDER BY r.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent casino rounds: %w", err)
	}
	defer rows.Close()

	var out []CasinoRound
	for rows.Next() {
		round, err := scanCasinoRound(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan casino round: %w", err)
		}
		out = append(out, round)
	}
	return out, rows.Err()
}

// CountOpenRoundsOnSeed reports how many live rounds ride on a seed pair.
// Rotation must refuse while this is non-zero: publishing the seed mid-round
// would hand the player the mine layout or the deck order while they can
// still act on it.
func CountOpenRoundsOnSeedTx(ctx context.Context, tx *Tx, seedID int64) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM casino_rounds WHERE seed_id = ? AND status = ?`,
		seedID, RoundOpen).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count open rounds: %w", err)
	}
	return count, nil
}

// CasinoRoundTotalsByGame sums round play per game for the admin floor.
func (s *Store) CasinoRoundTotalsByGame(ctx context.Context) ([]CasinoGameTotals, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT game_key, COUNT(*), COALESCE(SUM(stake_sat), 0),
		        COALESCE(SUM(win_sat), 0), COUNT(DISTINCT user_id)
		 FROM casino_rounds WHERE status != ?
		 GROUP BY game_key ORDER BY SUM(stake_sat) DESC`, RoundOpen)
	if err != nil {
		return nil, fmt.Errorf("store: casino round totals: %w", err)
	}
	defer rows.Close()

	var out []CasinoGameTotals
	for rows.Next() {
		var totals CasinoGameTotals
		if err := rows.Scan(&totals.GameKey, &totals.Spins, &totals.StakeSat,
			&totals.WinSat, &totals.Players); err != nil {
			return nil, fmt.Errorf("store: scan casino round totals: %w", err)
		}
		out = append(out, totals)
	}
	return out, rows.Err()
}
