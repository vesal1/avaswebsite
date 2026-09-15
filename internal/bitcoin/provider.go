// Package bitcoin abstracts the wallet infrastructure that actually holds
// customer funds.
//
// Nothing in this package implements custody itself. It defines the contract
// the sportsbook needs and adapts it to systems that are built for the job:
// BTCPay Server, or a Bitcoin Core node. The mock provider exists for
// development and tests and is refused in production by config validation.
package bitcoin

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Network names.
const (
	Mainnet = "mainnet"
	Testnet = "testnet"
	Regtest = "regtest"
	Signet  = "signet"
)

// Errors callers branch on.
var (
	ErrUnsupported     = errors.New("bitcoin: operation not supported by this provider")
	ErrInvalidAddress  = errors.New("bitcoin: invalid address")
	ErrInsufficientHot = errors.New("bitcoin: hot wallet balance is insufficient")
	ErrProvider        = errors.New("bitcoin: provider error")
)

// Address is a receiving address issued to one customer.
type Address struct {
	Address string
	// Ref is the provider's own identifier for the address or invoice, kept so
	// a payment can be reconciled back through the provider's records.
	Ref     string
	Network string
}

// Payment is an inbound on-chain payment seen by the provider.
type Payment struct {
	TxID          string
	Vout          int
	Address       string
	AmountSat     int64
	Confirmations int
	SeenAt        time.Time
}

// SendRequest asks the provider to pay out.
type SendRequest struct {
	ToAddress string
	AmountSat int64
	// FeeSat is the fee the customer is charged. Whether it is deducted from
	// the amount or added on top is the caller's decision, already made by the
	// time this is called; the provider sends exactly AmountSat.
	FeeSat int64
	// IdempotencyKey must be stable for a given withdrawal. A provider that
	// supports it will refuse to broadcast the same payout twice, which is the
	// difference between a retried request and a duplicated payment.
	IdempotencyKey string
}

// SendResult reports a broadcast payout.
type SendResult struct {
	TxID      string
	FeeSat    int64
	Broadcast time.Time
}

// Provider is the wallet infrastructure the sportsbook talks to.
//
// Implementations must be safe for concurrent use.
type Provider interface {
	// Name identifies the implementation in logs and the admin screen.
	Name() string
	// Network is the bitcoin network this provider is connected to.
	Network() string
	// NewDepositAddress issues a fresh receiving address for a customer.
	// Reusing an address across customers would make deposits unattributable,
	// so implementations must never return a shared one.
	NewDepositAddress(ctx context.Context, customerRef string) (Address, error)
	// PaymentsSince returns inbound payments first seen at or after a moment,
	// including payments already reported, with current confirmation counts.
	PaymentsSince(ctx context.Context, since time.Time) ([]Payment, error)
	// Send broadcasts a payout.
	Send(ctx context.Context, req SendRequest) (SendResult, error)
	// SpendableSat is the balance available to pay withdrawals from.
	SpendableSat(ctx context.Context) (int64, error)
	// Health reports whether the provider is reachable and usable.
	Health(ctx context.Context) error
}

// Config carries the settings every provider might need.
type Config struct {
	Network string

	BTCPayURL     string
	BTCPayStoreID string
	BTCPayAPIKey  string

	BitcoindURL      string
	BitcoindUser     string
	BitcoindPassword string
	BitcoindWallet   string
}

// New builds the provider named by kind.
func New(kind string, cfg Config) (Provider, error) {
	switch kind {
	case "mock", "":
		return NewMock(cfg.Network), nil
	case "btcpay":
		return NewBTCPay(cfg)
	case "bitcoind", "core":
		return NewBitcoind(cfg)
	default:
		return nil, fmt.Errorf("bitcoin: unknown provider %q", kind)
	}
}
