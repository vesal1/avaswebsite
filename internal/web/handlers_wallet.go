package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/bitcoin"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/wallet"
)

func (s *Server) handleWallet(w http.ResponseWriter, r *http.Request) {
	s.renderWallet(w, r, http.StatusOK, r.URL.Query().Get("flash"), "")
}

func (s *Server) renderWallet(w http.ResponseWriter, r *http.Request, status int, flash, message string) {
	ctx := r.Context()
	user, _ := CurrentUser(ctx)

	summary, err := s.wallet.Summary(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	deposits, err := s.store.DepositsForUser(ctx, user.ID, 25)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	withdrawals, err := s.store.WithdrawalsForUser(ctx, user.ID, 25)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ledger, err := s.store.LedgerHistory(ctx, user.ID, 50)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, status, "wallet.html", map[string]any{
		"Title":         "Wallet",
		"Flash":         flash,
		"Error":         message,
		"Summary":       summary,
		"Deposits":      deposits,
		"Withdrawals":   withdrawals,
		"Ledger":        ledger,
		"MinWithdrawal": s.cfg.MinWithdrawalSat,
		"WithdrawalFee": s.cfg.WithdrawalFeeSat,
		"Rate":          s.cfg.StaticBTCRate,
		"Currency":      s.cfg.DisplayCurrency,
		"KYCVerified":   user.KYCStatus == "verified",
	})
}

func (s *Server) handleWalletAddress(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	if _, err := s.wallet.EnsureDepositAddress(r.Context(), user.ID); err != nil {
		s.log.Error("issue deposit address", "user", user.ID, "error", err)
		s.renderWallet(w, r, http.StatusBadGateway,
			"", "We could not reach the payment system to issue an address. Try again shortly.")
		return
	}
	http.Redirect(w, r, "/wallet?flash=Your+deposit+address+is+ready.", http.StatusSeeOther)
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())

	address := strings.TrimSpace(r.FormValue("address"))
	amountSat, err := money.ParseBTC(r.FormValue("amount"))
	if err != nil {
		s.renderWallet(w, r, http.StatusBadRequest, "",
			"Enter an amount in BTC, for example 0.00500000.")
		return
	}
	if amountSat <= 0 {
		s.renderWallet(w, r, http.StatusBadRequest, "", "Enter an amount greater than zero.")
		return
	}

	id, err := s.wallet.RequestWithdrawal(r.Context(), user.ID, address, amountSat)
	if err != nil {
		switch {
		case errors.Is(err, bitcoin.ErrInvalidAddress):
			s.renderWallet(w, r, http.StatusBadRequest, "",
				"That does not look like a valid "+s.wallet.Provider().Network()+
					" bitcoin address. Check it carefully: a payment to the wrong "+
					"address cannot be recovered.")
		case errors.Is(err, wallet.ErrInsufficientFunds):
			s.renderWallet(w, r, http.StatusBadRequest, "",
				"That is more than your available balance, once the "+
					money.FormatBTC(s.cfg.WithdrawalFeeSat)+" BTC network fee is included.")
		default:
			if refusal, ok := compliance.AsRefusal(err); ok {
				s.renderWallet(w, r, http.StatusForbidden, "", refusal.Message)
				return
			}
			s.serverError(w, r, err)
		}
		return
	}

	http.Redirect(w, r, "/wallet?flash=Withdrawal+"+strconv.FormatInt(id, 10)+
		"+requested.+We+will+email+you+when+it+is+sent.", http.StatusSeeOther)
}

func (s *Server) handleCancelWithdrawal(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	user, _ := CurrentUser(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That withdrawal could not be read.")
		return
	}
	if err := s.wallet.CancelWithdrawal(r.Context(), user.ID, id); err != nil {
		if errors.Is(err, wallet.ErrNotPermitted) {
			s.renderWallet(w, r, http.StatusConflict, "",
				"That withdrawal has already been processed and cannot be cancelled.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/wallet?flash=Withdrawal+cancelled+and+funds+returned.", http.StatusSeeOther)
}

// handleSimulateDeposit pays a customer's own deposit address on the mock
// provider, so the deposit and crediting path can be walked end to end in
// development. It refuses to do anything on a real provider.
func (s *Server) handleSimulateDeposit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	mock, ok := s.wallet.Provider().(*bitcoin.Mock)
	if !ok || s.cfg.IsProduction() {
		s.renderError(w, r, http.StatusNotFound, "Page not found")
		return
	}

	ctx := r.Context()
	user, _ := CurrentUser(ctx)
	address, err := s.wallet.EnsureDepositAddress(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	amountSat, err := money.ParseBTC(r.FormValue("amount"))
	if err != nil || amountSat <= 0 {
		s.renderWallet(w, r, http.StatusBadRequest, "", "Enter an amount in BTC to simulate.")
		return
	}
	if _, err := mock.CreditPayment(address, amountSat, s.cfg.DepositConfirmations); err != nil {
		s.serverError(w, r, err)
		return
	}
	result, err := s.wallet.SyncDeposits(ctx, time.Time{})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	flash := "Simulated a deposit of " + money.FormatBTC(amountSat) + " BTC. " +
		strconv.Itoa(result.Credited) + " deposit(s) credited."
	s.renderWallet(w, r, http.StatusOK, flash, "")
}

func (s *Server) handleSyncDeposits(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	result, err := s.wallet.SyncDeposits(r.Context(), time.Time{})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("deposit sync", "seen", result.Seen, "recorded", result.Recorded,
		"credited", result.Credited, "skipped", result.Skipped, "errors", result.Errors)
	http.Redirect(w, r, "/admin?flash=Synced+"+strconv.Itoa(result.Seen)+
		"+payments,+credited+"+strconv.Itoa(result.Credited)+".", http.StatusSeeOther)
}
