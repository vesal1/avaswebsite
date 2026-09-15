package bitcoin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Mock is an in-process provider for development and tests.
//
// It mints genuinely valid addresses for the configured network and keeps
// payments in memory, so the crediting pipeline, address validation and
// confirmation handling are all exercised exactly as they would be in
// production. It holds no keys and moves no coins; config.Validate refuses to
// start a production server that is configured to use it.
type Mock struct {
	network string

	mu        sync.Mutex
	counter   int64
	addresses map[string]string // address -> customer ref
	payments  []Payment
	sent      map[string]SendResult // idempotency key -> result
	spendable int64
	now       func() time.Time
}

// NewMock builds a mock provider for a network.
func NewMock(network string) *Mock {
	if network == "" {
		network = Regtest
	}
	return &Mock{
		network:   network,
		addresses: make(map[string]string),
		sent:      make(map[string]SendResult),
		spendable: 500 * 100_000_000, // a comfortable notional hot wallet
		now:       time.Now,
	}
}

// WithClock replaces the mock's clock. Test-only.
func (m *Mock) WithClock(clock func() time.Time) *Mock {
	m.now = clock
	return m
}

// Name identifies the provider.
func (m *Mock) Name() string { return "mock" }

// Network is the network this provider pretends to be on.
func (m *Mock) Network() string { return m.network }

// NewDepositAddress mints a fresh, valid address derived from the customer
// reference and a counter, so addresses are never reused across customers.
func (m *Mock) NewDepositAddress(ctx context.Context, customerRef string) (Address, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	seed := sha256.Sum256([]byte(fmt.Sprintf("avas-mock|%s|%d", customerRef, m.counter)))
	address, err := EncodeSegwitAddress(m.network, 0, seed[:20])
	if err != nil {
		return Address{}, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	m.addresses[address] = customerRef
	return Address{Address: address, Ref: fmt.Sprintf("mock-%d", m.counter), Network: m.network}, nil
}

// PaymentsSince returns the recorded payments first seen at or after a moment.
func (m *Mock) PaymentsSince(ctx context.Context, since time.Time) ([]Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Payment
	for _, payment := range m.payments {
		if !payment.SeenAt.Before(since) {
			out = append(out, payment)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SeenAt.Before(out[j].SeenAt) })
	return out, nil
}

// Send records a payout, refusing to broadcast the same idempotency key twice.
func (m *Mock) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	if err := ValidateAddress(req.ToAddress, m.network); err != nil {
		return SendResult{}, err
	}
	if req.AmountSat <= 0 {
		return SendResult{}, fmt.Errorf("%w: amount must be positive", ErrProvider)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.IdempotencyKey != "" {
		if existing, ok := m.sent[req.IdempotencyKey]; ok {
			return existing, nil
		}
	}
	if req.AmountSat+req.FeeSat > m.spendable {
		return SendResult{}, ErrInsufficientHot
	}
	m.spendable -= req.AmountSat + req.FeeSat
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", req.ToAddress, req.AmountSat, req.IdempotencyKey)))
	result := SendResult{
		TxID:      fmt.Sprintf("%x", sum),
		FeeSat:    req.FeeSat,
		Broadcast: m.now().UTC(),
	}
	if req.IdempotencyKey != "" {
		m.sent[req.IdempotencyKey] = result
	}
	return result, nil
}

// SpendableSat reports the notional hot wallet balance.
func (m *Mock) SpendableSat(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spendable, nil
}

// Health always succeeds; the mock has nothing to be unreachable.
func (m *Mock) Health(ctx context.Context) error { return nil }

// ---------------------------------------------------------------------------
// Test and development controls
// ---------------------------------------------------------------------------

// CreditPayment simulates an inbound payment to an address the mock issued.
// The development wallet page calls it so a deposit flow can be walked through
// end to end without a node.
func (m *Mock) CreditPayment(address string, amountSat int64, confirmations int) (Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.addresses[address]; !known {
		return Payment{}, fmt.Errorf("%w: %s was not issued by this provider", ErrProvider, address)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", address, amountSat, len(m.payments))))
	payment := Payment{
		TxID:          fmt.Sprintf("%x", sum),
		Vout:          0,
		Address:       address,
		AmountSat:     amountSat,
		Confirmations: confirmations,
		SeenAt:        m.now().UTC(),
	}
	m.payments = append(m.payments, payment)
	return payment, nil
}

// ConfirmAll advances every recorded payment to a confirmation count.
func (m *Mock) ConfirmAll(confirmations int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.payments {
		if m.payments[i].Confirmations < confirmations {
			m.payments[i].Confirmations = confirmations
		}
	}
}
