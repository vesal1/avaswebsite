package betting

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/store"
)

// Result is the posted outcome of an event.
type Result struct {
	EventID int64
	// Participants carries a final score, a finishing position, or both,
	// depending on the shape of the contest.
	Participants []ParticipantResult
	// Abandoned voids every market on the event regardless of any partial
	// score: a contest that did not finish did not produce a result.
	Abandoned bool
	// PlacesPaid overrides the sport's default each-way place terms.
	PlacesPaid int
}

// ParticipantResult is one competitor's outcome.
type ParticipantResult struct {
	ParticipantID  int64
	Score          int64
	HasScore       bool
	FinishPosition int
	Withdrawn      bool
}

// participantView indexes a result for grading.
type participantView struct {
	byID    map[int64]ParticipantResult
	home    ParticipantResult
	away    ParticipantResult
	hasHome bool
	hasAway bool
}

func newParticipantView(participants []store.Participant, result Result) participantView {
	view := participantView{byID: make(map[int64]ParticipantResult, len(result.Participants))}
	for _, outcome := range result.Participants {
		view.byID[outcome.ParticipantID] = outcome
	}
	for _, participant := range participants {
		outcome, ok := view.byID[participant.ID]
		if !ok {
			continue
		}
		switch participant.HomeAway {
		case "home":
			view.home, view.hasHome = outcome, true
		case "away":
			view.away, view.hasAway = outcome, true
		}
	}
	return view
}

// hasMatchScore reports whether both sides of a head-to-head have a score.
func (v participantView) hasMatchScore() bool {
	return v.hasHome && v.hasAway && v.home.HasScore && v.away.HasScore
}

// GradeMarket decides the outcome of every selection in a market.
//
// It returns a map from selection id to one of the store outcome constants.
// A market it cannot grade from the posted result returns ErrManualGrading,
// which is not a failure: many markets are graded by a trader at every real
// book, because no score line encodes "method of victory" or "first scorer".
func GradeMarket(sport catalog.Sport, market store.Market, participants []store.Participant, result Result) (map[int64]string, error) {
	outcomes := make(map[int64]string, len(market.Selections))

	// An abandoned event voids everything, whatever partial score exists.
	if result.Abandoned {
		for _, selection := range market.Selections {
			outcomes[selection.ID] = store.OutcomeVoid
		}
		return outcomes, nil
	}

	def, ok := catalog.Market(catalog.MarketKind(market.Kind))
	if !ok {
		return nil, fmt.Errorf("betting: market %d has unknown kind %q", market.ID, market.Kind)
	}
	if def.Grading == catalog.GradedManually {
		return nil, ErrManualGrading
	}

	view := newParticipantView(participants, result)

	switch catalog.MarketKind(market.Kind) {
	case catalog.KindMoneyline2, catalog.KindMoneyline3, catalog.KindDrawNoBet, catalog.KindDoubleChance:
		return gradeResultMarket(market, view)
	case catalog.KindHandicap:
		return gradeHandicap(market, view, false)
	case catalog.KindAsianHandicap:
		return gradeHandicap(market, view, true)
	case catalog.KindTotals, catalog.KindPeriodTotals:
		return gradeTotals(market, view, false)
	case catalog.KindAsianTotals:
		return gradeTotals(market, view, true)
	case catalog.KindTeamTotals:
		return gradeTeamTotals(market, view)
	case catalog.KindTotalRounds, catalog.KindTotalSets, catalog.KindTotalGames:
		return gradeTotals(market, view, false)
	case catalog.KindBothTeamsScore:
		return gradeBothTeamsScore(market, view)
	case catalog.KindOddEven:
		return gradeOddEven(market, view)
	case catalog.KindCleanSheet:
		return gradeCleanSheet(market, view)
	case catalog.KindCorrectScore, catalog.KindSetBetting, catalog.KindFrameBetting:
		return gradeCorrectScore(market, view)
	case catalog.KindHalfTimeFull:
		// Needs a half-time score as well as a final one, which the result
		// model does not carry; a trader posts it.
		return nil, ErrManualGrading
	case catalog.KindWinningMargin:
		return gradeWinningMargin(market, view)
	case catalog.KindPeriodWinner:
		// The score model is per event, not per period, so a period result is
		// posted by a trader.
		return nil, ErrManualGrading
	case catalog.KindOutright, catalog.KindStageWinner, catalog.KindSeasonWinner,
		catalog.KindConstructor, catalog.KindGroupWinner, catalog.KindTopNational:
		return gradeByPosition(market, view, 1)
	case catalog.KindPodium:
		return gradeByPosition(market, view, 3)
	case catalog.KindPlace, catalog.KindEachWay, catalog.KindTopFinish:
		return gradeByPosition(market, view, placesPaid(sport, market, result))
	case catalog.KindHeadToHead:
		return gradeHeadToHead(market, view)
	default:
		return nil, ErrManualGrading
	}
}

func placesPaid(sport catalog.Sport, market store.Market, result Result) int {
	if result.PlacesPaid > 0 {
		return result.PlacesPaid
	}
	// A line on a place market carries the number of places, scaled like every
	// other line column.
	if market.HasLine && market.LineX100 > 0 {
		return int(market.LineX100 / 100)
	}
	if sport.PlaceTerms > 0 {
		return sport.PlaceTerms
	}
	return 1
}

// ---------------------------------------------------------------------------
// head-to-head result markets
// ---------------------------------------------------------------------------

func gradeResultMarket(market store.Market, view participantView) (map[int64]string, error) {
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	home, away := view.home.Score, view.away.Score

	winner := "draw"
	switch {
	case home > away:
		winner = "home"
	case away > home:
		winner = "away"
	}

	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		code := strings.ToLower(selection.OutcomeCode)
		switch catalog.MarketKind(market.Kind) {
		case catalog.KindMoneyline2:
			// A two-way market on a sport that can still tie: the tie voids
			// rather than losing both sides.
			if winner == "draw" {
				outcomes[selection.ID] = store.OutcomeVoid
				continue
			}
			outcomes[selection.ID] = wonIf(code == winner)
		case catalog.KindDrawNoBet:
			if winner == "draw" {
				outcomes[selection.ID] = store.OutcomeVoid
				continue
			}
			outcomes[selection.ID] = wonIf(code == winner)
		case catalog.KindDoubleChance:
			outcomes[selection.ID] = wonIf(doubleChanceCovers(code, winner))
		default: // three-way
			outcomes[selection.ID] = wonIf(code == winner)
		}
	}
	return outcomes, nil
}

func doubleChanceCovers(code, winner string) bool {
	normalised := strings.NewReplacer("1", "home", "x", "draw", "2", "away").Replace(code)
	switch code {
	case "1x", "x1", "home_draw":
		return winner == "home" || winner == "draw"
	case "12", "21", "home_away":
		return winner == "home" || winner == "away"
	case "x2", "2x", "draw_away":
		return winner == "draw" || winner == "away"
	}
	return strings.Contains(normalised, winner)
}

// ---------------------------------------------------------------------------
// handicaps
// ---------------------------------------------------------------------------

// gradeHandicap settles a spread. The market's line is the handicap applied to
// the home side; the away side carries its negation.
//
// When asian is set and the line is a quarter (x.25 or x.75), the stake is
// split across the two neighbouring half lines, which is what produces the
// half-win and half-loss outcomes unique to Asian handicap betting.
func gradeHandicap(market store.Market, view participantView, asian bool) (map[int64]string, error) {
	if !market.HasLine {
		return nil, fmt.Errorf("%w: handicap market %d has no line", ErrIncompleteResult, market.ID)
	}
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	home, away := view.home.Score*100, view.away.Score*100

	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		side := strings.ToLower(selection.OutcomeCode)
		line := market.LineX100
		if side == "away" {
			line = -line
		} else if side != "home" {
			return nil, fmt.Errorf("betting: handicap selection %d has outcome code %q",
				selection.ID, selection.OutcomeCode)
		}

		if asian && isQuarterLine(line) {
			lower := gradeSpreadAt(home, away, side, line-25)
			upper := gradeSpreadAt(home, away, side, line+25)
			outcomes[selection.ID] = combineHalves(lower, upper)
			continue
		}
		outcomes[selection.ID] = gradeSpreadAt(home, away, side, line)
	}
	return outcomes, nil
}

// isQuarterLine reports whether a line sits on a quarter rather than a whole
// or a half, e.g. -0.25 or 1.75.
func isQuarterLine(lineX100 int64) bool {
	remainder := lineX100 % 100
	if remainder < 0 {
		remainder = -remainder
	}
	return remainder == 25 || remainder == 75
}

// gradeSpreadAt grades one side against a single, non-quarter line.
func gradeSpreadAt(homeX100, awayX100 int64, side string, lineX100 int64) string {
	var mine, theirs int64
	if side == "home" {
		mine, theirs = homeX100+lineX100, awayX100
	} else {
		mine, theirs = awayX100+lineX100, homeX100
	}
	switch {
	case mine > theirs:
		return store.OutcomeWon
	case mine < theirs:
		return store.OutcomeLost
	default:
		return store.OutcomeVoid
	}
}

// combineHalves merges the two halves of a quarter-line bet.
func combineHalves(lower, upper string) string {
	switch {
	case lower == store.OutcomeWon && upper == store.OutcomeWon:
		return store.OutcomeWon
	case lower == store.OutcomeLost && upper == store.OutcomeLost:
		return store.OutcomeLost
	case lower == store.OutcomeVoid && upper == store.OutcomeVoid:
		return store.OutcomeVoid
	case lower == store.OutcomeWon || upper == store.OutcomeWon:
		// One half won, the other pushed.
		return store.OutcomeHalfWon
	default:
		// One half lost, the other pushed.
		return store.OutcomeHalfLost
	}
}

// ---------------------------------------------------------------------------
// totals
// ---------------------------------------------------------------------------

func gradeTotals(market store.Market, view participantView, asian bool) (map[int64]string, error) {
	if !market.HasLine {
		return nil, fmt.Errorf("%w: totals market %d has no line", ErrIncompleteResult, market.ID)
	}
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	total := (view.home.Score + view.away.Score) * 100

	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		side := strings.ToLower(selection.OutcomeCode)
		if side != "over" && side != "under" {
			return nil, fmt.Errorf("betting: totals selection %d has outcome code %q",
				selection.ID, selection.OutcomeCode)
		}
		if asian && isQuarterLine(market.LineX100) {
			lower := gradeTotalAt(total, side, market.LineX100-25)
			upper := gradeTotalAt(total, side, market.LineX100+25)
			outcomes[selection.ID] = combineHalves(lower, upper)
			continue
		}
		outcomes[selection.ID] = gradeTotalAt(total, side, market.LineX100)
	}
	return outcomes, nil
}

func gradeTotalAt(totalX100 int64, side string, lineX100 int64) string {
	switch {
	case totalX100 > lineX100:
		return wonIf(side == "over")
	case totalX100 < lineX100:
		return wonIf(side == "under")
	default:
		return store.OutcomeVoid
	}
}

func gradeTeamTotals(market store.Market, view participantView) (map[int64]string, error) {
	if !market.HasLine {
		return nil, fmt.Errorf("%w: team totals market %d has no line", ErrIncompleteResult, market.ID)
	}
	subject, ok := view.byID[market.SubjectID]
	if !ok || !subject.HasScore {
		return nil, fmt.Errorf("%w: team totals market %d has no score for its subject",
			ErrIncompleteResult, market.ID)
	}
	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		side := strings.ToLower(selection.OutcomeCode)
		outcomes[selection.ID] = gradeTotalAt(subject.Score*100, side, market.LineX100)
	}
	return outcomes, nil
}

// ---------------------------------------------------------------------------
// score composition
// ---------------------------------------------------------------------------

func gradeBothTeamsScore(market store.Market, view participantView) (map[int64]string, error) {
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	both := view.home.Score > 0 && view.away.Score > 0
	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		yes := strings.EqualFold(selection.OutcomeCode, "yes")
		outcomes[selection.ID] = wonIf(yes == both)
	}
	return outcomes, nil
}

func gradeOddEven(market store.Market, view participantView) (map[int64]string, error) {
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	odd := (view.home.Score+view.away.Score)%2 != 0
	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		wantsOdd := strings.EqualFold(selection.OutcomeCode, "odd")
		outcomes[selection.ID] = wonIf(wantsOdd == odd)
	}
	return outcomes, nil
}

func gradeCleanSheet(market store.Market, view participantView) (map[int64]string, error) {
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	// The subject keeps a clean sheet when the other side failed to score.
	var conceded int64
	switch market.SubjectID {
	case view.home.ParticipantID:
		conceded = view.away.Score
	case view.away.ParticipantID:
		conceded = view.home.Score
	default:
		return nil, fmt.Errorf("%w: clean sheet market %d has no subject", ErrIncompleteResult, market.ID)
	}
	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		yes := strings.EqualFold(selection.OutcomeCode, "yes")
		outcomes[selection.ID] = wonIf(yes == (conceded == 0))
	}
	return outcomes, nil
}

// gradeCorrectScore settles an exact-score market. Outcome codes are written
// "home-away", e.g. "2-1", from the home side's point of view.
func gradeCorrectScore(market store.Market, view participantView) (map[int64]string, error) {
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	actual := fmt.Sprintf("%d-%d", view.home.Score, view.away.Score)
	outcomes := make(map[int64]string, len(market.Selections))
	anyWinner := false
	for _, selection := range market.Selections {
		won := strings.TrimSpace(selection.OutcomeCode) == actual
		anyWinner = anyWinner || won
		outcomes[selection.ID] = wonIf(won)
	}
	if !anyWinner {
		// The real score was not among the prices offered. Losing every
		// selection would be taking money for an outcome that was never
		// available, so the market voids.
		for id := range outcomes {
			outcomes[id] = store.OutcomeVoid
		}
	}
	return outcomes, nil
}

// gradeWinningMargin settles margin bands. Outcome codes are "side:min-max",
// e.g. "home:1-2", or "draw".
func gradeWinningMargin(market store.Market, view participantView) (map[int64]string, error) {
	if !view.hasMatchScore() {
		return nil, fmt.Errorf("%w: market %d needs a score for both sides", ErrIncompleteResult, market.ID)
	}
	margin := view.home.Score - view.away.Score
	side := "home"
	if margin < 0 {
		side, margin = "away", -margin
	}
	if margin == 0 {
		side = "draw"
	}

	outcomes := make(map[int64]string, len(market.Selections))
	anyWinner := false
	for _, selection := range market.Selections {
		code := strings.ToLower(strings.TrimSpace(selection.OutcomeCode))
		if code == "draw" {
			won := side == "draw"
			anyWinner = anyWinner || won
			outcomes[selection.ID] = wonIf(won)
			continue
		}
		codeSide, band, ok := strings.Cut(code, ":")
		if !ok {
			return nil, fmt.Errorf("betting: winning margin selection %d has outcome code %q",
				selection.ID, selection.OutcomeCode)
		}
		low, high, err := parseBand(band)
		if err != nil {
			return nil, fmt.Errorf("betting: selection %d: %w", selection.ID, err)
		}
		won := codeSide == side && margin >= low && margin <= high
		anyWinner = anyWinner || won
		outcomes[selection.ID] = wonIf(won)
	}
	if !anyWinner {
		for id := range outcomes {
			outcomes[id] = store.OutcomeVoid
		}
	}
	return outcomes, nil
}

func parseBand(band string) (int64, int64, error) {
	lowStr, highStr, ok := strings.Cut(band, "-")
	if !ok {
		value, err := strconv.ParseInt(strings.TrimSpace(band), 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("%q is not a margin band", band)
		}
		return value, value, nil
	}
	low, err := strconv.ParseInt(strings.TrimSpace(lowStr), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a margin band", band)
	}
	highStr = strings.TrimSpace(highStr)
	if highStr == "" || highStr == "+" {
		return low, 1 << 30, nil // an open-ended top band
	}
	high, err := strconv.ParseInt(highStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a margin band", band)
	}
	return low, high, nil
}

// ---------------------------------------------------------------------------
// field events
// ---------------------------------------------------------------------------

// gradeByPosition settles a market on finishing order, paying anything inside
// the given number of places.
func gradeByPosition(market store.Market, view participantView, places int) (map[int64]string, error) {
	if places < 1 {
		places = 1
	}
	outcomes := make(map[int64]string, len(market.Selections))
	for _, selection := range market.Selections {
		if selection.ParticipantID == 0 {
			return nil, fmt.Errorf("%w: selection %d is not tied to a competitor",
				ErrIncompleteResult, selection.ID)
		}
		outcome, ok := view.byID[selection.ParticipantID]
		if !ok {
			return nil, fmt.Errorf("%w: no result posted for competitor %d",
				ErrIncompleteResult, selection.ParticipantID)
		}
		// A non-runner's stake comes back; they were never given a chance to
		// win the bet that was struck on them.
		if outcome.Withdrawn {
			outcomes[selection.ID] = store.OutcomeVoid
			continue
		}
		outcomes[selection.ID] = wonIf(outcome.FinishPosition >= 1 && outcome.FinishPosition <= places)
	}
	return outcomes, nil
}

// gradeHeadToHead settles a two-competitor matchup inside a field event.
func gradeHeadToHead(market store.Market, view participantView) (map[int64]string, error) {
	if len(market.Selections) != 2 {
		return nil, fmt.Errorf("%w: head to head market %d has %d selections, want 2",
			ErrIncompleteResult, market.ID, len(market.Selections))
	}
	first, ok1 := view.byID[market.Selections[0].ParticipantID]
	second, ok2 := view.byID[market.Selections[1].ParticipantID]
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("%w: head to head market %d is missing a result", ErrIncompleteResult, market.ID)
	}

	outcomes := make(map[int64]string, 2)
	switch {
	case first.Withdrawn && second.Withdrawn:
		outcomes[market.Selections[0].ID] = store.OutcomeVoid
		outcomes[market.Selections[1].ID] = store.OutcomeVoid
	case first.Withdrawn:
		// A matchup with only one runner is not a matchup.
		outcomes[market.Selections[0].ID] = store.OutcomeVoid
		outcomes[market.Selections[1].ID] = store.OutcomeVoid
	case second.Withdrawn:
		outcomes[market.Selections[0].ID] = store.OutcomeVoid
		outcomes[market.Selections[1].ID] = store.OutcomeVoid
	default:
		firstAhead := betterPosition(first.FinishPosition, second.FinishPosition)
		switch firstAhead {
		case 0:
			outcomes[market.Selections[0].ID] = store.OutcomeVoid
			outcomes[market.Selections[1].ID] = store.OutcomeVoid
		case 1:
			outcomes[market.Selections[0].ID] = store.OutcomeWon
			outcomes[market.Selections[1].ID] = store.OutcomeLost
		default:
			outcomes[market.Selections[0].ID] = store.OutcomeLost
			outcomes[market.Selections[1].ID] = store.OutcomeWon
		}
	}
	return outcomes, nil
}

// betterPosition returns 1 when a finished ahead, 2 when b did, 0 when they
// cannot be separated. A competitor who did not finish is behind one who did.
func betterPosition(a, b int) int {
	switch {
	case a < 1 && b < 1:
		return 0
	case a < 1:
		return 2
	case b < 1:
		return 1
	case a < b:
		return 1
	case b < a:
		return 2
	default:
		return 0
	}
}

func wonIf(condition bool) string {
	if condition {
		return store.OutcomeWon
	}
	return store.OutcomeLost
}
