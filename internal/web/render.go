package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/slots"
	"github.com/vesal1/avaswebsite/internal/store"
)

// pageData is the envelope every template receives. Handlers add their own
// keys to the Data map.
type pageData struct {
	Title     string
	Brand     string
	Licence   string
	Support   string
	User      *store.User
	CSRF      string
	Country   string
	Nav       []catalog.CategoryGroup
	SlipCount int
	// OnSlip marks which selections are already on the bet slip, so a price
	// button can render as selected. Computed here rather than per handler:
	// every page that shows a price needs it, and one that forgot would
	// silently render every button as unselected.
	OnSlip     map[int64]bool
	BalanceSat int64
	OddsFormat money.OddsFormat
	Flash      string
	Error      string
	IsProd     bool
	MockWallet bool
	Data       map[string]any
}

// render writes a template with the standard envelope.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	page := pageData{
		Brand:      s.cfg.BrandName,
		Licence:    s.cfg.LicenceText,
		Support:    s.cfg.SupportMail,
		CSRF:       s.csrfToken(r.Context()),
		Country:    Country(r.Context()),
		Nav:        catalog.Grouped(),
		OddsFormat: money.FormatDecimal,
		IsProd:     s.cfg.IsProduction(),
		MockWallet: s.wallet != nil && s.wallet.Provider().Name() == "mock",
		Data:       data,
	}
	if data != nil {
		if title, ok := data["Title"].(string); ok {
			page.Title = title
		}
		if flash, ok := data["Flash"].(string); ok {
			page.Flash = flash
		}
		if message, ok := data["Error"].(string); ok {
			page.Error = message
		}
	}
	if user, ok := CurrentUser(r.Context()); ok {
		page.User = &user
		page.OddsFormat = money.OddsFormat(user.OddsFormat)
		if balance, err := s.store.BalanceSat(r.Context(), user.ID); err == nil {
			page.BalanceSat = balance
		}
	}
	slip := readSlip(r)
	page.SlipCount = len(slip)
	page.OnSlip = make(map[int64]bool, len(slip))
	for _, entry := range slip {
		page.OnSlip[entry.SelectionID] = true
	}

	set, ok := s.templates[name]
	if !ok {
		s.log.Error("unknown template", "template", name)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	// Render to a buffer first: a template that fails halfway must not leave a
	// half-written page and a 200 behind it.
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "layout", page); err != nil {
		s.log.Error("render template", "template", name, "error", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// renderError shows a plain error page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "error.html", map[string]any{
		"Title":   http.StatusText(status),
		"Status":  status,
		"Message": message,
	})
}

// writeJSON writes an API response.
func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		s.log.Error("write json", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"btc":      money.FormatBTC,
		"sats":     money.FormatSat,
		"odds":     money.FormatDecimalOdds,
		"oddsAs":   func(milli int64, style money.OddsFormat) string { return money.Format(milli, style) },
		"american": money.MilliToAmerican,
		"fiat": func(sat, rate int64, currency string) string {
			return money.FormatFiat(money.ToFiatCents(sat, rate), currency)
		},
		"line":      formatLine,
		"kickoff":   formatKickoff,
		"datetime":  func(t time.Time) string { return t.UTC().Format("2 Jan 2006 15:04") + " UTC" },
		"date":      func(t time.Time) string { return t.UTC().Format("2 Jan 2006") },
		"since":     humaniseSince,
		"title":     titleCase,
		"statusTag": statusTag,
		"sportName": sportName,
		"marketDoc": marketDoc,
		"add":       func(a, b int) int { return a + b },
		"mulInt":    func(a, b int) int { return a * b },
		"sub":       func(a, b int64) int64 { return a - b },
		"div": func(a, b int64) int64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"pct":      formatPercent,
		"hasValue": func(v any) bool { return v != nil },
		"dict":     dict,
		"seq":      seq,
		"mul":      func(a, b float64) float64 { return a * b },
		// oneIn turns a probability into the "one spin in N" phrasing a
		// paytable uses. A probability of zero would divide by zero, so it
		// comes back as a number large enough to read as "never".
		"oneIn": func(p float64) float64 {
			if p <= 0 {
				return math.Inf(1)
			}
			return 1 / p
		},
		"payX100":     formatMultiplier,
		"payX10000":   formatMultiplierX10000,
		"symbolGlyph": symbolGlyph,
		"mathsFor": func(key string) slots.Maths {
			maths, _ := slots.MathsFor(key)
			return maths
		},
		"list": func(items ...string) []string { return items },
		"impliedBps": func(milli int64) int64 {
			bps, err := money.ImpliedProbabilityBps(milli)
			if err != nil {
				return 0
			}
			return bps
		},
		// bookBps is the market's total implied probability. Above 10000 is the
		// operator's margin; at or below it the book is priced to lose money.
		"bookBps": func(selections []store.Selection) int64 {
			prices := make([]int64, 0, len(selections))
			for _, selection := range selections {
				prices = append(prices, selection.OddsMilli)
			}
			total, err := money.BookPercentageBps(prices)
			if err != nil {
				return 0
			}
			return total
		},
	}
}

// formatLine renders a stored handicap or total, which is scaled by 100.
func formatLine(lineX100 int64, hasLine bool) string {
	if !hasLine {
		return ""
	}
	sign := ""
	value := lineX100
	if value < 0 {
		sign, value = "-", -value
	}
	whole, frac := value/100, value%100
	switch {
	case frac == 0:
		return fmt.Sprintf("%s%d.0", sign, whole)
	case frac%10 == 0:
		return fmt.Sprintf("%s%d.%d", sign, whole, frac/10)
	default:
		return fmt.Sprintf("%s%d.%02d", sign, whole, frac)
	}
}

func formatKickoff(t time.Time) string {
	return t.UTC().Format("Mon 2 Jan, 15:04") + " UTC"
}

func humaniseSince(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	elapsed := time.Since(t)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%d min ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%d hr ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(elapsed.Hours()/24))
	}
}

func titleCase(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// statusTag maps a status onto a CSS modifier, so the stylesheet decides the
// colour rather than each template.
//
// Statuses are matched on their string values, which several of the store's
// constants deliberately share: "rejected" means the same kind of thing to a
// deposit and to a withdrawal, and should read the same way on screen.
func statusTag(status string) string {
	switch status {
	case "won", "open", "live", "active", "verified", "confirmed", "credited", "accepted":
		return "good"
	case "lost", "suspended_account", "closed", "rejected", "failed":
		return "bad"
	case "void", "half_won", "half_lost", "suspended", "postponed", "review",
		"frozen", "pending", "cancelled", "orphaned":
		return "warn"
	default:
		return "neutral"
	}
}

func sportName(key string) string {
	if sport, ok := catalog.Lookup(key); ok {
		return sport.Name
	}
	return key
}

// marketDoc returns the plain-English rule for a market kind, shown next to
// the market so a customer knows what they are backing before they stake.
func marketDoc(kind string) string {
	def, ok := catalog.Market(catalog.MarketKind(kind))
	if !ok {
		return ""
	}
	return def.Description
}

// dict builds a map inside a template, for passing several values to a
// sub-template.
func dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, fmt.Errorf("dict needs an even number of arguments")
	}
	out := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict keys must be strings")
		}
		out[key] = values[i+1]
	}
	return out, nil
}

// contextLike is the subset of context.Context the store needs, kept as an
// alias so handlers can pass a request context straight through.
type contextLike = interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}

// seq counts 0..n-1, so a template can lay out a grid of reels and rows
// without the handler having to build a slice for it.
func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// formatMultiplier renders a paytable value, which is stored in hundredths of
// the stake. Whole multiples read as "12x" rather than "12.00x".
func formatMultiplier(x100 int64) string {
	if x100%100 == 0 {
		return fmt.Sprintf("%dx", x100/100)
	}
	if x100%10 == 0 {
		return fmt.Sprintf("%d.%dx", x100/100, (x100%100)/10)
	}
	return fmt.Sprintf("%d.%02dx", x100/100, x100%100)
}

// symbolGlyph looks up what to draw for a cell of a spin window.
func symbolGlyph(game *slots.Game, id slots.SymbolID) string {
	if game == nil || int(id) < 0 || int(id) >= len(game.Symbols) {
		return ""
	}
	return game.Symbols[id].Glyph
}

// formatPercent renders basis points. Negatives are handled explicitly: the
// obvious "%d.%02d" of a negative value prints the sign twice and the
// fractional part backwards, which turned a -19.70% gap into "-19.-70%".
func formatPercent(bps int64) string {
	sign := ""
	if bps < 0 {
		sign, bps = "-", -bps
	}
	return fmt.Sprintf("%s%d.%02d%%", sign, bps/100, bps%100)
}

// formatMultiplierX10000 renders a Mines-style multiplier stored in
// ten-thousandths: "2.06x", "2231.30x".
func formatMultiplierX10000(x10000 int64) string {
	whole := x10000 / 10_000
	frac := x10000 % 10_000
	if frac == 0 {
		return fmt.Sprintf("%dx", whole)
	}
	// Two decimals reads best on a ladder; the JSON keeps full precision.
	return fmt.Sprintf("%d.%02dx", whole, frac/100)
}
