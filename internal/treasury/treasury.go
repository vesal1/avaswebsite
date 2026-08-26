// Package treasury moves real customer money by hand.
//
// Manual adjustments exist because reality intrudes: a goodwill credit after
// an outage, a correction to a mis-settled market, a refund. They are also the
// most dangerous thing in the system, because they mint or destroy customer
// balance without a deposit or a bet behind them.
//
// So they are deliberately awkward. Every adjustment is a request with a
// written reason, anything at or above the configured threshold needs a second
// person, and nobody can approve their own.
package treasury

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Errors callers branch on.
var (
	ErrReasonRequired = errors.New("treasury: an adjustment needs a written reason")
	ErrTooLarge       = errors.New("treasury: adjustment exceeds the permitted size")
	ErrSelfApproval   = errors.New("treasury: an adjustment cannot be approved by the person who requested it")
	ErrWouldOverdraw  = errors.New("treasury: that debit would take the balance below zero")
	ErrNotPending     = errors.New("treasury: that adjustment has already been decided")
)

// Service handles manual money movement.
type Service struct {
	store *store.Store
	cfg   *config.Config
	now   func() time.Time
}

// New builds a treasury service.
func New(s *store.Store, cfg *config.Config) *Service {
	return &Service{store: s, cfg: cfg, now: func() time.Time { return s.Now() }}
}

// WithClock replaces the service clock. Test-only.
func (s *Service) WithClock(clock func() time.Time) *Service {
	s.now = clock
	return s
}

// Outcome reports what happened to a request.
type Outcome struct {
	AdjustmentID int64
	// Applied is false when the request was queued for a second approver.
	Applied       bool
	NeedsSecond   bool
	NewBalanceSat int64
}

// Request asks for an adjustment. A positive amount credits the customer, a
// negative one debits them.
func (s *Service) Request(ctx context.Context, userID, amountSat, requestedBy int64, reason string) (Outcome, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Outcome{}, ErrReasonRequired
	}
	if amountSat == 0 {
		return Outcome{}, fmt.Errorf("treasury: an adjustment of nothing is not an adjustment")
	}

	size := amountSat
	if size < 0 {
		size = -size
	}
	if s.cfg.MaxManualAdjustSat > 0 && size > s.cfg.MaxManualAdjustSat {
		return Outcome{}, fmt.Errorf("%w: %s BTC is above the %s BTC ceiling",
			ErrTooLarge, money.FormatBTC(size), money.FormatBTC(s.cfg.MaxManualAdjustSat))
	}

	adjustmentID, err := s.store.CreateManualAdjustment(ctx, store.ManualAdjustment{
		UserID: userID, AmountSat: amountSat, Reason: reason, RequestedBy: requestedBy,
	})
	if err != nil {
		return Outcome{}, err
	}

	if err := s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: requestedBy, SubjectUserID: userID, Action: "adjustment_requested",
		Detail: fmt.Sprintf("adjustment %d of %s BTC: %s",
			adjustmentID, money.FormatBTC(amountSat), reason),
	}); err != nil {
		return Outcome{}, err
	}

	// Small adjustments apply straight away; large ones wait for a second
	// person. The threshold is a policy dial, not a hard-coded number.
	if s.needsSecondApprover(size) {
		return Outcome{AdjustmentID: adjustmentID, NeedsSecond: true}, nil
	}

	balance, err := s.apply(ctx, adjustmentID, requestedBy, "under the approval threshold", true)
	if err != nil {
		return Outcome{AdjustmentID: adjustmentID}, err
	}
	return Outcome{AdjustmentID: adjustmentID, Applied: true, NewBalanceSat: balance}, nil
}

func (s *Service) needsSecondApprover(size int64) bool {
	return s.cfg.ManualApprovalSat > 0 && size >= s.cfg.ManualApprovalSat
}

// Approve applies a pending adjustment.
func (s *Service) Approve(ctx context.Context, adjustmentID, approverID int64, note string) (int64, error) {
	return s.apply(ctx, adjustmentID, approverID, note, false)
}

// Reject closes a pending adjustment without moving any money.
func (s *Service) Reject(ctx context.Context, adjustmentID, approverID int64, note string) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return fmt.Errorf("%w: say why it was rejected", ErrReasonRequired)
	}
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		adjustment, err := store.GetManualAdjustmentTx(ctx, tx, adjustmentID)
		if err != nil {
			return err
		}
		if adjustment.Status != store.AdjustmentPending {
			return ErrNotPending
		}
		return store.DecideManualAdjustmentTx(ctx, tx, adjustmentID,
			store.AdjustmentRejected, approverID, 0, note, store.Timestamp(s.now()))
	})
	if err != nil {
		return err
	}
	return s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: approverID, Action: "adjustment_rejected",
		Detail: fmt.Sprintf("adjustment %d: %s", adjustmentID, note),
	})
}

// apply moves the money and records the decision in one transaction.
func (s *Service) apply(ctx context.Context, adjustmentID, approverID int64, note string, selfApproved bool) (int64, error) {
	var newBalance int64

	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		adjustment, err := store.GetManualAdjustmentTx(ctx, tx, adjustmentID)
		if err != nil {
			return err
		}
		if adjustment.Status != store.AdjustmentPending {
			return ErrNotPending
		}
		// Self-approval is the whole point of the second pair of eyes. It is
		// checked here, at the moment the money moves, rather than in a
		// handler that could be bypassed.
		if !selfApproved && adjustment.RequestedBy != 0 && adjustment.RequestedBy == approverID {
			return ErrSelfApproval
		}

		balance, err := store.BalanceSatTx(ctx, tx, adjustment.UserID)
		if err != nil {
			return err
		}
		if balance+adjustment.AmountSat < 0 {
			return fmt.Errorf("%w: balance is %s BTC and the adjustment is %s BTC",
				ErrWouldOverdraw, money.FormatBTC(balance), money.FormatBTC(adjustment.AmountSat))
		}

		at := store.Timestamp(s.now())
		// The counterparty is house revenue: an adjustment is the book giving
		// money away or taking it back, and it should show up in the book's
		// own result rather than appearing from nowhere.
		txnID, err := store.PostTxn(ctx, tx, at, store.TxnSpec{
			Kind:      store.TxnAdjustment,
			Memo:      "Manual adjustment: " + adjustment.Reason,
			RefType:   "manual_adjustment",
			RefID:     adjustment.ID,
			CreatedBy: approverID,
			Entries: []store.Entry{
				{Account: store.AccountUserCash, UserID: adjustment.UserID, AmountSat: adjustment.AmountSat},
				{Account: store.AccountHouseRevenue, AmountSat: -adjustment.AmountSat},
			},
		})
		if err != nil {
			return err
		}
		if err := store.DecideManualAdjustmentTx(ctx, tx, adjustmentID,
			store.AdjustmentApplied, approverID, txnID, note, at); err != nil {
			return err
		}
		newBalance = balance + adjustment.AmountSat
		return nil
	})
	if err != nil {
		return 0, err
	}

	return newBalance, s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: approverID, Action: "adjustment_applied",
		Detail: fmt.Sprintf("adjustment %d applied: %s", adjustmentID, note),
	})
}

// Pending lists adjustments waiting for a decision.
func (s *Service) Pending(ctx context.Context) ([]store.ManualAdjustment, error) {
	return s.store.ManualAdjustmentsByStatus(ctx, store.AdjustmentPending)
}

// Recent lists decided adjustments, for the audit view.
func (s *Service) Recent(ctx context.Context) ([]store.ManualAdjustment, error) {
	return s.store.ManualAdjustmentsByStatus(ctx, store.AdjustmentApplied, store.AdjustmentRejected)
}

// ApprovalThresholdSat is the amount at or above which a second person is
// needed, for display in the back office.
func (s *Service) ApprovalThresholdSat() int64 { return s.cfg.ManualApprovalSat }
