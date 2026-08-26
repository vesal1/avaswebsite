// Package casino runs the slot floor: it takes a stake, asks the slot engine
// what the seed says happened, and settles the result through the ledger.
//
// The split matters. internal/slots knows how a machine pays and nothing about
// money or players; this package knows about money and players and nothing
// about how a machine pays. Neither can reach the other's levers, which is
// what makes "the game does not know who is playing" a structural fact rather
// than a promise.
package casino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/fair"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/slots"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Errors callers branch on.
var (
	ErrUnknownGame       = errors.New("casino: no such game")
	ErrStakeNotOffered   = errors.New("casino: that stake is not offered on this game")
	ErrInsufficientFunds = errors.New("casino: insufficient balance")
	ErrClientSeed        = errors.New("casino: that client seed cannot be used")
)

// MaxClientSeed bounds what a player may type as their seed. It is a label on
// a hash input, not a secret, and a very long one is only ever a way to make
// something else fall over.
const MaxClientSeed = 64

// WageringRecorder is told about a stake so a promotional wagering requirement
// can advance. Same interface as the sportsbook uses, for the same reason: the
// casino should not have to know what a promotion is.
type WageringRecorder interface {
	RecordStake(ctx context.Context, userID, refID, stakeSat, oddsMilli int64, source string) error
}

// Service is the slot floor.
type Service struct {
	store      *store.Store
	compliance *compliance.Service
	cfg        *config.Config
	wagering   WageringRecorder
	now        func() time.Time
}

// New builds the casino service.
func New(s *store.Store, comp *compliance.Service, cfg *config.Config) *Service {
	return &Service{store: s, compliance: comp, cfg: cfg, now: func() time.Time { return s.Now() }}
}

// WithWagering attaches a wagering recorder.
func (s *Service) WithWagering(recorder WageringRecorder) *Service {
	s.wagering = recorder
	return s
}

// Games returns the floor.
func (s *Service) Games() []*slots.Game { return slots.All() }

// Game returns one machine and its published figures.
func (s *Service) Game(key string) (*slots.Game, slots.Maths, error) {
	game, ok := slots.Lookup(key)
	if !ok {
		return nil, slots.Maths{}, fmt.Errorf("%w: %q", ErrUnknownGame, key)
	}
	maths, _ := slots.MathsFor(key)
	return game, maths, nil
}

// ---------------------------------------------------------------------------
// Seeds
// ---------------------------------------------------------------------------

// Seed returns the player's live seed pair, creating one if this is their
// first visit.
//
// The commitment is published here, before any stake is taken. Everything that
// follows under this pair was decided at this moment.
func (s *Service) Seed(ctx context.Context, userID int64) (store.CasinoSeed, error) {
	seed, err := s.store.LiveCasinoSeed(ctx, userID)
	if err == nil {
		return seed, nil
	}
	if !errors.Is(err, store.ErrNoCasinoSeed) {
		return store.CasinoSeed{}, err
	}

	commitment, secret, err := newPair()
	if err != nil {
		return store.CasinoSeed{}, err
	}
	clientSeed, err := fair.NewClientSeed()
	if err != nil {
		return store.CasinoSeed{}, fmt.Errorf("casino: %w", err)
	}
	created, err := s.store.CreateCasinoSeed(ctx, userID, commitment, secret, clientSeed)
	if err != nil {
		// Another request for the same player got there first, which is fine:
		// there is exactly one live pair and this is it.
		if live, lookupErr := s.store.LiveCasinoSeed(ctx, userID); lookupErr == nil {
			return live, nil
		}
		return store.CasinoSeed{}, err
	}
	return created, nil
}

// Rotate retires the player's pair, publishing its server seed so every spin
// taken under it can be checked, and commits to a new one.
func (s *Service) Rotate(ctx context.Context, userID int64, clientSeed string) (retired, next store.CasinoSeed, err error) {
	if _, err := s.Seed(ctx, userID); err != nil {
		return store.CasinoSeed{}, store.CasinoSeed{}, err
	}
	clientSeed, err = cleanClientSeed(clientSeed)
	if err != nil {
		return store.CasinoSeed{}, store.CasinoSeed{}, err
	}
	commitment, secret, err := newPair()
	if err != nil {
		return store.CasinoSeed{}, store.CasinoSeed{}, err
	}
	return s.store.RotateCasinoSeed(ctx, userID, commitment, secret, clientSeed)
}

// SetClientSeed changes the player's contribution without retiring the pair.
func (s *Service) SetClientSeed(ctx context.Context, userID int64, clientSeed string) error {
	if _, err := s.Seed(ctx, userID); err != nil {
		return err
	}
	cleaned, err := cleanClientSeed(clientSeed)
	if err != nil {
		return err
	}
	return s.store.SetCasinoClientSeed(ctx, userID, cleaned)
}

func newPair() (commitment, secret string, err error) {
	seed, err := fair.NewSeed("", 0)
	if err != nil {
		return "", "", fmt.Errorf("casino: %w", err)
	}
	return seed.Commitment, seed.ServerSeedHex(), nil
}

func cleanClientSeed(seed string) (string, error) {
	trimmed := ""
	for _, r := range seed {
		// Printable ASCII only: the seed is echoed back on the verification
		// page and copied into other people's scripts.
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("%w: use letters, digits and punctuation", ErrClientSeed)
		}
		trimmed += string(r)
	}
	if len(trimmed) == 0 {
		return fair.NewClientSeed()
	}
	if len(trimmed) > MaxClientSeed {
		return "", fmt.Errorf("%w: at most %d characters", ErrClientSeed, MaxClientSeed)
	}
	return trimmed, nil
}

// ---------------------------------------------------------------------------
// Playing
// ---------------------------------------------------------------------------

// Result is a settled round as the caller needs it.
type Result struct {
	SpinID       int64
	Round        *slots.Round
	Game         *slots.Game
	StakeSat     int64
	WinSat       int64
	FromBonusSat int64
	BalanceSat   int64
	Seed         store.CasinoSeed
}

// Won reports whether the round returned anything.
func (r Result) Won() bool { return r.WinSat > 0 }

// NetSat is the round's result from the player's side.
func (r Result) NetSat() int64 { return r.WinSat - r.StakeSat }

// Spin takes a stake, resolves a round and settles it.
//
// The order is deliberate and is the order an auditor would want: the
// commitment already exists, the nonce is claimed, the outcome is computed from
// those two and the player's seed, and only then does any money move. Nothing
// between the stake and the result can look at either.
func (s *Service) Spin(ctx context.Context, user store.User, gameKey string, stakeSat int64) (Result, error) {
	game, maths, err := s.Game(gameKey)
	if err != nil {
		return Result{}, err
	}
	if !game.AcceptsStake(stakeSat) {
		return Result{}, fmt.Errorf("%w: %s takes %s to %s BTC", ErrStakeNotOffered, game.Name,
			money.FormatBTC(game.MinStakeSat()), money.FormatBTC(game.MaxStakeSat()))
	}
	if err := s.compliance.CheckCanBet(ctx, user, stakeSat); err != nil {
		return Result{}, err
	}
	seed, err := s.Seed(ctx, user.ID)
	if err != nil {
		return Result{}, err
	}

	var result Result
	err = s.store.Tx(ctx, func(tx *store.Tx) error {
		nonce, err := store.ClaimCasinoNonce(ctx, tx, seed.ID)
		if err != nil {
			return err
		}
		round, err := playRound(game, seed, nonce, stakeSat)
		if err != nil {
			return err
		}

		cash, bonusAvailable, err := store.PlayableBalanceTx(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		if cash+bonusAvailable < stakeSat {
			return fmt.Errorf("%w: you have %s BTC to stake, and the spin costs %s BTC",
				ErrInsufficientFunds, money.FormatBTC(cash+bonusAvailable), money.FormatBTC(stakeSat))
		}
		debits, fromBonus, err := store.SpendEntries(user.ID, stakeSat, cash, bonusAvailable)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInsufficientFunds, err)
		}

		at := store.Timestamp(s.now())
		stakeTxn, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:    store.TxnCasinoStake,
			Memo:    fmt.Sprintf("%s spin", game.Name),
			RefType: "casino_spin",
			Entries: append(debits, store.Entry{
				Account: store.AccountHouseRevenue, AmountSat: stakeSat,
			}),
		})
		if err != nil {
			return err
		}

		var payoutTxn int64
		if round.TotalWinSat > 0 {
			// Wins are paid in cash even when the stake came from a bonus.
			// The wagering requirement is what holds the money in place, not a
			// second class of winnings nobody can account for.
			payoutTxn, err = store.PostTxn(ctx, tx, at, store.TxnSpec{
				Kind:    store.TxnCasinoPayout,
				Memo:    fmt.Sprintf("%s win", game.Name),
				RefType: "casino_spin",
				Entries: []store.Entry{
					{Account: store.AccountHouseRevenue, AmountSat: -round.TotalWinSat},
					{Account: store.AccountUserCash, UserID: user.ID, AmountSat: round.TotalWinSat},
				},
			})
			if err != nil {
				return err
			}
		}

		encoded, err := json.Marshal(round)
		if err != nil {
			return fmt.Errorf("casino: record round: %w", err)
		}
		spinID, err := store.RecordCasinoSpin(ctx, tx, store.CasinoSpin{
			UserID: user.ID, SeedID: seed.ID, GameKey: game.Key,
			Commitment: seed.Commitment, ClientSeed: seed.ClientSeed, Nonce: nonce,
			StakeSat: stakeSat, WinSat: round.TotalWinSat, FreeSpins: round.FreeSpinsPlayed,
			RTPBps: maths.RTPBps, Result: string(encoded),
			StakeTxnID: stakeTxn, PayoutTxnID: payoutTxn,
		})
		if err != nil {
			return err
		}

		balance, err := store.BalanceSatTx(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		result = Result{
			SpinID: spinID, Round: round, Game: game, StakeSat: stakeSat,
			WinSat: round.TotalWinSat, FromBonusSat: fromBonus, BalanceSat: balance,
			Seed: seed,
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	// Wagering is advanced after the money has moved. A failure here must not
	// undo a settled spin, so it is reported by the caller's logs rather than
	// by rolling back a round the player has already seen.
	if s.wagering != nil {
		if err := s.wagering.RecordStake(ctx, user.ID, result.SpinID, stakeSat, 0, "slots"); err != nil {
			return result, fmt.Errorf("casino: spin %d settled but wagering was not recorded: %w",
				result.SpinID, err)
		}
	}
	return result, nil
}

// playRound resolves the round for a claimed nonce.
func playRound(game *slots.Game, seed store.CasinoSeed, nonce, stakeSat int64) (*slots.Round, error) {
	restored, err := fair.RestoreSeed(seed.ServerSeed, seed.ClientSeed, nonce)
	if err != nil {
		return nil, fmt.Errorf("casino: %w", err)
	}
	if restored.Commitment != seed.Commitment {
		// The stored secret no longer hashes to what the player was shown.
		// Refuse the spin: paying out against a broken commitment is worse
		// than an outage.
		return nil, fmt.Errorf("casino: seed %d does not match its published commitment", seed.ID)
	}
	return slots.Play(game, restored, stakeSat)
}

// ---------------------------------------------------------------------------
// History and verification
// ---------------------------------------------------------------------------

// History returns a player's recent rounds.
func (s *Service) History(ctx context.Context, userID int64, limit int) ([]store.CasinoSpin, error) {
	return s.store.RecentCasinoSpins(ctx, userID, limit)
}

// Totals returns a player's lifetime casino figures.
func (s *Service) Totals(ctx context.Context, userID int64) (store.CasinoTotals, error) {
	return s.store.CasinoTotalsFor(ctx, userID)
}

// Verified is the outcome of checking a spin from its published seed.
type Verified struct {
	Spin     store.CasinoSpin
	Round    *slots.Round
	Game     *slots.Game
	Matches  bool
	Recorded *slots.Round
}

// Verify replays a spin from the published seed and checks it against what was
// paid.
//
// This is the function a suspicious player would reimplement, so it does the
// work rather than trusting the stored round: the commitment is checked
// against the seed, the reels are spun again from scratch, and the result is
// compared with the record.
func (s *Service) Verify(ctx context.Context, spinID int64) (Verified, error) {
	spin, err := s.store.CasinoSpinByID(ctx, spinID)
	if err != nil {
		return Verified{}, err
	}
	game, _, err := s.Game(spin.GameKey)
	if err != nil {
		return Verified{}, err
	}
	out := Verified{Spin: spin, Game: game}

	if spin.Result != "" {
		var recorded slots.Round
		if err := json.Unmarshal([]byte(spin.Result), &recorded); err == nil {
			out.Recorded = &recorded
		}
	}
	if !spin.Verifiable() {
		// The pair is still live, so the seed is still secret. Nothing to
		// check yet, and saying otherwise would be a lie.
		return out, nil
	}
	if err := fair.CheckCommitment(spin.Commitment, spin.ServerSeed); err != nil {
		return out, err
	}
	seed, err := fair.RestoreSeed(spin.ServerSeed, spin.ClientSeed, spin.Nonce)
	if err != nil {
		return out, err
	}
	round, err := slots.Play(game, seed, spin.StakeSat)
	if err != nil {
		return out, err
	}
	out.Round = round
	out.Matches = round.TotalWinSat == spin.WinSat
	return out, nil
}
