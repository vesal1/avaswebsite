package blackjack

// PublishedRTPBps is the figure printed on the game page: the return of the
// real single-deck engine played under the strategy card on this page, over
// twenty million deterministically seeded rounds, as returned sat per staked
// sat in basis points. rtp_test.go re-runs the identical simulation and fails
// if this constant drifts from what the engine actually does.
//
// It is a measured figure, not the DP's: the DP models an infinite deck to
// derive the card, while the game deals one real deck, which is slightly
// friendlier to the player. The test also checks the two agree to within the
// gap a single deck is known to open.
const PublishedRTPBps = 9_987
