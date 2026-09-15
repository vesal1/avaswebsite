// Package money provides exact arithmetic for balances and prices.
//
// Two rules hold throughout the sportsbook:
//
//  1. Money is an int64 count of satoshi. Floating point never touches a
//     balance, a stake or a payout.
//  2. Odds are decimal odds stored as integer thousandths ("milli-odds"):
//     decimal 2.50 is stored as 2500. Every other odds format is a display
//     conversion applied at the edge.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const (
	// SatPerBTC is the number of satoshi in one bitcoin.
	SatPerBTC int64 = 100_000_000
	// OddsScale is the fixed-point scale for milli-odds.
	OddsScale int64 = 1000
	// MinOddsMilli is 1.001. Anything at or below evens-with-no-profit is
	// rejected: it cannot be priced or settled meaningfully.
	MinOddsMilli int64 = 1001
	// MaxOddsMilli is 10000.00, the ceiling on any single or combined price.
	MaxOddsMilli int64 = 10_000_000
)

// ErrMoney is the base for every arithmetic rejection in this package.
var ErrMoney = errors.New("money")

func fail(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMoney, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// satoshi helpers
// ---------------------------------------------------------------------------

// ParseBTC converts a decimal BTC string to satoshi. Sub-satoshi precision is
// rejected rather than silently rounded, because a rounded deposit is a
// reconciliation break.
func ParseBTC(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fail("empty BTC amount")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 8 {
		trimmed := strings.TrimRight(frac[8:], "0")
		if trimmed != "" {
			return 0, fail("%s BTC is finer than one satoshi", s)
		}
		frac = frac[:8]
	}
	frac += strings.Repeat("0", 8-len(frac))
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fail("%q is not a decimal BTC amount", s)
			}
		}
	}
	wholeVal, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fail("BTC amount %q is out of range", s)
	}
	fracVal, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fail("BTC amount %q is out of range", s)
	}
	if wholeVal > math.MaxInt64/SatPerBTC {
		return 0, fail("BTC amount %q overflows", s)
	}
	sat := wholeVal*SatPerBTC + fracVal
	if neg {
		sat = -sat
	}
	return sat, nil
}

// FormatBTC renders satoshi as a fixed 8-decimal BTC string.
func FormatBTC(sat int64) string {
	sign := ""
	if sat < 0 {
		sign, sat = "-", -sat
	}
	return fmt.Sprintf("%s%d.%08d", sign, sat/SatPerBTC, sat%SatPerBTC)
}

// FormatSat renders satoshi with thousands separators, e.g. "1,250,000 sat".
func FormatSat(sat int64) string {
	sign := ""
	if sat < 0 {
		sign, sat = "-", -sat
	}
	digits := strconv.FormatInt(sat, 10)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String() + " sat"
}

// ToFiatCents values sat at ratePerBTC whole fiat units per BTC, rounding to
// the nearest cent. Display only: balances are never stored in fiat.
func ToFiatCents(sat int64, ratePerBTC int64) int64 {
	if ratePerBTC <= 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(sat), big.NewInt(ratePerBTC))
	num.Mul(num, big.NewInt(100))
	return divRoundHalfUp(num, big.NewInt(SatPerBTC)).Int64()
}

// FormatFiat renders integer cents with a currency suffix.
func FormatFiat(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	whole := strconv.FormatInt(cents/100, 10)
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("%s%s.%02d %s", sign, b.String(), cents%100, currency)
}

// ---------------------------------------------------------------------------
// odds conversion
// ---------------------------------------------------------------------------

// ValidateOdds enforces the tradable price range.
func ValidateOdds(milli int64) error {
	if milli < MinOddsMilli {
		return fail("odds %d are below the minimum %d", milli, MinOddsMilli)
	}
	if milli > MaxOddsMilli {
		return fail("odds %d exceed the maximum %d", milli, MaxOddsMilli)
	}
	return nil
}

// ParseDecimalOdds converts a decimal odds string such as "2.50" to milli-odds.
// It truncates rather than rounds up, so a rounding edge never favours the
// customer at the book's expense.
func ParseDecimalOdds(s string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 3 {
		frac = frac[:3]
	}
	frac += strings.Repeat("0", 3-len(frac))
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fail("%q is not decimal odds", s)
			}
		}
	}
	wholeVal, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fail("odds %q are out of range", s)
	}
	fracVal, _ := strconv.ParseInt(frac, 10, 64)
	milli := wholeVal*OddsScale + fracVal
	if err := ValidateOdds(milli); err != nil {
		return 0, err
	}
	return milli, nil
}

// FormatDecimalOdds renders milli-odds as a two-decimal price.
func FormatDecimalOdds(milli int64) string {
	return fmt.Sprintf("%d.%02d", milli/OddsScale, (milli%OddsScale)/10)
}

// AmericanToMilli converts a moneyline price: -110 becomes 1909, +250 becomes 3500.
func AmericanToMilli(american int64) (int64, error) {
	if american == 0 {
		return 0, fail("0 is not a valid American price")
	}
	var milli int64
	if american > 0 {
		// 1 + american/100
		milli = OddsScale + american*OddsScale/100
	} else {
		// 1 + 100/-american
		milli = OddsScale + 100*OddsScale/(-american)
	}
	if err := ValidateOdds(milli); err != nil {
		return 0, err
	}
	return milli, nil
}

// MilliToAmerican is the inverse of AmericanToMilli, rounded to a whole price.
func MilliToAmerican(milli int64) int64 {
	if milli >= 2*OddsScale {
		return divRoundHalfUpInt((milli-OddsScale)*100, OddsScale)
	}
	return -divRoundHalfUpInt(100*OddsScale, milli-OddsScale)
}

// MilliToFractional renders milli-odds as the smallest exact fraction, e.g.
// 2500 becomes "3/2".
func MilliToFractional(milli int64) string {
	num, den := milli-OddsScale, OddsScale
	g := gcd(num, den)
	if g == 0 {
		return "0/1"
	}
	return fmt.Sprintf("%d/%d", num/g, den/g)
}

// ParseFractionalOdds converts "3/2" to milli-odds.
func ParseFractionalOdds(s string) (int64, error) {
	numStr, denStr, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, fail("%q is not a fractional price", s)
	}
	num, err1 := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	den, err2 := strconv.ParseInt(strings.TrimSpace(denStr), 10, 64)
	if err1 != nil || err2 != nil || den == 0 {
		return 0, fail("%q is not a fractional price", s)
	}
	milli := OddsScale + num*OddsScale/den
	if err := ValidateOdds(milli); err != nil {
		return 0, err
	}
	return milli, nil
}

// OddsFormat names a customer-facing price display style.
type OddsFormat string

const (
	FormatDecimal    OddsFormat = "decimal"
	FormatAmerican   OddsFormat = "american"
	FormatFractional OddsFormat = "fractional"
)

// Format renders milli-odds in the customer's chosen style.
func Format(milli int64, style OddsFormat) string {
	switch style {
	case FormatAmerican:
		american := MilliToAmerican(milli)
		if american > 0 {
			return fmt.Sprintf("+%d", american)
		}
		return strconv.FormatInt(american, 10)
	case FormatFractional:
		return MilliToFractional(milli)
	default:
		return FormatDecimalOdds(milli)
	}
}

// ---------------------------------------------------------------------------
// stake, payout and pricing
// ---------------------------------------------------------------------------

// Payout is the total returned to the customer on a win, stake included.
// Fractional satoshi are truncated: the book never pays a partial satoshi.
func Payout(stakeSat, oddsMilli int64) (int64, error) {
	if stakeSat < 0 {
		return 0, fail("stake cannot be negative")
	}
	if err := ValidateOdds(oddsMilli); err != nil {
		return 0, err
	}
	product := new(big.Int).Mul(big.NewInt(stakeSat), big.NewInt(oddsMilli))
	product.Div(product, big.NewInt(OddsScale))
	if !product.IsInt64() {
		return 0, fail("payout overflows int64")
	}
	return product.Int64(), nil
}

// Profit is the winnings excluding the returned stake.
func Profit(stakeSat, oddsMilli int64) (int64, error) {
	payout, err := Payout(stakeSat, oddsMilli)
	if err != nil {
		return 0, err
	}
	return payout - stakeSat, nil
}

// CombineOdds multiplies parlay legs, keeping full precision until the final
// truncation. The result is capped at MaxOddsMilli.
func CombineOdds(legs []int64) (int64, error) {
	if len(legs) == 0 {
		return 0, fail("a parlay needs at least one leg")
	}
	acc := big.NewInt(OddsScale)
	for _, milli := range legs {
		if err := ValidateOdds(milli); err != nil {
			return 0, err
		}
		acc.Mul(acc, big.NewInt(milli))
		acc.Div(acc, big.NewInt(OddsScale))
		if acc.Cmp(big.NewInt(MaxOddsMilli)) > 0 {
			return MaxOddsMilli, nil
		}
	}
	return acc.Int64(), nil
}

// ImpliedProbabilityBps is the implied probability in basis points. Summed over
// a market's selections it gives the book percentage: 10000 is a fair book and
// anything above carries the operator's margin.
func ImpliedProbabilityBps(milli int64) (int64, error) {
	if err := ValidateOdds(milli); err != nil {
		return 0, err
	}
	return divRoundHalfUpInt(10_000*OddsScale, milli), nil
}

// BookPercentageBps sums the implied probabilities of a full market.
func BookPercentageBps(market []int64) (int64, error) {
	var total int64
	for _, milli := range market {
		bps, err := ImpliedProbabilityBps(milli)
		if err != nil {
			return 0, err
		}
		total += bps
	}
	return total, nil
}

// ApplyMargin turns fair prices into offered prices carrying marginBps of
// overround. Margin is applied proportionally, so every fair probability is
// scaled by the same factor and the shape of the market is preserved.
func ApplyMargin(fairMilli []int64, marginBps int64) ([]int64, error) {
	if marginBps < 0 {
		return nil, fail("margin cannot be negative")
	}
	priced := make([]int64, 0, len(fairMilli))
	for _, milli := range fairMilli {
		if err := ValidateOdds(milli); err != nil {
			return nil, err
		}
		// offered = fair * 10000 / (10000 + margin)
		offered := big.NewInt(milli)
		offered.Mul(offered, big.NewInt(10_000))
		offered.Div(offered, big.NewInt(10_000+marginBps))
		value := offered.Int64()
		if value < MinOddsMilli {
			value = MinOddsMilli
		}
		priced = append(priced, value)
	}
	return priced, nil
}

// OddsFromWeights turns arbitrary positive weights (a model's opinion, a
// trader's numbers) into a priced market carrying marginBps of overround.
func OddsFromWeights(weights []int64, marginBps int64) ([]int64, error) {
	if len(weights) == 0 {
		return nil, fail("a market needs at least one outcome")
	}
	var total int64
	for _, w := range weights {
		if w <= 0 {
			return nil, fail("every outcome needs a positive weight")
		}
		total += w
	}
	fair := make([]int64, 0, len(weights))
	for _, w := range weights {
		// fair decimal odds = total/w, in milli-odds
		milli := total * OddsScale / w
		if milli < MinOddsMilli {
			milli = MinOddsMilli
		}
		if milli > MaxOddsMilli {
			milli = MaxOddsMilli
		}
		fair = append(fair, milli)
	}
	return ApplyMargin(fair, marginBps)
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func divRoundHalfUpInt(num, den int64) int64 {
	return divRoundHalfUp(big.NewInt(num), big.NewInt(den)).Int64()
}

func divRoundHalfUp(num, den *big.Int) *big.Int {
	neg := num.Sign() < 0 != (den.Sign() < 0)
	n := new(big.Int).Abs(num)
	d := new(big.Int).Abs(den)
	if d.Sign() == 0 {
		return big.NewInt(0)
	}
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	r.Mul(r, big.NewInt(2))
	if r.Cmp(d) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	return q
}
