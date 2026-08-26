package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/store"
)

// eventCard is an event with enough of its headline market to render a row on
// a listing page.
type eventCard struct {
	Event       store.Event
	Sport       catalog.Sport
	Headline    store.Market
	HasMarkets  bool
	MarketCount int
	// Open is whether new bets may still be struck, resolved once here rather
	// than recomputed against the clock inside each template.
	Open bool
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	upcoming, err := s.store.ListEvents(ctx, store.EventFilter{
		Statuses: []string{store.EventScheduled, store.EventLive},
		From:     s.now().Add(-6 * time.Hour),
		Limit:    24,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	cards, err := s.buildCards(ctx, upcoming)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	counts, err := s.store.CountEventsBySport(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "home.html", map[string]any{
		"Title":       s.cfg.BrandName,
		"Cards":       cards,
		"SportCounts": counts,
		"SportTotal":  catalog.Count(),
		"Live":        filterLive(cards),
	})
}

func filterLive(cards []eventCard) []eventCard {
	var live []eventCard
	for _, card := range cards {
		if card.Event.Status == store.EventLive {
			live = append(live, card)
		}
	}
	return live
}

func (s *Server) handleSportsIndex(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	groups := catalog.Grouped()
	var results []catalog.Sport
	if query != "" {
		results = catalog.Search(query)
	}
	counts, err := s.store.CountEventsBySport(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "sports.html", map[string]any{
		"Title":   "All sports",
		"Groups":  groups,
		"Query":   query,
		"Results": results,
		"Counts":  counts,
		"Total":   catalog.Count(),
	})
}

func (s *Server) handleSport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sport, ok := catalog.Lookup(r.PathValue("sport"))
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "We do not have a sport by that name.")
		return
	}

	filter := store.EventFilter{
		SportKey: sport.Key,
		Statuses: []string{store.EventScheduled, store.EventLive},
		Limit:    100,
	}
	if raw := r.URL.Query().Get("competition"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			filter.CompetitionID = id
		}
	}
	events, err := s.store.ListEvents(ctx, filter)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	cards, err := s.buildCards(ctx, events)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	competitions, err := s.store.CompetitionsForSport(ctx, sport.Key)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	markets := make([]catalog.MarketDef, 0, len(sport.Markets))
	for _, kind := range sport.Markets {
		if def, ok := catalog.Market(kind); ok {
			def.Name = sport.MarketTitle(kind)
			markets = append(markets, def)
		}
	}

	s.render(w, r, http.StatusOK, "sport.html", map[string]any{
		"Title":        sport.Name,
		"Sport":        sport,
		"Cards":        cards,
		"Competitions": competitions,
		"Markets":      markets,
		"FilterID":     filter.CompetitionID,
	})
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such event.")
		return
	}
	event, err := s.store.GetEvent(ctx, id)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "No such event.")
		return
	}
	sport, _ := catalog.Lookup(event.SportKey)
	participants, err := s.store.ParticipantsForEvent(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	markets, err := s.store.MarketsForEvent(ctx, id, false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "event.html", map[string]any{
		"Title":        event.Name,
		"Event":        event,
		"Sport":        sport,
		"Participants": participants,
		"Markets":      markets,
		"Open":         event.IsOpenForBetting(s.now()),
	})
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "rules.html", map[string]any{
		"Title":   "Betting rules",
		"Markets": catalog.AllMarkets(),
		"Config":  s.cfg,
	})
}

func (s *Server) handleResponsibleGambling(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "responsible.html", map[string]any{
		"Title":  "Responsible gambling",
		"Config": s.cfg,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{"status": "ok", "time": s.now().UTC()}
	code := http.StatusOK

	if err := s.store.DB().PingContext(r.Context()); err != nil {
		status["status"], status["database"] = "degraded", err.Error()
		code = http.StatusServiceUnavailable
	}
	if err := s.wallet.Provider().Health(r.Context()); err != nil {
		status["status"], status["bitcoin"] = "degraded", err.Error()
		code = http.StatusServiceUnavailable
	}
	// A broken ledger invariant is an emergency, and the health check is where
	// the on-call rotation will see it first.
	if problems, err := s.store.CheckLedger(r.Context()); err != nil {
		status["ledger"] = err.Error()
		code = http.StatusServiceUnavailable
	} else if len(problems) > 0 {
		messages := make([]string, 0, len(problems))
		for _, problem := range problems {
			messages = append(messages, problem.String())
		}
		status["status"], status["ledger"] = "broken", messages
		code = http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, status)
}

// buildCards attaches each event's headline market for a listing row.
func (s *Server) buildCards(ctx contextLike, events []store.Event) ([]eventCard, error) {
	cards := make([]eventCard, 0, len(events))
	for _, event := range events {
		sport, _ := catalog.Lookup(event.SportKey)
		markets, err := s.store.MarketsForEvent(ctx, event.ID, true)
		if err != nil {
			return nil, err
		}
		card := eventCard{
			Event: event, Sport: sport, MarketCount: len(markets),
			Open: event.IsOpenForBetting(s.now()),
		}
		if headline := pickHeadline(markets); headline != nil {
			card.Headline, card.HasMarkets = *headline, true
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// pickHeadline chooses the market to show on a listing row: the main result
// market where there is one, otherwise the first open market.
func pickHeadline(markets []store.Market) *store.Market {
	preferred := []catalog.MarketKind{
		catalog.KindMoneyline3, catalog.KindMoneyline2, catalog.KindOutright,
		catalog.KindHandicap, catalog.KindTotals,
	}
	for _, want := range preferred {
		for i := range markets {
			if markets[i].Kind == string(want) && len(markets[i].Selections) > 0 {
				// An outright field can be very long; a listing row shows the
				// shortest few prices rather than the whole book.
				if want == catalog.KindOutright && len(markets[i].Selections) > 4 {
					trimmed := markets[i]
					trimmed.Selections = shortestPrices(trimmed.Selections, 4)
					return &trimmed
				}
				return &markets[i]
			}
		}
	}
	for i := range markets {
		if len(markets[i].Selections) > 0 {
			return &markets[i]
		}
	}
	return nil
}

func shortestPrices(selections []store.Selection, n int) []store.Selection {
	sorted := make([]store.Selection, len(selections))
	copy(sorted, selections)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].OddsMilli < sorted[j-1].OddsMilli; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("handler", "path", r.URL.Path, "error", err)
	s.renderError(w, r, http.StatusInternalServerError,
		"Something went wrong at our end. Nothing has been charged.")
}
