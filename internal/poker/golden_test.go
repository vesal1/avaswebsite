package poker

import "testing"

// TestGoldenDeck pins the exact output of the shuffle for a known seed.
//
// The algorithm is published on the verification page and players are invited
// to reimplement it. That makes it a compatibility surface: changing it, even
// by refactoring, would silently invalidate every hand history anybody has
// checked. This test fails if the bytes ever move.
func TestGoldenDeck(t *testing.T) {
	shuffle, err := RestoreShuffle("00112233445566778899aabbccddeeff", "golden-client-seed", 7)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Jc 5s 2h 6d Kc Qd Js 2d 4d 9s Ac 8s 8d 3c 6c 4c 9h 7h 5h Ad " +
		"2s 3s Ts As 8c Qh 2c Jd 4s Kd 4h 5d Td 3d Ks Ah Tc 7s 6h Qs Jh 5c " +
		"9c 7d 6s 3h Th 7c Qc 8h 9d Kh"
	if got := FormatCards(shuffle.Deck()); got != want {
		t.Errorf("the shuffle output has changed.\n got: %s\nwant: %s", got, want)
	}
}
