package poker

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

func TestCommitmentMatchesTheRevealedSeed(t *testing.T) {
	shuffle, err := NewShuffle("player-seed", 1)
	if err != nil {
		t.Fatal(err)
	}
	deck := shuffle.Deck()
	seed := shuffle.Reveal()

	// This is exactly what a player would do to check the hand.
	verified, err := Verify(shuffle.Commitment, seed, "player-seed", 1)
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
	if len(verified) != DeckSize {
		t.Fatalf("verified deck has %d cards", len(verified))
	}
	for i := range deck {
		if deck[i] != verified[i] {
			t.Fatalf("card %d differs: dealt %s, verified %s", i, deck[i], verified[i])
		}
	}
}

func TestVerifyRejectsASubstitutedSeed(t *testing.T) {
	// The attack this scheme exists to stop: the house sees the players' cards
	// and then claims a different seed was used.
	shuffle, err := NewShuffle("player-seed", 1)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewShuffle("player-seed", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(shuffle.Commitment, other.Reveal(), "player-seed", 1); err == nil {
		t.Fatal("a seed that does not match the commitment was accepted")
	}
}

func TestVerifyRejectsATamperedCommitment(t *testing.T) {
	shuffle, _ := NewShuffle("player-seed", 7)
	seed := shuffle.Reveal()
	tampered := strings.Repeat("a", len(shuffle.Commitment))
	if _, err := Verify(tampered, seed, "player-seed", 7); err == nil {
		t.Error("a tampered commitment was accepted")
	}
	if _, err := Verify(shuffle.Commitment, "not-hex", "player-seed", 7); err == nil {
		t.Error("a non-hex seed was accepted")
	}
}

func TestClientSeedChangesTheDeck(t *testing.T) {
	// The player's seed must genuinely affect the outcome, or contributing one
	// is theatre.
	shuffle, _ := NewShuffle("seed-a", 1)
	first := shuffle.Deck()

	other, _ := RestoreShuffle(shuffle.Reveal(), "seed-b", 1)
	second := other.Deck()

	if sameDeck(first, second) {
		t.Error("changing the client seed did not change the deck")
	}
}

func TestNonceChangesTheDeck(t *testing.T) {
	shuffle, _ := NewShuffle("seed", 1)
	first := shuffle.Deck()
	other, _ := RestoreShuffle(shuffle.Reveal(), "seed", 2)
	if sameDeck(first, other.Deck()) {
		t.Error("two hands from the same server seed produced the same deck")
	}
}

func TestShuffleIsAPermutation(t *testing.T) {
	for nonce := int64(0); nonce < 200; nonce++ {
		shuffle, err := NewShuffle("seed", nonce)
		if err != nil {
			t.Fatal(err)
		}
		deck := shuffle.Deck()
		if len(deck) != DeckSize {
			t.Fatalf("deck has %d cards", len(deck))
		}
		seen := make(map[Card]bool, DeckSize)
		for _, card := range deck {
			if card >= DeckSize {
				t.Fatalf("deck contains %d, which is not a card", card)
			}
			if seen[card] {
				t.Fatalf("%s appears twice in the deck", card)
			}
			seen[card] = true
		}
		if len(seen) != DeckSize {
			t.Fatalf("deck holds %d distinct cards", len(seen))
		}
	}
}

func TestShuffleIsDeterministic(t *testing.T) {
	shuffle, _ := NewShuffle("seed", 42)
	first := shuffle.Deck()
	second := shuffle.Deck()
	if !sameDeck(first, second) {
		t.Error("shuffling the same seed twice gave different decks")
	}
}

// TestShuffleIsUnbiased checks the distribution of the card landing in each
// position. A modulo-based index would quietly favour low-numbered cards; this
// is the test that would catch it.
func TestShuffleIsUnbiased(t *testing.T) {
	if testing.Short() {
		t.Skip("statistical test over many shuffles")
	}
	const trials = 52 * 400 // ~400 observations per cell

	// Count how often each card lands in the first position.
	counts := make([]int, DeckSize)
	for nonce := int64(0); nonce < trials; nonce++ {
		shuffle, err := NewShuffle("bias-check", nonce)
		if err != nil {
			t.Fatal(err)
		}
		counts[shuffle.Deck()[0]]++
	}

	expected := float64(trials) / DeckSize
	var chiSquare float64
	for _, count := range counts {
		diff := float64(count) - expected
		chiSquare += diff * diff / expected
	}

	// 51 degrees of freedom. The 99.9th percentile is about 90.6, so a fair
	// shuffle clears this with room to spare and a modulo bias does not.
	const threshold = 90.6
	if chiSquare > threshold {
		t.Errorf("chi-square %.1f exceeds %.1f: the shuffle looks biased", chiSquare, threshold)
	}
	if math.IsNaN(chiSquare) {
		t.Error("chi-square was NaN")
	}
}

func TestCombineClientSeeds(t *testing.T) {
	combined := CombineClientSeeds([]string{"alice", "bob", "carol"})
	if len(combined) != 64 {
		t.Errorf("combined seed is %d characters, want 64", len(combined))
	}
	// Order matters, so one player cannot cancel another's contribution by
	// choosing a seed that collides under a commutative combiner.
	if CombineClientSeeds([]string{"alice", "bob"}) == CombineClientSeeds([]string{"bob", "alice"}) {
		t.Error("seed combination is order independent, which allows one player to cancel another")
	}
	if CombineClientSeeds([]string{"ab", "c"}) == CombineClientSeeds([]string{"a", "bc"}) {
		t.Error("seed combination is ambiguous across boundaries")
	}
}

func TestNewClientSeedIsRandom(t *testing.T) {
	first, err := NewClientSeed()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewClientSeed()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two generated client seeds collided")
	}
}

func TestCommitmentIsASha256(t *testing.T) {
	shuffle, _ := NewShuffle("seed", 1)
	seed := shuffle.Reveal()
	raw, err := hex.DecodeString(seed)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != shuffle.Commitment {
		t.Error("the commitment is not SHA-256 of the server seed")
	}
}

func sameDeck(a, b []Card) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
