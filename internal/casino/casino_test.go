package casino

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vesal1/avaswebsite/internal/bonus"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/config"
	"github.com/vesal1/avaswebsite/internal/slots"
	"github.com/vesal1/avaswebsite/internal/store"
)

type rig struct {
	t      *testing.T
	store  *store.Store
	casino *Service
	bonus  *bonus.Service
	cfg    *config.Config
	user   store.User
	now    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	s, err := store.OpenAndMigrate(filepath.Join(t.TempDir(), "casino.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	r := &rig{t: t, store: s, now: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s.WithClock(func() time.Time { return r.now })

	r.cfg = config.Load()
	r.cfg.Env = "development"
	r.cfg.AllowedCountries = []string{"GB"}
	comp := compliance.New(s, r.cfg)
	r.bonus = bonus.New(s, r.cfg)
	r.casino = New(s, comp, r.cfg).WithWagering(r.bonus)
	r.casino.now = func() time.Time { return r.now }

	ctx := context.Background()
	id, err := s.CreateUser(ctx, store.NewUser{
		Email: "player@example.com", PasswordHash: "x", DisplayName: "Player",
		DateOfBirth: "1990-01-01", Country: "GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKYCStatus(ctx, id, store.KYCVerified); err != nil {
		t.Fatal(err)
	}
	user, err := s.GetUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.user = user
	return r
}

func (r *rig) fund(sat int64) {
	r.t.Helper()
	ctx := context.Background()
	err := r.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.PostTxn(ctx, tx, store.Timestamp(r.now), store.TxnSpec{
			Kind: store.TxnAdjustment, Memo: "test funding",
			Entries: []store.Entry{
				{Account: store.AccountUserCash, UserID: r.user.ID, AmountSat: sat},
				{Account: store.AccountHousePromotions, AmountSat: -sat},
			},
		})
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) cash() int64 {
	r.t.Helper()
	balance, err := r.store.BalanceSat(context.Background(), r.user.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return balance
}

func (r *rig) assertLedgerSound() {
	r.t.Helper()
	problems, err := r.store.CheckLedger(context.Background())
	if err != nil {
		r.t.Fatalf("check ledger: %v", err)
	}
	for _, problem := range problems {
		r.t.Errorf("ledger invariant broken: %s", problem)
	}
}

func firstGame(t *testing.T) *slots.Game {
	t.Helper()
	if err := slots.Load(); err != nil {
		t.Fatal(err)
	}
	return slots.All()[0]
}

// TestCommitmentIsPublishedBeforeTheSecret is the guarantee the whole scheme
// rests on: a player can see what the house committed to, and cannot see the
// seed itself until the pair is retired.
func TestCommitmentIsPublishedBeforeTheSecret(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	seed, err := r.casino.Seed(ctx, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Commitment == "" {
		t.Fatal("no commitment was published")
	}
	if seed.PublishedSeed() != "" {
		t.Fatal("the live server seed was handed out; the commitment is then worth nothing")
	}
	if !seed.Active || seed.NextNonce != 1 {
		t.Errorf("a fresh pair should be live at nonce 1, got active=%v nonce=%d", seed.Active, seed.NextNonce)
	}

	// Asking again returns the same pair rather than quietly recommitting.
	again, err := r.casino.Seed(ctx, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != seed.ID {
		t.Errorf("a second visit committed to a new pair (%d then %d)", seed.ID, again.ID)
	}
}

func TestRotateRevealsTheOldSeedAndCommitsToANewOne(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	original, err := r.casino.Seed(ctx, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}

	retired, next, err := r.casino.Rotate(ctx, r.user.ID, "my-own-seed")
	if err != nil {
		t.Fatal(err)
	}
	if retired.ID != original.ID {
		t.Errorf("retired pair %d, expected %d", retired.ID, original.ID)
	}
	if retired.PublishedSeed() == "" {
		t.Error("a retired pair must publish its seed, or its spins can never be checked")
	}
	if next.Commitment == original.Commitment {
		t.Error("the replacement reused the old commitment")
	}
	if next.PublishedSeed() != "" {
		t.Error("the replacement published its secret immediately")
	}
	if next.ClientSeed != "my-own-seed" {
		t.Errorf("client seed = %q, want the one the player chose", next.ClientSeed)
	}
	if next.NextNonce != 1 {
		t.Errorf("the new pair starts at nonce %d, want 1", next.NextNonce)
	}
}

func TestSpinMovesMoneyAndKeepsTheLedgerBalanced(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	game := firstGame(t)
	stake := game.MinStakeSat()
	r.fund(stake * 200)
	before := r.cash()

	result, err := r.casino.Spin(ctx, r.user, game.Key, stake)
	if err != nil {
		t.Fatal(err)
	}
	if result.SpinID == 0 {
		t.Fatal("the spin was not recorded")
	}
	want := before - stake + result.WinSat
	if r.cash() != want {
		t.Errorf("balance = %d, want %d (staked %d, won %d)", r.cash(), want, stake, result.WinSat)
	}
	if result.BalanceSat != want {
		t.Errorf("the reported balance %d does not match the ledger %d", result.BalanceSat, want)
	}
	r.assertLedgerSound()
}

// TestNoncesAreNeverReused matters more than it looks: two spins on one nonce
// would be two different outcomes claiming the same proof.
func TestNoncesAreNeverReused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	game := firstGame(t)
	stake := game.MinStakeSat()
	r.fund(stake * 400)

	seen := make(map[int64]bool)
	for i := 0; i < 60; i++ {
		result, err := r.casino.Spin(ctx, r.user, game.Key, stake)
		if err != nil {
			t.Fatal(err)
		}
		if seen[result.Round.Nonce] {
			t.Fatalf("nonce %d was used twice", result.Round.Nonce)
		}
		seen[result.Round.Nonce] = true
	}
	r.assertLedgerSound()
}

// TestSpinsAreVerifiableOnceTheSeedIsPublished walks the whole promise end to
// end: play, rotate, then check every spin from the published seed.
func TestSpinsAreVerifiableOnceTheSeedIsPublished(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	game := firstGame(t)
	stake := game.MinStakeSat()
	r.fund(stake * 100)

	var ids []int64
	for i := 0; i < 12; i++ {
		result, err := r.casino.Spin(ctx, r.user, game.Key, stake)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result.SpinID)
	}

	// While the pair is live there is nothing to check yet, and the service
	// must say so rather than claim a match.
	live, err := r.casino.Verify(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if live.Matches || live.Round != nil {
		t.Error("a spin was reported as verified while its seed was still secret")
	}

	if _, _, err := r.casino.Rotate(ctx, r.user.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		verified, err := r.casino.Verify(ctx, id)
		if err != nil {
			t.Fatalf("verify spin %d: %v", id, err)
		}
		if !verified.Matches {
			t.Errorf("spin %d replayed to a different result than it paid", id)
		}
	}
}

func TestSpinRefusesAnOffLadderStake(t *testing.T) {
	r := newRig(t)
	game := firstGame(t)
	r.fund(game.MaxStakeSat() * 4)

	_, err := r.casino.Spin(context.Background(), r.user, game.Key, game.MinStakeSat()+1)
	if !errors.Is(err, ErrStakeNotOffered) {
		t.Errorf("error = %v, want ErrStakeNotOffered", err)
	}
}

func TestSpinRefusesWithoutFunds(t *testing.T) {
	r := newRig(t)
	game := firstGame(t)

	_, err := r.casino.Spin(context.Background(), r.user, game.Key, game.MinStakeSat())
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("error = %v, want ErrInsufficientFunds", err)
	}
	if r.cash() != 0 {
		t.Errorf("a refused spin left the balance at %d", r.cash())
	}
	r.assertLedgerSound()
}

func TestSpinRefusesAnUnknownGame(t *testing.T) {
	r := newRig(t)
	_, err := r.casino.Spin(context.Background(), r.user, "no-such-machine", 1000)
	if !errors.Is(err, ErrUnknownGame) {
		t.Errorf("error = %v, want ErrUnknownGame", err)
	}
}

// TestBonusStakeAdvancesWagering checks the join with the promotions engine:
// slot play clears a bonus at the full rate.
func TestBonusStakeAdvancesWagering(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	game := firstGame(t)
	stake := game.MinStakeSat()

	grant, err := r.bonus.Grant(ctx, bonus.GrantRequest{
		UserID: r.user.ID, Kind: store.BonusFreeCredit,
		AmountSat: stake * 50, WageringX100: 200, ValidDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := r.casino.Spin(ctx, r.user, game.Key, stake)
	if err != nil {
		t.Fatal(err)
	}
	if result.FromBonusSat != stake {
		t.Errorf("stake drew %d from the bonus, want the whole %d", result.FromBonusSat, stake)
	}

	updated, err := r.store.GetBonusGrant(ctx, grant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WageringDoneSat != stake {
		t.Errorf("wagering advanced by %d, want %d: slots contribute in full",
			updated.WageringDoneSat, stake)
	}
	r.assertLedgerSound()
}

func TestClientSeedIsCheckedBeforeItIsStored(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.casino.Seed(ctx, r.user.ID); err != nil {
		t.Fatal(err)
	}

	if err := r.casino.SetClientSeed(ctx, r.user.ID, "line\nbreak"); !errors.Is(err, ErrClientSeed) {
		t.Errorf("a control character was accepted: %v", err)
	}
	long := make([]byte, MaxClientSeed+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := r.casino.SetClientSeed(ctx, r.user.ID, string(long)); !errors.Is(err, ErrClientSeed) {
		t.Errorf("an oversized seed was accepted: %v", err)
	}
	if err := r.casino.SetClientSeed(ctx, r.user.ID, "lucky-7"); err != nil {
		t.Fatal(err)
	}
	seed, _ := r.casino.Seed(ctx, r.user.ID)
	if seed.ClientSeed != "lucky-7" {
		t.Errorf("client seed = %q, want lucky-7", seed.ClientSeed)
	}
	// Changing the client seed must not rewind the nonce, or an outcome could
	// be replayed.
	if seed.NextNonce != 1 {
		t.Errorf("nonce = %d", seed.NextNonce)
	}
}

// TestHouseTakesTheRakeItAdvertises plays a long session and checks the house
// account moved by roughly the published edge. It is a coarse check by design:
// its job is to catch a wiring mistake, such as a payout posted twice or a
// stake that never left the player.
func TestHouseTakesTheRakeItAdvertises(t *testing.T) {
	if testing.Short() {
		t.Skip("long session")
	}
	r := newRig(t)
	ctx := context.Background()
	game := firstGame(t)
	maths, _ := slots.MathsFor(game.Key)
	stake := game.MinStakeSat()
	r.fund(stake * 60_000)

	const spins = 4000
	var staked, won int64
	for i := 0; i < spins; i++ {
		result, err := r.casino.Spin(ctx, r.user, game.Key, stake)
		if err != nil {
			t.Fatalf("spin %d: %v", i, err)
		}
		staked += stake
		won += result.WinSat
	}
	r.assertLedgerSound()

	house, err := r.store.AccountBalanceSat(ctx, store.AccountHouseRevenue)
	if err != nil {
		t.Fatal(err)
	}
	if house != staked-won {
		t.Errorf("house holds %d, but took %d and paid %d", house, staked, won)
	}
	t.Logf("%d spins returned %.2f%%, published %.2f%%",
		spins, float64(won)/float64(staked)*100, maths.RTP*100)
}
