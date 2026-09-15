package bitcoin

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// ValidateAddress checks that an address is well formed and belongs to the
// given network.
//
// This matters more than most validation: a payout to a mistyped address is
// unrecoverable. Both address encodings carry a checksum precisely so that a
// typo can be caught before any coins move, so both checksums are verified
// here rather than pattern-matching the shape of the string.
func ValidateAddress(address, network string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return fmt.Errorf("%w: address is empty", ErrInvalidAddress)
	}
	if strings.ContainsAny(address, " \t\r\n") {
		return fmt.Errorf("%w: address contains whitespace", ErrInvalidAddress)
	}
	// Bech32 addresses are case-insensitive but must not be mixed case, which
	// is itself a corruption signal.
	lower := strings.ToLower(address)
	for _, hrp := range bech32Prefixes(network) {
		if strings.HasPrefix(lower, hrp+"1") {
			return validateBech32(address, hrp)
		}
	}
	// Anything that looks like a segwit address for the wrong network is
	// reported as such rather than falling through to a confusing base58 error.
	for _, other := range []string{"bc", "tb", "bcrt"} {
		if strings.HasPrefix(lower, other+"1") {
			return fmt.Errorf("%w: %s is a %s address, not %s", ErrInvalidAddress,
				address, networkForHRP(other), network)
		}
	}
	return validateBase58Check(address, network)
}

func bech32Prefixes(network string) []string {
	switch network {
	case Mainnet:
		return []string{"bc"}
	case Testnet, Signet:
		return []string{"tb"}
	case Regtest:
		return []string{"bcrt"}
	default:
		return nil
	}
}

func networkForHRP(hrp string) string {
	switch hrp {
	case "bc":
		return Mainnet
	case "tb":
		return Testnet
	case "bcrt":
		return Regtest
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// bech32 / bech32m (BIP-173, BIP-350)
// ---------------------------------------------------------------------------

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

const (
	bech32Const  uint32 = 1
	bech32mConst uint32 = 0x2bc830a3
)

func validateBech32(address, hrp string) error {
	if address != strings.ToLower(address) && address != strings.ToUpper(address) {
		return fmt.Errorf("%w: mixed-case bech32 address", ErrInvalidAddress)
	}
	lower := strings.ToLower(address)
	if len(lower) < 14 || len(lower) > 90 {
		return fmt.Errorf("%w: bech32 address has an implausible length", ErrInvalidAddress)
	}
	pos := strings.LastIndex(lower, "1")
	if pos < 1 || pos+7 > len(lower) {
		return fmt.Errorf("%w: malformed bech32 address", ErrInvalidAddress)
	}
	if lower[:pos] != hrp {
		return fmt.Errorf("%w: unexpected human-readable prefix %q", ErrInvalidAddress, lower[:pos])
	}

	data := make([]byte, 0, len(lower)-pos-1)
	for _, r := range lower[pos+1:] {
		idx := strings.IndexRune(bech32Charset, r)
		if idx < 0 {
			return fmt.Errorf("%w: %q is not a bech32 character", ErrInvalidAddress, r)
		}
		data = append(data, byte(idx))
	}
	if len(data) < 6 {
		return fmt.Errorf("%w: bech32 checksum is truncated", ErrInvalidAddress)
	}

	// The witness version selects which checksum constant applies: version 0
	// uses bech32, versions 1 and above use bech32m.
	witnessVersion := data[0]
	if witnessVersion > 16 {
		return fmt.Errorf("%w: witness version %d is out of range", ErrInvalidAddress, witnessVersion)
	}
	want := bech32Const
	if witnessVersion != 0 {
		want = bech32mConst
	}
	if got := bech32Polymod(append(bech32HRPExpand(hrp), data...)); got != want {
		return fmt.Errorf("%w: checksum does not match; the address may be mistyped", ErrInvalidAddress)
	}

	program, err := convertBits(data[1:len(data)-6], 5, 8, false)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}
	if len(program) < 2 || len(program) > 40 {
		return fmt.Errorf("%w: witness program is %d bytes", ErrInvalidAddress, len(program))
	}
	// Version 0 is defined only for P2WPKH (20 bytes) and P2WSH (32 bytes).
	if witnessVersion == 0 && len(program) != 20 && len(program) != 32 {
		return fmt.Errorf("%w: a version 0 witness program must be 20 or 32 bytes", ErrInvalidAddress)
	}
	return nil
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func bech32Polymod(values []byte) uint32 {
	generator := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, value := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(value)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= generator[i]
			}
		}
	}
	return chk
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint
	maxValue := uint32(1)<<to - 1
	var out []byte
	for _, value := range data {
		if uint32(value)>>from != 0 {
			return nil, fmt.Errorf("value %d overflows %d bits", value, from)
		}
		acc = acc<<from | uint32(value)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxValue))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxValue))
		}
	} else if bits >= from || acc<<(to-bits)&maxValue != 0 {
		return nil, fmt.Errorf("invalid padding")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// base58check (legacy P2PKH and P2SH)
// ---------------------------------------------------------------------------

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func validateBase58Check(address, network string) error {
	decoded, err := base58Decode(address)
	if err != nil {
		return err
	}
	if len(decoded) != 25 {
		return fmt.Errorf("%w: decoded address is %d bytes, want 25", ErrInvalidAddress, len(decoded))
	}
	payload, checksum := decoded[:21], decoded[21:]
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	for i := 0; i < 4; i++ {
		if second[i] != checksum[i] {
			return fmt.Errorf("%w: checksum does not match; the address may be mistyped", ErrInvalidAddress)
		}
	}

	version := payload[0]
	allowed := base58Versions(network)
	for _, want := range allowed {
		if version == want {
			return nil
		}
	}
	return fmt.Errorf("%w: version byte 0x%02x does not belong to %s", ErrInvalidAddress, version, network)
}

func base58Versions(network string) []byte {
	switch network {
	case Mainnet:
		return []byte{0x00, 0x05} // P2PKH, P2SH
	case Testnet, Regtest, Signet:
		return []byte{0x6f, 0xc4}
	default:
		return nil
	}
}

func base58Decode(s string) ([]byte, error) {
	// Big-endian base conversion into a byte slice. The input is short enough
	// that the quadratic cost does not matter.
	result := []byte{0}
	for _, r := range s {
		idx := strings.IndexRune(base58Alphabet, r)
		if idx < 0 {
			return nil, fmt.Errorf("%w: %q is not a base58 character", ErrInvalidAddress, r)
		}
		carry := idx
		for i := len(result) - 1; i >= 0; i-- {
			carry += int(result[i]) * 58
			result[i] = byte(carry % 256)
			carry /= 256
		}
		for carry > 0 {
			result = append([]byte{byte(carry % 256)}, result...)
			carry /= 256
		}
	}
	// Each leading '1' in base58 is a leading zero byte.
	var leading int
	for leading < len(s) && s[leading] == '1' {
		leading++
	}
	// Drop the placeholder zero the accumulator started with.
	for len(result) > 1 && result[0] == 0 {
		result = result[1:]
	}
	if len(result) == 1 && result[0] == 0 {
		result = nil
	}
	out := make([]byte, leading+len(result))
	copy(out[leading:], result)
	return out, nil
}

// EncodeSegwitAddress builds a bech32 (version 0) or bech32m (version 1+)
// address from a witness program. The mock provider uses it to mint addresses
// that are genuinely valid for the network, so development exercises the same
// validation path production does.
func EncodeSegwitAddress(network string, witnessVersion byte, program []byte) (string, error) {
	prefixes := bech32Prefixes(network)
	if len(prefixes) == 0 {
		return "", fmt.Errorf("bitcoin: unknown network %q", network)
	}
	hrp := prefixes[0]
	if witnessVersion > 16 {
		return "", fmt.Errorf("bitcoin: witness version %d is out of range", witnessVersion)
	}
	converted, err := convertBits(program, 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("bitcoin: encode witness program: %w", err)
	}
	data := append([]byte{witnessVersion}, converted...)

	constant := bech32Const
	if witnessVersion != 0 {
		constant = bech32mConst
	}
	checksum := bech32Checksum(hrp, data, constant)

	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, value := range append(data, checksum...) {
		b.WriteByte(bech32Charset[value])
	}
	return b.String(), nil
}

func bech32Checksum(hrp string, data []byte, constant uint32) []byte {
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ constant
	checksum := make([]byte, 6)
	for i := 0; i < 6; i++ {
		checksum[i] = byte(polymod >> uint(5*(5-i)) & 31)
	}
	return checksum
}
