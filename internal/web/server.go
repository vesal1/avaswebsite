// Package web serves the sportsbook: customer pages, the bet slip, the wallet
// and the back office.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/betting"
	"github.com/vesal1/avaswebsite/internal/bonus"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/pokerhouse"
	"github.com/vesal1/avaswebsite/internal/store"
	"github.com/vesal1/avaswebsite/internal/treasury"
	"github.com/vesal1/avaswebsite/internal/wallet"
)

//go:embed templates/*.html
var templateFS embed.FS

// sharedTemplates are parsed into every page set.
var sharedTemplates = []string{"templates/layout.html", "templates/partials.html"}

// parseTemplates builds one template set per page.
func parseTemplates() (map[string]*template.Template, error) {
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	sets := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		if slices.Contains(sharedTemplates, page) {
			continue
		}
		files := append(append([]string{}, sharedTemplates...), page)
		set, err := template.New(path.Base(page)).Funcs(templateFuncs()).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", page, err)
		}
		sets[path.Base(page)] = set
	}
	return sets, nil
}

//go:embed static/*
var staticFS embed.FS

// Server holds everything the handlers need.
type Server struct {
	cfg        *config.Config
	store      *store.Store
	betting    *betting.Service
	wallet     *wallet.Service
	compliance *compliance.Service
	bonus      *bonus.Service
	treasury   *treasury.Service
	poker      *pokerhouse.House
	signals    *pokerhouse.Signalling
	log        *slog.Logger
	// templates holds one parsed set per page. Each page defines a template
	// named "content" that the shared layout calls, so the sets have to be
	// kept apart: parsing them together would leave only the last "content".
	templates map[string]*template.Template
	mux       *http.ServeMux
	now       func() time.Time
}

// Options are the dependencies a server is built from.
type Options struct {
	Config     *config.Config
	Store      *store.Store
	Betting    *betting.Service
	Wallet     *wallet.Service
	Compliance *compliance.Service
	Bonus      *bonus.Service
	Treasury   *treasury.Service
	Poker      *pokerhouse.House
	Signalling *pokerhouse.Signalling
	Logger     *slog.Logger
}

// New builds the HTTP server.
//
// Missing dependencies are refused here rather than left to panic on the first
// request that needs them. A server that starts and then dies on a customer's
// wallet page is worse than one that never starts.
func New(opts Options) (*Server, error) {
	for name, present := range map[string]bool{
		"Config":     opts.Config != nil,
		"Store":      opts.Store != nil,
		"Betting":    opts.Betting != nil,
		"Wallet":     opts.Wallet != nil,
		"Compliance": opts.Compliance != nil,
		"Bonus":      opts.Bonus != nil,
		"Treasury":   opts.Treasury != nil,
		"Poker":      opts.Poker != nil,
		"Signalling": opts.Signalling != nil,
		"Logger":     opts.Logger != nil,
	} {
		if !present {
			return nil, fmt.Errorf("web: %s is required", name)
		}
	}

	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: opts.Config, store: opts.Store, betting: opts.Betting,
		wallet: opts.Wallet, compliance: opts.Compliance,
		bonus: opts.Bonus, treasury: opts.Treasury,
		poker: opts.Poker, signals: opts.Signalling, log: opts.Logger,
		templates: templates, mux: http.NewServeMux(),
		now: func() time.Time { return opts.Store.Now() },
	}
	s.routes()
	return s, nil
}

// ServeHTTP runs the middleware chain around the router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler := s.withSecurityHeaders(s.withSession(s.withLogging(s.mux)))
	handler.ServeHTTP(w, r)
}

func (s *Server) routes() {
	static := http.FileServer(http.FS(staticFS))
	s.mux.Handle("GET /static/", static)

	// Public pages.
	s.mux.HandleFunc("GET /{$}", s.handleHome)
	s.mux.HandleFunc("GET /sports", s.handleSportsIndex)
	s.mux.HandleFunc("GET /sports/{sport}", s.handleSport)
	s.mux.HandleFunc("GET /events/{id}", s.handleEvent)
	s.mux.HandleFunc("GET /rules", s.handleRules)
	s.mux.HandleFunc("GET /responsible-gambling", s.handleResponsibleGambling)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	// Registration and sign-in.
	s.mux.HandleFunc("GET /register", s.handleRegisterForm)
	s.mux.HandleFunc("POST /register", s.handleRegister)
	s.mux.HandleFunc("GET /login", s.handleLoginForm)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	// Bet slip.
	s.mux.HandleFunc("GET /betslip", s.requireCustomer(s.handleBetslip))
	s.mux.HandleFunc("POST /betslip/add", s.requireCustomer(s.handleBetslipAdd))
	s.mux.HandleFunc("POST /betslip/remove", s.requireCustomer(s.handleBetslipRemove))
	s.mux.HandleFunc("POST /betslip/clear", s.requireCustomer(s.handleBetslipClear))
	s.mux.HandleFunc("POST /betslip/place", s.requireCustomer(s.handleBetslipPlace))
	s.mux.HandleFunc("GET /bets", s.requireCustomer(s.handleBets))

	// Wallet.
	s.mux.HandleFunc("GET /wallet", s.requireCustomer(s.handleWallet))
	s.mux.HandleFunc("POST /wallet/address", s.requireCustomer(s.handleWalletAddress))
	s.mux.HandleFunc("POST /wallet/withdraw", s.requireCustomer(s.handleWithdraw))
	s.mux.HandleFunc("POST /wallet/withdraw/{id}/cancel", s.requireCustomer(s.handleCancelWithdrawal))
	s.mux.HandleFunc("POST /wallet/simulate", s.requireCustomer(s.handleSimulateDeposit))

	// Account and player protection.
	s.mux.HandleFunc("GET /account", s.requireCustomer(s.handleAccount))
	s.mux.HandleFunc("POST /account/odds-format", s.requireCustomer(s.handleOddsFormat))
	s.mux.HandleFunc("POST /account/limits", s.requireCustomer(s.handleSetLimit))
	s.mux.HandleFunc("POST /account/break", s.requireCustomer(s.handleTakeABreak))
	s.mux.HandleFunc("POST /account/self-exclude", s.requireCustomer(s.handleSelfExclude))
	s.mux.HandleFunc("POST /account/verify", s.requireCustomer(s.handleSubmitKYC))
	s.mux.HandleFunc("GET /reality-check", s.requireCustomer(s.handleRealityCheck))
	s.mux.HandleFunc("POST /reality-check", s.requireCustomer(s.handleRealityCheckAck))

	// Back office.
	s.mux.HandleFunc("GET /admin", s.requireStaff(s.handleAdminDashboard))
	s.mux.HandleFunc("GET /admin/events", s.requireTrader(s.handleAdminEvents))
	s.mux.HandleFunc("POST /admin/events", s.requireTrader(s.handleAdminCreateEvent))
	s.mux.HandleFunc("GET /admin/events/{id}", s.requireTrader(s.handleAdminEvent))
	s.mux.HandleFunc("POST /admin/events/{id}/odds", s.requireTrader(s.handleAdminUpdateOdds))
	s.mux.HandleFunc("POST /admin/events/{id}/status", s.requireTrader(s.handleAdminEventStatus))
	s.mux.HandleFunc("POST /admin/events/{id}/settle", s.requireTrader(s.handleAdminSettle))
	s.mux.HandleFunc("POST /admin/markets/{id}/settle", s.requireTrader(s.handleAdminSettleMarket))
	s.mux.HandleFunc("POST /admin/markets/{id}/void", s.requireTrader(s.handleAdminVoidMarket))
	s.mux.HandleFunc("GET /admin/withdrawals", s.requireCompliance(s.handleAdminWithdrawals))
	s.mux.HandleFunc("POST /admin/withdrawals/{id}/release", s.requireCompliance(s.handleReleaseWithdrawal))
	s.mux.HandleFunc("POST /admin/withdrawals/{id}/reject", s.requireCompliance(s.handleRejectWithdrawal))
	s.mux.HandleFunc("GET /admin/flags", s.requireCompliance(s.handleAdminFlags))
	s.mux.HandleFunc("POST /admin/flags/{id}/resolve", s.requireCompliance(s.handleResolveFlag))
	// Poker.
	s.mux.HandleFunc("GET /poker", s.handlePokerLobby)
	s.mux.HandleFunc("GET /poker/verify", s.handlePokerVerifyForm)
	s.mux.HandleFunc("POST /poker/verify", s.handlePokerVerify)
	s.mux.HandleFunc("GET /poker/hands/{id}", s.handlePokerHand)
	s.mux.HandleFunc("GET /poker/my-hands", s.requireCustomer(s.handlePokerMyHands))
	s.mux.HandleFunc("GET /poker/tables/{id}", s.handlePokerTable)
	s.mux.HandleFunc("GET /poker/tables/{id}/stream", s.handlePokerStream)
	s.mux.HandleFunc("POST /poker/tables/{id}/sit", s.requireCustomer(s.handlePokerSit))
	s.mux.HandleFunc("POST /poker/tables/{id}/leave", s.requireCustomer(s.handlePokerLeave))
	s.mux.HandleFunc("POST /poker/tables/{id}/act", s.requireCustomer(s.handlePokerAct))
	s.mux.HandleFunc("POST /poker/tables/{id}/seed", s.requireCustomer(s.handlePokerSeed))
	s.mux.HandleFunc("POST /poker/tables/{id}/video", s.requireCustomer(s.handlePokerVideoConsent))
	s.mux.HandleFunc("POST /poker/tables/{id}/signal", s.requireCustomer(s.handlePokerSignal))

	s.mux.HandleFunc("GET /promotions", s.handlePromotions)
	s.mux.HandleFunc("POST /account/forfeit-bonus", s.requireCustomer(s.handleForfeitBonus))

	s.mux.HandleFunc("GET /admin/poker", s.requireTrader(s.handleAdminPoker))
	s.mux.HandleFunc("POST /admin/poker", s.requireTrader(s.handleAdminCreatePokerTable))
	s.mux.HandleFunc("POST /admin/poker/{id}/toggle", s.requireTrader(s.handleAdminTogglePokerTable))
	s.mux.HandleFunc("GET /admin/players", s.requireStaff(s.handleAdminPlayers))
	s.mux.HandleFunc("GET /admin/players/{id}", s.requireStaff(s.handleAdminPlayer))
	s.mux.HandleFunc("POST /admin/players/{id}/bonus", s.requireCompliance(s.handleAdminGrantBonus))
	s.mux.HandleFunc("POST /admin/players/{id}/adjust", s.requireCompliance(s.handleAdminAdjust))
	s.mux.HandleFunc("POST /admin/bonus/{id}/cancel", s.requireCompliance(s.handleAdminCancelBonus))
	s.mux.HandleFunc("GET /admin/adjustments", s.requireCompliance(s.handleAdminAdjustments))
	s.mux.HandleFunc("POST /admin/adjustments/{id}/approve", s.requireCompliance(s.handleApproveAdjustment))
	s.mux.HandleFunc("POST /admin/adjustments/{id}/reject", s.requireCompliance(s.handleRejectAdjustment))
	s.mux.HandleFunc("GET /admin/offers", s.requireCompliance(s.handleAdminOffers))
	s.mux.HandleFunc("POST /admin/offers", s.requireCompliance(s.handleCreateOffer))
	s.mux.HandleFunc("POST /admin/offers/{id}/toggle", s.requireCompliance(s.handleToggleOffer))
	s.mux.HandleFunc("GET /admin/kyc", s.requireCompliance(s.handleAdminKYC))
	s.mux.HandleFunc("POST /admin/kyc/{id}/decide", s.requireCompliance(s.handleDecideKYC))
	s.mux.HandleFunc("POST /admin/sync-deposits", s.requireStaff(s.handleSyncDeposits))

	// Read-only JSON, for a client that would rather render its own board.
	s.mux.HandleFunc("GET /api/sports", s.handleAPISports)
	s.mux.HandleFunc("GET /api/sports/{sport}/events", s.handleAPIEvents)
	s.mux.HandleFunc("GET /api/events/{id}", s.handleAPIEvent)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type contextKey string

const (
	ctxUser    contextKey = "user"
	ctxSession contextKey = "session"
	ctxCountry contextKey = "country"
	// ctxToken carries the raw session cookie value for the life of the
	// request. CSRF tokens are derived from it, so it must never be persisted
	// or logged: only its hash is stored.
	ctxToken contextKey = "session_token"
)

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", recorder.status,
			"duration", s.now().Sub(start))
	})
}

// statusRecorder notes the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap exposes the wrapped writer so http.ResponseController can reach the
// capabilities this wrapper does not itself implement. Without it, wrapping
// the writer silently strips flushing, which breaks every streaming response
// on the site.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush passes a flush through, so a wrapped writer still streams.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		// No inline script is used anywhere, so the policy can forbid it
		// outright rather than carrying a nonce.
		header.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		header.Set("X-Frame-Options", "DENY")
		if s.cfg.IsProduction() {
			header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// withSession resolves the signed-in customer, if any, and records the
// country the request appears to come from.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxCountry, s.countryFor(r))

		cookie, err := r.Cookie(sessionCookieName)
		if err == nil && cookie.Value != "" {
			session, user, ok := s.resolveSession(r.Context(), cookie.Value)
			if ok {
				ctx = context.WithValue(ctx, ctxSession, session)
				ctx = context.WithValue(ctx, ctxUser, user)
				ctx = context.WithValue(ctx, ctxToken, cookie.Value)
			} else {
				clearSessionCookie(w, s.cfg.IsProduction())
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// countryFor determines the request's country from the geolocation header set
// by the edge. An unset header yields an empty country, which the compliance
// layer treats as "not permitted" rather than "unrestricted".
func (s *Server) countryFor(r *http.Request) string {
	if header := s.cfg.GeoHeader; header != "" {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return strings.ToUpper(value)
		}
	}
	// A development-only fallback. config.Validate refuses to start a
	// production server while it is set.
	return strings.ToUpper(s.cfg.GeoDefaultCountry)
}

// Country reads the request's country.
func Country(ctx context.Context) string {
	country, _ := ctx.Value(ctxCountry).(string)
	return country
}

// CurrentUser reads the signed-in customer, if any.
func CurrentUser(ctx context.Context) (store.User, bool) {
	user, ok := ctx.Value(ctxUser).(store.User)
	return user, ok
}

// CurrentSession reads the current session, if any.
func CurrentSession(ctx context.Context) (store.Session, bool) {
	session, ok := ctx.Value(ctxSession).(store.Session)
	return session, ok
}
