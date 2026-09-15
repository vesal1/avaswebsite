// Package config holds runtime settings, read from the environment.
//
// Every default here is a development default. Validate reports the settings
// that must be changed before the book can legally take a real wager, and the
// server refuses to start in production while any of them remain.
package config

import (
	"os"
	"strconv"
	"strings"
)

// DevSecret is the placeholder session key. Production must override it.
const DevSecret = "dev-only-insecure-secret-change-me"

// Config is the fully resolved runtime configuration.
type Config struct {
	// core
	Env          string
	SecretKey    string
	DatabasePath string
	Host         string
	Port         int

	// brand
	BrandName   string
	LicenceText string
	SupportMail string

	// betting limits, in satoshi
	MinStakeSat        int64
	MaxStakeSat        int64
	MaxPayoutSat       int64
	MaxParlayLegs      int
	MaxLiabilityPerSel int64
	DefaultMarginBps   int64

	// bitcoin
	BitcoinProvider      string
	BitcoinNetwork       string
	DepositConfirmations int
	MinDepositSat        int64
	MinWithdrawalSat     int64
	WithdrawalFeeSat     int64
	ManualReviewSat      int64

	BTCPayURL      string
	BTCPayStoreID  string
	BTCPayAPIKey   string
	BitcoindURL    string
	BitcoindUser   string
	BitcoindPass   string
	BitcoindWallet string

	// bonuses and manual money movement
	AllowTestCredits    bool
	ManualApprovalSat   int64
	MaxManualAdjustSat  int64
	DefaultWageringX100 int64

	// compliance
	AllowedCountries  []string
	BlockedCountries  []string
	MinimumAge        int
	KYCRequiredToBet  bool
	KYCThresholdSat   int64
	RealityCheckMin   int
	GeoHeader         string
	GeoDefaultCountry string
	AMLReportingSat   int64

	// display
	DisplayCurrency string
	StaticBTCRate   int64
	RateProvider    string
}

// Load reads configuration from the process environment.
func Load() *Config {
	return &Config{
		Env:          env("AVAS_ENV", "development"),
		SecretKey:    env("AVAS_SECRET_KEY", DevSecret),
		DatabasePath: env("AVAS_DB", "avas.sqlite3"),
		Host:         env("AVAS_HOST", "127.0.0.1"),
		Port:         envInt("AVAS_PORT", 8000),

		BrandName:   env("AVAS_BRAND", "Avas Sportsbook"),
		LicenceText: env("AVAS_LICENCE", "Licence not configured - set AVAS_LICENCE before going live."),
		SupportMail: env("AVAS_SUPPORT_EMAIL", "support@example.invalid"),

		MinStakeSat:        envInt64("AVAS_MIN_STAKE_SAT", 1_000),
		MaxStakeSat:        envInt64("AVAS_MAX_STAKE_SAT", 50_000_000),
		MaxPayoutSat:       envInt64("AVAS_MAX_PAYOUT_SAT", 500_000_000),
		MaxParlayLegs:      envInt("AVAS_MAX_PARLAY_LEGS", 15),
		MaxLiabilityPerSel: envInt64("AVAS_MAX_LIABILITY_SAT", 2_000_000_000),
		DefaultMarginBps:   envInt64("AVAS_MARGIN_BPS", 500),

		BitcoinProvider:      env("AVAS_BTC_PROVIDER", "mock"),
		BitcoinNetwork:       env("AVAS_BTC_NETWORK", "regtest"),
		DepositConfirmations: envInt("AVAS_BTC_CONFIRMATIONS", 2),
		MinDepositSat:        envInt64("AVAS_MIN_DEPOSIT_SAT", 20_000),
		MinWithdrawalSat:     envInt64("AVAS_MIN_WITHDRAWAL_SAT", 50_000),
		WithdrawalFeeSat:     envInt64("AVAS_WITHDRAWAL_FEE_SAT", 2_500),
		ManualReviewSat:      envInt64("AVAS_MANUAL_REVIEW_SAT", 100_000_000),

		BTCPayURL:      env("AVAS_BTCPAY_URL", ""),
		BTCPayStoreID:  env("AVAS_BTCPAY_STORE_ID", ""),
		BTCPayAPIKey:   env("AVAS_BTCPAY_API_KEY", ""),
		BitcoindURL:    env("AVAS_BITCOIND_URL", ""),
		BitcoindUser:   env("AVAS_BITCOIND_USER", ""),
		BitcoindPass:   env("AVAS_BITCOIND_PASSWORD", ""),
		BitcoindWallet: env("AVAS_BITCOIND_WALLET", ""),

		// Test money must not coexist with real customer money. Production
		// refuses it unless somebody has deliberately said otherwise, and
		// Validate warns loudly when they have.
		AllowTestCredits:    envBool("AVAS_ALLOW_TEST_CREDITS", false),
		ManualApprovalSat:   envInt64("AVAS_MANUAL_APPROVAL_SAT", 10_000_000),
		MaxManualAdjustSat:  envInt64("AVAS_MAX_MANUAL_ADJUST_SAT", 1_000_000_000),
		DefaultWageringX100: envInt64("AVAS_DEFAULT_WAGERING_X100", 500),

		// An empty allowlist blocks everyone. Failing closed is the only safe
		// default: an unlicensed jurisdiction must never be served by accident.
		AllowedCountries:  envList("AVAS_ALLOWED_COUNTRIES", ""),
		BlockedCountries:  envList("AVAS_BLOCKED_COUNTRIES", ""),
		MinimumAge:        envInt("AVAS_MIN_AGE", 18),
		KYCRequiredToBet:  envBool("AVAS_KYC_FOR_BETTING", true),
		KYCThresholdSat:   envInt64("AVAS_KYC_THRESHOLD_SAT", 0),
		RealityCheckMin:   envInt("AVAS_REALITY_CHECK_MIN", 60),
		GeoHeader:         env("AVAS_GEO_HEADER", "CF-IPCountry"),
		GeoDefaultCountry: env("AVAS_GEO_DEFAULT", ""),
		AMLReportingSat:   envInt64("AVAS_AML_REPORTING_SAT", 100_000_000),

		DisplayCurrency: env("AVAS_DISPLAY_CURRENCY", "USD"),
		StaticBTCRate:   envInt64("AVAS_STATIC_BTC_RATE", 60_000),
		RateProvider:    env("AVAS_RATE_PROVIDER", "static"),
	}
}

// IsProduction reports whether this process believes it is serving real money.
func (c *Config) IsProduction() bool {
	switch strings.ToLower(c.Env) {
	case "production", "prod", "live":
		return true
	}
	return false
}

// Validate returns the misconfigurations that block a production launch.
// It is deliberately blunt: each item is a legal or custody risk, not a nit.
func (c *Config) Validate() []string {
	if !c.IsProduction() {
		return nil
	}
	var problems []string
	if c.SecretKey == DevSecret || len(c.SecretKey) < 32 {
		problems = append(problems, "AVAS_SECRET_KEY must be a unique value of at least 32 characters")
	}
	if c.BitcoinProvider == "mock" {
		problems = append(problems, "AVAS_BTC_PROVIDER=mock cannot hold customer funds")
	}
	if c.BitcoinNetwork != "mainnet" {
		problems = append(problems, "AVAS_BTC_NETWORK must be 'mainnet' in production")
	}
	if len(c.AllowedCountries) == 0 {
		problems = append(problems, "AVAS_ALLOWED_COUNTRIES is empty: no jurisdiction has been declared licensed")
	}
	if c.GeoDefaultCountry != "" {
		problems = append(problems, "AVAS_GEO_DEFAULT bypasses geolocation and must be unset in production")
	}
	if strings.Contains(c.LicenceText, "not configured") {
		problems = append(problems, "AVAS_LICENCE must carry the operating licence details shown in the footer")
	}
	if c.AllowTestCredits {
		problems = append(problems,
			"AVAS_ALLOW_TEST_CREDITS=true lets staff mint money that is not backed by a deposit")
	}
	if !c.KYCRequiredToBet {
		problems = append(problems, "AVAS_KYC_FOR_BETTING=false disables identity checks required by AML rules")
	}
	return problems
}

func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(env(name, "")); err == nil {
		return v
	}
	return fallback
}

func envInt64(name string, fallback int64) int64 {
	if v, err := strconv.ParseInt(env(name, ""), 10, 64); err == nil {
		return v
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

func envList(name, fallback string) []string {
	raw := env(name, fallback)
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.ToUpper(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return out
}
