// Package fair holds the provably fair random number generation shared by
// every game in the house: poker's shuffle, the slot reels, and anything added
// later.
//
// There is one implementation on purpose. A second copy of this code, however
// carefully written, is a second thing an auditor has to read and a second
// place a bias can hide. Games differ in what they do with the numbers, never
// in where the numbers come from.
//
// The scheme is commit-reveal:
//
//  1. Before a round the server draws a secret 32-byte seed and publishes only
//     SHA-256 of it. That hash is the commitment, and it is shown to players
//     before anything is dealt or spun.
//  2. Players contribute a client seed. The outcome is derived from both, so
//     neither side alone chooses it.
//  3. When the round is settled the server publishes the secret seed. Anybody
//     can hash it, compare against the commitment they were shown first, and
//     replay the round to the same result.
//
// The house therefore cannot see the outcome and change it, cannot choose a
// seed after seeing the players' cards, and cannot deal anything other than
// what the seed dictates. Any of those would break a commitment that was
// already published.
package fair

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

// SeedBytes is the length of a server seed.
const SeedBytes = 32

// Seed is a committed server seed together with the client seed and nonce that
// specialise it to one round.
type Seed struct {
	// serverSeed is secret until the round is over.
	serverSeed []byte
	// Commitment is the hex SHA-256 of the server seed, published up front.
	Commitment string
	// ClientSeed is the players' contribution.
	ClientSeed string
	// Nonce distinguishes rounds that share a server seed.
	Nonce int64
	// revealed is set once the seed has been published.
	revealed bool
}

// NewSeed draws a fresh server seed and commits to it.
func NewSeed(clientSeed string, nonce int64) (*Seed, error) {
	seed := make([]byte, SeedBytes)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("fair: draw server seed: %w", err)
	}
	return &Seed{
		serverSeed: seed,
		Commitment: Commit(seed),
		ClientSeed: clientSeed,
		Nonce:      nonce,
	}, nil
}

// RestoreSeed rebuilds a seed from a known server seed, for verification and
// for replaying a round from its history. The result counts as revealed: the
// secret is already in the caller's hands.
func RestoreSeed(serverSeedHex, clientSeed string, nonce int64) (*Seed, error) {
	seed, err := decodeSeed(serverSeedHex)
	if err != nil {
		return nil, err
	}
	return &Seed{
		serverSeed: seed,
		Commitment: Commit(seed),
		ClientSeed: clientSeed,
		Nonce:      nonce,
		revealed:   true,
	}, nil
}

// Commit returns the published form of a server seed.
func Commit(serverSeed []byte) string {
	sum := sha256.Sum256(serverSeed)
	return hex.EncodeToString(sum[:])
}

// Reveal publishes the server seed, ending the round's secrecy.
func (s *Seed) Reveal() string {
	s.revealed = true
	return hex.EncodeToString(s.serverSeed)
}

// Revealed reports whether the seed has been published.
func (s *Seed) Revealed() bool { return s.revealed }

// ServerSeedHex returns the secret without marking it revealed. Callers that
// persist a seed mid-round use this; anything player-facing uses Reveal.
func (s *Seed) ServerSeedHex() string { return hex.EncodeToString(s.serverSeed) }

// Stream opens the deterministic byte source for this round. Each call starts
// again from the beginning, so a round can be replayed as often as needed.
func (s *Seed) Stream() *Stream {
	return &Stream{
		serverSeed: s.serverSeed,
		message:    fmt.Sprintf("%s|%d|", s.ClientSeed, s.Nonce),
	}
}

// CheckCommitment reports whether a revealed seed matches the commitment a
// player was shown before the round.
//
// The comparison is constant time so that verification cannot be turned into
// an oracle for guessing a seed that has not been revealed yet.
func CheckCommitment(commitment, serverSeedHex string) error {
	seed, err := decodeSeed(serverSeedHex)
	if err != nil {
		return err
	}
	got := Commit(seed)
	want := strings.ToLower(strings.TrimSpace(commitment))
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("fair: the seed does not match the commitment: "+
			"published %s, seed hashes to %s", commitment, got)
	}
	return nil
}

func decodeSeed(serverSeedHex string) ([]byte, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(serverSeedHex))
	if err != nil || len(seed) == 0 {
		return nil, fmt.Errorf("fair: %q is not a server seed", serverSeedHex)
	}
	return seed, nil
}

// CombineClientSeeds folds several players' seeds into one, so no single
// player and not the house alone decides the outcome.
//
// The seeds are joined in the caller's order with a separator and hashed, so a
// player who sees another's seed cannot pick one that cancels it out.
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
		return "", fmt.Errorf("fair: draw client seed: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ---------------------------------------------------------------------------
// Keystream
// ---------------------------------------------------------------------------

// Stream is an endless deterministic byte source derived from a seed. It is
// HMAC-SHA256 over an incrementing counter, which is the construction players
// are asked to reimplement when they check a round.
//
// A Stream is not safe for concurrent use; each round draws from its own.
type Stream struct {
	serverSeed []byte
	message    string
	counter    uint64
	block      []byte
	offset     int
}

// Next returns the next byte, generating another HMAC block when needed.
func (s *Stream) Next() byte {
	if s.offset >= len(s.block) {
		mac := hmac.New(sha256.New, s.serverSeed)
		mac.Write([]byte(s.message))
		var counter [8]byte
		binary.BigEndian.PutUint64(counter[:], s.counter)
		mac.Write(counter[:])
		s.block = mac.Sum(nil)
		s.offset = 0
		s.counter++
	}
	value := s.block[s.offset]
	s.offset++
	return value
}

// BoundedIndex returns a uniform value in [0, n).
//
// Rejection sampling rather than a modulo. Taking a byte mod 52 would make the
// first twelve cards slightly likelier to be chosen at each step, and folding
// a byte into a 100-stop reel strip would over-weight the first 56 stops.
// Either is a small bias in the house's gift, and ruling those out is the
// entire reason this package exists.
func (s *Stream) BoundedIndex(n int) int {
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
			value = value<<8 | uint64(s.Next())
		}
		if value < bound {
			return int(value % uint64(n))
		}
		// Landed in the tail that would skew the distribution: draw again.
	}
}

// Weighted picks an index in proportion to the given weights, drawing one
// unbiased number below the total. Weights must be non-negative and sum to a
// positive value; a zero weight is never picked.
//
// Slot machines use this for reel strips written as symbol weights rather than
// as a physical strip of stops.
func (s *Stream) Weighted(weights []int) (int, error) {
	total := 0
	for i, weight := range weights {
		if weight < 0 {
			return 0, fmt.Errorf("fair: weight %d at index %d is negative", weight, i)
		}
		total += weight
	}
	if total <= 0 {
		return 0, fmt.Errorf("fair: weights sum to %d, nothing to pick", total)
	}
	roll := s.BoundedIndex(total)
	for i, weight := range weights {
		if roll < weight {
			return i, nil
		}
		roll -= weight
	}
	// Unreachable: roll is below the total, so some weight always claims it.
	return len(weights) - 1, nil
}

// Permutation returns the numbers [0, n) in a uniformly random order, using
// Fisher-Yates from the top down.
func (s *Stream) Permutation(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	s.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// Shuffle permutes n items by calling swap, exactly as poker shuffles a deck.
//
// Every permutation is equally likely provided each index is drawn without
// bias, which is what BoundedIndex is for.
func (s *Stream) Shuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		j := s.BoundedIndex(i + 1)
		swap(i, j)
	}
}
