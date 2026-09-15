package slots

import (
	"fmt"
	"sort"
	"sync"
)

// This file is the floor: the actual machines, written out as par sheets.
//
// Each game is a plain specification. Nothing here reaches for the player's
// balance, their session history or the time of day, because there is no
// mechanism by which it could — Play takes a game, a seed and a stake, and
// that is all it takes. A machine that pays worse to a player who is ahead
// would have to be a different machine.

// standard symbol faces reused across the five-reel games.
func card(code, name, glyph string) Symbol { return Symbol{Code: code, Name: name, Glyph: glyph} }
func wildFace(code, name, glyph string) Symbol {
	return Symbol{Code: code, Name: name, Glyph: glyph, Wild: true}
}
func scatterFace(code, name, glyph string) Symbol {
	return Symbol{Code: code, Name: name, Glyph: glyph, Scatter: true}
}
func blankFace(code, name, glyph string) Symbol {
	return Symbol{Code: code, Name: name, Glyph: glyph, Blank: true}
}

// stakeLadder builds the stake buttons: every level is a whole number of
// hundredths per unit bet, which is what keeps wins from being rounded.
func stakeLadder(units int, multiples ...int64) []int64 {
	out := make([]int64, 0, len(multiples))
	for _, m := range multiples {
		out = append(out, m*int64(units)*100)
	}
	return out
}

// ---------------------------------------------------------------------------
// Satoshi Sevens — three reels, five lines, no features, big swings
// ---------------------------------------------------------------------------

func satoshiSevens() *Game {
	b := newBuilder(
		card("7", "Lucky Seven", "7\ufe0f\u20e3"),
		card("B3", "Triple Bar", "\U0001f36b"),
		card("B2", "Double Bar", "\U0001f36c"),
		card("B1", "Single Bar", "\U0001f36a"),
		card("CH", "Cherry", "\U0001f352"),
		wildFace("W", "Satoshi", "\u20bf"),
		blankFace("-", "Blank", ""),
	)
	return &Game{
		Key:        "satoshi-sevens",
		Name:       "Satoshi Sevens",
		Blurb:      "Three reels, five lines and nowhere to hide. The old machine, priced in the open.",
		Volatility: "High",
		Symbols:    b.symbols,
		Rows:       3,
		Reels: [][]SymbolID{
			b.strip("7 - CH - B1 - B2 - CH - B3 - B1 - CH - B2 - W - B1 - CH - B2 - B1 - CH -"),
			b.strip("- 7 - CH - B1 - B2 - CH - B1 - B3 - CH - B2 - W - B1 - CH - B2 - B1 - CH"),
			b.strip("- CH - 7 - B1 - CH - B2 - B1 - B3 - CH - B2 - B1 - W - CH - B2 - B1 - CH"),
		},
		Lines: lines(
			[]int{1, 1, 1},
			[]int{0, 0, 0},
			[]int{2, 2, 2},
			[]int{0, 1, 2},
			[]int{2, 1, 0},
		),
		PayX100: b.pays(map[string][]int64{
			//      0  1  2      3
			"7":  {0, 0, 0, 100000},
			"B3": {0, 0, 0, 26000},
			"B2": {0, 0, 0, 8800},
			"B1": {0, 0, 0, 4300},
			"CH": {0, 0, 200, 1600},
		}),
		StakeLevels: stakeLadder(5, 20, 60, 200, 600, 2000, 6000),
	}
}

// ---------------------------------------------------------------------------
// Bitcoin Bonanza — five reels, twenty lines, wilds, scatters, free spins
// ---------------------------------------------------------------------------

func bitcoinBonanza() *Game {
	b := newBuilder(
		card("BTC", "Bitcoin", "₿"),
		card("BAR", "Gold Bar", "🪙"),
		card("CHP", "Chip Stack", "🎰"),
		card("KEY", "Cold Key", "🔑"),
		card("A", "Ace", "A"),
		card("K", "King", "K"),
		card("Q", "Queen", "Q"),
		card("J", "Jack", "J"),
		card("T", "Ten", "10"),
		wildFace("W", "Vault Wild", "🧱"),
		scatterFace("S", "Free Spin", "⭐"),
	)
	pays := b.pays(map[string][]int64{
		// Multipliers of the line bet, in hundredths, by how many the line
		// shows. Only Bitcoin pays for two.
		//        0  1  2    3      4      5
		"BTC": {0, 0, 550, 11000, 55000, 220000},
		"BAR": {0, 0, 0, 4400, 22000, 88000},
		"CHP": {0, 0, 0, 2750, 11000, 55000},
		"KEY": {0, 0, 0, 2200, 8800, 44000},
		"A":   {0, 0, 0, 1650, 6600, 27500},
		"K":   {0, 0, 0, 1100, 5500, 22000},
		"Q":   {0, 0, 0, 1100, 4400, 16500},
		"J":   {0, 0, 0, 550, 2750, 11000},
		"T":   {0, 0, 0, 550, 2750, 11000},
	})
	return &Game{
		Key:        "bitcoin-bonanza",
		Name:       "Bitcoin Bonanza",
		Blurb:      "Twenty lines, stacked vault wilds and a free-spin round that pays triple.",
		Volatility: "Medium-high",
		Symbols:    b.symbols,
		Rows:       3,
		Reels: [][]SymbolID{
			b.strip("T J Q K A KEY CHP T J Q S K A BAR T J Q K CHP A KEY J T Q W K A J T BTC Q K T J A CHP Q K J T"),
			b.strip("J Q T K A CHP KEY J T Q K S A T J BAR Q K A J T CHP Q W K J T A Q BTC K J T Q A KEY T J Q K"),
			b.strip("Q T J A K KEY CHP Q J T A K S BAR J Q T K A CHP T J W Q K A T J Q BTC K A J T KEY Q J T A K"),
			b.strip("K J T Q A CHP KEY T J Q A S K BAR J T Q K A CHP J Q T W K A J Q T BTC K J A T KEY Q J K T A"),
			b.strip("A T J Q K CHP KEY J T Q A S K T BAR J Q A K CHP T J Q W A K J T Q BTC A K J T KEY Q T J A K"),
		},
		FreeReels: [][]SymbolID{
			b.strip("T J Q K A KEY CHP W J Q S K A BAR T W Q K CHP A KEY J T Q W K A J T BTC Q K W J A CHP Q K J T"),
			b.strip("J Q T K A CHP KEY J W Q K S A T J BAR Q K A W T CHP Q W K J T A Q BTC K J W Q A KEY T J Q K"),
			b.strip("Q T J A K KEY CHP Q J W A K S BAR J Q T K A CHP T W W Q K A T J Q BTC K A J W KEY Q J T A K"),
			b.strip("K J T Q A CHP KEY T J W A S K BAR J T Q K A CHP J Q W W K A J Q T BTC K J A W KEY Q J K T A"),
			b.strip("A T J Q K CHP KEY J T W A S K T BAR J Q A K CHP T W Q W A K J T Q BTC A K J W KEY Q T J A K"),
		},
		Lines: lines(
			[]int{1, 1, 1, 1, 1},
			[]int{0, 0, 0, 0, 0},
			[]int{2, 2, 2, 2, 2},
			[]int{0, 1, 2, 1, 0},
			[]int{2, 1, 0, 1, 2},
			[]int{0, 0, 1, 2, 2},
			[]int{2, 2, 1, 0, 0},
			[]int{1, 0, 0, 0, 1},
			[]int{1, 2, 2, 2, 1},
			[]int{1, 0, 1, 2, 1},
			[]int{1, 2, 1, 0, 1},
			[]int{0, 1, 1, 1, 0},
			[]int{2, 1, 1, 1, 2},
			[]int{0, 1, 0, 1, 0},
			[]int{2, 1, 2, 1, 2},
			[]int{1, 1, 0, 1, 1},
			[]int{1, 1, 2, 1, 1},
			[]int{0, 0, 1, 0, 0},
			[]int{2, 2, 1, 2, 2},
			[]int{1, 0, 2, 0, 1},
		),
		PayX100: pays,
		//                       0  1  2  3    4     5     6+
		ScatterPayX100:     []int64{0, 0, 0, 200, 1000, 5000, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		FreeSpinsFor:       []int{0, 0, 0, 10, 15, 25, 25, 25, 25, 25, 25, 25, 25, 25, 25, 25},
		FreeMultiplierX100: 300,
		MaxFreeSpins:       120,
		StakeLevels:        stakeLadder(20, 5, 15, 50, 150, 500, 1500),
	}
}

// ---------------------------------------------------------------------------
// Aztec Vault — twenty-five lines, wilds only in the middle, richer free reels
// ---------------------------------------------------------------------------

func aztecVault() *Game {
	b := newBuilder(
		card("IDL", "Sun Idol", "🗿"),
		card("MSK", "Jade Mask", "😷"),
		card("SNK", "Feathered Snake", "🐍"),
		card("JAG", "Jaguar", "🐆"),
		card("A", "Ace", "A"),
		card("K", "King", "K"),
		card("Q", "Queen", "Q"),
		card("J", "Jack", "J"),
		wildFace("W", "Vault Door", "🚪"),
		scatterFace("S", "Sun Stone", "🌞"),
	)
	pays := b.pays(map[string][]int64{
		//        0  1  2    3      4      5
		"IDL": {0, 0, 300, 6000, 24000, 88000},
		"MSK": {0, 0, 0, 2400, 9000, 35000},
		"SNK": {0, 0, 0, 1500, 6000, 24000},
		"JAG": {0, 0, 0, 1200, 4800, 18000},
		"A":   {0, 0, 0, 900, 3000, 12000},
		"K":   {0, 0, 0, 600, 2400, 9000},
		"Q":   {0, 0, 0, 450, 1800, 6000},
		"J":   {0, 0, 0, 420, 1500, 4800},
	})
	return &Game{
		Key:        "aztec-vault",
		Name:       "Aztec Vault",
		Blurb:      "The door only opens on the middle three reels. Free spins swap in strips thick with wilds.",
		Volatility: "Medium",
		Symbols:    b.symbols,
		Rows:       3,
		Reels: [][]SymbolID{
			// No wild on reels one and five: the door is in the middle of the
			// vault, which is why the top line is worth what it is.
			b.strip("J Q K A JAG SNK J Q K MSK A J S Q K JAG A J Q IDL K A J SNK Q K J A JAG Q K J MSK A Q J K"),
			b.strip("Q J K A SNK JAG Q J W K A MSK J Q S K A J Q W JAG K A J IDL Q K J A SNK Q W K J MSK A Q K J"),
			b.strip("K J Q A JAG SNK K Q W J A MSK Q K S J A Q K W JAG A Q K MSK J A Q IDL SNK K W J Q A K J Q"),
			b.strip("A J Q K SNK JAG A K W Q J MSK K A S Q J K A W JAG J Q K IDL A Q J K SNK A W Q J MSK K A J Q"),
			b.strip("J K Q A JAG SNK Q J K A MSK J Q S K A J Q K JAG A J IDL Q K J A SNK K Q J A MSK Q J K A Q J"),
		},
		FreeReels: [][]SymbolID{
			b.strip("J Q K A JAG SNK J Q K MSK A J S Q K JAG A J Q IDL K A J SNK Q K J A JAG Q K J MSK A Q J K"),
			b.strip("Q J K W A SNK JAG Q J W K A MSK J W S K A J W JAG K A W IDL Q K J W SNK Q W K J MSK W Q K J"),
			b.strip("K J W A JAG SNK K Q W J A MSK W K S J A W K W JAG A Q W MSK J A W IDL SNK K W J W A K J Q"),
			b.strip("A J W K SNK JAG A K W Q J MSK K W S Q J W A W JAG J Q W IDL A Q W K SNK A W Q J MSK W A J Q"),
			b.strip("J K Q A JAG SNK Q J K A MSK J Q S K A J Q K JAG A J IDL Q K J A SNK K Q J A MSK Q J K A Q J"),
		},
		Lines: lines(
			[]int{1, 1, 1, 1, 1},
			[]int{0, 0, 0, 0, 0},
			[]int{2, 2, 2, 2, 2},
			[]int{0, 1, 2, 1, 0},
			[]int{2, 1, 0, 1, 2},
			[]int{0, 0, 1, 2, 2},
			[]int{2, 2, 1, 0, 0},
			[]int{1, 0, 0, 0, 1},
			[]int{1, 2, 2, 2, 1},
			[]int{1, 0, 1, 2, 1},
			[]int{1, 2, 1, 0, 1},
			[]int{0, 1, 1, 1, 0},
			[]int{2, 1, 1, 1, 2},
			[]int{0, 1, 0, 1, 0},
			[]int{2, 1, 2, 1, 2},
			[]int{1, 1, 0, 1, 1},
			[]int{1, 1, 2, 1, 1},
			[]int{0, 0, 1, 0, 0},
			[]int{2, 2, 1, 2, 2},
			[]int{1, 0, 2, 0, 1},
			[]int{0, 2, 0, 2, 0},
			[]int{2, 0, 2, 0, 2},
			[]int{0, 2, 2, 2, 0},
			[]int{2, 0, 0, 0, 2},
			[]int{1, 2, 0, 2, 1},
		),
		PayX100:            pays,
		ScatterPayX100:     []int64{0, 0, 0, 100, 500, 2500, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		FreeSpinsFor:       []int{0, 0, 0, 12, 18, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
		FreeMultiplierX100: 200,
		MaxFreeSpins:       150,
		StakeLevels:        stakeLadder(25, 4, 12, 40, 120, 400, 1200),
	}
}

// ---------------------------------------------------------------------------
// Lightning Ways — 1024 ways across five reels of four
// ---------------------------------------------------------------------------

func lightningWays() *Game {
	b := newBuilder(
		card("BLT", "Lightning Bolt", "\u26a1"),
		card("NOD", "Node", "\U0001f5a5\ufe0f"),
		card("SAT", "Satellite", "\U0001f6f0\ufe0f"),
		card("PLG", "Plug", "\U0001f50c"),
		card("A", "Ace", "A"),
		card("K", "King", "K"),
		card("Q", "Queen", "Q"),
		card("J", "Jack", "J"),
		card("T", "Ten", "10"),
		card("N9", "Nine", "9"),
		wildFace("W", "Surge Wild", "\U0001f329\ufe0f"),
		scatterFace("S", "Channel", "\U0001f517"),
	)
	pays := b.pays(map[string][]int64{
		// Only the bolt pays for two. With four rows and 1024 ways, a low
		// symbol paying from two would land on almost every spin, and a
		// machine that pays on every spin is mostly paying you less than you
		// staked while telling you that you won.
		//        0  1  2    3     4     5
		"BLT": {0, 0, 360, 2050, 8200, 35000},
		"NOD": {0, 0, 0, 1130, 4100, 16400},
		"SAT": {0, 0, 0, 700, 2770, 11300},
		"PLG": {0, 0, 0, 565, 2250, 8200},
		"A":   {0, 0, 0, 275, 1130, 3500},
		"K":   {0, 0, 0, 225, 820, 2770},
		"Q":   {0, 0, 0, 165, 700, 2250},
		"J":   {0, 0, 0, 145, 565, 1640},
		"T":   {0, 0, 0, 145, 565, 1640},
		"N9":  {0, 0, 0, 145, 490, 1435},
	})
	return &Game{
		Key:        "lightning-ways",
		Name:       "Lightning Ways",
		Blurb:      "No paylines at all \u2014 1024 ways, left to right, and every symbol pays at once.",
		Volatility: "Medium-high",
		Symbols:    b.symbols,
		Rows:       4,
		Ways:       true,
		WaysUnits:  40,
		// Fifty-five stops a reel with a single channel on each, so a reel can
		// never show two. Three, four and five channels therefore mean three,
		// four and five reels, which is what the paytable says they mean.
		Reels: [][]SymbolID{
			b.strip("A K A PLG NOD K S N9 K SAT N9 SAT PLG T SAT Q K A N9 J Q N9 Q NOD K PLG T NOD BLT J NOD N9 T BLT T Q SAT SAT PLG A N9 BLT T Q K J J A N9 J J Q T PLG T"),
			b.strip("N9 A PLG J N9 T A Q J T S NOD Q N9 A NOD T SAT T Q PLG PLG SAT W BLT T NOD A SAT N9 SAT NOD K J PLG T Q N9 J K J BLT W N9 A BLT K K W K J Q Q PLG SAT"),
			b.strip("Q A N9 K K A S PLG PLG J A NOD J K BLT T Q PLG Q T NOD SAT J T NOD SAT Q A BLT N9 K T W Q K W W T SAT SAT J SAT N9 Q PLG N9 N9 N9 NOD A W J T J PLG"),
			b.strip("BLT Q A J N9 J PLG J K PLG NOD T K W W T PLG T NOD A T K PLG N9 K BLT N9 K NOD J Q BLT Q T N9 A S N9 NOD Q SAT A Q SAT SAT J SAT J N9 T Q PLG A W SAT"),
			b.strip("T Q Q A T K J K NOD K S PLG J BLT N9 Q Q K NOD T SAT A PLG N9 T PLG J BLT J N9 N9 T PLG N9 J NOD SAT Q Q T SAT J BLT N9 K A A SAT T K PLG N9 NOD A SAT"),
		},
		// The free strips carry more surge wilds and fewer nines.
		FreeReels: [][]SymbolID{
			b.strip("T PLG J K K Q S J PLG Q BLT T NOD A PLG J K N9 BLT BLT N9 T NOD SAT Q BLT J T NOD NOD Q PLG A SAT SAT K PLG N9 J Q Q T A SAT A A N9 NOD J T K N9 T K SAT"),
			b.strip("K NOD SAT SAT A J W J K PLG S A SAT T BLT J NOD Q A K T SAT N9 SAT Q W T K PLG BLT PLG PLG W W A A Q J T PLG J NOD NOD N9 K T W N9 BLT N9 NOD Q N9 Q BLT"),
			b.strip("SAT N9 BLT A NOD A T N9 S K K W W N9 W J NOD SAT W N9 J PLG J A A PLG W Q J N9 K A NOD BLT J Q NOD BLT T Q K PLG PLG T Q T SAT NOD SAT PLG Q W T SAT K"),
			b.strip("BLT K K S K Q Q T SAT T K PLG A J BLT W SAT SAT T J NOD NOD BLT J N9 W A NOD BLT N9 PLG SAT SAT W N9 A J T T NOD PLG Q NOD PLG A PLG Q A N9 W N9 Q K J W"),
			b.strip("NOD K N9 NOD A A PLG S N9 K N9 Q K Q A N9 J A BLT SAT PLG K N9 T J J T PLG BLT T J SAT T PLG K T BLT T PLG K Q NOD Q T J NOD SAT J Q SAT SAT A NOD BLT Q"),
		},
		PayX100: pays,
		//                     0  1  2   3     4     5   and beyond
		ScatterPayX100: []int64{0, 0, 0, 200, 1000, 5000, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		FreeSpinsFor: []int{0, 0, 0, 15, 20, 30, 30, 30, 30, 30, 30,
			30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
		FreeMultiplierX100: 300,
		MaxFreeSpins:       150,
		StakeLevels:        stakeLadder(40, 3, 8, 25, 75, 250, 750),
	}
}

// ---------------------------------------------------------------------------
// The registry
// ---------------------------------------------------------------------------

var (
	registryOnce sync.Once
	registry     map[string]*Game
	order        []string
	mathsCache   map[string]Maths
	registryErr  error
)

func load() {
	registry = make(map[string]*Game)
	mathsCache = make(map[string]Maths)
	for _, build := range []func() *Game{satoshiSevens, bitcoinBonanza, aztecVault, lightningWays} {
		game := build()
		if err := game.Validate(); err != nil {
			registryErr = err
			return
		}
		if _, clash := registry[game.Key]; clash {
			registryErr = fmt.Errorf("slots: two games share the key %q", game.Key)
			return
		}
		maths, err := Analyse(game)
		if err != nil {
			registryErr = err
			return
		}
		registry[game.Key] = game
		mathsCache[game.Key] = maths
		order = append(order, game.Key)
	}
	sort.Strings(order)
}

// Load prepares the game floor, validating every machine and computing its
// return. It is called once; a game that does not add up stops the server
// rather than going live unpriced.
func Load() error {
	registryOnce.Do(load)
	return registryErr
}

// Lookup returns a game by key.
func Lookup(key string) (*Game, bool) {
	if Load() != nil {
		return nil, false
	}
	game, ok := registry[key]
	return game, ok
}

// MathsFor returns a game's precomputed return figures.
func MathsFor(key string) (Maths, bool) {
	if Load() != nil {
		return Maths{}, false
	}
	maths, ok := mathsCache[key]
	return maths, ok
}

// All returns every game in a stable order.
func All() []*Game {
	if Load() != nil {
		return nil
	}
	out := make([]*Game, 0, len(order))
	for _, key := range order {
		out = append(out, registry[key])
	}
	return out
}
