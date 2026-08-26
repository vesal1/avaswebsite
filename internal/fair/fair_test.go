package fair

import (
	"math"
	"strings"
	"testing"
)

func TestCommitmentMatchesRevealedSeed(t *testing.T) {
	seed, err := NewSeed("player", 1)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Revealed() {
		t.Fatal("a fresh seed must not start out revealed")
	}
	revealed := seed.Reveal()
	if !seed.Revealed() {
		t.Fatal("Reveal must mark the seed revealed")
	}
	if err := CheckCommitment(seed.Commitment, revealed); err != nil {
		t.Fatalf("the published commitment did not match its own seed: %v", err)
	}
}

func TestCheckCommitmentRejectsASubstitutedSeed(t *testing.T) {
	honest, _ := NewSeed("player", 1)
	swapped, _ := NewSeed("player", 1)

	err := CheckCommitment(honest.Commitment, swapped.Reveal())
	if err == nil {
		t.Fatal("a seed that does not hash to the commitment was accepted")
	}
	if !strings.Contains(err.Error(), "does not match the commitment") {
		t.Errorf("unhelpful error for a broken commitment: %v", err)
	}
}

func TestCheckCommitmentToleratesCasingAndSpace(t *testing.T) {
	seed, _ := NewSeed("player", 1)
	published := "  " + strings.ToUpper(seed.Commitment) + "\n"
	if err := CheckCommitment(published, " "+seed.Reveal()+" "); err != nil {
		t.Errorf("a copy-pasted commitment should still verify: %v", err)
	}
}

func TestServerSeedHexDoesNotReveal(t *testing.T) {
	// Persisting a seed mid-round must not mark the round's secret as public,
	// or the table would offer it up while the hand is still being played.
	seed, _ := NewSeed("player", 1)
	if _ = seed.ServerSeedHex(); seed.Revealed() {
		t.Fatal("reading the seed for storage revealed it")
	}
}

func TestStreamIsDeterministicAndSeedDependent(t *testing.T) {
	seed, _ := NewSeed("player", 1)
	hex := seed.Reveal()

	first, _ := RestoreSeed(hex, "player", 1)
	second, _ := RestoreSeed(hex, "player", 1)
	if got, want := draw(first, 64), draw(second, 64); got != want {
		t.Error("the same seed produced two different streams")
	}

	other, _ := RestoreSeed(hex, "player", 2)
	if draw(first, 64) == draw(other, 64) {
		t.Error("changing the nonce did not change the stream")
	}
	elsewhere, _ := RestoreSeed(hex, "someone-else", 1)
	if draw(first, 64) == draw(elsewhere, 64) {
		t.Error("changing the client seed did not change the stream")
	}
}

func TestStreamRestartsFromTheBeginning(t *testing.T) {
	// A round has to be replayable as often as an auditor cares to replay it.
	seed, _ := NewSeed("player", 1)
	if draw(seed, 128) != draw(seed, 128) {
		t.Error("a second Stream() did not reproduce the first")
	}
}

func draw(seed *Seed, n int) string {
	stream := seed.Stream()
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(stream.Next())
	}
	return b.String()
}

func TestBoundedIndexStaysInRange(t *testing.T) {
	seed, _ := NewSeed("range", 1)
	stream := seed.Stream()
	for _, n := range []int{0, 1, 2, 3, 52, 100, 255, 256, 257, 1000, 70000} {
		for i := 0; i < 500; i++ {
			got := stream.BoundedIndex(n)
			if n <= 1 {
				if got != 0 {
					t.Fatalf("BoundedIndex(%d) = %d, want 0", n, got)
				}
				continue
			}
			if got < 0 || got >= n {
				t.Fatalf("BoundedIndex(%d) = %d, outside [0,%d)", n, got, n)
			}
		}
	}
}

// TestBoundedIndexIsUnbiased is the test that matters. A modulo of a single
// byte over 100 would over-weight the first 56 values by about 50%, which is
// exactly the sort of quiet edge this package exists to rule out.
func TestBoundedIndexIsUnbiased(t *testing.T) {
	const (
		n      = 100 // deliberately not a power of two
		trials = 400_000
	)
	counts := make([]int, n)
	seed, _ := NewSeed("bias-check", 1)
	stream := seed.Stream()
	for i := 0; i < trials; i++ {
		counts[stream.BoundedIndex(n)]++
	}

	expected := float64(trials) / float64(n)
	chi := 0.0
	for _, count := range counts {
		diff := float64(count) - expected
		chi += diff * diff / expected
	}
	// 99 degrees of freedom: the 0.999 critical value is about 148. A modulo
	// bias here would score in the thousands.
	if chi > 148 {
		t.Errorf("chi-squared %.1f over %d degrees of freedom: the draw is not uniform", chi, n-1)
	}
}

func TestWeightedFollowsTheWeights(t *testing.T) {
	weights := []int{50, 30, 0, 20}
	const trials = 200_000
	counts := make([]int, len(weights))

	seed, _ := NewSeed("weights", 1)
	stream := seed.Stream()
	for i := 0; i < trials; i++ {
		idx, err := stream.Weighted(weights)
		if err != nil {
			t.Fatal(err)
		}
		counts[idx]++
	}

	if counts[2] != 0 {
		t.Errorf("a zero weight was picked %d times", counts[2])
	}
	total := 0
	for _, weight := range weights {
		total += weight
	}
	for i, count := range counts {
		want := float64(trials) * float64(weights[i]) / float64(total)
		if want == 0 {
			continue
		}
		if math.Abs(float64(count)-want)/want > 0.02 {
			t.Errorf("index %d drawn %d times, expected about %.0f", i, count, want)
		}
	}
}

func TestWeightedRejectsImpossibleWeights(t *testing.T) {
	seed, _ := NewSeed("weights", 1)
	stream := seed.Stream()
	if _, err := stream.Weighted([]int{0, 0}); err == nil {
		t.Error("weights summing to zero should be an error, not a silent pick")
	}
	if _, err := stream.Weighted([]int{5, -1}); err == nil {
		t.Error("a negative weight should be an error")
	}
	if _, err := stream.Weighted(nil); err == nil {
		t.Error("no weights should be an error")
	}
}

func TestPermutationIsAPermutation(t *testing.T) {
	seed, _ := NewSeed("perm", 1)
	got := seed.Stream().Permutation(52)
	if len(got) != 52 {
		t.Fatalf("got %d entries, want 52", len(got))
	}
	seen := make(map[int]bool, 52)
	for _, value := range got {
		if value < 0 || value >= 52 {
			t.Fatalf("value %d outside [0,52)", value)
		}
		if seen[value] {
			t.Fatalf("value %d appears twice", value)
		}
		seen[value] = true
	}
}

// TestPermutationCoversEveryPosition checks that no position is stuck. A
// Fisher-Yates written with the wrong bound leaves the last element in place
// far too often, and that is invisible unless you count.
func TestPermutationCoversEveryPosition(t *testing.T) {
	const (
		n      = 6
		trials = 60_000
	)
	fixed := make([]int, n)
	for nonce := 0; nonce < trials; nonce++ {
		seed, _ := RestoreSeed("aabb", "cover", int64(nonce))
		for position, value := range seed.Stream().Permutation(n) {
			if position == value {
				fixed[position]++
			}
		}
	}
	want := float64(trials) / float64(n)
	for position, count := range fixed {
		if math.Abs(float64(count)-want)/want > 0.06 {
			t.Errorf("position %d held its own value %d times, expected about %.0f",
				position, count, want)
		}
	}
}

func TestCombineClientSeeds(t *testing.T) {
	if CombineClientSeeds([]string{"alice", "bob"}) == CombineClientSeeds([]string{"bob", "alice"}) {
		t.Error("order must matter, or a player could cancel another's seed")
	}
	if CombineClientSeeds([]string{"ab", "c"}) == CombineClientSeeds([]string{"a", "bc"}) {
		t.Error("the separator is not doing its job")
	}
	if len(CombineClientSeeds(nil)) != 64 {
		t.Error("combining no seeds should still give a usable seed")
	}
}

func TestNewClientSeedIsRandom(t *testing.T) {
	first, err := NewClientSeed()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := NewClientSeed()
	if first == second {
		t.Fatal("two client seeds came out identical")
	}
	if len(first) != 32 {
		t.Errorf("client seed is %d hex chars, want 32", len(first))
	}
}

func TestRestoreSeedRejectsRubbish(t *testing.T) {
	for _, input := range []string{"", "nothex", "zz"} {
		if _, err := RestoreSeed(input, "c", 1); err == nil {
			t.Errorf("RestoreSeed(%q) should have failed", input)
		}
	}
}
