package bitcoin

import (
	"context"
	"errors"
	"testing"
)

func TestMockIssuesUniqueValidAddresses(t *testing.T) {
	ctx := context.Background()
	provider := NewMock(Regtest)

	seen := make(map[string]bool)
	for i := 0; i < 25; i++ {
		addr, err := provider.NewDepositAddress(ctx, "customer-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateAddress(addr.Address, Regtest); err != nil {
			t.Fatalf("the mock minted an invalid address %s: %v", addr.Address, err)
		}
		if seen[addr.Address] {
			t.Fatalf("address %s was issued twice", addr.Address)
		}
		seen[addr.Address] = true
	}
}

func TestMockSendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	provider := NewMock(Regtest)
	addr, err := provider.NewDepositAddress(ctx, "payee")
	if err != nil {
		t.Fatal(err)
	}

	req := SendRequest{ToAddress: addr.Address, AmountSat: 100_000, FeeSat: 2_000, IdempotencyKey: "withdrawal-7"}
	first, err := provider.Send(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Send(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.TxID != second.TxID {
		t.Error("retrying a send with the same idempotency key broadcast a second payment")
	}

	before, _ := provider.SpendableSat(ctx)
	if _, err := provider.Send(ctx, req); err != nil {
		t.Fatal(err)
	}
	after, _ := provider.SpendableSat(ctx)
	if before != after {
		t.Errorf("the retry moved %d sat out of the hot wallet", before-after)
	}
}

func TestMockRefusesInvalidPayoutAddress(t *testing.T) {
	provider := NewMock(Regtest)
	_, err := provider.Send(context.Background(), SendRequest{
		ToAddress: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", // mainnet address
		AmountSat: 100_000,
	})
	if !errors.Is(err, ErrInvalidAddress) {
		t.Errorf("got %v, want ErrInvalidAddress", err)
	}
}

func TestMockRefusesToOverdrawHotWallet(t *testing.T) {
	ctx := context.Background()
	provider := NewMock(Regtest)
	addr, _ := provider.NewDepositAddress(ctx, "payee")
	spendable, _ := provider.SpendableSat(ctx)

	_, err := provider.Send(ctx, SendRequest{ToAddress: addr.Address, AmountSat: spendable + 1})
	if !errors.Is(err, ErrInsufficientHot) {
		t.Errorf("got %v, want ErrInsufficientHot", err)
	}
}

func TestMockOnlyCreditsAddressesItIssued(t *testing.T) {
	provider := NewMock(Regtest)
	if _, err := provider.CreditPayment("bcrt1qmadeupaddress", 100_000, 1); err == nil {
		t.Error("the mock credited an address it never issued")
	}
}

func TestBTCStringConversion(t *testing.T) {
	cases := []struct {
		text string
		sat  int64
	}{
		{"0.00000001", 1},
		{"1.00000000", 100_000_000},
		{"0.1", 10_000_000},
		{"-0.00002500", -2_500},
		{"21000000.00000000", 21_000_000 * 100_000_000},
		{"", 0},
	}
	for _, tc := range cases {
		got, err := btcStringToSat(tc.text)
		if err != nil {
			t.Fatalf("btcStringToSat(%q): %v", tc.text, err)
		}
		if got != tc.sat {
			t.Errorf("btcStringToSat(%q) = %d, want %d", tc.text, got, tc.sat)
		}
	}
	// The conversion must round trip so a balance read back from a node and
	// re-sent to it does not drift.
	for _, sat := range []int64{0, 1, 2_500, 100_000_000, 123_456_789} {
		back, err := btcStringToSat(satToBTCString(sat))
		if err != nil {
			t.Fatal(err)
		}
		if back != sat {
			t.Errorf("round trip of %d sat gave %d", sat, back)
		}
	}
	if _, err := btcStringToSat("0.000000001"); err == nil {
		t.Error("a sub-satoshi amount should be rejected rather than rounded")
	}
}
