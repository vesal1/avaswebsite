package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// The JSON API is read-only. Bets are struck through the form flow, which
// carries the CSRF token, the compliance checks and the price-change consent;
// exposing placement here would be a second, weaker path to the same money.

type apiSport struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Category     string   `json:"category"`
	Formats      []string `json:"formats"`
	HasDraw      bool     `json:"has_draw"`
	Markets      []string `json:"markets"`
	Competitions []string `json:"competitions"`
	OpenEvents   int      `json:"open_events"`
}

func (s *Server) handleAPISports(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.CountEventsBySport(r.Context())
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	sports := catalog.All()
	out := make([]apiSport, 0, len(sports))
	for _, sport := range sports {
		formats := make([]string, 0, len(sport.Formats))
		for _, format := range sport.Formats {
			formats = append(formats, string(format))
		}
		markets := make([]string, 0, len(sport.Markets))
		for _, kind := range sport.Markets {
			markets = append(markets, string(kind))
		}
		out = append(out, apiSport{
			Key: sport.Key, Name: sport.Name, Category: string(sport.Category),
			Formats: formats, HasDraw: sport.HasDraw, Markets: markets,
			Competitions: sport.Competitions, OpenEvents: counts[sport.Key],
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "sports": out})
}

type apiEvent struct {
	ID          int64       `json:"id"`
	Sport       string      `json:"sport"`
	Competition string      `json:"competition,omitempty"`
	Name        string      `json:"name"`
	Format      string      `json:"format"`
	StartsAt    time.Time   `json:"starts_at"`
	Status      string      `json:"status"`
	Venue       string      `json:"venue,omitempty"`
	Markets     []apiMarket `json:"markets,omitempty"`
}

type apiMarket struct {
	ID         int64          `json:"id"`
	Kind       string         `json:"kind"`
	Title      string         `json:"title"`
	Line       *string        `json:"line,omitempty"`
	Period     string         `json:"period,omitempty"`
	Status     string         `json:"status"`
	Selections []apiSelection `json:"selections"`
}

type apiSelection struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Outcome    string `json:"outcome_code,omitempty"`
	Decimal    string `json:"decimal"`
	American   int64  `json:"american"`
	Fractional string `json:"fractional"`
	// OddsMilli is the canonical price: decimal odds times 1000, as an
	// integer, so a client never has to parse a float to compare prices.
	OddsMilli int64  `json:"odds_milli"`
	Status    string `json:"status"`
}

func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	sport, ok := catalog.Lookup(r.PathValue("sport"))
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown sport"})
		return
	}
	events, err := s.store.ListEvents(r.Context(), store.EventFilter{
		SportKey: sport.Key,
		Statuses: []string{store.EventScheduled, store.EventLive},
		Limit:    200,
	})
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	out := make([]apiEvent, 0, len(events))
	for _, event := range events {
		out = append(out, toAPIEvent(event, nil))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"sport": sport.Key, "count": len(out), "events": out,
	})
}

func (s *Server) handleAPIEvent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown event"})
		return
	}
	event, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown event"})
		return
	}
	markets, err := s.store.MarketsForEvent(r.Context(), id, false)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, toAPIEvent(event, markets))
}

func toAPIEvent(event store.Event, markets []store.Market) apiEvent {
	out := apiEvent{
		ID: event.ID, Sport: event.SportKey, Competition: event.Competition,
		Name: event.Name, Format: event.Format, StartsAt: event.StartsAt,
		Status: event.Status, Venue: event.Venue,
	}
	for _, market := range markets {
		converted := apiMarket{
			ID: market.ID, Kind: market.Kind, Title: market.Title,
			Period: market.Period, Status: market.Status,
		}
		if market.HasLine {
			line := formatLine(market.LineX100, true)
			converted.Line = &line
		}
		for _, selection := range market.Selections {
			converted.Selections = append(converted.Selections, apiSelection{
				ID: selection.ID, Name: selection.Name, Outcome: selection.OutcomeCode,
				Decimal:    money.FormatDecimalOdds(selection.OddsMilli),
				American:   money.MilliToAmerican(selection.OddsMilli),
				Fractional: money.MilliToFractional(selection.OddsMilli),
				OddsMilli:  selection.OddsMilli,
				Status:     selection.Status,
			})
		}
		out.Markets = append(out.Markets, converted)
	}
	return out
}
