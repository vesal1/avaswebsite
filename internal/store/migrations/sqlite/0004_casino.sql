-- Casino slot play: the seed pairs behind it and every spin it produced
-- (SQLite).
--
-- A spin is worth storing only if it can be checked afterwards. Each row keeps
-- the commitment that was published before the spin, the client seed and the
-- nonce, so once the server seed is revealed anybody can replay the spin and
-- arrive at the same reels. Without those three, a spin history is just the
-- house telling you what happened.

-- One live seed pair per player. The server seed is committed to when the pair
-- is created and written out only when the player rotates it, which is what
-- makes the commitment worth anything: the outcomes of every spin under this
-- pair were fixed before the player staked on any of them.
CREATE TABLE IF NOT EXISTS casino_seeds (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         BIGINT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- SHA-256 of the server seed, shown to the player before they spin.
    commitment      TEXT    NOT NULL,
    -- The secret itself. It has to live somewhere for the spins to be
    -- computed at all; what makes the commitment mean something is that it is
    -- never handed out while the pair is live. Every read path that reaches a
    -- player blanks it until active drops to 0.
    server_seed     TEXT    NOT NULL,
    -- The player's own contribution, which they may change at any time by
    -- rotating the pair.
    client_seed     TEXT    NOT NULL,
    -- The nonce the next spin will use. Never reused, never rewound.
    next_nonce      BIGINT  NOT NULL DEFAULT 1 CHECK (next_nonce >= 1),
    active          BIGINT  NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL,
    retired_at      TEXT
);
-- At most one live pair per player, enforced by the database rather than by
-- remembering to check.
CREATE UNIQUE INDEX IF NOT EXISTS idx_casino_seeds_live
    ON casino_seeds(user_id) WHERE active = 1;
CREATE INDEX IF NOT EXISTS idx_casino_seeds_user ON casino_seeds(user_id, id DESC);

CREATE TABLE IF NOT EXISTS casino_spins (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         BIGINT  NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    seed_id         BIGINT  NOT NULL REFERENCES casino_seeds(id) ON DELETE RESTRICT,
    game_key        TEXT    NOT NULL,
    -- Copied from the seed pair so a spin stays verifiable even if the pair is
    -- later rotated, and so the row is self-contained for an auditor.
    commitment      TEXT    NOT NULL,
    client_seed     TEXT    NOT NULL,
    nonce           BIGINT  NOT NULL,
    stake_sat       BIGINT  NOT NULL CHECK (stake_sat > 0),
    win_sat         BIGINT  NOT NULL DEFAULT 0 CHECK (win_sat >= 0),
    free_spins      BIGINT  NOT NULL DEFAULT 0 CHECK (free_spins >= 0),
    -- The published return of the machine at the time of the spin, so a change
    -- to a paytable cannot rewrite what a player was offered.
    rtp_bps         BIGINT  NOT NULL DEFAULT 0,
    -- The full round as JSON: reels, wins and every free spin.
    result          TEXT    NOT NULL DEFAULT '',
    stake_txn_id    BIGINT  REFERENCES ledger_txns(id) ON DELETE SET NULL,
    payout_txn_id   BIGINT  REFERENCES ledger_txns(id) ON DELETE SET NULL,
    created_at      TEXT    NOT NULL,
    -- A nonce is used once per seed pair. If this ever collides, two spins
    -- claimed the same outcome and the house has a bug worth stopping for.
    UNIQUE (seed_id, nonce)
);
CREATE INDEX IF NOT EXISTS idx_casino_spins_user ON casino_spins(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_casino_spins_game ON casino_spins(game_key, id DESC);
