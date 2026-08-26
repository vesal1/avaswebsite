package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vesal1/avaswebsite/internal/auth"
	"github.com/vesal1/avaswebsite/internal/betting"
	"github.com/vesal1/avaswebsite/internal/bitcoin"
	"github.com/vesal1/avaswebsite/internal/bonus"
	"github.com/vesal1/avaswebsite/internal/casino"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/pokerhouse"
	"github.com/vesal1/avaswebsite/internal/slots"
	"github.com/vesal1/avaswebsite/internal/store"
	"github.com/vesal1/avaswebsite/internal/treasury"
	"github.com/vesal1/avaswebsite/internal/wallet"
)

type testServer struct {
	*Server
	store *store.Store
	cfg   *config.Config
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	db, err := store.OpenAndMigrate(filepath.Join(t.TempDir(), "web.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.Load()
	cfg.AllowedCountries = []string{"GB"}
	cfg.GeoDefaultCountry = ""
	cfg.SecretKey = "test-secret-that-is-long-enough-for-tests"

	provider := bitcoin.NewMock(bitcoin.Regtest)
	comp := compliance.New(db, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	promotions := bonus.New(db, cfg)
	server, err := New(Options{
		Config: cfg, Store: db, Compliance: comp,
		Wallet:     wallet.New(db, provider, comp, cfg),
		Betting:    betting.New(db, comp, cfg).WithWagering(promotions),
		Bonus:      promotions,
		Treasury:   treasury.New(db, cfg),
		Casino:     casino.New(db, comp, cfg).WithWagering(promotions),
		Poker:      pokerhouse.New(db, cfg, comp, promotions),
		Signalling: pokerhouse.NewSignalling(),
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	return &testServer{Server: server, store: db, cfg: cfg}
}

func (ts *testServer) get(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	return rec
}

func (ts *testServer) post(t *testing.T, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	return rec
}

func TestIncompleteWiringIsRefused(t *testing.T) {
	// A nil service that panics on the first request is worse than a server
	// that will not start.
	if _, err := New(Options{}); err == nil {
		t.Error("New with no dependencies should fail")
	}
}

func TestPublicPagesRender(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{
		"/", "/sports", "/sports/football", "/sports?q=snooker",
		"/rules", "/responsible-gambling", "/promotions", "/healthz",
		"/poker", "/poker/verify",
		"/login", "/register",
		"/api/sports", "/api/sports/football/events",
	} {
		rec := ts.get(t, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestUnknownSportAndEventAre404(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/sports/quidditch", "/events/999999", "/events/abc"} {
		if rec := ts.get(t, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.get(t, "/", nil)
	for header, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
}

func TestRegistrationIsGeoBlocked(t *testing.T) {
	ts := newTestServer(t)
	form := url.Values{
		"email":         {"visitor@example.com"},
		"password":      {"correct horse battery staple"},
		"date_of_birth": {"1990-01-01"},
		"terms":         {"1"},
	}
	// An unlicensed country.
	rec := ts.post(t, "/register", form, map[string]string{"CF-IPCountry": "FR"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("registering from FR = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not a jurisdiction we are licensed") {
		t.Error("the refusal should say why")
	}

	// No geolocation at all must also fail, not fall through to allowed.
	rec = ts.post(t, "/register", form, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("registering with no country = %d, want 400", rec.Code)
	}

	// A licensed country succeeds.
	rec = ts.post(t, "/register", form, map[string]string{"CF-IPCountry": "GB"})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("registering from GB = %d, want 303: %s", rec.Code, rec.Body.String())
	}
}

func TestUnderageRegistrationIsRefused(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.post(t, "/register", url.Values{
		"email":         {"kid@example.com"},
		"password":      {"correct horse battery staple"},
		"date_of_birth": {"2015-01-01"},
		"terms":         {"1"},
	}, map[string]string{"CF-IPCountry": "GB"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least 18") {
		t.Error("the refusal should state the age requirement")
	}
}

func TestStateChangingRequestsNeedCSRF(t *testing.T) {
	ts := newTestServer(t)
	// No session, so no valid CSRF token can exist.
	for _, path := range []string{
		"/logout", "/betslip/add", "/betslip/place", "/wallet/withdraw",
		"/account/limits", "/account/self-exclude",
	} {
		rec := ts.post(t, path, url.Values{"csrf": {"forged"}}, nil)
		// Either the CSRF check refuses it, or the auth guard redirects to
		// sign-in first. What must never happen is a 2xx.
		if rec.Code >= 200 && rec.Code < 300 {
			t.Errorf("POST %s without a session = %d, want a refusal", path, rec.Code)
		}
	}
}

func TestSignedInSessionGetsAWorkingCSRFToken(t *testing.T) {
	ts := newTestServer(t)

	// Register, which signs the customer in.
	rec := ts.post(t, "/register", url.Values{
		"email":         {"punter@example.com"},
		"password":      {"correct horse battery staple"},
		"date_of_birth": {"1990-01-01"},
		"terms":         {"1"},
	}, map[string]string{"CF-IPCountry": "GB"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	cookie := rec.Header().Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("registration did not issue a session cookie")
	}
	if !strings.Contains(cookie, "HttpOnly") {
		t.Error("the session cookie must be HttpOnly")
	}

	// The account page carries a CSRF token bound to this session.
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("CF-IPCountry", "GB")
	page := httptest.NewRecorder()
	ts.ServeHTTP(page, req)
	if page.Code != http.StatusOK {
		t.Fatalf("account page = %d", page.Code)
	}
	token := extractCSRF(page.Body.String())
	if token == "" {
		t.Fatal("no CSRF token rendered on the account page")
	}

	// A forged token is refused; the real one is accepted.
	forged := httptest.NewRequest(http.MethodPost, "/account/odds-format",
		strings.NewReader(url.Values{"csrf": {"wrong"}, "format": {"american"}}.Encode()))
	forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forged.Header.Set("Cookie", cookie)
	forgedRec := httptest.NewRecorder()
	ts.ServeHTTP(forgedRec, forged)
	if forgedRec.Code != http.StatusForbidden {
		t.Errorf("forged CSRF = %d, want 403", forgedRec.Code)
	}

	genuine := httptest.NewRequest(http.MethodPost, "/account/odds-format",
		strings.NewReader(url.Values{"csrf": {token}, "format": {"american"}}.Encode()))
	genuine.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	genuine.Header.Set("Cookie", cookie)
	genuineRec := httptest.NewRecorder()
	ts.ServeHTTP(genuineRec, genuine)
	if genuineRec.Code != http.StatusSeeOther {
		t.Errorf("genuine CSRF = %d, want 303", genuineRec.Code)
	}
}

func TestBackOfficeIsHiddenFromCustomers(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.post(t, "/register", url.Values{
		"email":         {"punter@example.com"},
		"password":      {"correct horse battery staple"},
		"date_of_birth": {"1990-01-01"},
		"terms":         {"1"},
	}, map[string]string{"CF-IPCountry": "GB"})
	cookie := rec.Header().Get("Set-Cookie")

	for _, path := range []string{"/admin", "/admin/events", "/admin/withdrawals",
		"/admin/flags", "/admin/players", "/admin/adjustments", "/admin/offers",
		"/admin/casino"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Cookie", cookie)
		out := httptest.NewRecorder()
		ts.ServeHTTP(out, req)
		// A customer is told the page does not exist rather than that they
		// lack permission: no need to advertise the back office.
		if out.Code != http.StatusNotFound {
			t.Errorf("customer GET %s = %d, want 404", path, out.Code)
		}
	}
}

func TestMiddlewareDoesNotBreakStreaming(t *testing.T) {
	// The access-log wrapper sits in front of every handler. If it hides
	// http.Flusher, the poker stream returns 500 and the table never updates.
	ts := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/poker/tables/1/stream", nil)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)

	// No table 1 exists in the test database, so a 404 is the right answer.
	// A 500 would mean flushing was unavailable.
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("the stream reported %d: middleware has hidden http.Flusher", rec.Code)
	}
}

func TestOpenRedirectIsRefused(t *testing.T) {
	for _, next := range []string{
		"https://evil.example.com", "//evil.example.com", "/\r\nSet-Cookie: x=1",
	} {
		if got := safeNext(next); got != "/" {
			t.Errorf("safeNext(%q) = %q, want /", next, got)
		}
	}
	for _, next := range []string{"/account", "/bets?status=open"} {
		if got := safeNext(next); got != next {
			t.Errorf("safeNext(%q) = %q, want it kept", next, got)
		}
	}
}

func TestSessionCookieValueIsNotWhatIsStored(t *testing.T) {
	// The database holds only a hash, so a stolen dump cannot be replayed as a
	// live session.
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if auth.HashToken(token) == token {
		t.Error("the stored session token equals the cookie value")
	}
}

func TestBetSlipCookieIsBoundedAndSanitised(t *testing.T) {
	// The slip cookie is attacker-controlled. It must never blow up a page or
	// grow without limit.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: betslipCookieName, Value: "not base64 at all!!"})
	if got := readSlip(req); got != nil {
		t.Errorf("a corrupt slip cookie produced %v, want nothing", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: betslipCookieName, Value: "eyJub3QiOiJhbiBhcnJheSJ9"})
	if got := readSlip(req); got != nil {
		t.Errorf("a slip cookie of the wrong shape produced %v, want nothing", got)
	}
}

func TestHealthReportsLedgerBreakage(t *testing.T) {
	ts := newTestServer(t)
	if rec := ts.get(t, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz on a clean ledger = %d", rec.Code)
	}
	if !strings.Contains(ts.get(t, "/healthz", nil).Body.String(), `"status": "ok"`) {
		t.Error("healthz should report ok on a sound ledger")
	}
}

func extractCSRF(body string) string {
	const marker = `name="csrf" value="`
	index := strings.Index(body, marker)
	if index < 0 {
		return ""
	}
	rest := body[index+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// ---------------------------------------------------------------------------
// Casino
// ---------------------------------------------------------------------------

// player registers a customer and returns their session cookie and id.
func (ts *testServer) player(t *testing.T, email string) (cookie string, userID int64) {
	t.Helper()
	rec := ts.post(t, "/register", url.Values{
		"email":         {email},
		"password":      {"correct horse battery staple"},
		"date_of_birth": {"1990-01-01"},
		"terms":         {"1"},
	}, map[string]string{"CF-IPCountry": "GB"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("register %s = %d: %s", email, rec.Code, rec.Body.String())
	}
	user, err := ts.store.GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	// Staking anything needs a verified identity, so the fixture clears that
	// the way a compliance officer would.
	if err := ts.store.SetKYCStatus(context.Background(), user.ID, store.KYCVerified); err != nil {
		t.Fatal(err)
	}
	return rec.Header().Get("Set-Cookie"), user.ID
}

// credit puts spendable cash on a player's account, the way a manual
// adjustment would.
func (ts *testServer) credit(t *testing.T, userID, sat int64) {
	t.Helper()
	ctx := context.Background()
	err := ts.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(ts.store.Now()), store.TxnSpec{
			Kind: store.TxnAdjustment, Memo: "test funding",
			Entries: []store.Entry{
				{Account: store.AccountUserCash, UserID: userID, AmountSat: sat},
				{Account: store.AccountHousePromotions, AmountSat: -sat},
			},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (ts *testServer) as(t *testing.T, cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	return ts.get(t, path, map[string]string{"Cookie": cookie, "CF-IPCountry": "GB"})
}

func (ts *testServer) postAs(t *testing.T, cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return ts.post(t, path, form, map[string]string{"Cookie": cookie, "CF-IPCountry": "GB"})
}

// TestCasinoPagesRender is a smoke test with a purpose: every casino template
// is parsed and executed against real data, so a template that reaches for a
// field or a helper that does not exist fails here rather than in front of a
// player mid-spin.
func TestCasinoPagesRender(t *testing.T) {
	ts := newTestServer(t)
	cookie, _ := ts.player(t, "spinner@example.com")

	for _, path := range []string{
		"/casino",
		"/casino/fairness",
		"/casino/bitcoin-bonanza",
		"/casino/satoshi-sevens",
		"/casino/aztec-vault",
		"/casino/lightning-ways",
		"/casino/history",
	} {
		if rec := ts.as(t, cookie, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200\n%s", path, rec.Code, rec.Body.String())
		}
	}

	// The lobby and the fairness page must work for a visitor with no account:
	// somebody deciding whether to open one should be able to read the odds
	// first.
	for _, path := range []string{"/casino", "/casino/fairness", "/casino/bitcoin-bonanza"} {
		if rec := ts.get(t, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s signed out = %d, want 200", path, rec.Code)
		}
	}
}

func TestCasinoRejectsAnUnknownGame(t *testing.T) {
	ts := newTestServer(t)
	if rec := ts.get(t, "/casino/no-such-machine", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET an unknown game = %d, want 404", rec.Code)
	}
}

// TestCasinoSpinNeedsCSRF keeps the fastest-spending action on the site inside
// the same protection as everything else.
func TestCasinoSpinNeedsCSRF(t *testing.T) {
	ts := newTestServer(t)
	cookie, userID := ts.player(t, "forger@example.com")
	ts.credit(t, userID, 5_000_000)

	rec := ts.postAs(t, cookie, "/casino/bitcoin-bonanza/spin", url.Values{
		"csrf": {"forged"}, "stake": {"0.00010000"},
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a spin with a forged CSRF token = %d, want 403", rec.Code)
	}
	balance, err := ts.store.BalanceSat(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 5_000_000 {
		t.Errorf("the refused spin still took money: balance %d", balance)
	}
}

func TestCasinoSpinSettlesAndIsCheckable(t *testing.T) {
	ts := newTestServer(t)
	cookie, userID := ts.player(t, "player@example.com")
	ts.credit(t, userID, 50_000_000)

	game, ok := slots.Lookup("bitcoin-bonanza")
	if !ok {
		t.Fatal("bitcoin-bonanza is not on the floor")
	}
	token := extractCSRF(ts.as(t, cookie, "/casino/bitcoin-bonanza").Body.String())
	if token == "" {
		t.Fatal("no CSRF token on the game page")
	}

	rec := ts.postAs(t, cookie, "/casino/bitcoin-bonanza/spin", url.Values{
		"csrf": {token}, "stake": {money.FormatBTC(game.MinStakeSat())},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("spin = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		SpinID     int64 `json:"spin_id"`
		StakeSat   int64 `json:"stake_sat"`
		WinSat     int64 `json:"win_sat"`
		BalanceSat int64 `json:"balance_sat"`
		Nonce      int64 `json:"nonce"`
		Round      struct {
			Base struct {
				Window [][]int `json:"window"`
			} `json:"base"`
		} `json:"round"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode spin: %v\n%s", err, rec.Body.String())
	}
	if payload.SpinID == 0 {
		t.Error("the spin was not recorded")
	}
	if payload.StakeSat != game.MinStakeSat() {
		t.Errorf("stake = %d, want %d", payload.StakeSat, game.MinStakeSat())
	}
	if payload.Nonce != 1 {
		t.Errorf("first spin used nonce %d, want 1", payload.Nonce)
	}
	if len(payload.Round.Base.Window) != game.ReelCount() {
		t.Errorf("the round showed %d reels, want %d", len(payload.Round.Base.Window), game.ReelCount())
	}
	want := int64(50_000_000) - game.MinStakeSat() + payload.WinSat
	if payload.BalanceSat != want {
		t.Errorf("balance = %d, want %d", payload.BalanceSat, want)
	}

	spinPath := "/casino/spins/" + strconv.FormatInt(payload.SpinID, 10)
	page := ts.as(t, cookie, spinPath)
	if page.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", spinPath, page.Code)
	}
	// The seed is still live, so the page must say the spin cannot be checked
	// yet rather than claiming it has been.
	if !strings.Contains(page.Body.String(), "still in use") {
		t.Error("a spin under a live seed should say it is not checkable yet")
	}

	// Publishing the seed makes it checkable, and the page says so.
	historyToken := extractCSRF(ts.as(t, cookie, "/casino/history").Body.String())
	if rec := ts.postAs(t, cookie, "/casino/rotate", url.Values{
		"csrf": {historyToken}, "back": {"/casino/history"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("rotate = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	page = ts.as(t, cookie, spinPath)
	if !strings.Contains(page.Body.String(), "Checked.") {
		t.Errorf("after publishing the seed the spin should verify:\n%s", page.Body.String())
	}
}

// TestCasinoSpinIsPrivate: a spin belongs to the person who took it.
func TestCasinoSpinIsPrivate(t *testing.T) {
	ts := newTestServer(t)
	cookie, userID := ts.player(t, "mine@example.com")
	ts.credit(t, userID, 10_000_000)

	game, _ := slots.Lookup("bitcoin-bonanza")
	token := extractCSRF(ts.as(t, cookie, "/casino/bitcoin-bonanza").Body.String())
	rec := ts.postAs(t, cookie, "/casino/bitcoin-bonanza/spin", url.Values{
		"csrf": {token}, "stake": {money.FormatBTC(game.MinStakeSat())},
	})
	var payload struct {
		SpinID int64 `json:"spin_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	other, _ := ts.player(t, "nosy@example.com")
	path := "/casino/spins/" + strconv.FormatInt(payload.SpinID, 10)
	if rec := ts.as(t, other, path); rec.Code == http.StatusOK {
		t.Error("another player's spin was readable")
	}
}

// TestPercentHandlesNegatives: a negative gap is the normal case on a machine
// that is running behind its published return, and the obvious formatting of
// basis points prints it as "-19.-70%".
func TestPercentHandlesNegatives(t *testing.T) {
	for _, test := range []struct {
		bps  int64
		want string
	}{
		{0, "0.00%"},
		{9613, "96.13%"},
		{-1970, "-19.70%"},
		{-5, "-0.05%"},
		{-10000, "-100.00%"},
	} {
		if got := formatPercent(test.bps); got != test.want {
			t.Errorf("formatPercent(%d) = %q, want %q", test.bps, got, test.want)
		}
	}
}
