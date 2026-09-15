package betting

import (
	"errors"
	"testing"

	"github.com/vesal1/avaswebsite/internal/catalog"
	"github.com/vesal1/avaswebsite/internal/store"
)

const (
	homeID int64 = 10
	awayID int64 = 20
)

var matchParticipants = []store.Participant{
	{ID: homeID, Name: "Home", HomeAway: "home"},
	{ID: awayID, Name: "Away", HomeAway: "away"},
}

func score(home, away int64) Result {
	return Result{Participants: []ParticipantResult{
		{ParticipantID: homeID, Score: home, HasScore: true},
		{ParticipantID: awayID, Score: away, HasScore: true},
	}}
}

func market(kind catalog.MarketKind, lineX100 int64, hasLine bool, selections ...store.Selection) store.Market {
	return store.Market{
		ID: 1, Kind: string(kind), LineX100: lineX100, HasLine: hasLine, Selections: selections,
	}
}

func sel(id int64, code string) store.Selection {
	return store.Selection{ID: id, OutcomeCode: code, OddsMilli: 2000}
}

func football(t *testing.T) catalog.Sport {
	t.Helper()
	sport, ok := catalog.Lookup("football")
	if !ok {
		t.Fatal("football missing from the catalog")
	}
	return sport
}

func grade(t *testing.T, m store.Market, result Result) map[int64]string {
	t.Helper()
	outcomes, err := GradeMarket(football(t), m, matchParticipants, result)
	if err != nil {
		t.Fatalf("GradeMarket: %v", err)
	}
	return outcomes
}

func TestThreeWayResult(t *testing.T) {
	m := market(catalog.KindMoneyline3, 0, false,
		sel(1, "home"), sel(2, "draw"), sel(3, "away"))

	got := grade(t, m, score(2, 1))
	want := map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost, 3: store.OutcomeLost}
	assertOutcomes(t, got, want)

	got = grade(t, m, score(1, 1))
	want = map[int64]string{1: store.OutcomeLost, 2: store.OutcomeWon, 3: store.OutcomeLost}
	assertOutcomes(t, got, want)
}

func TestTwoWayMarketVoidsOnATie(t *testing.T) {
	// A sport priced two-way can still end level through an abandonment or a
	// rule quirk. Losing both sides would be taking money for an outcome that
	// was never offered.
	m := market(catalog.KindMoneyline2, 0, false, sel(1, "home"), sel(2, "away"))
	got := grade(t, m, score(3, 3))
	assertOutcomes(t, got, map[int64]string{1: store.OutcomeVoid, 2: store.OutcomeVoid})
}

func TestDrawNoBetReturnsStakeOnADraw(t *testing.T) {
	m := market(catalog.KindDrawNoBet, 0, false, sel(1, "home"), sel(2, "away"))
	assertOutcomes(t, grade(t, m, score(0, 0)),
		map[int64]string{1: store.OutcomeVoid, 2: store.OutcomeVoid})
	assertOutcomes(t, grade(t, m, score(1, 0)),
		map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost})
}

func TestDoubleChance(t *testing.T) {
	m := market(catalog.KindDoubleChance, 0, false,
		sel(1, "1X"), sel(2, "12"), sel(3, "X2"))

	assertOutcomes(t, grade(t, m, score(2, 0)), map[int64]string{
		1: store.OutcomeWon, 2: store.OutcomeWon, 3: store.OutcomeLost,
	})
	assertOutcomes(t, grade(t, m, score(1, 1)), map[int64]string{
		1: store.OutcomeWon, 2: store.OutcomeLost, 3: store.OutcomeWon,
	})
	assertOutcomes(t, grade(t, m, score(0, 3)), map[int64]string{
		1: store.OutcomeLost, 2: store.OutcomeWon, 3: store.OutcomeWon,
	})
}

func TestWholeLineHandicapCanPush(t *testing.T) {
	// Home -1.00 with a 2-1 win: the adjusted score is level, so the stake
	// comes back on both sides.
	m := market(catalog.KindHandicap, -100, true, sel(1, "home"), sel(2, "away"))
	assertOutcomes(t, grade(t, m, score(2, 1)),
		map[int64]string{1: store.OutcomeVoid, 2: store.OutcomeVoid})

	assertOutcomes(t, grade(t, m, score(3, 1)),
		map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost})
	assertOutcomes(t, grade(t, m, score(1, 1)),
		map[int64]string{1: store.OutcomeLost, 2: store.OutcomeWon})
}

func TestHalfLineHandicapCannotPush(t *testing.T) {
	m := market(catalog.KindHandicap, -50, true, sel(1, "home"), sel(2, "away"))
	for _, result := range []Result{score(1, 0), score(2, 1), score(0, 0), score(0, 1)} {
		for id, outcome := range grade(t, m, result) {
			if outcome == store.OutcomeVoid {
				t.Errorf("selection %d pushed on a half line", id)
			}
		}
	}
}

func TestAsianQuarterLineSplitsTheStake(t *testing.T) {
	// Home -0.25 splits into 0.0 and -0.5.
	m := market(catalog.KindAsianHandicap, -25, true, sel(1, "home"), sel(2, "away"))

	// A draw: the 0.0 half pushes, the -0.5 half loses. Half the stake back.
	assertOutcomes(t, grade(t, m, score(1, 1)), map[int64]string{
		1: store.OutcomeHalfLost, 2: store.OutcomeHalfWon,
	})
	// A home win by one: both halves win outright.
	assertOutcomes(t, grade(t, m, score(2, 1)), map[int64]string{
		1: store.OutcomeWon, 2: store.OutcomeLost,
	})
	// An away win: both halves lose.
	assertOutcomes(t, grade(t, m, score(0, 1)), map[int64]string{
		1: store.OutcomeLost, 2: store.OutcomeWon,
	})
}

func TestAsianThreeQuarterLine(t *testing.T) {
	// Home -0.75 splits into -0.5 and -1.0. A one-goal win wins the -0.5 half
	// and pushes the -1.0 half.
	m := market(catalog.KindAsianHandicap, -75, true, sel(1, "home"), sel(2, "away"))
	assertOutcomes(t, grade(t, m, score(1, 0)), map[int64]string{
		1: store.OutcomeHalfWon, 2: store.OutcomeHalfLost,
	})
	assertOutcomes(t, grade(t, m, score(2, 0)), map[int64]string{
		1: store.OutcomeWon, 2: store.OutcomeLost,
	})
}

func TestTotals(t *testing.T) {
	m := market(catalog.KindTotals, 250, true, sel(1, "over"), sel(2, "under"))
	assertOutcomes(t, grade(t, m, score(2, 1)), map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost})
	assertOutcomes(t, grade(t, m, score(1, 1)), map[int64]string{1: store.OutcomeLost, 2: store.OutcomeWon})

	// A whole line that lands exactly pushes.
	whole := market(catalog.KindTotals, 300, true, sel(1, "over"), sel(2, "under"))
	assertOutcomes(t, grade(t, whole, score(2, 1)),
		map[int64]string{1: store.OutcomeVoid, 2: store.OutcomeVoid})
}

func TestTeamTotals(t *testing.T) {
	m := store.Market{
		ID: 1, Kind: string(catalog.KindTeamTotals), LineX100: 150, HasLine: true,
		SubjectID:  homeID,
		Selections: []store.Selection{sel(1, "over"), sel(2, "under")},
	}
	assertOutcomes(t, grade(t, m, score(2, 5)), map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost})
	assertOutcomes(t, grade(t, m, score(1, 5)), map[int64]string{1: store.OutcomeLost, 2: store.OutcomeWon})
}

func TestBothTeamsToScoreAndOddEven(t *testing.T) {
	btts := market(catalog.KindBothTeamsScore, 0, false, sel(1, "yes"), sel(2, "no"))
	assertOutcomes(t, grade(t, btts, score(1, 2)), map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost})
	assertOutcomes(t, grade(t, btts, score(3, 0)), map[int64]string{1: store.OutcomeLost, 2: store.OutcomeWon})

	parity := market(catalog.KindOddEven, 0, false, sel(1, "odd"), sel(2, "even"))
	assertOutcomes(t, grade(t, parity, score(2, 1)), map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost})
	assertOutcomes(t, grade(t, parity, score(2, 2)), map[int64]string{1: store.OutcomeLost, 2: store.OutcomeWon})
}

func TestCleanSheet(t *testing.T) {
	m := store.Market{
		ID: 1, Kind: string(catalog.KindCleanSheet), SubjectID: homeID,
		Selections: []store.Selection{sel(1, "yes"), sel(2, "no")},
	}
	// The home side keeps a clean sheet when the away side fails to score.
	assertOutcomes(t, grade(t, m, score(0, 0)), map[int64]string{1: store.OutcomeWon, 2: store.OutcomeLost})
	assertOutcomes(t, grade(t, m, score(3, 1)), map[int64]string{1: store.OutcomeLost, 2: store.OutcomeWon})
}

func TestCorrectScore(t *testing.T) {
	m := market(catalog.KindCorrectScore, 0, false,
		sel(1, "2-1"), sel(2, "1-1"), sel(3, "0-0"))
	assertOutcomes(t, grade(t, m, score(2, 1)), map[int64]string{
		1: store.OutcomeWon, 2: store.OutcomeLost, 3: store.OutcomeLost,
	})
}

func TestUnpricedCorrectScoreVoidsTheMarket(t *testing.T) {
	// 7-4 was never on the board. Losing every selection would be charging for
	// an outcome the customer could not have backed.
	m := market(catalog.KindCorrectScore, 0, false, sel(1, "2-1"), sel(2, "1-1"))
	assertOutcomes(t, grade(t, m, score(7, 4)),
		map[int64]string{1: store.OutcomeVoid, 2: store.OutcomeVoid})
}

func TestWinningMarginBands(t *testing.T) {
	m := market(catalog.KindWinningMargin, 0, false,
		sel(1, "home:1-2"), sel(2, "home:3-"), sel(3, "away:1-2"), sel(4, "draw"))

	assertOutcomes(t, grade(t, m, score(3, 1)), map[int64]string{
		1: store.OutcomeWon, 2: store.OutcomeLost, 3: store.OutcomeLost, 4: store.OutcomeLost,
	})
	assertOutcomes(t, grade(t, m, score(6, 0)), map[int64]string{
		1: store.OutcomeLost, 2: store.OutcomeWon, 3: store.OutcomeLost, 4: store.OutcomeLost,
	})
	assertOutcomes(t, grade(t, m, score(2, 2)), map[int64]string{
		1: store.OutcomeLost, 2: store.OutcomeLost, 3: store.OutcomeLost, 4: store.OutcomeWon,
	})
}

func TestAbandonedEventVoidsEverything(t *testing.T) {
	m := market(catalog.KindMoneyline3, 0, false, sel(1, "home"), sel(2, "draw"), sel(3, "away"))
	result := score(3, 0)
	result.Abandoned = true
	assertOutcomes(t, grade(t, m, result), map[int64]string{
		1: store.OutcomeVoid, 2: store.OutcomeVoid, 3: store.OutcomeVoid,
	})
}

func TestMissingScoreIsNotGuessedAt(t *testing.T) {
	m := market(catalog.KindMoneyline3, 0, false, sel(1, "home"), sel(2, "draw"), sel(3, "away"))
	_, err := GradeMarket(football(t), m, matchParticipants, Result{})
	if !errors.Is(err, ErrIncompleteResult) {
		t.Errorf("got %v, want ErrIncompleteResult", err)
	}
}

func TestPropMarketsAreLeftToATrader(t *testing.T) {
	for _, kind := range []catalog.MarketKind{
		catalog.KindMethodOfVictory, catalog.KindFirstScorer, catalog.KindHalfTimeFull,
		catalog.KindPeriodWinner, catalog.KindTossWinner,
	} {
		m := market(kind, 0, false, sel(1, "anything"))
		_, err := GradeMarket(football(t), m, matchParticipants, score(1, 0))
		if !errors.Is(err, ErrManualGrading) {
			t.Errorf("%s: got %v, want ErrManualGrading", kind, err)
		}
	}
}

func TestFieldEventGrading(t *testing.T) {
	sport, _ := catalog.Lookup("horse-racing-flat")
	runners := []store.Participant{
		{ID: 1, Name: "First"}, {ID: 2, Name: "Second"},
		{ID: 3, Name: "Third"}, {ID: 4, Name: "Non-runner"},
	}
	result := Result{Participants: []ParticipantResult{
		{ParticipantID: 1, FinishPosition: 1},
		{ParticipantID: 2, FinishPosition: 2},
		{ParticipantID: 3, FinishPosition: 3},
		{ParticipantID: 4, Withdrawn: true},
	}}

	win := store.Market{ID: 1, Kind: string(catalog.KindOutright), Selections: []store.Selection{
		{ID: 11, ParticipantID: 1}, {ID: 12, ParticipantID: 2},
		{ID: 13, ParticipantID: 3}, {ID: 14, ParticipantID: 4},
	}}
	outcomes, err := GradeMarket(sport, win, runners, result)
	if err != nil {
		t.Fatal(err)
	}
	assertOutcomes(t, outcomes, map[int64]string{
		11: store.OutcomeWon, 12: store.OutcomeLost, 13: store.OutcomeLost,
		14: store.OutcomeVoid, // a non-runner's stake comes back
	})

	place := store.Market{ID: 2, Kind: string(catalog.KindPodium), Selections: []store.Selection{
		{ID: 21, ParticipantID: 1}, {ID: 22, ParticipantID: 3},
	}}
	outcomes, err = GradeMarket(sport, place, runners, result)
	if err != nil {
		t.Fatal(err)
	}
	assertOutcomes(t, outcomes, map[int64]string{21: store.OutcomeWon, 22: store.OutcomeWon})
}

func TestHeadToHeadVoidsWhenARunnerIsWithdrawn(t *testing.T) {
	sport, _ := catalog.Lookup("golf")
	players := []store.Participant{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}}
	m := store.Market{ID: 1, Kind: string(catalog.KindHeadToHead), Selections: []store.Selection{
		{ID: 31, ParticipantID: 1}, {ID: 32, ParticipantID: 2},
	}}

	withdrawn := Result{Participants: []ParticipantResult{
		{ParticipantID: 1, FinishPosition: 4},
		{ParticipantID: 2, Withdrawn: true},
	}}
	outcomes, err := GradeMarket(sport, m, players, withdrawn)
	if err != nil {
		t.Fatal(err)
	}
	assertOutcomes(t, outcomes, map[int64]string{31: store.OutcomeVoid, 32: store.OutcomeVoid})

	played := Result{Participants: []ParticipantResult{
		{ParticipantID: 1, FinishPosition: 4},
		{ParticipantID: 2, FinishPosition: 9},
	}}
	outcomes, err = GradeMarket(sport, m, players, played)
	if err != nil {
		t.Fatal(err)
	}
	assertOutcomes(t, outcomes, map[int64]string{31: store.OutcomeWon, 32: store.OutcomeLost})
}

func assertOutcomes(t *testing.T, got, want map[int64]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("graded %d selections, want %d: %v", len(got), len(want), got)
	}
	for id, expected := range want {
		if got[id] != expected {
			t.Errorf("selection %d graded %q, want %q", id, got[id], expected)
		}
	}
}
