package poker

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
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
type Shuffle struct {
	// serverSeed is secret until the hand is finished.
	serverSeed []byte
	// Commitment is the hex SHA-256 of the server seed, published up front.
	Commitment string
	// ClientSeed is the players' contribution.
	ClientSeed string
	// Nonce distinguishes hands that share a server seed.
	Nonce int64
	// revealed is set once the seed has been published.
	revealed bool
}

// NewShuffle draws a fresh server seed and commits to it.
func NewShuffle(clientSeed string, nonce int64) (*Shuffle, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("poker: draw server seed: %w", err)
	}
	sum := sha256.Sum256(seed)
	return &Shuffle{
		serverSeed: seed,
		Commitment: hex.EncodeToString(sum[:]),
		ClientSeed: clientSeed,
		Nonce:      nonce,
	}, nil
}

// RestoreShuffle rebuilds a shuffle from a known server seed, for verification
// and for replaying a hand from its history.
func RestoreShuffle(serverSeedHex, clientSeed string, nonce int64) (*Shuffle, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(serverSeedHex))
	if err != nil || len(seed) == 0 {
		return nil, fmt.Errorf("poker: %q is not a server seed", serverSeedHex)
	}
	sum := sha256.Sum256(seed)
	return &Shuffle{
		serverSeed: seed,
		Commitment: hex.EncodeToString(sum[:]),
		ClientSeed: clientSeed,
		Nonce:      nonce,
		revealed:   true,
	}, nil
}

// Deck returns the shuffled deck this seed produces. It is deterministic: the
// same seeds always give the same order, which is the whole point.
func (s *Shuffle) Deck() []Card {
	deck := NewDeck()
	stream := newKeystream(s.serverSeed, s.ClientSeed, s.Nonce)

	// Fisher-Yates from the top down. Every permutation is equally likely
	// provided the index is drawn without bias, which is what boundedIndex is
	// for.
	for i := len(deck) - 1; i > 0; i-- {
		j := stream.boundedIndex(i + 1)
		deck[i], deck[j] = deck[j], deck[i]
	}
	return deck
}

// Reveal publishes the server seed, ending the hand's secrecy.
func (s *Shuffle) Reveal() string {
	s.revealed = true
	return hex.EncodeToString(s.serverSeed)
}

// Revealed reports whether the seed has been published.
func (s *Shuffle) Revealed() bool { return s.revealed }

// Verify checks a revealed shuffle against the commitment a player was shown,
// and returns the deck it produces. This is the function a suspicious player's
// own script would reimplement, so it is written to be easy to reimplement.
func Verify(commitment, serverSeedHex, clientSeed string, nonce int64) ([]Card, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(serverSeedHex))
	if err != nil {
		return nil, fmt.Errorf("poker: the server seed is not valid hex")
	}
	sum := sha256.Sum256(seed)
	got := hex.EncodeToString(sum[:])

	// Constant time, so verification cannot be turned into a guessing oracle.
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(strings.TrimSpace(commitment)))) != 1 {
		return nil, fmt.Errorf("poker: the seed does not match the commitment: "+
			"published %s, seed hashes to %s", commitment, got)
	}

	shuffle := &Shuffle{serverSeed: seed, Commitment: got, ClientSeed: clientSeed, Nonce: nonce}
	return shuffle.Deck(), nil
}

// keystream is an endless deterministic byte source derived from the seeds.
type keystream struct {
	serverSeed []byte
	message    string
	counter    uint64
	block      []byte
	offset     int
}

func newKeystream(serverSeed []byte, clientSeed string, nonce int64) *keystream {
	return &keystream{
		serverSeed: serverSeed,
		message:    fmt.Sprintf("%s|%d|", clientSeed, nonce),
	}
}

// next returns the next byte, generating another HMAC block when needed.
func (k *keystream) next() byte {
	if k.offset >= len(k.block) {
		mac := hmac.New(sha256.New, k.serverSeed)
		mac.Write([]byte(k.message))
		var counter [8]byte
		binary.BigEndian.PutUint64(counter[:], k.counter)
		mac.Write(counter[:])
		k.block = mac.Sum(nil)
		k.offset = 0
		k.counter++
	}
	value := k.block[k.offset]
	k.offset++
	return value
}

// boundedIndex returns a uniform value in [0, n).
//
// Rejection sampling rather than a modulo. Taking a byte mod 52 would make the
// first twelve cards slightly likelier to be chosen at each step, which is a
// small bias in the house's gift and exactly the kind of thing this package
// exists to rule out.
func (k *keystream) boundedIndex(n int) int {
	if n <= 1 {
		return 0
	}
	// Read enough bytes to cover n, then reject anything in the ragged tail.
	var width uint
	for value := n - 1; value > 0; value >>= 8 {
		width++
	}
	limit := uint64(1) << (8 * width)
	bound := limit - (limit % uint64(n))

	for {
		var value uint64
		for i := uint(0); i < width; i++ {
			value = value<<8 | uint64(k.next())
		}
		if value < bound {
			return int(value % uint64(n))
		}
		// Landed in the tail that would skew the distribution: draw again.
	}
}

// CombineClientSeeds folds every seated player's seed into one, so no single
// player and not the house alone decides the deck.
//
// The seats are joined in a fixed order and the result hashed, so a player
// cannot choose their seed to cancel out another's.
func CombineClientSeeds(seeds []string) string {
	hasher := sha256.New()
	for _, seed := range seeds {
		hasher.Write([]byte(seed))
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// NewClientSeed generates a random seed for a player who has not set one.
func NewClientSeed() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("poker: draw client seed: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
