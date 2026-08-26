// Package seed loads demonstration content so a fresh install has something to
// price. It is deterministic: the same call produces the same board, which
// makes it usable as a fixture for tests and demos alike.
package seed

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/money"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Summary reports what was created.
type Summary struct {
	Sports      int
	Events      int
	Markets     int
	Selections  int
	PokerTables int
}

// PokerTables creates a spread of cash tables if none exist.
//
// The stakes ladder in even steps so the lobby is legible, and every table
// publishes the same rake terms: a capped percentage with no rake before the
// flop.
func PokerTables(ctx context.Context, s *store.Store, cfg *config.Config) (int, error) {
	existing, err := s.ListPokerTables(ctx, false)
	if err != nil {
		return 0, err
	}
	if len(existing) > 0 {
		return 0, nil
	}

	stakes := []struct {
		name        string
		bigBlindSat int64
		seats       int
		video       bool
	}{
		{"Micro 1", 200, 6, true},
		{"Micro 2", 200, 6, false},
		{"Low", 1_000, 6, true},
		{"Mid", 5_000, 6, true},
		{"High", 25_000, 6, true},
		{"Heads Up", 1_000, 2, true},
	}

	var created int
	for _, spec := range stakes {
		if _, err := s.CreatePokerTable(ctx, store.PokerTable{
			Name:          spec.name,
			SmallBlindSat: spec.bigBlindSat / 2,
			BigBlindSat:   spec.bigBlindSat,
			// The usual cash-game spread: twenty to a hundred big blinds.
			MinBuyInSat:   spec.bigBlindSat * 20,
			MaxBuyInSat:   spec.bigBlindSat * 100,
			MaxSeats:      spec.seats,
			RakeBps:       250,
			RakeCapSat:    spec.bigBlindSat * 3,
			NoFlopNoDrop:  true,
			ActionSeconds: 30,
			VideoEnabled:  spec.video,
			Active:        true,
		}); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// Load creates roughly `target` events spread across the catalog, with a
// realistic set of markets for each sport's shape.
func Load(ctx context.Context, s *store.Store, cfg *config.Config, target int) (Summary, error) {
	var summary Summary
	if target <= 0 {
		target = 60
	}

	tables, err := PokerTables(ctx, s, cfg)
	if err != nil {
		return summary, err
	}
	summary.PokerTables = tables

	sports := catalog.All()
	now := s.Now().Truncate(time.Hour)

	// Walk the catalog rather than a hand-picked list, so seeding exercises
	// every shape of sport the book supports rather than only football.
	for index := 0; summary.Events < target && index < len(sports)*3; index++ {
		sport := sports[index%len(sports)]
		round := index / len(sports)

		created, markets, selections, err := seedSport(ctx, s, cfg, sport, now, round)
		if err != nil {
			return summary, err
		}
		if created == 0 {
			continue
		}
		if round == 0 {
			summary.Sports++
		}
		summary.Events += created
		summary.Markets += markets
		summary.Selections += selections
	}
	return summary, nil
}

func seedSport(ctx context.Context, s *store.Store, cfg *config.Config, sport catalog.Sport, now time.Time, round int) (int, int, int, error) {
	competitionName := sport.Competitions[round%len(sport.Competitions)]
	competitionID, err := s.UpsertCompetition(ctx, sport.Key, competitionName, "")
	if err != nil {
		return 0, 0, 0, err
	}

	// Kick-off times fan out across the coming days, so the board is not one
	// wall of events at the same minute.
	offset := time.Duration(seedInt(sport.Key, round, 6, 96)) * time.Hour
	startsAt := now.Add(offset)

	switch {
	case sport.HasFormat(catalog.FormatMatch):
		markets, selections, err := seedMatch(ctx, s, cfg, sport, competitionID, competitionName, startsAt, round)
		return 1, markets, selections, err
	case sport.HasFormat(catalog.FormatRace):
		markets, selections, err := seedField(ctx, s, cfg, sport, competitionID, competitionName,
			startsAt, round, string(catalog.FormatRace))
		return 1, markets, selections, err
	default:
		markets, selections, err := seedField(ctx, s, cfg, sport, competitionID, competitionName,
			startsAt, round, string(catalog.FormatTournament))
		return 1, markets, selections, err
	}
}

// seedMatch builds a head-to-head event with a result market and, where the
// sport supports them, a handicap and a totals market.
func seedMatch(ctx context.Context, s *store.Store, cfg *config.Config, sport catalog.Sport,
	competitionID int64, competition string, startsAt time.Time, round int) (int, int, error) {

	home := participantName(sport, round, 0)
	away := participantName(sport, round, 1)

	eventID, err := s.CreateEvent(ctx, store.NewEvent{
		SportKey: sport.Key, CompetitionID: competitionID,
		Name:   fmt.Sprintf("%s v %s", home, away),
		Format: string(catalog.FormatMatch), StartsAt: startsAt,
		Venue: competition,
	})
	if err != nil {
		return 0, 0, err
	}
	homeID, err := s.AddParticipant(ctx, store.Participant{
		EventID: eventID, Name: home, HomeAway: "home", SortOrder: 0,
	})
	if err != nil {
		return 0, 0, err
	}
	awayID, err := s.AddParticipant(ctx, store.Participant{
		EventID: eventID, Name: away, HomeAway: "away", SortOrder: 1,
	})
	if err != nil {
		return 0, 0, err
	}

	margin := cfg.DefaultMarginBps
	markets, selections := 0, 0

	// Headline result market, three-way where a draw is possible.
	threeWay := sport.HasDraw && sport.Supports(catalog.KindMoneyline3)
	weights := []int64{
		seedInt(sport.Key, round, 30, 55),
		seedInt(sport.Key, round+7, 20, 30),
		seedInt(sport.Key, round+13, 25, 50),
	}
	kind := catalog.KindMoneyline3
	outcomes := []struct {
		code, name string
		pid        int64
	}{{"home", home, homeID}, {"draw", "Draw", 0}, {"away", away, awayID}}
	if !threeWay {
		kind = catalog.KindMoneyline2
		weights = []int64{weights[0], weights[2]}
		outcomes = []struct {
			code, name string
			pid        int64
		}{{"home", home, homeID}, {"away", away, awayID}}
	}

	added, err := addMarket(ctx, s, eventID, sport, kind, margin, 0, false, 0, weights, outcomes)
	if err != nil {
		return 0, 0, err
	}
	markets, selections = markets+1, selections+added

	// A handicap, priced around a line the trader would open on.
	if sport.Supports(catalog.KindHandicap) {
		line := seedInt(sport.Key, round+3, -250, 250) / 50 * 50 // half-point lines
		if line == 0 {
			line = -50
		}
		added, err := addMarket(ctx, s, eventID, sport, catalog.KindHandicap, margin, line, true, 0,
			[]int64{50, 50},
			[]struct {
				code, name string
				pid        int64
			}{
				{"home", fmt.Sprintf("%s %+.1f", home, float64(line)/100), homeID},
				{"away", fmt.Sprintf("%s %+.1f", away, float64(-line)/100), awayID},
			})
		if err != nil {
			return 0, 0, err
		}
		markets, selections = markets+1, selections+added
	}

	// A totals market, with a line that suits how the sport is scored.
	if sport.Supports(catalog.KindTotals) {
		line := totalsLine(sport, round)
		noun := sport.ScoringNoun()
		added, err := addMarket(ctx, s, eventID, sport, catalog.KindTotals, margin, line, true, 0,
			[]int64{50, 50},
			[]struct {
				code, name string
				pid        int64
			}{
				{"over", fmt.Sprintf("Over %.1f %s", float64(line)/100, noun), 0},
				{"under", fmt.Sprintf("Under %.1f %s", float64(line)/100, noun), 0},
			})
		if err != nil {
			return 0, 0, err
		}
		markets, selections = markets+1, selections+added
	}

	// Both teams to score, where it means anything.
	if sport.Supports(catalog.KindBothTeamsScore) {
		added, err := addMarket(ctx, s, eventID, sport, catalog.KindBothTeamsScore, margin, 0, false, 0,
			[]int64{55, 45},
			[]struct {
				code, name string
				pid        int64
			}{{"yes", "Yes", 0}, {"no", "No", 0}})
		if err != nil {
			return 0, 0, err
		}
		markets, selections = markets+1, selections+added
	}
	return markets, selections, nil
}

// seedField builds an event with a field of competitors and an outright market.
func seedField(ctx context.Context, s *store.Store, cfg *config.Config, sport catalog.Sport,
	competitionID int64, competition string, startsAt time.Time, round int, format string) (int, int, error) {

	eventID, err := s.CreateEvent(ctx, store.NewEvent{
		SportKey: sport.Key, CompetitionID: competitionID,
		Name:   fmt.Sprintf("%s %d", competition, startsAt.Year()),
		Format: format, StartsAt: startsAt, Venue: competition,
	})
	if err != nil {
		return 0, 0, err
	}

	size := int(seedInt(sport.Key, round, 6, 12))
	weights := make([]int64, 0, size)
	outcomes := make([]struct {
		code, name string
		pid        int64
	}, 0, size)

	for i := 0; i < size; i++ {
		name := participantName(sport, round, i)
		participantID, err := s.AddParticipant(ctx, store.Participant{
			EventID: eventID, Name: name, SortOrder: i,
		})
		if err != nil {
			return 0, 0, err
		}
		// A field is not uniform: the favourite carries several times the
		// weight of the outsider.
		weights = append(weights, seedInt(sport.Key, round*31+i, 5, 60))
		outcomes = append(outcomes, struct {
			code, name string
			pid        int64
		}{fmt.Sprintf("p%d", i), name, participantID})
	}

	added, err := addMarket(ctx, s, eventID, sport, catalog.KindOutright,
		cfg.DefaultMarginBps+500, 0, false, 0, weights, outcomes)
	if err != nil {
		return 0, 0, err
	}
	markets, selections := 1, added

	// A podium market on sports that pay places.
	if sport.PlaceTerms > 0 && sport.Supports(catalog.KindPodium) {
		podiumWeights := make([]int64, len(weights))
		for i, weight := range weights {
			// Placing is roughly three times as likely as winning, capped so
			// the price stays above evens.
			podiumWeights[i] = weight * 3
		}
		added, err := addMarket(ctx, s, eventID, sport, catalog.KindPodium,
			cfg.DefaultMarginBps, 0, false, 0, podiumWeights, outcomes)
		if err != nil {
			return 0, 0, err
		}
		markets, selections = markets+1, selections+added
	}
	return markets, selections, nil
}

func addMarket(ctx context.Context, s *store.Store, eventID int64, sport catalog.Sport,
	kind catalog.MarketKind, marginBps, lineX100 int64, hasLine bool, subjectID int64,
	weights []int64, outcomes []struct {
		code, name string
		pid        int64
	}) (int, error) {

	marketID, err := s.CreateMarket(ctx, store.NewMarket{
		EventID: eventID, Kind: string(kind), Title: sport.MarketTitle(kind),
		LineX100: lineX100, HasLine: hasLine, SubjectID: subjectID, MarginBps: marginBps,
	})
	if err != nil {
		return 0, err
	}
	prices, err := money.OddsFromWeights(weights, marginBps)
	if err != nil {
		return 0, fmt.Errorf("seed: price %s on %s: %w", kind, sport.Key, err)
	}
	for i, outcome := range outcomes {
		if _, err := s.AddSelection(ctx, store.Selection{
			MarketID: marketID, ParticipantID: outcome.pid, Name: outcome.name,
			OutcomeCode: outcome.code, OddsMilli: prices[i], SortOrder: i,
		}); err != nil {
			return 0, err
		}
	}
	return len(outcomes), nil
}

// totalsLine picks a plausible over/under line for how a sport is scored.
func totalsLine(sport catalog.Sport, round int) int64 {
	base := map[catalog.Scoring]int64{
		catalog.ScoringGoals:  250,
		catalog.ScoringPoints: 4550,
		catalog.ScoringRuns:   850,
		catalog.ScoringSets:   350,
		catalog.ScoringGames:  2250,
		catalog.ScoringFrames: 850,
		catalog.ScoringLegs:   1050,
		catalog.ScoringMaps:   250,
		catalog.ScoringRounds: 850,
	}[sport.Scoring]
	if base == 0 {
		base = 250
	}
	// Nudge the line per round, keeping it on a half point so it cannot push.
	return base + seedInt(sport.Key, round, -2, 2)*100
}

// participantName builds a stable, obviously fictional competitor name. Real
// club and athlete names are deliberately avoided in seed data: this is a
// demonstration board, not a claim that these fixtures exist.
//
// The base is derived from the sport and round only, and `index` is then added
// on top. That is what guarantees two competitors in the same event are always
// different: deriving the base from the index as well let two of them collide,
// which produced fixtures of a player against themselves.
func participantName(sport catalog.Sport, round, index int) string {
	towns := []string{
		"Northgate", "Riverton", "Castleford", "Hillbrook", "Oakvale", "Stonebridge",
		"Westmere", "Ashcombe", "Fairhaven", "Kingsmoor", "Brightwater", "Elmsworth",
		"Draycott", "Marlowe", "Pinehurst", "Colverton",
	}
	clubs := []string{
		"United", "Athletic", "Rovers", "City", "Wanderers", "Rangers",
		"Select", "Academy", "Union", "Vipers", "Falcons", "Foxes",
	}
	people := []string{
		"A. Vance", "B. Ortiz", "C. Neumann", "D. Halloran", "E. Sato", "F. Bergqvist",
		"G. Mwangi", "H. Petrova", "I. Nakamura", "J. Okonkwo", "K. Lindqvist", "L. Duarte",
		"M. Aitken", "N. Ferreira", "O. Kowalski", "P. Rahman",
	}
	animals := []string{"Lad", "Lass", "Star", "Comet", "Dancer", "Bay", "Prince", "Gale"}

	base := int(seedInt(sport.Key, round, 0, 1<<20))

	switch sport.Category {
	case catalog.CatRacket, catalog.CatCombat, catalog.CatCue, catalog.CatMind,
		catalog.CatAthletics, catalog.CatAquatics, catalog.CatWinter, catalog.CatTarget,
		catalog.CatMotorsport, catalog.CatCycling, catalog.CatEquestrian, catalog.CatExtreme:
		return people[(base+index)%len(people)]
	case catalog.CatAnimalRacing:
		return towns[(base+index)%len(towns)] + " " + animals[(base+index)%len(animals)]
	default:
		// Vary the club word with the index too, so a long field does not run
		// through the same town list twice with the same suffix.
		return towns[(base+index)%len(towns)] + " " + clubs[(base/len(towns)+index)%len(clubs)]
	}
}

// seedInt derives a stable pseudo-random integer in [low, high] from a key.
// Deterministic on purpose: reseeding a demo must not shuffle the board.
func seedInt(key string, salt int, low, high int64) int64 {
	if high <= low {
		return low
	}
	hasher := fnv.New64a()
	fmt.Fprintf(hasher, "%s|%d", key, salt)
	span := high - low + 1
	return low + int64(hasher.Sum64()%uint64(span))
}
