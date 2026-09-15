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
	"time"
)

// BTCPay talks to a BTCPay Server instance over its Greenfield API.
//
// BTCPay is a self-hosted payment processor: it holds the wallet, watches the
// chain and signs payouts. Delegating custody to it rather than reimplementing
// key management here is the point of this adapter.
type BTCPay struct {
	baseURL string
	storeID string
	apiKey  string
	network string
	client  *http.Client
}

// NewBTCPay builds a BTCPay-backed provider.
func NewBTCPay(cfg Config) (*BTCPay, error) {
	if cfg.BTCPayURL == "" || cfg.BTCPayStoreID == "" || cfg.BTCPayAPIKey == "" {
		return nil, fmt.Errorf("bitcoin: btcpay needs AVAS_BTCPAY_URL, AVAS_BTCPAY_STORE_ID and AVAS_BTCPAY_API_KEY")
	}
	return &BTCPay{
		baseURL: strings.TrimRight(cfg.BTCPayURL, "/"),
		storeID: cfg.BTCPayStoreID,
		apiKey:  cfg.BTCPayAPIKey,
		network: cfg.Network,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Name identifies the provider.
func (b *BTCPay) Name() string { return "btcpay" }

// Network is the bitcoin network the store is configured for.
func (b *BTCPay) Network() string { return b.network }

func (b *BTCPay) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: encode request: %v", ErrProvider, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("%w: build request: %v", ErrProvider, err)
	}
	// Greenfield takes an API key rather than a bearer token.
	req.Header.Set("Authorization", "token "+b.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrProvider, method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrProvider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: %s %s returned %d: %s", ErrProvider, method, path,
			resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrProvider, err)
	}
	return nil
}

type btcpayInvoice struct {
	ID       string `json:"id"`
	Checkout struct {
		PaymentMethods []string `json:"paymentMethods"`
	} `json:"checkout"`
}

type btcpayPaymentMethod struct {
	PaymentMethod string `json:"paymentMethod"`
	Destination   string `json:"destination"`
	Payments      []struct {
		ID           string `json:"id"`
		ReceivedDate int64  `json:"receivedDate"`
		Value        string `json:"value"`
		Status       string `json:"status"`
		Destination  string `json:"destination"`
	} `json:"payments"`
}

// NewDepositAddress creates a BTCPay invoice and returns the bitcoin address
// it allocated. One invoice per customer request keeps deposits attributable.
func (b *BTCPay) NewDepositAddress(ctx context.Context, customerRef string) (Address, error) {
	body := map[string]any{
		// An invoice with no fixed amount accepts whatever the customer sends,
		// which is what a top-up is.
		"metadata": map[string]any{"customerRef": customerRef, "source": "avas-deposit"},
		"checkout": map[string]any{"paymentMethods": []string{"BTC"}, "expirationMinutes": 1440},
	}
	var invoice btcpayInvoice
	if err := b.do(ctx, http.MethodPost,
		"/api/v1/stores/"+b.storeID+"/invoices", body, &invoice); err != nil {
		return Address{}, err
	}

	var methods []btcpayPaymentMethod
	if err := b.do(ctx, http.MethodGet,
		"/api/v1/stores/"+b.storeID+"/invoices/"+invoice.ID+"/payment-methods", nil, &methods); err != nil {
		return Address{}, err
	}
	for _, method := range methods {
		if strings.HasPrefix(strings.ToUpper(method.PaymentMethod), "BTC") && method.Destination != "" {
			if err := ValidateAddress(method.Destination, b.network); err != nil {
				return Address{}, fmt.Errorf("btcpay returned an address this server will not accept: %w", err)
			}
			return Address{Address: method.Destination, Ref: invoice.ID, Network: b.network}, nil
		}
	}
	return Address{}, fmt.Errorf("%w: invoice %s has no BTC payment method", ErrProvider, invoice.ID)
}

// PaymentsSince returns on-chain payments to this store's invoices.
func (b *BTCPay) PaymentsSince(ctx context.Context, since time.Time) ([]Payment, error) {
	path := fmt.Sprintf("/api/v1/stores/%s/invoices?startDate=%d", b.storeID, since.Unix())
	var invoices []btcpayInvoice
	if err := b.do(ctx, http.MethodGet, path, nil, &invoices); err != nil {
		return nil, err
	}

	var out []Payment
	for _, invoice := range invoices {
		var methods []btcpayPaymentMethod
		if err := b.do(ctx, http.MethodGet,
			"/api/v1/stores/"+b.storeID+"/invoices/"+invoice.ID+"/payment-methods", nil, &methods); err != nil {
			return nil, err
		}
		for _, method := range methods {
			for _, payment := range method.Payments {
				amountSat, err := btcStringToSat(payment.Value)
				if err != nil {
					return nil, fmt.Errorf("%w: invoice %s: %v", ErrProvider, invoice.ID, err)
				}
				txid, vout := splitOutpoint(payment.ID)
				// BTCPay reports settlement rather than a raw confirmation
				// count; treat a settled payment as having met the store's own
				// confirmation policy.
				confirmations := 0
				if strings.EqualFold(payment.Status, "Settled") {
					confirmations = 6
				}
				destination := payment.Destination
				if destination == "" {
					destination = method.Destination
				}
				out = append(out, Payment{
					TxID:          txid,
					Vout:          vout,
					Address:       destination,
					AmountSat:     amountSat,
					Confirmations: confirmations,
					SeenAt:        time.Unix(payment.ReceivedDate, 0).UTC(),
				})
			}
		}
	}
	return out, nil
}

// Send creates a payout through the store's payout processor.
func (b *BTCPay) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	if err := ValidateAddress(req.ToAddress, b.network); err != nil {
		return SendResult{}, err
	}
	body := map[string]any{
		"destination":   req.ToAddress,
		"amount":        satToBTCString(req.AmountSat),
		"paymentMethod": "BTC",
		"metadata":      map[string]any{"idempotencyKey": req.IdempotencyKey},
	}
	var payout struct {
		ID          string `json:"id"`
		State       string `json:"state"`
		Destination string `json:"destination"`
	}
	if err := b.do(ctx, http.MethodPost,
		"/api/v1/stores/"+b.storeID+"/payouts", body, &payout); err != nil {
		return SendResult{}, err
	}
	// BTCPay's payout is queued for the store's processor, so there is no txid
	// yet. The payout id is recorded and the txid is filled in when the
	// reconciliation pass sees the payout settled.
	return SendResult{TxID: payout.ID, FeeSat: req.FeeSat, Broadcast: time.Now().UTC()}, nil
}

// SpendableSat reports the store's on-chain balance.
func (b *BTCPay) SpendableSat(ctx context.Context) (int64, error) {
	var wallet struct {
		Balance          string `json:"balance"`
		ConfirmedBalance string `json:"confirmedBalance"`
	}
	if err := b.do(ctx, http.MethodGet,
		"/api/v1/stores/"+b.storeID+"/payment-methods/onchain/BTC/wallet", nil, &wallet); err != nil {
		return 0, err
	}
	balance := wallet.ConfirmedBalance
	if balance == "" {
		balance = wallet.Balance
	}
	return btcStringToSat(balance)
}

// Health checks that the API is reachable and the key is accepted.
func (b *BTCPay) Health(ctx context.Context) error {
	return b.do(ctx, http.MethodGet, "/api/v1/stores/"+b.storeID, nil, nil)
}

func splitOutpoint(id string) (string, int) {
	txid, voutStr, ok := strings.Cut(id, "-")
	if !ok {
		return id, 0
	}
	vout, err := strconv.Atoi(voutStr)
	if err != nil {
		return txid, 0
	}
	return txid, vout
}
