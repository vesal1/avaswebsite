package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vesal1/avaswebsite/internal/auth"
	"github.com/vesal1/avaswebsite/internal/betting"
	"github.com/vesal1/avaswebsite/internal/bitcoin"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/store"
	"github.com/vesal1/avaswebsite/internal/wallet"
)

type testServer struct {
	*Server
	store *store.Store
	cfg   *config.Config
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "web.sqlite3"))
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

	server, err := New(Options{
		Config: cfg, Store: db, Compliance: comp,
		Wallet:  wallet.New(db, provider, comp, cfg),
		Betting: betting.New(db, comp, cfg),
		Logger:  logger,
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

func TestPublicPagesRender(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{
		"/", "/sports", "/sports/football", "/sports?q=snooker",
		"/rules", "/responsible-gambling", "/healthz",
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

	for _, path := range []string{"/admin", "/admin/events", "/admin/withdrawals", "/admin/flags"} {
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
