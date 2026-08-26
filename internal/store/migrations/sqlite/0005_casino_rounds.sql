-- Stateful casino rounds (SQLite).
--
-- A slot spin settles in one request; blackjack and Mines do not. A round is
-- opened, the stake moves at once, and the player acts on it over several
-- requests before it settles. The state between requests lives here rather
-- than in memory, so a restart hands a player back their half-played hand
-- instead of their money vanishing into an account nobody can explain.
--
-- The randomness behind a round is committed exactly like a spin's: the seed
-- pair and a claimed nonce fix the deck order or the mine layout before the
-- first action, and the round's whole course is the player's decisions applied
-- to that fixed randomness. The action log is stored in the state so the
-- round can be replayed move for move once the seed is published.

CREATE TABLE IF NOT EXISTS casino_rounds (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         BIGINT  NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    seed_id         BIGINT  NOT NULL REFERENCES casino_seeds(id) ON DELETE RESTRICT,
    game_key        TEXT    NOT NULL,
    -- Copied from the seed pair so the round stays verifiable on its own.
    commitment      TEXT    NOT NULL,
    client_seed     TEXT    NOT NULL,
    nonce           BIGINT  NOT NULL,
    -- The opening stake. Blackjack doubles and splits add to it.
    stake_sat       BIGINT  NOT NULL CHECK (stake_sat > 0),
    win_sat         BIGINT  NOT NULL DEFAULT 0 CHECK (win_sat >= 0),
    status          TEXT    NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open', 'won', 'lost', 'pushed')),
    -- The engine's state as JSON, including the action log.
    state           TEXT    NOT NULL DEFAULT '',
    rtp_bps         BIGINT  NOT NULL DEFAULT 0,
    stake_txn_id    BIGINT  REFERENCES ledger_txns(id) ON DELETE SET NULL,
    payout_txn_id   BIGINT  REFERENCES ledger_txns(id) ON DELETE SET NULL,
    created_at      TEXT    NOT NULL,
    settled_at      TEXT,
    -- A nonce is used once per seed pair, shared with casino_spins through the
    -- same counter, so no two outcomes can ever claim the same proof.
    UNIQUE (seed_id, nonce)
);
-- One open round per player per game, enforced by the database. A second
-- "start" while one is live must resume it, never open a parallel round.
CREATE UNIQUE INDEX IF NOT EXISTS idx_casino_rounds_open
    ON casino_rounds(user_id, game_key) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_casino_rounds_user ON casino_rounds(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_casino_rounds_game ON casino_rounds(game_key, id DESC);
