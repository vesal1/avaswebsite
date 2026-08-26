package poker

import (
	"fmt"

	"github.com/vesal1/avaswebsite/internal/fair"
)

// Shuffle is a deck order the house commits to before the deal and can prove
// afterwards.
//
// How it works, and why it is worth the trouble:
//
//  1. Before any card is dealt the server draws a random secret seed and
//     publishes only SHA-256 of it. That is the commitment.
//  2. Players contribute their own seed. The deck is derived from both, so
//     neither side alone chooses it.
//  3. When the hand is over the server publishes the secret seed. Anybody can
//     check it hashes to the commitment they were shown before the deal, and
//     replay the shuffle to get the exact same deck.
//
// The house therefore cannot look at the deck and change it, cannot deal a
// different card than the one the seed dictates, and cannot pick a seed after
// seeing the players' cards. Any of those would break the commitment, and the
// commitment was published first.
//
// The randomness itself lives in internal/fair, shared with the casino games:
// one implementation to audit, not one per game.
type Shuffle struct {
	seed *fair.Seed
	// Commitment is the hex SHA-256 of the server seed, published up front.
	Commitment string
	// ClientSeed is the players' contribution.
	ClientSeed string
	// Nonce distinguishes hands that share a server seed.
	Nonce int64
}

// NewShuffle draws a fresh server seed and commits to it.
func NewShuffle(clientSeed string, nonce int64) (*Shuffle, error) {
	seed, err := fair.NewSeed(clientSeed, nonce)
	if err != nil {
		return nil, fmt.Errorf("poker: %w", err)
	}
	return wrap(seed), nil
}

// RestoreShuffle rebuilds a shuffle from a known server seed, for verification
// and for replaying a hand from its history.
func RestoreShuffle(serverSeedHex, clientSeed string, nonce int64) (*Shuffle, error) {
	seed, err := fair.RestoreSeed(serverSeedHex, clientSeed, nonce)
	if err != nil {
		return nil, fmt.Errorf("poker: %w", err)
	}
	return wrap(seed), nil
}

func wrap(seed *fair.Seed) *Shuffle {
	return &Shuffle{
		seed:       seed,
		Commitment: seed.Commitment,
		ClientSeed: seed.ClientSeed,
		Nonce:      seed.Nonce,
	}
}

// Deck returns the shuffled deck this seed produces. It is deterministic: the
// same seeds always give the same order, which is the whole point.
func (s *Shuffle) Deck() []Card {
	deck := NewDeck()
	// Fisher-Yates from the top down, over the round's keystream. Every
	// permutation is equally likely provided each index is drawn without
	// bias, which fair.Stream guarantees by rejection sampling.
	s.seed.Stream().Shuffle(len(deck), func(i, j int) {
		deck[i], deck[j] = deck[j], deck[i]
	})
	return deck
}

// Reveal publishes the server seed, ending the hand's secrecy.
func (s *Shuffle) Reveal() string { return s.seed.Reveal() }

// Revealed reports whether the seed has been published.
func (s *Shuffle) Revealed() bool { return s.seed.Revealed() }

// Verify checks a revealed shuffle against the commitment a player was shown,
// and returns the deck it produces. This is the function a suspicious player's
// own script would reimplement, so it is written to be easy to reimplement.
func Verify(commitment, serverSeedHex, clientSeed string, nonce int64) ([]Card, error) {
	if err := fair.CheckCommitment(commitment, serverSeedHex); err != nil {
		return nil, fmt.Errorf("poker: %w", err)
	}
	shuffle, err := RestoreShuffle(serverSeedHex, clientSeed, nonce)
	if err != nil {
		return nil, err
	}
	return shuffle.Deck(), nil
}

// CombineClientSeeds folds every seated player's seed into one, so no single
// player and not the house alone decides the deck.
func CombineClientSeeds(seeds []string) string { return fair.CombineClientSeeds(seeds) }

// NewClientSeed generates a random seed for a player who has not set one.
func NewClientSeed() (string, error) { return fair.NewClientSeed() }
