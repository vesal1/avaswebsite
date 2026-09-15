// Package catalog defines the sports the book offers and the betting markets
// that each sport can carry.
//
// The catalog is static data compiled into the binary. Events, prices and
// results are dynamic and live in the database; what a *football* match is
// allowed to price, and what a *snooker* match is allowed to price, is a
// property of the sport itself and belongs here.
package catalog

// Format describes the structural shape of a contest, which is what actually
// decides the markets that can be priced. A tennis match and a darts match are
// different sports but the same shape: two individuals, no draw, scored in
// sets. Pricing follows shape, not name.
type Format string

const (
	// FormatMatch is a head-to-head contest between exactly two sides.
	FormatMatch Format = "match"
	// FormatRace is a single contest with a field of many competitors where
	// finishing position is the result.
	FormatRace Format = "race"
	// FormatTournament is a competition resolved over many contests, priced
	// as an outright long before it finishes.
	FormatTournament Format = "tournament"
)

// Scoring names the unit a sport is scored in. It drives the wording of totals
// markets ("Total Goals" against "Total Points") and the granularity of the
// lines the trader is offered.
type Scoring string

const (
	ScoringGoals    Scoring = "goals"
	ScoringPoints   Scoring = "points"
	ScoringRuns     Scoring = "runs"
	ScoringSets     Scoring = "sets"
	ScoringGames    Scoring = "games"
	ScoringFrames   Scoring = "frames"
	ScoringLegs     Scoring = "legs"
	ScoringHoles    Scoring = "holes"
	ScoringStrokes  Scoring = "strokes"
	ScoringRounds   Scoring = "rounds"
	ScoringPosition Scoring = "position"
	ScoringTime     Scoring = "time"
	ScoringDistance Scoring = "distance"
	ScoringJudged   Scoring = "judged"
	ScoringMaps     Scoring = "maps"
	ScoringWickets  Scoring = "wickets"
	ScoringMedals   Scoring = "medals"
)

// MarketKind identifies a type of market. Grading rules are attached to the
// kind, so a new sport that reuses a kind inherits its settlement logic.
type MarketKind string

const (
	// Two- and three-way results.
	KindMoneyline2   MarketKind = "moneyline_2way"
	KindMoneyline3   MarketKind = "moneyline_3way"
	KindDoubleChance MarketKind = "double_chance"
	KindDrawNoBet    MarketKind = "draw_no_bet"

	// Lines.
	KindHandicap      MarketKind = "handicap"
	KindAsianHandicap MarketKind = "asian_handicap"
	KindTotals        MarketKind = "totals"
	KindAsianTotals   MarketKind = "asian_totals"
	KindTeamTotals    MarketKind = "team_totals"
	KindWinningMargin MarketKind = "winning_margin"

	// Score composition.
	KindCorrectScore   MarketKind = "correct_score"
	KindHalfTimeFull   MarketKind = "half_time_full_time"
	KindBothTeamsScore MarketKind = "both_teams_to_score"
	KindOddEven        MarketKind = "odd_even"
	KindCleanSheet     MarketKind = "clean_sheet"

	// Segments of a contest.
	KindPeriodWinner MarketKind = "period_winner"
	KindPeriodTotals MarketKind = "period_totals"
	KindRaceToScore  MarketKind = "race_to_score"

	// Field events.
	KindOutright    MarketKind = "outright"
	KindPlace       MarketKind = "place"
	KindEachWay     MarketKind = "each_way"
	KindPodium      MarketKind = "podium"
	KindHeadToHead  MarketKind = "head_to_head"
	KindTopNational MarketKind = "top_nationality"
	KindStageWinner MarketKind = "stage_winner"
	KindGroupWinner MarketKind = "group_winner"
	KindToQualify   MarketKind = "to_qualify"
	KindMissTheCut  MarketKind = "make_miss_cut"

	// Combat sports.
	KindMethodOfVictory MarketKind = "method_of_victory"
	KindRoundBetting    MarketKind = "round_betting"
	KindTotalRounds     MarketKind = "total_rounds"
	KindGoesDistance    MarketKind = "goes_the_distance"

	// Set- and frame-scored sports.
	KindSetBetting   MarketKind = "set_betting"
	KindTotalSets    MarketKind = "total_sets"
	KindTotalGames   MarketKind = "total_games"
	KindFrameBetting MarketKind = "frame_betting"
	KindMostBreaks   MarketKind = "highest_break"
	KindNineDarter   MarketKind = "nine_dart_finish"
	KindCheckout     MarketKind = "highest_checkout"

	// Motorsport.
	KindPolePosition     MarketKind = "pole_position"
	KindFastestLap       MarketKind = "fastest_lap"
	KindClassifiedFinish MarketKind = "classified_finish"
	KindSafetyCar        MarketKind = "safety_car"
	KindConstructor      MarketKind = "constructors_title"

	// Cricket and baseball style.
	KindTopRunScorer   MarketKind = "top_run_scorer"
	KindTopWicketTaker MarketKind = "top_wicket_taker"
	KindTossWinner     MarketKind = "toss_winner"
	KindInningsRuns    MarketKind = "innings_runs"

	// Player and generic props.
	KindPlayerProp    MarketKind = "player_prop"
	KindFirstScorer   MarketKind = "first_scorer"
	KindAnytimeScorer MarketKind = "anytime_scorer"
	KindSpecial       MarketKind = "special"

	// Season-long.
	KindSeasonWinner MarketKind = "season_winner"
	KindRelegation   MarketKind = "relegation"
	KindTopFinish    MarketKind = "top_n_finish"
	KindMedalCount   MarketKind = "medal_count"
)

// Grading says how a market's winning selection is determined at settlement.
type Grading string

const (
	// GradedFromScore is computed automatically from the posted event score.
	GradedFromScore Grading = "from_score"
	// GradedFromPositions is computed from the posted finishing order.
	GradedFromPositions Grading = "from_positions"
	// GradedManually needs a trader to mark the winning selection. Most props
	// land here, exactly as they do at a real book: no score line encodes
	// "method of victory" or "first goalscorer".
	GradedManually Grading = "manual"
)

// MarketDef is the static definition of a market kind.
type MarketDef struct {
	Kind MarketKind
	// Name is the customer-facing title, with %s replaced by the scoring unit
	// where the wording depends on the sport ("Total %s" -> "Total Goals").
	Name string
	// Grading is how the settlement engine resolves it.
	Grading Grading
	// NeedsLine marks markets that are meaningless without a handicap or
	// total line attached (spreads, over/under).
	NeedsLine bool
	// AllowsPush marks markets that can land exactly on the line and return
	// the stake rather than winning or losing.
	AllowsPush bool
	// Description is shown in the market help text and the rules page.
	Description string
}

// marketDefs is the single source of truth for market behaviour.
var marketDefs = map[MarketKind]MarketDef{
	KindMoneyline2: {KindMoneyline2, "Match Winner", GradedFromScore, false, false,
		"Pick the winner. There is no draw option in this sport, so a tie voids the market."},
	KindMoneyline3: {KindMoneyline3, "Match Result (1X2)", GradedFromScore, false, false,
		"Pick home win, draw or away win at the end of regulation."},
	KindDoubleChance: {KindDoubleChance, "Double Chance", GradedFromScore, false, false,
		"Cover two of the three results in one selection."},
	KindDrawNoBet: {KindDrawNoBet, "Draw No Bet", GradedFromScore, false, true,
		"Pick a winner; the stake is returned if the match is drawn."},
	KindHandicap: {KindHandicap, "Handicap", GradedFromScore, true, true,
		"One side starts with a virtual lead or deficit applied to the final score."},
	KindAsianHandicap: {KindAsianHandicap, "Asian Handicap", GradedFromScore, true, true,
		"Handicap betting with quarter lines, which can split a stake into a half win and a half push."},
	KindTotals: {KindTotals, "Total %s", GradedFromScore, true, true,
		"Bet on the combined %s being over or under the posted line."},
	KindAsianTotals: {KindAsianTotals, "Asian Total %s", GradedFromScore, true, true,
		"Over/under with quarter lines, which can split a stake across two outcomes."},
	KindTeamTotals: {KindTeamTotals, "Team Total %s", GradedFromScore, true, true,
		"Bet on a single side's %s being over or under the posted line."},
	KindWinningMargin: {KindWinningMargin, "Winning Margin", GradedFromScore, false, false,
		"Pick the winner and the band their margin of victory falls in."},
	KindCorrectScore: {KindCorrectScore, "Correct Score", GradedFromScore, false, false,
		"Pick the exact final score."},
	KindHalfTimeFull: {KindHalfTimeFull, "Half Time / Full Time", GradedFromScore, false, false,
		"Pick the leader at the interval and the final result together."},
	KindBothTeamsScore: {KindBothTeamsScore, "Both Teams to Score", GradedFromScore, false, false,
		"Bet on whether both sides find the net."},
	KindOddEven: {KindOddEven, "Odd or Even %s", GradedFromScore, false, false,
		"Bet on the parity of the combined final total."},
	KindCleanSheet: {KindCleanSheet, "Clean Sheet", GradedFromScore, false, false,
		"Bet on a side conceding nothing."},
	KindPeriodWinner: {KindPeriodWinner, "Period Winner", GradedFromScore, false, false,
		"Bet on the result of a single named period rather than the whole contest."},
	KindPeriodTotals: {KindPeriodTotals, "Period Total %s", GradedFromScore, true, true,
		"Over/under on a single named period."},
	KindRaceToScore: {KindRaceToScore, "Race to Score", GradedManually, false, false,
		"Bet on which side reaches a given score first."},
	KindOutright: {KindOutright, "Outright Winner", GradedFromPositions, false, false,
		"Bet on the outright winner of the event."},
	KindPlace: {KindPlace, "To Place", GradedFromPositions, false, false,
		"Bet on a competitor finishing inside the paid places."},
	KindEachWay: {KindEachWay, "Each Way", GradedFromPositions, false, false,
		"Half the stake on the win, half on the place, settled at the posted place terms."},
	KindPodium: {KindPodium, "Podium Finish", GradedFromPositions, false, false,
		"Bet on a competitor finishing in the top three."},
	KindHeadToHead: {KindHeadToHead, "Head to Head", GradedFromPositions, false, true,
		"Bet on which of two named competitors finishes ahead of the other."},
	KindTopNational: {KindTopNational, "Top Nationality", GradedFromPositions, false, false,
		"Bet on the best-placed competitor of a given nationality."},
	KindStageWinner: {KindStageWinner, "Stage Winner", GradedFromPositions, false, false,
		"Bet on the winner of an individual stage."},
	KindGroupWinner: {KindGroupWinner, "Group Betting", GradedFromPositions, false, false,
		"Bet on the best finisher within a named group of competitors."},
	KindToQualify: {KindToQualify, "To Qualify", GradedManually, false, false,
		"Bet on a side progressing to the next round, extra time and penalties included."},
	KindMissTheCut: {KindMissTheCut, "To Make the Cut", GradedManually, false, false,
		"Bet on a competitor surviving the halfway cut."},
	KindMethodOfVictory: {KindMethodOfVictory, "Method of Victory", GradedManually, false, false,
		"Bet on how the contest ends: knockout, submission, decision or draw."},
	KindRoundBetting: {KindRoundBetting, "Round Betting", GradedManually, false, false,
		"Bet on the fighter and the round the contest ends in."},
	KindTotalRounds: {KindTotalRounds, "Total Rounds", GradedFromScore, true, true,
		"Over/under on how many completed rounds the contest lasts."},
	KindGoesDistance: {KindGoesDistance, "To Go the Distance", GradedManually, false, false,
		"Bet on whether the contest reaches the final bell."},
	KindSetBetting: {KindSetBetting, "Set Betting", GradedFromScore, false, false,
		"Pick the exact set score."},
	KindTotalSets: {KindTotalSets, "Total Sets", GradedFromScore, true, true,
		"Over/under on the number of sets played."},
	KindTotalGames: {KindTotalGames, "Total Games", GradedFromScore, true, true,
		"Over/under on the number of games played across the match."},
	KindFrameBetting: {KindFrameBetting, "Frame Betting", GradedFromScore, false, false,
		"Pick the exact frame score."},
	KindMostBreaks: {KindMostBreaks, "Highest Break", GradedManually, false, false,
		"Bet on which player compiles the highest break."},
	KindNineDarter: {KindNineDarter, "Nine Dart Finish", GradedManually, false, false,
		"Bet on a perfect leg being thrown."},
	KindCheckout: {KindCheckout, "Highest Checkout", GradedManually, false, false,
		"Bet on which player records the highest checkout."},
	KindPolePosition: {KindPolePosition, "Pole Position", GradedManually, false, false,
		"Bet on the fastest qualifier."},
	KindFastestLap: {KindFastestLap, "Fastest Lap", GradedManually, false, false,
		"Bet on the driver or rider setting the fastest lap of the race."},
	KindClassifiedFinish: {KindClassifiedFinish, "To Be Classified", GradedManually, false, false,
		"Bet on a competitor being classified as a finisher."},
	KindSafetyCar: {KindSafetyCar, "Safety Car", GradedManually, false, false,
		"Bet on a safety car or equivalent neutralisation being deployed."},
	KindConstructor: {KindConstructor, "Constructors' Championship", GradedFromPositions, false, false,
		"Bet on the team taking the constructors' title."},
	KindTopRunScorer: {KindTopRunScorer, "Top Run Scorer", GradedManually, false, false,
		"Bet on the leading run scorer."},
	KindTopWicketTaker: {KindTopWicketTaker, "Top Wicket Taker", GradedManually, false, false,
		"Bet on the leading wicket taker."},
	KindTossWinner: {KindTossWinner, "Toss Winner", GradedManually, false, false,
		"Bet on which captain wins the toss."},
	KindInningsRuns: {KindInningsRuns, "Innings Runs", GradedManually, true, true,
		"Over/under on the runs scored in a named innings."},
	KindPlayerProp: {KindPlayerProp, "Player Props", GradedManually, false, false,
		"Bets on an individual competitor's statistical output."},
	KindFirstScorer: {KindFirstScorer, "First Scorer", GradedManually, false, false,
		"Bet on the first competitor to score."},
	KindAnytimeScorer: {KindAnytimeScorer, "Anytime Scorer", GradedManually, false, false,
		"Bet on a competitor scoring at any point."},
	KindSpecial: {KindSpecial, "Specials", GradedManually, false, false,
		"Event-specific markets priced by a trader."},
	KindSeasonWinner: {KindSeasonWinner, "Season Winner", GradedFromPositions, false, false,
		"Bet on the champion at the end of the season."},
	KindRelegation: {KindRelegation, "Relegation", GradedManually, false, false,
		"Bet on a side finishing in a relegation place."},
	KindTopFinish: {KindTopFinish, "Top Finish", GradedFromPositions, false, false,
		"Bet on a side finishing inside a named number of places."},
	KindMedalCount: {KindMedalCount, "Medal Count", GradedManually, true, true,
		"Over/under on the medals won by a nation."},
}

// Market returns the definition for a kind.
func Market(kind MarketKind) (MarketDef, bool) {
	def, ok := marketDefs[kind]
	return def, ok
}

// MustMarket returns the definition for a kind, panicking on an unknown one.
// Only safe for compiled-in catalog data, never for user input.
func MustMarket(kind MarketKind) MarketDef {
	def, ok := marketDefs[kind]
	if !ok {
		panic("catalog: unknown market kind " + string(kind))
	}
	return def
}

// AllMarkets lists every defined market kind.
func AllMarkets() []MarketDef {
	out := make([]MarketDef, 0, len(marketDefs))
	for _, def := range marketDefs {
		out = append(out, def)
	}
	sortMarketDefs(out)
	return out
}
