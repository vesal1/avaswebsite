package bitcoin

import (
	"errors"
	"strings"
	"testing"
)

func TestValidMainnetAddresses(t *testing.T) {
	// Well-known mainnet addresses across every encoding in use.
	valid := []string{
		"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",                             // P2PKH, the genesis coinbase
		"3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy",                             // P2SH
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",                     // P2WPKH
		"bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3", // P2WSH
		"bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0", // P2TR, bech32m
	}
	for _, address := range valid {
		if err := ValidateAddress(address, Mainnet); err != nil {
			t.Errorf("ValidateAddress(%s) = %v, want nil", address, err)
		}
	}
}

func TestUppercaseBech32IsAccepted(t *testing.T) {
	address := strings.ToUpper("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4")
	if err := ValidateAddress(address, Mainnet); err != nil {
		t.Errorf("an all-uppercase bech32 address should be valid: %v", err)
	}
}

func TestMistypedAddressesAreCaught(t *testing.T) {
	// Each of these is one character away from a valid address. Catching them
	// is the entire reason the checksum is verified rather than the shape.
	cases := map[string]string{
		"transposed bech32 character": "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t5",
		"altered base58 character":    "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNb",
		"truncated bech32":            "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7k",
		"mixed case bech32":           "bc1QW508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
	}
	for name, address := range cases {
		if err := ValidateAddress(address, Mainnet); err == nil {
			t.Errorf("%s: %s was accepted", name, address)
		} else if !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("%s: got %v, want ErrInvalidAddress", name, err)
		}
	}
}

func TestWrongNetworkIsRejected(t *testing.T) {
	// Paying a mainnet address from a testnet wallet, or the reverse, is a
	// mistake worth naming explicitly rather than reporting as malformed.
	err := ValidateAddress("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", Testnet)
	if err == nil {
		t.Fatal("a mainnet address was accepted on testnet")
	}
	if !strings.Contains(err.Error(), "mainnet") {
		t.Errorf("error should name the address's real network, got: %v", err)
	}

	if err := ValidateAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", Testnet); err == nil {
		t.Error("a mainnet P2PKH address was accepted on testnet")
	}
}

func TestEmptyAndJunkAddresses(t *testing.T) {
	for _, address := range []string{"", "   ", "not an address", "0OIl", "bc1"} {
		if err := ValidateAddress(address, Mainnet); err == nil {
			t.Errorf("%q was accepted", address)
		}
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	program := make([]byte, 20)
	for i := range program {
		program[i] = byte(i * 7)
	}
	for _, network := range []string{Mainnet, Testnet, Regtest} {
		address, err := EncodeSegwitAddress(network, 0, program)
		if err != nil {
			t.Fatalf("encode for %s: %v", network, err)
		}
		if err := ValidateAddress(address, network); err != nil {
			t.Errorf("%s address %s failed its own validation: %v", network, address, err)
		}
	}

	// A taproot-style 32-byte program at version 1 must use bech32m, and the
	// validator must accept it under that constant rather than bech32's.
	taproot := make([]byte, 32)
	for i := range taproot {
		taproot[i] = byte(255 - i)
	}
	address, err := EncodeSegwitAddress(Mainnet, 1, taproot)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAddress(address, Mainnet); err != nil {
		t.Errorf("bech32m address %s failed validation: %v", address, err)
	}
}

func TestVersionZeroProgramLengthIsEnforced(t *testing.T) {
	// 19 bytes is neither P2WPKH nor P2WSH; the checksum would still pass, so
	// only the explicit length rule rejects it.
	program := make([]byte, 19)
	address, err := EncodeSegwitAddress(Mainnet, 0, program)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAddress(address, Mainnet); err == nil {
		t.Error("a 19-byte version 0 witness program was accepted")
	}
}
