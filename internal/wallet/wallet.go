// Package wallet moves customer money between the bitcoin network and the
// sportsbook's ledger.
//
// Every movement here is a balanced double-entry transaction. The customer's
// balance is never written as a column; it is the sum of their ledger entries,
// so any balance can be explained line by line and a bug shows up as a broken
// invariant rather than as quietly wrong money.
package wallet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/vesal1/avaswebsite/internal/bitcoin"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Errors callers branch on.
var (
	ErrInsufficientFunds = errors.New("wallet: insufficient balance")
	ErrNotPermitted      = errors.New("wallet: not permitted")
)

// Service is the wallet.
type Service struct {
	store      *store.Store
	provider   bitcoin.Provider
	compliance *compliance.Service
	cfg        *config.Config
	now        func() time.Time
}

// New builds a wallet service.
func New(s *store.Store, provider bitcoin.Provider, comp *compliance.Service, cfg *config.Config) *Service {
	return &Service{
		store: s, provider: provider, compliance: comp, cfg: cfg,
		now: func() time.Time { return s.Now() },
	}
}

// Provider exposes the configured bitcoin backend, for the admin health view
// and the development deposit simulator.
func (s *Service) Provider() bitcoin.Provider { return s.provider }

// Balance is a customer's spendable balance.
func (s *Service) Balance(ctx context.Context, userID int64) (int64, error) {
	return s.store.BalanceSat(ctx, userID)
}

// Summary is what the wallet page shows.
type Summary struct {
	BalanceSat         int64
	PendingWithdrawSat int64
	EscrowedInBetsSat  int64
	DepositAddress     string
	Network            string
	MinDepositSat      int64
	Confirmations      int
	FiatCents          int64
	FiatCurrency       string
}

// Summary gathers the customer's wallet state in one pass.
func (s *Service) Summary(ctx context.Context, userID int64) (Summary, error) {
	balance, err := s.store.BalanceSat(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	pending, err := s.store.PendingWithdrawalSat(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	escrow, err := s.escrowedForUser(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{
		BalanceSat:         balance,
		PendingWithdrawSat: pending,
		EscrowedInBetsSat:  escrow,
		Network:            s.provider.Network(),
		MinDepositSat:      s.cfg.MinDepositSat,
		Confirmations:      s.cfg.DepositConfirmations,
		FiatCents:          money.ToFiatCents(balance, s.cfg.StaticBTCRate),
		FiatCurrency:       s.cfg.DisplayCurrency,
	}
	if address, err := s.store.ActiveDepositAddress(ctx, userID); err == nil {
		summary.DepositAddress = address.Address
	} else if !errors.Is(err, store.ErrNotFound) {
		return Summary{}, err
	}
	return summary, nil
}

func (s *Service) escrowedForUser(ctx context.Context, userID int64) (int64, error) {
	bets, err := s.store.ListBetsForUser(ctx, userID, 500, "open")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, bet := range bets {
		total += bet.StakeSat
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Deposits
// ---------------------------------------------------------------------------

// EnsureDepositAddress returns the customer's deposit address, issuing one
// from the provider if they do not have an active one.
func (s *Service) EnsureDepositAddress(ctx context.Context, userID int64) (string, error) {
	existing, err := s.store.ActiveDepositAddress(ctx, userID)
	if err == nil {
		return existing.Address, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}

	issued, err := s.provider.NewDepositAddress(ctx, strconv.FormatInt(userID, 10))
	if err != nil {
		return "", err
	}
	// Validate what the provider handed back before showing it to a customer:
	// an address the book cannot recognise is one it cannot credit either.
	if err := bitcoin.ValidateAddress(issued.Address, s.provider.Network()); err != nil {
		return "", fmt.Errorf("wallet: provider issued an unusable address: %w", err)
	}
	if _, err := s.store.SaveDepositAddress(ctx, store.DepositAddress{
		UserID: userID, Address: issued.Address, Provider: s.provider.Name(),
		ProviderRef: issued.Ref, Network: issued.Network,
	}); err != nil {
		return "", err
	}
	if err := s.store.Audit(ctx, store.AuditEntry{
		SubjectUserID: userID, Action: "deposit_address_issued", Detail: issued.Address,
	}); err != nil {
		return "", err
	}
	return issued.Address, nil
}

// SyncResult reports what a sync pass did.
type SyncResult struct {
	Seen     int
	Recorded int
	Credited int
	Skipped  int
	Errors   []string
}

// SyncDeposits pulls payments from the provider, records them, and credits
// those that have reached the required confirmation depth.
//
// It is safe to run repeatedly and concurrently with itself: recording is
// keyed on the on-chain output, and crediting is guarded by a status check
// inside the same transaction that posts the ledger entries.
func (s *Service) SyncDeposits(ctx context.Context, since time.Time) (SyncResult, error) {
	var result SyncResult

	payments, err := s.provider.PaymentsSince(ctx, since)
	if err != nil {
		return result, err
	}
	result.Seen = len(payments)

	for _, payment := range payments {
		userID, err := s.store.UserForDepositAddress(ctx, payment.Address)
		if errors.Is(err, store.ErrNotFound) {
			// A payment to an address this book did not issue. It is recorded
			// nowhere and credited to nobody; it needs a human.
			result.Skipped++
			result.Errors = append(result.Errors,
				fmt.Sprintf("payment %s:%d to unknown address %s", payment.TxID, payment.Vout, payment.Address))
			continue
		}
		if err != nil {
			return result, err
		}
		if _, err := s.store.RecordDeposit(ctx, store.Deposit{
			UserID: userID, Address: payment.Address, TxID: payment.TxID, Vout: payment.Vout,
			AmountSat: payment.AmountSat, Confirmations: payment.Confirmations,
		}); err != nil {
			return result, err
		}
		result.Recorded++
	}

	ready, err := s.store.CreditableDeposits(ctx, s.cfg.DepositConfirmations)
	if err != nil {
		return result, err
	}
	for _, deposit := range ready {
		credited, err := s.CreditDeposit(ctx, deposit.ID)
		switch {
		case err != nil:
			result.Errors = append(result.Errors, fmt.Sprintf("deposit %d: %v", deposit.ID, err))
		case credited:
			result.Credited++
		default:
			result.Skipped++
		}
	}
	return result, nil
}

// CreditDeposit credits a confirmed deposit to its customer's balance.
//
// Returns false without an error when the deposit was already credited, which
// is the expected outcome of a replayed webhook rather than a failure.
func (s *Service) CreditDeposit(ctx context.Context, depositID int64) (bool, error) {
	deposit, err := s.store.DepositByID(ctx, depositID)
	if err != nil {
		return false, err
	}
	if deposit.Status == store.DepositCredited {
		return false, nil
	}
	if deposit.Confirmations < s.cfg.DepositConfirmations {
		return false, nil
	}

	user, err := s.store.GetUser(ctx, deposit.UserID)
	if err != nil {
		return false, err
	}

	// A deposit that breaches a limit or lands on an unusable account is
	// frozen rather than credited: the money is held, visible, and returnable.
	if err := s.compliance.CheckDeposit(ctx, user, deposit.AmountSat); err != nil {
		if refusal, ok := compliance.AsRefusal(err); ok {
			if setErr := s.store.SetDepositStatus(ctx, deposit.ID, store.DepositFrozen); setErr != nil {
				return false, setErr
			}
			if _, flagErr := s.store.RaiseFlag(ctx, store.ComplianceFlag{
				UserID:   user.ID,
				Kind:     "deposit_held",
				Severity: "warning",
				Detail: fmt.Sprintf("Deposit %d of %s BTC held: %s",
					deposit.ID, money.FormatBTC(deposit.AmountSat), refusal.Message),
			}); flagErr != nil {
				return false, flagErr
			}
			return false, nil
		}
		return false, err
	}

	flags := s.compliance.ScreenDeposit(user, deposit.AmountSat)

	err = s.store.Tx(ctx, func(tx *store.Tx) error {
		at := store.Timestamp(s.now())
		txnID, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:    store.TxnDeposit,
			Memo:    fmt.Sprintf("Deposit %s:%d", deposit.TxID, deposit.Vout),
			RefType: "deposit",
			RefID:   deposit.ID,
			Entries: []store.Entry{
				{Account: store.AccountUserCash, UserID: deposit.UserID, AmountSat: deposit.AmountSat},
				{Account: store.AccountExternalBitcoin, AmountSat: -deposit.AmountSat},
			},
		})
		if err != nil {
			return err
		}
		if err := store.MarkDepositCreditedTx(ctx, tx, deposit.ID, txnID, at); err != nil {
			return err
		}
		for _, flag := range flags {
			if err := store.RaiseFlagTx(ctx, tx, flag, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// A concurrent run got there first. That is the guard doing its job.
		if errors.Is(err, store.ErrConflict) {
			return false, nil
		}
		return false, err
	}

	return true, s.store.Audit(ctx, store.AuditEntry{
		SubjectUserID: deposit.UserID,
		Action:        "deposit_credited",
		Detail:        fmt.Sprintf("%s BTC from %s:%d", money.FormatBTC(deposit.AmountSat), deposit.TxID, deposit.Vout),
	})
}

// ---------------------------------------------------------------------------
// Withdrawals
// ---------------------------------------------------------------------------

// RequestWithdrawal debits a customer's balance and queues a payout.
//
// The debit happens now, not at release: money that has been requested for
// withdrawal must not also be available to bet with. If the payout is later
// rejected the debit is reversed.
func (s *Service) RequestWithdrawal(ctx context.Context, userID int64, address string, amountSat int64) (int64, error) {
	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	if err := bitcoin.ValidateAddress(address, s.provider.Network()); err != nil {
		return 0, err
	}
	decision, err := s.compliance.CheckWithdrawal(ctx, user, amountSat)
	if err != nil {
		return 0, err
	}

	fee := s.cfg.WithdrawalFeeSat
	total := amountSat + fee

	status := store.WithdrawalRequested
	if decision.NeedsReview {
		status = store.WithdrawalReview
	}

	var withdrawalID int64
	err = s.store.Tx(ctx, func(tx *store.Tx) error {
		// Read the balance inside the transaction so two concurrent requests
		// cannot both pass the check against the same funds.
		balance, err := store.BalanceSatTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if balance < total {
			return fmt.Errorf("%w: balance is %s BTC, withdrawal plus fee is %s BTC",
				ErrInsufficientFunds, money.FormatBTC(balance), money.FormatBTC(total))
		}

		at := store.Timestamp(s.now())
		txnID, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:    store.TxnWithdrawal,
			Memo:    "Withdrawal to " + address,
			RefType: "withdrawal",
			Entries: []store.Entry{
				{Account: store.AccountUserCash, UserID: userID, AmountSat: -total},
				{Account: store.AccountWithdrawalSuspense, AmountSat: amountSat},
				{Account: store.AccountHouseFees, AmountSat: fee},
			},
		})
		if err != nil {
			return err
		}

		withdrawalID, err = store.InsertWithdrawalTx(ctx, tx, store.Withdrawal{
			UserID: userID, Address: address, AmountSat: amountSat, FeeSat: fee,
			Status: status, LedgerTxnID: txnID,
		}, at)
		if err != nil {
			return err
		}
		for _, flag := range decision.Flags {
			if err := store.RaiseFlagTx(ctx, tx, flag, at); err != nil {
				return err
			}
		}
		if decision.NeedsReview {
			if err := store.RaiseFlagTx(ctx, tx, store.ComplianceFlag{
				UserID: userID, Kind: "withdrawal_review", Severity: "info",
				Detail: decision.Reason,
			}, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	return withdrawalID, s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: userID, SubjectUserID: userID, Action: "withdrawal_requested",
		Detail: fmt.Sprintf("%s BTC to %s (fee %s BTC)",
			money.FormatBTC(amountSat), address, money.FormatBTC(fee)),
	})
}

// ReleaseWithdrawal broadcasts a queued payout.
//
// The state is moved to 'broadcast' before the provider is called, and is
// never rolled back on a provider error. An error from a send is ambiguous:
// the transaction may already be on the wire. Reversing on ambiguity is how a
// payout gets made twice, so an uncertain send is escalated to a human rather
// than retried.
func (s *Service) ReleaseWithdrawal(ctx context.Context, withdrawalID, officerID int64) error {
	withdrawal, err := s.store.GetWithdrawal(ctx, withdrawalID)
	if err != nil {
		return err
	}
	switch withdrawal.Status {
	case store.WithdrawalRequested, store.WithdrawalReview, store.WithdrawalApproved:
	default:
		return fmt.Errorf("%w: withdrawal %d is %s", ErrNotPermitted, withdrawalID, withdrawal.Status)
	}

	at := store.Timestamp(s.now())
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		return store.SetWithdrawalStatusTx(ctx, tx, withdrawalID,
			[]string{store.WithdrawalRequested, store.WithdrawalReview, store.WithdrawalApproved},
			store.WithdrawalBroadcast, officerID, "released", at)
	}); err != nil {
		return err
	}

	result, sendErr := s.provider.Send(ctx, bitcoin.SendRequest{
		ToAddress: withdrawal.Address,
		AmountSat: withdrawal.AmountSat,
		FeeSat:    withdrawal.FeeSat,
		// Stable per withdrawal, so a provider that honours it will not pay
		// the same request twice.
		IdempotencyKey: fmt.Sprintf("avas-withdrawal-%d", withdrawal.ID),
	})
	if sendErr != nil {
		if _, err := s.store.RaiseFlag(ctx, store.ComplianceFlag{
			UserID: withdrawal.UserID, Kind: "withdrawal_send_failed", Severity: "critical",
			Detail: fmt.Sprintf("Withdrawal %d to %s failed at the provider: %v. "+
				"Confirm on-chain whether it was broadcast before retrying.",
				withdrawal.ID, withdrawal.Address, sendErr),
		}); err != nil {
			return err
		}
		return fmt.Errorf("wallet: send withdrawal %d: %w", withdrawal.ID, sendErr)
	}

	if err := s.store.SetWithdrawalTxID(ctx, withdrawal.ID, result.TxID); err != nil {
		return err
	}
	// The coins have left; move the suspense balance out to the network.
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(s.now()), store.TxnSpec{
			Kind:      store.TxnWithdrawal,
			Memo:      "Broadcast " + result.TxID,
			RefType:   "withdrawal",
			RefID:     withdrawal.ID,
			CreatedBy: officerID,
			Entries: []store.Entry{
				{Account: store.AccountWithdrawalSuspense, AmountSat: -withdrawal.AmountSat},
				{Account: store.AccountExternalBitcoin, AmountSat: withdrawal.AmountSat},
			},
		})
		return err
	}); err != nil {
		return err
	}

	return s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: officerID, SubjectUserID: withdrawal.UserID, Action: "withdrawal_released",
		Detail: fmt.Sprintf("withdrawal %d, txid %s", withdrawal.ID, result.TxID),
	})
}

// RejectWithdrawal cancels a queued payout and returns the funds, fee
// included, to the customer's balance.
func (s *Service) RejectWithdrawal(ctx context.Context, withdrawalID, officerID int64, reason string) error {
	withdrawal, err := s.store.GetWithdrawal(ctx, withdrawalID)
	if err != nil {
		return err
	}
	switch withdrawal.Status {
	case store.WithdrawalRequested, store.WithdrawalReview, store.WithdrawalApproved:
	default:
		return fmt.Errorf("%w: withdrawal %d is %s and cannot be rejected",
			ErrNotPermitted, withdrawalID, withdrawal.Status)
	}

	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		at := store.Timestamp(s.now())
		if err := store.SetWithdrawalStatusTx(ctx, tx, withdrawalID,
			[]string{store.WithdrawalRequested, store.WithdrawalReview, store.WithdrawalApproved},
			store.WithdrawalRejected, officerID, reason, at); err != nil {
			return err
		}
		_, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:      store.TxnWithdrawalReversal,
			Memo:      "Withdrawal rejected: " + reason,
			RefType:   "withdrawal",
			RefID:     withdrawal.ID,
			CreatedBy: officerID,
			Entries: []store.Entry{
				{Account: store.AccountWithdrawalSuspense, AmountSat: -withdrawal.AmountSat},
				{Account: store.AccountHouseFees, AmountSat: -withdrawal.FeeSat},
				{Account: store.AccountUserCash, UserID: withdrawal.UserID,
					AmountSat: withdrawal.TotalDebitSat()},
			},
		})
		return err
	}); err != nil {
		return err
	}

	return s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: officerID, SubjectUserID: withdrawal.UserID, Action: "withdrawal_rejected",
		Detail: fmt.Sprintf("withdrawal %d: %s", withdrawal.ID, reason),
	})
}

// CancelWithdrawal lets a customer withdraw their own request before it is
// released.
func (s *Service) CancelWithdrawal(ctx context.Context, userID, withdrawalID int64) error {
	withdrawal, err := s.store.GetWithdrawal(ctx, withdrawalID)
	if err != nil {
		return err
	}
	if withdrawal.UserID != userID {
		return fmt.Errorf("%w: that withdrawal belongs to another account", ErrNotPermitted)
	}
	switch withdrawal.Status {
	case store.WithdrawalRequested, store.WithdrawalReview:
	default:
		return fmt.Errorf("%w: this withdrawal is already %s", ErrNotPermitted, withdrawal.Status)
	}

	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		at := store.Timestamp(s.now())
		if err := store.SetWithdrawalStatusTx(ctx, tx, withdrawalID,
			[]string{store.WithdrawalRequested, store.WithdrawalReview},
			store.WithdrawalCancelled, userID, "cancelled by customer", at); err != nil {
			return err
		}
		_, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:      store.TxnWithdrawalReversal,
			Memo:      "Withdrawal cancelled by customer",
			RefType:   "withdrawal",
			RefID:     withdrawal.ID,
			CreatedBy: userID,
			Entries: []store.Entry{
				{Account: store.AccountWithdrawalSuspense, AmountSat: -withdrawal.AmountSat},
				{Account: store.AccountHouseFees, AmountSat: -withdrawal.FeeSat},
				{Account: store.AccountUserCash, UserID: userID, AmountSat: withdrawal.TotalDebitSat()},
			},
		})
		return err
	}); err != nil {
		return err
	}

	return s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: userID, SubjectUserID: userID, Action: "withdrawal_cancelled",
		Detail: fmt.Sprintf("withdrawal %d", withdrawal.ID),
	})
}
