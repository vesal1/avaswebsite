package catalog

import (
	"strings"
	"testing"
)

func TestEverySportIsWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for _, sport := range All() {
		if sport.Key == "" || sport.Name == "" {
			t.Errorf("sport %+v is missing a key or name", sport)
		}
		if seen[sport.Key] {
			t.Errorf("duplicate sport key %q", sport.Key)
		}
		seen[sport.Key] = true
		if strings.ToLower(sport.Key) != sport.Key || strings.Contains(sport.Key, " ") {
			t.Errorf("sport key %q must be a lower-case slug", sport.Key)
		}
		if len(sport.Formats) == 0 {
			t.Errorf("%s declares no contest format", sport.Key)
		}
		if len(sport.Markets) == 0 {
			t.Errorf("%s offers no markets", sport.Key)
		}
		if len(sport.Competitions) == 0 {
			t.Errorf("%s lists no competitions", sport.Key)
		}
		for _, kind := range sport.Markets {
			if _, ok := Market(kind); !ok {
				t.Errorf("%s references undefined market kind %q", sport.Key, kind)
			}
		}
	}
}

func TestHeadlineMarketMatchesDrawCapability(t *testing.T) {
	// A three-way headline market on a sport that cannot draw would strand a
	// selection that can never win, and a two-way market on a sport that can
	// draw has no way to settle a level result. Either is a mis-settlement.
	for _, sport := range All() {
		if !sport.HasFormat(FormatMatch) {
			continue
		}
		hasTwo := sport.Supports(KindMoneyline2)
		hasThree := sport.Supports(KindMoneyline3)
		if !hasTwo && !hasThree {
			t.Errorf("%s is a match sport with no headline winner market", sport.Key)
		}
		if sport.HasDraw && hasThree && !sport.Supports(KindDrawNoBet) && !sport.Supports(KindMethodOfVictory) {
			t.Errorf("%s prices a draw but offers no draw-no-bet alternative", sport.Key)
		}
	}
}

func TestFieldSportsCarryOutrights(t *testing.T) {
	for _, sport := range All() {
		if sport.HasFormat(FormatRace) && !sport.Supports(KindOutright) {
			t.Errorf("%s runs races but cannot price an outright winner", sport.Key)
		}
		if sport.PlaceTerms > 0 && !sport.Supports(KindPlace) && !sport.Supports(KindPodium) {
			t.Errorf("%s pays %d places but offers no place market", sport.Key, sport.PlaceTerms)
		}
	}
}

func TestGroupedCoversEverySport(t *testing.T) {
	var total int
	for _, group := range Grouped() {
		if len(group.Sports) == 0 {
			t.Errorf("category %q is empty but present in the menu", group.Category)
		}
		total += len(group.Sports)
	}
	if total != Count() {
		t.Errorf("Grouped covers %d sports, catalog has %d: a category is missing from categoryOrder", total, Count())
	}
}

func TestMarketTitleUsesSportScoringUnit(t *testing.T) {
	football, _ := Lookup("football")
	if got := football.MarketTitle(KindTotals); got != "Total Goals" {
		t.Errorf("football totals title = %q, want %q", got, "Total Goals")
	}
	basketball, _ := Lookup("basketball")
	if got := basketball.MarketTitle(KindTotals); got != "Total Points" {
		t.Errorf("basketball totals title = %q, want %q", got, "Total Points")
	}
	cricket, _ := Lookup("cricket")
	if got := cricket.MarketTitle(KindTotals); got != "Total Runs" {
		t.Errorf("cricket totals title = %q, want %q", got, "Total Runs")
	}
}

func TestSearch(t *testing.T) {
	if len(Search("Premier League")) == 0 {
		t.Error("searching a competition name should find its sport")
	}
	if got := Search("tennis"); len(got) < 2 {
		t.Errorf("searching 'tennis' should match tennis and table tennis, got %d", len(got))
	}
	if len(Search("")) != 0 {
		t.Error("an empty query should match nothing")
	}
}

func TestCatalogIsBroad(t *testing.T) {
	// The product promise is "every sport". Guard against the catalog being
	// quietly gutted: this is a floor, not a target.
	if Count() < 120 {
		t.Errorf("catalog has only %d sports", Count())
	}
	for _, category := range Categories() {
		if len(InCategory(category)) == 0 {
			t.Errorf("category %q has no sports", category)
		}
	}
}
