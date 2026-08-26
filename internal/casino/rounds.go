package casino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vesal1/avaswebsite/internal/blackjack"
	"github.com/vesal1/avaswebsite/internal/fair"
	"github.com/vesal1/avaswebsite/internal/mines"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Game keys for the stateful tables.
const (
	KeyBlackjack = "blackjack"
	KeyMines     = "mines"
)

// ErrRoundOpen is returned when a fresh round is asked for while one is
// already being played: the open one resumes instead.
var ErrRoundOpen = errors.New("casino: you have a round in play; finish it first")

// dealDeck is blackjack's shuffle: the 52 cards permuted by the round's
// keystream, the same construction the poker tables use and the verification
// page documents.
func dealDeck(seed *fair.Seed) []blackjack.Card {
	deck := blackjack.NewDeck()
	seed.Stream().Shuffle(len(deck), func(i, j int) {
		deck[i], deck[j] = deck[j], deck[i]
	})
	return deck
}

// roundSeed rebuilds the fair seed a stored round was dealt from. It needs
// the secret, so it loads the pair server-side; nothing here reaches a
// player until the pair is retired.
func (s *Service) roundSeed(ctx context.Context, round store.CasinoRound) (*fair.Seed, error) {
	pair, err := s.store.CasinoSeedByID(ctx, round.SeedID)
	if err != nil {
		return nil, err
	}
	seed, err := fair.RestoreSeed(pair.ServerSeed, round.ClientSeed, round.Nonce)
	if err != nil {
		return nil, fmt.Errorf("casino: rebuild round seed: %w", err)
	}
	if seed.Commitment != round.Commitment {
		return nil, fmt.Errorf("casino: round %d no longer matches its commitment", round.ID)
	}
	return seed, nil
}

// blackjackState is what lives in the round row: the engine state plus the
// base stake every unit bet is a multiple of.
type blackjackState struct {
	BaseStakeSat int64           `json:"base_stake_sat"`
	Game         blackjack.State `json:"game"`
}

// minesState wraps the Mines engine state.
type minesState struct {
	Game mines.State `json:"game"`
}

// openRound begins any stateful round: compliance, seed, nonce, stake and the
// row, all in one transaction. buildState is called with the claimed seed to
// produce the engine's opening state and, for a round that settles on the
// deal (a blackjack natural), its immediate outcome.
func (s *Service) openRound(ctx context.Context, user store.User, gameKey string,
	stakeSat, rtpBps int64,
	buildState func(seed *fair.Seed) (state string, settled bool, status string, winSat int64, err error),
) (store.CasinoRound, error) {

	if err := s.compliance.CheckCanBet(ctx, user, stakeSat); err != nil {
		return store.CasinoRound{}, err
	}
	pair, err := s.Seed(ctx, user.ID)
	if err != nil {
		return store.CasinoRound{}, err
	}

	var out store.CasinoRound
	err = s.store.Tx(ctx, func(tx *store.Tx) error {
		if _, err := store.OpenCasinoRoundTx(ctx, tx, user.ID, gameKey); err == nil {
			return ErrRoundOpen
		} else if !errors.Is(err, store.ErrNoOpenRound) {
			return err
		}

		nonce, err := store.ClaimCasinoNonce(ctx, tx, pair.ID)
		if err != nil {
			return err
		}
		seed, err := fair.RestoreSeed(pair.ServerSeed, pair.ClientSeed, nonce)
		if err != nil {
			return fmt.Errorf("casino: %w", err)
		}

		cash, bonusAvailable, err := store.PlayableBalanceTx(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		if cash+bonusAvailable < stakeSat {
			return fmt.Errorf("%w: you have %s BTC to stake, and the round costs %s BTC",
				ErrInsufficientFunds, money.FormatBTC(cash+bonusAvailable), money.FormatBTC(stakeSat))
		}
		debits, _, err := store.SpendEntries(user.ID, stakeSat, cash, bonusAvailable)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInsufficientFunds, err)
		}

		at := store.Timestamp(s.now())
		stakeTxn, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:    store.TxnCasinoStake,
			Memo:    fmt.Sprintf("%s round", gameKey),
			RefType: "casino_round",
			Entries: append(debits, store.Entry{
				Account: store.AccountHouseRevenue, AmountSat: stakeSat,
			}),
		})
		if err != nil {
			return err
		}

		state, settled, status, winSat, err := buildState(seed)
		if err != nil {
			return err
		}
		roundID, err := store.InsertCasinoRoundTx(ctx, tx, at, store.CasinoRound{
			UserID: user.ID, SeedID: pair.ID, GameKey: gameKey,
			Commitment: seed.Commitment, ClientSeed: seed.ClientSeed, Nonce: nonce,
			StakeSat: stakeSat, State: state, RTPBps: rtpBps, StakeTxnID: stakeTxn,
		})
		if err != nil {
			return err
		}

		if settled {
			// A natural settles on the deal: pay and close in the same
			// transaction that opened it.
			payoutTxn, err := s.payRound(ctx, tx, at, gameKey, user.ID, winSat)
			if err != nil {
				return err
			}
			if err := store.SettleCasinoRoundTx(ctx, tx, at, roundID, status, state, winSat, payoutTxn); err != nil {
				return err
			}
		}

		out, err = roundInTx(ctx, tx, roundID)
		return err
	})
	if err != nil {
		return store.CasinoRound{}, err
	}

	if s.wagering != nil {
		if err := s.wagering.RecordStake(ctx, user.ID, out.ID, stakeSat, 0, gameKey); err != nil {
			return out, fmt.Errorf("casino: round %d opened but wagering was not recorded: %w", out.ID, err)
		}
	}
	return out, nil
}

// payRound posts a round's payout, if there is one.
func (s *Service) payRound(ctx context.Context, tx *store.Tx, at, gameKey string, userID, winSat int64) (int64, error) {
	if winSat <= 0 {
		return 0, nil
	}
	return store.PostTxn(ctx, tx, at, store.TxnSpec{
		Kind:    store.TxnCasinoPayout,
		Memo:    fmt.Sprintf("%s payout", gameKey),
		RefType: "casino_round",
		Entries: []store.Entry{
			{Account: store.AccountHouseRevenue, AmountSat: -winSat},
			{Account: store.AccountUserCash, UserID: userID, AmountSat: winSat},
		},
	})
}

func roundInTx(ctx context.Context, tx *store.Tx, roundID int64) (store.CasinoRound, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT r.id, r.user_id, r.seed_id, r.game_key, r.commitment, r.client_seed,
		        r.nonce, r.stake_sat, r.win_sat, r.status, r.state, r.rtp_bps,
		        r.stake_txn_id, r.payout_txn_id, r.created_at, r.settled_at, ''
		 FROM casino_rounds r WHERE r.id = ?`, roundID)
	return store.ScanCasinoRoundRow(row)
}

// ---------------------------------------------------------------------------
// Mines
// ---------------------------------------------------------------------------

// StartMines opens a board.
func (s *Service) StartMines(ctx context.Context, user store.User, mineCount int, stakeSat int64) (store.CasinoRound, error) {
	if !mines.ValidMines(mineCount) {
		return store.CasinoRound{}, mines.ErrBadMines
	}
	if !mines.AcceptsStake(stakeSat) {
		return store.CasinoRound{}, fmt.Errorf("%w: Mines takes %s to %s BTC", ErrStakeNotOffered,
			money.FormatBTC(mines.StakeLevels[0]), money.FormatBTC(mines.StakeLevels[len(mines.StakeLevels)-1]))
	}
	rtp, err := mines.WorstStepRTPBps(mineCount)
	if err != nil {
		return store.CasinoRound{}, err
	}
	return s.openRound(ctx, user, KeyMines, stakeSat, rtp,
		func(seed *fair.Seed) (string, bool, string, int64, error) {
			round, err := mines.New(seed, mineCount)
			if err != nil {
				return "", false, "", 0, err
			}
			state, err := json.Marshal(minesState{Game: round.State})
			return string(state), false, "", 0, err
		})
}

// minesAct loads the open board, applies one move, and settles if it ended.
func (s *Service) minesAct(ctx context.Context, user store.User,
	act func(round *mines.Round) error) (store.CasinoRound, *mines.Round, error) {

	var (
		outRow   store.CasinoRound
		outRound *mines.Round
	)
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		row, err := store.OpenCasinoRoundTx(ctx, tx, user.ID, KeyMines)
		if err != nil {
			return err
		}
		var state minesState
		if err := json.Unmarshal([]byte(row.State), &state); err != nil {
			return fmt.Errorf("casino: round %d state: %w", row.ID, err)
		}
		seed, err := s.roundSeedTx(ctx, tx, row)
		if err != nil {
			return err
		}
		round, err := mines.Resume(seed, state.Game)
		if err != nil {
			return err
		}
		if err := act(round); err != nil {
			return err
		}

		state.Game = round.State
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		at := store.Timestamp(s.now())
		if round.State.Over {
			winSat := round.PayoutSat(row.StakeSat)
			status := store.RoundLost
			if winSat > 0 {
				status = store.RoundWon
			}
			payoutTxn, err := s.payRound(ctx, tx, at, KeyMines, user.ID, winSat)
			if err != nil {
				return err
			}
			if err := store.SettleCasinoRoundTx(ctx, tx, at, row.ID, status, string(encoded), winSat, payoutTxn); err != nil {
				return err
			}
		} else {
			if err := store.UpdateCasinoRoundTx(ctx, tx, row.ID, string(encoded), 0); err != nil {
				return err
			}
		}
		outRow, err = roundInTx(ctx, tx, row.ID)
		outRound = round
		return err
	})
	return outRow, outRound, err
}

// MinesReveal turns over one tile.
func (s *Service) MinesReveal(ctx context.Context, user store.User, cell int) (store.CasinoRound, *mines.Round, error) {
	return s.minesAct(ctx, user, func(round *mines.Round) error {
		_, _, err := round.Reveal(cell)
		return err
	})
}

// MinesCashOut takes the board at its current step.
func (s *Service) MinesCashOut(ctx context.Context, user store.User) (store.CasinoRound, *mines.Round, error) {
	return s.minesAct(ctx, user, func(round *mines.Round) error {
		return round.CashOut()
	})
}

// OpenMines returns a player's live board for rendering, or ErrNoOpenRound.
func (s *Service) OpenMines(ctx context.Context, userID int64) (store.CasinoRound, *mines.Round, error) {
	row, err := s.store.OpenCasinoRound(ctx, userID, KeyMines)
	if err != nil {
		return store.CasinoRound{}, nil, err
	}
	var state minesState
	if err := json.Unmarshal([]byte(row.State), &state); err != nil {
		return store.CasinoRound{}, nil, fmt.Errorf("casino: round %d state: %w", row.ID, err)
	}
	seed, err := s.roundSeed(ctx, row)
	if err != nil {
		return store.CasinoRound{}, nil, err
	}
	round, err := mines.Resume(seed, state.Game)
	if err != nil {
		return store.CasinoRound{}, nil, err
	}
	return row, round, nil
}

// ---------------------------------------------------------------------------
// Blackjack
// ---------------------------------------------------------------------------

// StartBlackjack deals a hand. Stake levels are shared with the slots' shape:
// multiples that keep every payout, the 3:2 natural included, in whole
// satoshi.
var BlackjackStakeLevels = []int64{10_000, 20_000, 50_000, 100_000, 500_000, 1_000_000}

// AcceptsBlackjackStake reports whether a stake is on the ladder.
func AcceptsBlackjackStake(sat int64) bool {
	for _, level := range BlackjackStakeLevels {
		if level == sat {
			return true
		}
	}
	return false
}

func (s *Service) StartBlackjack(ctx context.Context, user store.User, stakeSat int64) (store.CasinoRound, error) {
	if !AcceptsBlackjackStake(stakeSat) {
		return store.CasinoRound{}, fmt.Errorf("%w: blackjack takes %s to %s BTC", ErrStakeNotOffered,
			money.FormatBTC(BlackjackStakeLevels[0]),
			money.FormatBTC(BlackjackStakeLevels[len(BlackjackStakeLevels)-1]))
	}
	return s.openRound(ctx, user, KeyBlackjack, stakeSat, blackjack.PublishedRTPBps,
		func(seed *fair.Seed) (string, bool, string, int64, error) {
			round, err := blackjack.New(dealDeck(seed))
			if err != nil {
				return "", false, "", 0, err
			}
			state, err := json.Marshal(blackjackState{BaseStakeSat: stakeSat, Game: round.State})
			if err != nil {
				return "", false, "", 0, err
			}
			if round.Settled() {
				winSat := round.PayoutSat(stakeSat)
				return string(state), true, outcomeStatus(round, stakeSat), winSat, nil
			}
			return string(state), false, "", 0, nil
		})
}

func outcomeStatus(round *blackjack.Round, baseStake int64) string {
	switch round.Outcome(baseStake) {
	case "won":
		return store.RoundWon
	case "pushed":
		return store.RoundPushed
	default:
		return store.RoundLost
	}
}

// BlackjackAct applies one decision. A double or a split stakes another base
// bet, checked and moved inside the same transaction as the move itself.
func (s *Service) BlackjackAct(ctx context.Context, user store.User, action blackjack.Action) (store.CasinoRound, *blackjack.Round, error) {
	var (
		outRow   store.CasinoRound
		outRound *blackjack.Round
	)
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		row, err := store.OpenCasinoRoundTx(ctx, tx, user.ID, KeyBlackjack)
		if err != nil {
			return err
		}
		var state blackjackState
		if err := json.Unmarshal([]byte(row.State), &state); err != nil {
			return fmt.Errorf("casino: round %d state: %w", row.ID, err)
		}
		seed, err := s.roundSeedTx(ctx, tx, row)
		if err != nil {
			return err
		}
		round, err := blackjack.Resume(dealDeck(seed), state.Game)
		if err != nil {
			return err
		}

		extraUnits, err := round.Act(action)
		if err != nil {
			return err
		}
		at := store.Timestamp(s.now())

		extraSat := int64(extraUnits) * state.BaseStakeSat
		if extraSat > 0 {
			cash, bonusAvailable, err := store.PlayableBalanceTx(ctx, tx, user.ID)
			if err != nil {
				return err
			}
			if cash+bonusAvailable < extraSat {
				// The move needs money the player does not have. The engine
				// state is discarded with the transaction, so the hand is
				// exactly as it was: they can still hit or stand.
				return fmt.Errorf("%w: a %s stakes another %s BTC",
					ErrInsufficientFunds, action, money.FormatBTC(extraSat))
			}
			debits, _, err := store.SpendEntries(user.ID, extraSat, cash, bonusAvailable)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInsufficientFunds, err)
			}
			if _, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
				Kind:    store.TxnCasinoStake,
				Memo:    fmt.Sprintf("blackjack %s", action),
				RefType: "casino_round",
				RefID:   row.ID,
				Entries: append(debits, store.Entry{
					Account: store.AccountHouseRevenue, AmountSat: extraSat,
				}),
			}); err != nil {
				return err
			}
		}

		state.Game = round.State
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}

		if round.Settled() {
			winSat := round.PayoutSat(state.BaseStakeSat)
			payoutTxn, err := s.payRound(ctx, tx, at, KeyBlackjack, user.ID, winSat)
			if err != nil {
				return err
			}
			// Settle the row with the extra stake folded in first.
			if extraSat > 0 {
				if err := store.UpdateCasinoRoundTx(ctx, tx, row.ID, string(encoded), extraSat); err != nil {
					return err
				}
			}
			if err := store.SettleCasinoRoundTx(ctx, tx, at, row.ID,
				outcomeStatus(round, state.BaseStakeSat), string(encoded), winSat, payoutTxn); err != nil {
				return err
			}
		} else {
			if err := store.UpdateCasinoRoundTx(ctx, tx, row.ID, string(encoded), extraSat); err != nil {
				return err
			}
		}
		outRow, err = roundInTx(ctx, tx, row.ID)
		outRound = round
		return err
	})
	if err != nil {
		return store.CasinoRound{}, nil, err
	}

	// The extra stake on a double or split advances wagering like any other.
	if s.wagering != nil && len(outRound.State.Actions) > 0 {
		last := outRound.State.Actions[len(outRound.State.Actions)-1]
		if last == blackjack.Double || last == blackjack.Split {
			var state blackjackState
			if err := json.Unmarshal([]byte(outRow.State), &state); err == nil {
				if err := s.wagering.RecordStake(ctx, user.ID, outRow.ID*10+int64(len(outRound.State.Actions)),
					state.BaseStakeSat, 0, KeyBlackjack); err != nil {
					return outRow, outRound, fmt.Errorf("casino: round %d acted but wagering was not recorded: %w",
						outRow.ID, err)
				}
			}
		}
	}
	return outRow, outRound, nil
}

// OpenBlackjack returns a player's live hand for rendering.
func (s *Service) OpenBlackjack(ctx context.Context, userID int64) (store.CasinoRound, *blackjack.Round, int64, error) {
	row, err := s.store.OpenCasinoRound(ctx, userID, KeyBlackjack)
	if err != nil {
		return store.CasinoRound{}, nil, 0, err
	}
	var state blackjackState
	if err := json.Unmarshal([]byte(row.State), &state); err != nil {
		return store.CasinoRound{}, nil, 0, fmt.Errorf("casino: round %d state: %w", row.ID, err)
	}
	seed, err := s.roundSeed(ctx, row)
	if err != nil {
		return store.CasinoRound{}, nil, 0, err
	}
	round, err := blackjack.Resume(dealDeck(seed), state.Game)
	if err != nil {
		return store.CasinoRound{}, nil, 0, err
	}
	return row, round, state.BaseStakeSat, nil
}

// roundSeedTx is roundSeed inside a transaction.
func (s *Service) roundSeedTx(ctx context.Context, tx *store.Tx, round store.CasinoRound) (*fair.Seed, error) {
	var serverSeed string
	if err := tx.QueryRowContext(ctx,
		`SELECT server_seed FROM casino_seeds WHERE id = ?`, round.SeedID).Scan(&serverSeed); err != nil {
		return nil, fmt.Errorf("casino: round %d seed: %w", round.ID, err)
	}
	seed, err := fair.RestoreSeed(serverSeed, round.ClientSeed, round.Nonce)
	if err != nil {
		return nil, fmt.Errorf("casino: rebuild round seed: %w", err)
	}
	if seed.Commitment != round.Commitment {
		return nil, fmt.Errorf("casino: round %d no longer matches its commitment", round.ID)
	}
	return seed, nil
}

// Rounds returns a player's recent stateful rounds.
func (s *Service) Rounds(ctx context.Context, userID int64, limit int) ([]store.CasinoRound, error) {
	return s.store.RecentCasinoRounds(ctx, userID, limit)
}

// VerifiedRound is the outcome of checking a settled round from its published
// seed: the recomputed layout or deck, the replayed result, and whether it
// matches what was paid.
type VerifiedRound struct {
	Round store.CasinoRound
	// Blackjack fields.
	Deck      []blackjack.Card
	Blackjack *blackjack.Round
	// Mines fields.
	Mines      []int
	MinesState *mines.Round
	Matches    bool
}

// VerifyRound replays a round from the published seed and its action log, and
// checks the replay pays what the ledger paid.
func (s *Service) VerifyRound(ctx context.Context, roundID int64) (VerifiedRound, error) {
	row, err := s.store.CasinoRoundByID(ctx, roundID)
	if err != nil {
		return VerifiedRound{}, err
	}
	out := VerifiedRound{Round: row}
	if !row.Verifiable() {
		return out, nil
	}
	if err := fair.CheckCommitment(row.Commitment, row.ServerSeed); err != nil {
		return out, err
	}
	seed, err := fair.RestoreSeed(row.ServerSeed, row.ClientSeed, row.Nonce)
	if err != nil {
		return out, err
	}

	switch row.GameKey {
	case KeyBlackjack:
		var state blackjackState
		if err := json.Unmarshal([]byte(row.State), &state); err != nil {
			return out, err
		}
		out.Deck = dealDeck(seed)
		replay, err := blackjack.New(dealDeck(seed))
		if err != nil {
			return out, err
		}
		for _, action := range state.Game.Actions {
			if _, err := replay.Act(action); err != nil {
				return out, fmt.Errorf("casino: replaying %s: %w", action, err)
			}
		}
		out.Blackjack = replay
		out.Matches = replay.Settled() && replay.PayoutSat(state.BaseStakeSat) == row.WinSat
	case KeyMines:
		var state minesState
		if err := json.Unmarshal([]byte(row.State), &state); err != nil {
			return out, err
		}
		round, err := mines.Resume(seed, state.Game)
		if err != nil {
			return out, err
		}
		out.Mines = round.Mines()
		out.MinesState = round
		// Replay: every revealed cell must be safe on the recomputed board,
		// the hit tile a mine, and the payout the ladder's.
		matches := round.State.Over
		for _, cell := range state.Game.Revealed {
			for _, mine := range out.Mines {
				if cell == mine {
					matches = false
				}
			}
		}
		if state.Game.Hit >= 0 {
			isMine := false
			for _, mine := range out.Mines {
				if state.Game.Hit == mine {
					isMine = true
				}
			}
			matches = matches && isMine
		}
		out.Matches = matches && round.PayoutSat(row.StakeSat) == row.WinSat
	default:
		return out, fmt.Errorf("casino: no verifier for %q", row.GameKey)
	}
	return out, nil
}
