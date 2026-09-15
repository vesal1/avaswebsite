package money

import "testing"

func TestParseBTC(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1", SatPerBTC, true},
		{"0.00000001", 1, true},
		{"1.23456789", 123_456_789, true},
		{"0.5", 50_000_000, true},
		{"-0.001", -100_000, true},
		{"21000000", 21_000_000 * SatPerBTC, true},
		{"1.234567890", 123_456_789, true}, // trailing zero beyond 8dp is fine
		{"0.000000001", 0, false},          // sub-satoshi is rejected outright
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, err := ParseBTC(tc.in)
		if tc.ok != (err == nil) {
			t.Fatalf("ParseBTC(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
		}
		if tc.ok && got != tc.want {
			t.Errorf("ParseBTC(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatBTCRoundTrip(t *testing.T) {
	for _, sat := range []int64{0, 1, 99_999_999, SatPerBTC, 123_456_789, -5_000} {
		got, err := ParseBTC(FormatBTC(sat))
		if err != nil {
			t.Fatalf("round trip of %d failed: %v", sat, err)
		}
		if got != sat {
			t.Errorf("round trip of %d gave %d", sat, got)
		}
	}
}

func TestAmericanOdds(t *testing.T) {
	cases := []struct {
		american int64
		milli    int64
	}{
		{-110, 1909},
		{100, 2000},
		{250, 3500},
		{-200, 1500},
		{500, 6000},
	}
	for _, tc := range cases {
		got, err := AmericanToMilli(tc.american)
		if err != nil {
			t.Fatalf("AmericanToMilli(%d): %v", tc.american, err)
		}
		if got != tc.milli {
			t.Errorf("AmericanToMilli(%d) = %d, want %d", tc.american, got, tc.milli)
		}
	}
	// Round trips back to the same moneyline for exact prices.
	for _, american := range []int64{-200, 100, 250, 500} {
		milli, _ := AmericanToMilli(american)
		if back := MilliToAmerican(milli); back != american {
			t.Errorf("MilliToAmerican(%d) = %d, want %d", milli, back, american)
		}
	}
}

func TestFractionalOdds(t *testing.T) {
	cases := []struct {
		milli int64
		frac  string
	}{
		{2500, "3/2"},
		{2000, "1/1"},
		{1500, "1/2"},
		{6000, "5/1"},
		{1250, "1/4"},
	}
	for _, tc := range cases {
		if got := MilliToFractional(tc.milli); got != tc.frac {
			t.Errorf("MilliToFractional(%d) = %s, want %s", tc.milli, got, tc.frac)
		}
		back, err := ParseFractionalOdds(tc.frac)
		if err != nil {
			t.Fatalf("ParseFractionalOdds(%s): %v", tc.frac, err)
		}
		if back != tc.milli {
			t.Errorf("ParseFractionalOdds(%s) = %d, want %d", tc.frac, back, tc.milli)
		}
	}
}

func TestPayoutTruncatesTowardTheBook(t *testing.T) {
	// 3 sat at 1.333 is 3.999 sat: the customer gets 3, never 4.
	got, err := Payout(3, 1333)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("Payout(3, 1.333) = %d, want 3", got)
	}
	got, err = Payout(100_000, 2500)
	if err != nil {
		t.Fatal(err)
	}
	if got != 250_000 {
		t.Errorf("Payout(100000, 2.50) = %d, want 250000", got)
	}
	if profit, _ := Profit(100_000, 2500); profit != 150_000 {
		t.Errorf("Profit(100000, 2.50) = %d, want 150000", profit)
	}
}

func TestPayoutRejectsBadInput(t *testing.T) {
	if _, err := Payout(-1, 2000); err == nil {
		t.Error("negative stake should be rejected")
	}
	if _, err := Payout(100, 1000); err == nil {
		t.Error("odds of 1.00 should be rejected")
	}
	if _, err := Payout(100, MaxOddsMilli+1); err == nil {
		t.Error("odds above the ceiling should be rejected")
	}
}

func TestCombineOdds(t *testing.T) {
	// 2.00 * 1.50 * 3.00 = 9.00
	got, err := CombineOdds([]int64{2000, 1500, 3000})
	if err != nil {
		t.Fatal(err)
	}
	if got != 9000 {
		t.Errorf("CombineOdds = %d, want 9000", got)
	}
	if _, err := CombineOdds(nil); err == nil {
		t.Error("an empty parlay should be rejected")
	}
	// A long shot parlay is capped rather than overflowing.
	legs := make([]int64, 40)
	for i := range legs {
		legs[i] = 5000
	}
	capped, err := CombineOdds(legs)
	if err != nil {
		t.Fatal(err)
	}
	if capped != MaxOddsMilli {
		t.Errorf("CombineOdds capped = %d, want %d", capped, MaxOddsMilli)
	}
}

func TestMarginProducesOverround(t *testing.T) {
	fair := []int64{2000, 2000}
	if book, _ := BookPercentageBps(fair); book != 10_000 {
		t.Fatalf("fair two-way book = %d bps, want 10000", book)
	}
	priced, err := ApplyMargin(fair, 500)
	if err != nil {
		t.Fatal(err)
	}
	book, err := BookPercentageBps(priced)
	if err != nil {
		t.Fatal(err)
	}
	// 5% margin should land the book near 10500 bps, and always above fair.
	if book <= 10_000 {
		t.Errorf("priced book = %d bps, want more than 10000", book)
	}
	if book < 10_400 || book > 10_600 {
		t.Errorf("priced book = %d bps, want roughly 10500", book)
	}
	for i := range priced {
		if priced[i] >= fair[i] {
			t.Errorf("offered price %d is not shorter than fair %d", priced[i], fair[i])
		}
	}
}

func TestOddsFromWeights(t *testing.T) {
	// A 3-way market where the home side is twice as likely as the draw.
	priced, err := OddsFromWeights([]int64{50, 25, 25}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(priced) != 3 {
		t.Fatalf("got %d prices, want 3", len(priced))
	}
	if priced[0] >= priced[1] {
		t.Error("the favourite should be priced shorter than the draw")
	}
	if priced[1] != priced[2] {
		t.Error("equal weights should produce equal prices")
	}
	book, _ := BookPercentageBps(priced)
	if book <= 10_000 {
		t.Errorf("book = %d bps, want an overround", book)
	}
	if _, err := OddsFromWeights([]int64{1, 0}, 500); err == nil {
		t.Error("a zero weight should be rejected")
	}
}

func TestFiatDisplay(t *testing.T) {
	// 0.001 BTC at 60,000/BTC is 60.00.
	if cents := ToFiatCents(100_000, 60_000); cents != 6_000 {
		t.Errorf("ToFiatCents = %d, want 6000", cents)
	}
	if got := FormatFiat(123_456, "USD"); got != "1,234.56 USD" {
		t.Errorf("FormatFiat = %q", got)
	}
	if got := FormatSat(1_250_000); got != "1,250,000 sat" {
		t.Errorf("FormatSat = %q", got)
	}
}
