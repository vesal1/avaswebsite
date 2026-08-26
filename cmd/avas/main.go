// Command avas runs the Avas Sportsbook.
//
//	avas serve                 start the web server
//	avas seed                  load demonstration events across many sports
//	avas create-user           create an account, optionally with a staff role
//	avas sync-deposits         run one deposit reconciliation pass
//	avas check-ledger          verify the ledger invariants
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vesal1/avaswebsite/internal/auth"
	"github.com/vesal1/avaswebsite/internal/betting"
	"github.com/vesal1/avaswebsite/internal/bitcoin"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/seed"
	"github.com/vesal1/avaswebsite/internal/store"
	"github.com/vesal1/avaswebsite/internal/wallet"
	"github.com/vesal1/avaswebsite/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "avas:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "serve":
		return serve(args)
	case "seed":
		return runSeed(args)
	case "create-user":
		return createUser(args)
	case "sync-deposits":
		return syncDeposits(args)
	case "check-ledger":
		return checkLedger(args)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `avas - the Avas Sportsbook

Usage:
  avas serve                       start the web server
  avas seed [-events N]            load demonstration events across many sports
  avas create-user -email E -password P [-role R] [-dob YYYY-MM-DD] [-country CC]
  avas sync-deposits               run one deposit reconciliation pass
  avas check-ledger                verify the ledger invariants and exit

Configuration is read from the environment; see README.md.
`)
}

// application wires the layers together. Every command builds the same graph,
// so a background job runs against exactly the code the web server does.
type application struct {
	cfg        *config.Config
	store      *store.Store
	provider   bitcoin.Provider
	compliance *compliance.Service
	wallet     *wallet.Service
	betting    *betting.Service
	log        *slog.Logger
}

func build() (*application, error) {
	cfg := config.Load()

	level := slog.LevelInfo
	if !cfg.IsProduction() {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Refuse to serve real money with a development default still in place.
	if problems := cfg.Validate(); len(problems) > 0 {
		for _, problem := range problems {
			logger.Error("configuration blocks production start", "problem", problem)
		}
		return nil, errors.New("refusing to start in production with an unsafe configuration")
	}

	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}

	provider, err := bitcoin.New(cfg.BitcoinProvider, bitcoin.Config{
		Network:          cfg.BitcoinNetwork,
		BTCPayURL:        cfg.BTCPayURL,
		BTCPayStoreID:    cfg.BTCPayStoreID,
		BTCPayAPIKey:     cfg.BTCPayAPIKey,
		BitcoindURL:      cfg.BitcoindURL,
		BitcoindUser:     cfg.BitcoindUser,
		BitcoindPassword: cfg.BitcoindPass,
		BitcoindWallet:   cfg.BitcoindWallet,
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	comp := compliance.New(db, cfg)
	return &application{
		cfg: cfg, store: db, provider: provider, compliance: comp,
		wallet:  wallet.New(db, provider, comp, cfg),
		betting: betting.New(db, comp, cfg),
		log:     logger,
	}, nil
}

func (a *application) Close() error { return a.store.Close() }

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := flags.String("addr", "", "address to listen on, overriding AVAS_HOST and AVAS_PORT")
	if err := flags.Parse(args); err != nil {
		return err
	}

	app, err := build()
	if err != nil {
		return err
	}
	defer app.Close()

	server, err := web.New(web.Options{
		Config: app.cfg, Store: app.store, Betting: app.betting,
		Wallet: app.wallet, Compliance: app.compliance, Logger: app.log,
	})
	if err != nil {
		return err
	}

	listenAddr := *addr
	if listenAddr == "" {
		listenAddr = net.JoinHostPort(app.cfg.Host, strconv.Itoa(app.cfg.Port))
	}
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background upkeep: reconcile deposits, expire sessions and check the
	// ledger on a timer, so a break is noticed without waiting for a request.
	go app.background(ctx)

	errc := make(chan error, 1)
	go func() {
		app.log.Info("listening",
			"addr", listenAddr, "env", app.cfg.Env,
			"bitcoin", app.provider.Name(), "network", app.provider.Network())
		if !app.cfg.IsProduction() {
			app.log.Warn("development mode: the wallet is simulated and no licence is in force")
		}
		if len(app.cfg.AllowedCountries) == 0 {
			app.log.Warn("no allowed countries configured; every visitor will be geo-blocked",
				"hint", "set AVAS_ALLOWED_COUNTRIES, and AVAS_GEO_DEFAULT for local development")
		}
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		app.log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// background runs the periodic jobs.
func (a *application) background(ctx context.Context) {
	deposits := time.NewTicker(60 * time.Second)
	upkeep := time.NewTicker(15 * time.Minute)
	defer deposits.Stop()
	defer upkeep.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deposits.C:
			result, err := a.wallet.SyncDeposits(ctx, time.Time{})
			if err != nil {
				a.log.Error("deposit sync", "error", err)
				continue
			}
			if result.Credited > 0 || len(result.Errors) > 0 {
				a.log.Info("deposit sync", "credited", result.Credited,
					"recorded", result.Recorded, "errors", result.Errors)
			}
		case <-upkeep.C:
			if removed, err := a.store.PurgeExpiredSessions(ctx); err != nil {
				a.log.Error("purge sessions", "error", err)
			} else if removed > 0 {
				a.log.Debug("purged expired sessions", "count", removed)
			}
			problems, err := a.store.CheckLedger(ctx)
			if err != nil {
				a.log.Error("ledger check", "error", err)
				continue
			}
			for _, problem := range problems {
				// A broken ledger is the loudest thing this service can say.
				a.log.Error("LEDGER INVARIANT BROKEN", "problem", problem.String())
			}
		}
	}
}

func runSeed(args []string) error {
	flags := flag.NewFlagSet("seed", flag.ExitOnError)
	count := flags.Int("events", 60, "roughly how many events to create")
	if err := flags.Parse(args); err != nil {
		return err
	}

	app, err := build()
	if err != nil {
		return err
	}
	defer app.Close()

	summary, err := seed.Load(context.Background(), app.store, app.cfg, *count)
	if err != nil {
		return err
	}
	fmt.Printf("seeded %d events across %d sports with %d markets and %d prices\n",
		summary.Events, summary.Sports, summary.Markets, summary.Selections)
	return nil
}

func createUser(args []string) error {
	flags := flag.NewFlagSet("create-user", flag.ExitOnError)
	email := flags.String("email", "", "email address")
	password := flags.String("password", "", "password")
	role := flags.String("role", store.RoleCustomer, "customer, trader, compliance or admin")
	dob := flags.String("dob", "1990-01-01", "date of birth, YYYY-MM-DD")
	country := flags.String("country", "GB", "ISO country code")
	verified := flags.Bool("verified", false, "mark identity as already verified")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" || *password == "" {
		return errors.New("create-user needs -email and -password")
	}
	switch *role {
	case store.RoleCustomer, store.RoleTrader, store.RoleCompliance, store.RoleAdmin:
	default:
		return fmt.Errorf("unknown role %q", *role)
	}

	app, err := build()
	if err != nil {
		return err
	}
	defer app.Close()

	hash, err := auth.HashPassword(*password)
	if err != nil {
		return err
	}
	ctx := context.Background()
	userID, err := app.store.CreateUser(ctx, store.NewUser{
		Email: *email, PasswordHash: hash, DisplayName: strings.SplitN(*email, "@", 2)[0],
		DateOfBirth: *dob, Country: *country, Role: *role,
	})
	if err != nil {
		return err
	}
	if *verified || *role != store.RoleCustomer {
		if err := app.store.SetKYCStatus(ctx, userID, store.KYCVerified); err != nil {
			return err
		}
	}
	if err := app.store.Audit(ctx, store.AuditEntry{
		SubjectUserID: userID, Action: "account_created_by_cli", Detail: "role " + *role,
	}); err != nil {
		return err
	}
	fmt.Printf("created user %d (%s) with role %s\n", userID, *email, *role)
	return nil
}

func syncDeposits(args []string) error {
	app, err := build()
	if err != nil {
		return err
	}
	defer app.Close()

	result, err := app.wallet.SyncDeposits(context.Background(), time.Time{})
	if err != nil {
		return err
	}
	fmt.Printf("seen %d, recorded %d, credited %d, skipped %d\n",
		result.Seen, result.Recorded, result.Credited, result.Skipped)
	for _, message := range result.Errors {
		fmt.Fprintln(os.Stderr, "  needs attention:", message)
	}
	return nil
}

func checkLedger(args []string) error {
	app, err := build()
	if err != nil {
		return err
	}
	defer app.Close()

	ctx := context.Background()
	problems, err := app.store.CheckLedger(ctx)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		total, _ := app.store.LedgerTotal(ctx)
		fmt.Printf("ledger is sound: every transaction balances and the total is %d\n", total)
		return nil
	}
	for _, problem := range problems {
		fmt.Fprintln(os.Stderr, "BROKEN:", problem.String())
	}
	return fmt.Errorf("%d ledger invariant(s) broken", len(problems))
}
