package bitcoin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Bitcoind talks to a Bitcoin Core node over JSON-RPC.
//
// The node holds the wallet. This adapter only asks it for addresses,
// transactions and payouts; no key material passes through this process.
type Bitcoind struct {
	url      string
	user     string
	password string
	wallet   string
	network  string
	client   *http.Client
	counter  atomic.Int64
}

// NewBitcoind builds a Bitcoin Core backed provider.
func NewBitcoind(cfg Config) (*Bitcoind, error) {
	if cfg.BitcoindURL == "" {
		return nil, fmt.Errorf("bitcoin: bitcoind needs AVAS_BITCOIND_URL")
	}
	return &Bitcoind{
		url:      strings.TrimRight(cfg.BitcoindURL, "/"),
		user:     cfg.BitcoindUser,
		password: cfg.BitcoindPassword,
		wallet:   cfg.BitcoindWallet,
		network:  cfg.Network,
		client:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Name identifies the provider.
func (b *Bitcoind) Name() string { return "bitcoind" }

// Network is the bitcoin network the node is running on.
func (b *Bitcoind) Network() string { return b.network }

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (b *Bitcoind) call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "1.0",
		ID:      fmt.Sprintf("avas-%d", b.counter.Add(1)),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("%w: encode %s: %v", ErrProvider, method, err)
	}

	endpoint := b.url
	if b.wallet != "" {
		endpoint += "/wallet/" + b.wallet
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: build %s: %v", ErrProvider, method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.user != "" || b.password != "" {
		req.SetBasicAuth(b.user, b.password)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrProvider, method, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrProvider, method, err)
	}
	var decoded rpcResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Errorf("%w: %s returned %d: %s", ErrProvider, method, resp.StatusCode,
			strings.TrimSpace(string(payload)))
	}
	if decoded.Error != nil {
		return fmt.Errorf("%w: %s: %s (code %d)", ErrProvider, method,
			decoded.Error.Message, decoded.Error.Code)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(decoded.Result, out); err != nil {
		return fmt.Errorf("%w: decode %s result: %v", ErrProvider, method, err)
	}
	return nil
}

// NewDepositAddress asks the node's wallet for a fresh receiving address,
// labelled with the customer reference so the node's own records stay
// reconcilable with the sportsbook's.
func (b *Bitcoind) NewDepositAddress(ctx context.Context, customerRef string) (Address, error) {
	var address string
	if err := b.call(ctx, "getnewaddress", []any{"avas:" + customerRef, "bech32"}, &address); err != nil {
		return Address{}, err
	}
	if err := ValidateAddress(address, b.network); err != nil {
		return Address{}, fmt.Errorf("the node returned an address this server will not accept: %w", err)
	}
	return Address{Address: address, Ref: "avas:" + customerRef, Network: b.network}, nil
}

type listTransactionsEntry struct {
	Address       string  `json:"address"`
	Category      string  `json:"category"`
	Amount        float64 `json:"amount"`
	Confirmations int     `json:"confirmations"`
	TxID          string  `json:"txid"`
	Vout          int     `json:"vout"`
	Time          int64   `json:"time"`
}

// PaymentsSince returns inbound payments the wallet has seen. Amounts come
// back from Core as JSON numbers in BTC, so they are re-read from the raw
// token rather than a float to avoid a rounding error becoming a credit error.
func (b *Bitcoind) PaymentsSince(ctx context.Context, since time.Time) ([]Payment, error) {
	var raw json.RawMessage
	// A generous count with include_watchonly; the caller filters by time.
	if err := b.call(ctx, "listtransactions", []any{"*", 500, 0, true}, &raw); err != nil {
		return nil, err
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%w: decode listtransactions: %v", ErrProvider, err)
	}

	var out []Payment
	for _, entry := range entries {
		var meta listTransactionsEntry
		encoded, _ := json.Marshal(entry)
		if err := json.Unmarshal(encoded, &meta); err != nil {
			continue
		}
		if meta.Category != "receive" {
			continue
		}
		seenAt := time.Unix(meta.Time, 0).UTC()
		if seenAt.Before(since) {
			continue
		}
		amountSat, err := btcStringToSat(strings.Trim(string(entry["amount"]), `"`))
		if err != nil {
			return nil, fmt.Errorf("%w: transaction %s: %v", ErrProvider, meta.TxID, err)
		}
		if amountSat <= 0 {
			continue
		}
		out = append(out, Payment{
			TxID:          meta.TxID,
			Vout:          meta.Vout,
			Address:       meta.Address,
			AmountSat:     amountSat,
			Confirmations: meta.Confirmations,
			SeenAt:        seenAt,
		})
	}
	return out, nil
}

// Send pays out through the node's wallet.
//
// Core has no idempotency key, so the caller's key is written into the
// transaction comment. That is a reconciliation aid, not a guarantee: the
// wallet layer must not retry a Send whose outcome it does not know.
func (b *Bitcoind) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	if err := ValidateAddress(req.ToAddress, b.network); err != nil {
		return SendResult{}, err
	}
	if req.AmountSat <= 0 {
		return SendResult{}, fmt.Errorf("%w: amount must be positive", ErrProvider)
	}
	var txid string
	params := []any{
		req.ToAddress,
		satToBTCString(req.AmountSat),
		"avas withdrawal " + req.IdempotencyKey, // comment
		"",                                      // comment_to
		false,                                   // subtractfeefromamount
	}
	if err := b.call(ctx, "sendtoaddress", params, &txid); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "insufficient funds") {
			return SendResult{}, ErrInsufficientHot
		}
		return SendResult{}, err
	}
	return SendResult{TxID: txid, FeeSat: req.FeeSat, Broadcast: time.Now().UTC()}, nil
}

// SpendableSat reports the wallet's spendable balance.
func (b *Bitcoind) SpendableSat(ctx context.Context) (int64, error) {
	var raw json.RawMessage
	if err := b.call(ctx, "getbalance", []any{"*", 1, false}, &raw); err != nil {
		return 0, err
	}
	return btcStringToSat(strings.Trim(string(raw), `"`))
}

// Health checks the node is reachable and on the expected network.
func (b *Bitcoind) Health(ctx context.Context) error {
	var info struct {
		Chain string `json:"chain"`
	}
	if err := b.call(ctx, "getblockchaininfo", nil, &info); err != nil {
		return err
	}
	want := map[string]string{Mainnet: "main", Testnet: "test", Regtest: "regtest", Signet: "signet"}[b.network]
	if want != "" && info.Chain != want {
		return fmt.Errorf("%w: node is on chain %q but this server is configured for %s",
			ErrProvider, info.Chain, b.network)
	}
	return nil
}

// ---------------------------------------------------------------------------
// BTC string conversion
//
// Bitcoin RPCs speak decimal BTC. Parsing that through a float would introduce
// a rounding error into a balance, so the conversion is done on the digits.
// ---------------------------------------------------------------------------

func btcStringToSat(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	negative := false
	switch value[0] {
	case '-':
		negative, value = true, value[1:]
	case '+':
		value = value[1:]
	}
	whole, frac, _ := strings.Cut(value, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 8 {
		if strings.Trim(frac[8:], "0") != "" {
			return 0, fmt.Errorf("%q is finer than one satoshi", value)
		}
		frac = frac[:8]
	}
	frac += strings.Repeat("0", 8-len(frac))
	wholeVal, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a BTC amount", value)
	}
	fracVal, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a BTC amount", value)
	}
	sat := wholeVal*100_000_000 + fracVal
	if negative {
		sat = -sat
	}
	return sat, nil
}

func satToBTCString(sat int64) string {
	sign := ""
	if sat < 0 {
		sign, sat = "-", -sat
	}
	return fmt.Sprintf("%s%d.%08d", sign, sat/100_000_000, sat%100_000_000)
}
