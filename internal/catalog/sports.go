package catalog

import (
	"sort"
	"strings"
)

// Category groups sports for navigation. The order of the constants is the
// order the categories appear in the site's sport menu.
type Category string

const (
	CatFootballCodes Category = "Football Codes"
	CatTeamSports    Category = "Team Sports"
	CatRacket        Category = "Racket Sports"
	CatCombat        Category = "Combat Sports"
	CatMotorsport    Category = "Motorsport"
	CatAnimalRacing  Category = "Racing"
	CatCycling       Category = "Cycling"
	CatAthletics     Category = "Athletics & Strength"
	CatAquatics      Category = "Water Sports"
	CatWinter        Category = "Winter Sports"
	CatTarget        Category = "Target & Precision"
	CatCue           Category = "Cue Sports"
	CatEquestrian    Category = "Equestrian"
	CatExtreme       Category = "Action Sports"
	CatMind          Category = "Mind Sports"
	CatEsports       Category = "Esports"
	CatMultiSport    Category = "Multi-Sport Events"
)

// categoryOrder fixes the display order of the sport menu.
var categoryOrder = []Category{
	CatFootballCodes, CatTeamSports, CatRacket, CatCombat, CatMotorsport,
	CatAnimalRacing, CatCycling, CatAthletics, CatAquatics, CatWinter,
	CatTarget, CatCue, CatEquestrian, CatExtreme, CatMind, CatEsports,
	CatMultiSport,
}

// Sport is one bettable sport.
type Sport struct {
	// Key is the stable URL slug and database key. It never changes once
	// events exist against it.
	Key string
	// Name is the customer-facing title.
	Name     string
	Category Category
	// Scoring is the unit this sport is scored in, used to word totals markets.
	Scoring Scoring
	// Formats are the contest shapes this sport runs in. A sport can be both a
	// match sport and a tournament sport (tennis) or both a race and a season
	// championship (Formula 1).
	Formats []Format
	// HasDraw reports whether a contest can end level in regulation. It decides
	// whether the headline market is two-way or three-way, and it is the single
	// most common source of mis-settlement if it is wrong.
	HasDraw bool
	// Periods are the named segments a contest divides into, for period markets.
	Periods []string
	// Markets are the market kinds this sport may carry.
	Markets []MarketKind
	// Competitions are well-known competitions, used for seeding and for the
	// competition filter in the sport menu.
	Competitions []string
	// PlaceTerms is the default number of places paid on each-way bets in
	// field events. Zero means each-way is not offered.
	PlaceTerms int
}

// Supports reports whether the sport may carry the given market kind.
func (s Sport) Supports(kind MarketKind) bool {
	for _, k := range s.Markets {
		if k == kind {
			return true
		}
	}
	return false
}

// HasFormat reports whether the sport runs contests of the given shape.
func (s Sport) HasFormat(f Format) bool {
	for _, have := range s.Formats {
		if have == f {
			return true
		}
	}
	return false
}

// ScoringNoun is the capitalised scoring unit used in market titles.
func (s Sport) ScoringNoun() string {
	switch s.Scoring {
	case ScoringGoals:
		return "Goals"
	case ScoringPoints:
		return "Points"
	case ScoringRuns:
		return "Runs"
	case ScoringSets:
		return "Sets"
	case ScoringGames:
		return "Games"
	case ScoringFrames:
		return "Frames"
	case ScoringLegs:
		return "Legs"
	case ScoringHoles:
		return "Holes"
	case ScoringStrokes:
		return "Strokes"
	case ScoringRounds:
		return "Rounds"
	case ScoringMaps:
		return "Maps"
	case ScoringWickets:
		return "Wickets"
	case ScoringMedals:
		return "Medals"
	default:
		return "Points"
	}
}

// MarketTitle renders a market's customer-facing name for this sport,
// substituting the scoring unit where the definition asks for it.
func (s Sport) MarketTitle(kind MarketKind) string {
	def, ok := Market(kind)
	if !ok {
		return string(kind)
	}
	return strings.ReplaceAll(def.Name, "%s", s.ScoringNoun())
}

// ---------------------------------------------------------------------------
// shared market bundles
//
// Market lists are shared rather than repeated per sport: a change to what a
// draw-capable team sport can price should apply to every such sport at once.
// ---------------------------------------------------------------------------

var (
	// Team sports that can end level in regulation.
	marketsTeamDraw = []MarketKind{
		KindMoneyline3, KindDoubleChance, KindDrawNoBet, KindHandicap,
		KindAsianHandicap, KindTotals, KindAsianTotals, KindTeamTotals,
		KindBothTeamsScore, KindCorrectScore, KindHalfTimeFull, KindOddEven,
		KindCleanSheet, KindWinningMargin, KindPeriodWinner, KindPeriodTotals,
		KindFirstScorer, KindAnytimeScorer, KindPlayerProp, KindToQualify,
		KindSpecial,
	}
	// Team sports played to a decision.
	marketsTeamNoDraw = []MarketKind{
		KindMoneyline2, KindHandicap, KindAsianHandicap, KindTotals,
		KindAsianTotals, KindTeamTotals, KindWinningMargin, KindOddEven,
		KindPeriodWinner, KindPeriodTotals, KindRaceToScore, KindPlayerProp,
		KindToQualify, KindSpecial,
	}
	// Season-long markets appended to league sports.
	marketsSeason = []MarketKind{
		KindSeasonWinner, KindTopFinish, KindRelegation, KindOutright,
		KindGroupWinner,
	}
	// Two-player, set-scored sports: tennis and its relatives.
	marketsRacket = []MarketKind{
		KindMoneyline2, KindHandicap, KindAsianHandicap, KindSetBetting,
		KindTotalSets, KindTotalGames, KindTotals, KindOddEven,
		KindPeriodWinner, KindPlayerProp, KindOutright, KindSpecial,
	}
	// Fighting sports.
	marketsCombat = []MarketKind{
		KindMoneyline2, KindMethodOfVictory, KindRoundBetting, KindTotalRounds,
		KindGoesDistance, KindHandicap, KindSpecial,
	}
	// Field events decided on finishing order.
	marketsField = []MarketKind{
		KindOutright, KindPlace, KindEachWay, KindPodium, KindHeadToHead,
		KindTopNational, KindGroupWinner, KindSpecial,
	}
	// Motor racing, which adds session and race-incident markets.
	marketsMotorsport = []MarketKind{
		KindOutright, KindPodium, KindHeadToHead, KindPolePosition,
		KindFastestLap, KindClassifiedFinish, KindSafetyCar, KindConstructor,
		KindSeasonWinner, KindTopNational, KindGroupWinner, KindSpecial,
	}
	// Stage races.
	marketsStageRace = []MarketKind{
		KindOutright, KindStageWinner, KindPodium, KindHeadToHead, KindPlace,
		KindEachWay, KindTopNational, KindGroupWinner, KindSpecial,
	}
	// Golf, which is a field event in stroke play and a head-to-head sport in
	// match play, where a halved match is a genuine third result.
	marketsGolf = []MarketKind{
		KindMoneyline3, KindDrawNoBet, KindHandicap,
		KindOutright, KindPlace, KindEachWay, KindPodium, KindHeadToHead,
		KindMissTheCut, KindTopNational, KindGroupWinner, KindTopFinish,
		KindSpecial,
	}
	// Frame- and leg-scored cue and darts sports.
	marketsFrames = []MarketKind{
		KindMoneyline2, KindHandicap, KindFrameBetting, KindTotals,
		KindOddEven, KindMostBreaks, KindOutright, KindSpecial,
	}
	marketsDarts = []MarketKind{
		KindMoneyline2, KindHandicap, KindTotals, KindOddEven, KindCheckout,
		KindNineDarter, KindOutright, KindSpecial,
	}
	// Esports, which are map-scored and otherwise behave like no-draw team sports.
	marketsEsports = []MarketKind{
		KindMoneyline2, KindHandicap, KindTotals, KindTeamTotals,
		KindPeriodWinner, KindPeriodTotals, KindOddEven, KindRaceToScore,
		KindPlayerProp, KindOutright, KindToQualify, KindSpecial,
	}
	// Cricket. A first-class match can be drawn, so the three-way result and
	// its draw-no-bet alternative both have to be available.
	marketsCricket = []MarketKind{
		KindMoneyline3, KindMoneyline2, KindDrawNoBet, KindHandicap, KindTotals,
		KindInningsRuns, KindTopRunScorer, KindTopWicketTaker, KindTossWinner,
		KindOddEven, KindPlayerProp, KindOutright, KindToQualify, KindSpecial,
	}
	// Head-to-head contests scored by judges or a clock, with a championship.
	marketsIndividual = []MarketKind{
		KindMoneyline2, KindHandicap, KindOutright, KindPodium,
		KindHeadToHead, KindPlace, KindSpecial,
	}
	// Nation-level multi-sport events.
	marketsMedals = []MarketKind{
		KindOutright, KindMedalCount, KindTopNational, KindPodium, KindSpecial,
	}
)

func withSeason(base []MarketKind) []MarketKind {
	out := make([]MarketKind, 0, len(base)+len(marketsSeason))
	out = append(out, base...)
	out = append(out, marketsSeason...)
	return out
}

// Common period layouts.
var (
	periodsHalves   = []string{"1st Half", "2nd Half"}
	periodsQuarters = []string{"1st Quarter", "2nd Quarter", "3rd Quarter", "4th Quarter"}
	periodsThirds   = []string{"1st Period", "2nd Period", "3rd Period"}
	periodsInnings  = []string{"1st Innings", "2nd Innings"}
	periodsSets     = []string{"Set 1", "Set 2", "Set 3", "Set 4", "Set 5"}
	periodsMaps     = []string{"Map 1", "Map 2", "Map 3", "Map 4", "Map 5"}
	periodsNone     []string
)

func sortMarketDefs(defs []MarketDef) {
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
}
