-- Bonuses, comps and manual adjustments (SQLite).
--
-- Bonus money is not the same thing as customer cash and is deliberately kept
-- in its own ledger account. Only `user_cash` is withdrawable; a bonus becomes
-- cash by meeting its wagering requirement, and never any other way.

-- ---------------------------------------------------------------------------
-- Two new ledger accounts
-- ---------------------------------------------------------------------------
-- SQLite cannot alter a CHECK constraint, so the table is rebuilt. This is the
-- documented 12-step rebuild, minus the steps that only apply to tables other
-- rows point at: nothing has a foreign key onto ledger_entries, so copying it
-- and swapping the name is safe.
--
-- Every row is carried across, ids included: a ledger is append-only history
-- and losing or renumbering an entry would break the audit trail it exists to
-- provide.
CREATE TABLE ledger_entries_v2 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    txn_id          INTEGER NOT NULL REFERENCES ledger_txns(id) ON DELETE CASCADE,
    account         TEXT    NOT NULL
                            CHECK (account IN ('user_cash', 'user_bonus', 'bet_escrow',
                                               'house_revenue', 'house_fees',
                                               'house_promotions', 'deposit_suspense',
                                               'withdrawal_suspense', 'external_bitcoin')),
    user_id         INTEGER REFERENCES users(id) ON DELETE RESTRICT,
    amount_sat      INTEGER NOT NULL,
    at              TEXT    NOT NULL,
    -- Bonus balances are per customer, so they carry a user_id for the same
    -- reason cash does: money that cannot be attributed to somebody is not
    -- money, it is a reconciliation break waiting to happen.
    CHECK ((account IN ('user_cash', 'user_bonus') AND user_id IS NOT NULL)
        OR (account NOT IN ('user_cash', 'user_bonus')))
);

INSERT INTO ledger_entries_v2 (id, txn_id, account, user_id, amount_sat, at)
    SELECT id, txn_id, account, user_id, amount_sat, at FROM ledger_entries;

DROP TABLE ledger_entries;

ALTER TABLE ledger_entries_v2 RENAME TO ledger_entries;

CREATE INDEX IF NOT EXISTS idx_ledger_entries_txn ON ledger_entries(txn_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_user ON ledger_entries(user_id, account);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account ON ledger_entries(account);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_user_account
    ON ledger_entries(user_id, account, id);

-- ---------------------------------------------------------------------------
-- Offers: reusable templates a bonus can be granted from
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bonus_offers (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    code                TEXT    NOT NULL UNIQUE,
    name                TEXT    NOT NULL,
    description         TEXT    NOT NULL DEFAULT '',
    kind                TEXT    NOT NULL
                                CHECK (kind IN ('deposit_match', 'free_credit', 'comp', 'test_credit')),
    -- A fixed award, or a percentage of a qualifying deposit, or both with the
    -- percentage capped by max_amount_sat.
    amount_sat          INTEGER NOT NULL DEFAULT 0 CHECK (amount_sat >= 0),
    match_bps           INTEGER NOT NULL DEFAULT 0 CHECK (match_bps >= 0),
    max_amount_sat      INTEGER NOT NULL DEFAULT 0 CHECK (max_amount_sat >= 0),
    min_deposit_sat     INTEGER NOT NULL DEFAULT 0 CHECK (min_deposit_sat >= 0),
    -- Wagering multiplier x100: 500 means the bonus must be staked 5 times.
    wagering_x100       INTEGER NOT NULL DEFAULT 0 CHECK (wagering_x100 >= 0),
    -- Stakes at shorter prices than this do not count toward wagering.
    min_odds_milli      INTEGER NOT NULL DEFAULT 0,
    valid_days          INTEGER NOT NULL DEFAULT 30 CHECK (valid_days > 0),
    max_per_user        INTEGER NOT NULL DEFAULT 1 CHECK (max_per_user > 0),
    active              INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT    NOT NULL,
    created_by          INTEGER REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_bonus_offers_active ON bonus_offers(active);

-- ---------------------------------------------------------------------------
-- Grants: a bonus actually awarded to one customer
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bonus_grants (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id                 INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    offer_id                INTEGER REFERENCES bonus_offers(id) ON DELETE SET NULL,
    code                    TEXT    NOT NULL DEFAULT '',
    kind                    TEXT    NOT NULL
                                    CHECK (kind IN ('deposit_match', 'free_credit', 'comp', 'test_credit')),
    amount_sat              INTEGER NOT NULL CHECK (amount_sat > 0),
    wagering_required_sat   INTEGER NOT NULL DEFAULT 0 CHECK (wagering_required_sat >= 0),
    wagering_done_sat       INTEGER NOT NULL DEFAULT 0 CHECK (wagering_done_sat >= 0),
    min_odds_milli          INTEGER NOT NULL DEFAULT 0,
    status                  TEXT    NOT NULL DEFAULT 'active'
                                    CHECK (status IN ('active', 'completed', 'forfeited', 'expired', 'cancelled')),
    -- Test money never coexists with real money in production; the service
    -- refuses to grant it there and this column makes any that exists obvious.
    is_test                 INTEGER NOT NULL DEFAULT 0,
    note                    TEXT    NOT NULL DEFAULT '',
    granted_by              INTEGER REFERENCES users(id) ON DELETE SET NULL,
    granted_at              TEXT    NOT NULL,
    expires_at              TEXT    NOT NULL,
    closed_at               TEXT,
    close_reason            TEXT    NOT NULL DEFAULT '',
    ledger_txn_id           INTEGER REFERENCES ledger_txns(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_bonus_grants_user ON bonus_grants(user_id, status);
CREATE INDEX IF NOT EXISTS idx_bonus_grants_status ON bonus_grants(status, expires_at);

-- Every stake that moved a wagering requirement, so a customer can be shown
-- exactly why their progress reads what it does.
--
-- ref_id is the id of whatever produced the stake within `source`: a bet for
-- the sportsbook, a hand for poker, a spin for slots. It carries no foreign
-- key precisely because those live in different tables; the unique index on
-- (grant_id, source, ref_id) is what stops one stake being counted twice.
CREATE TABLE IF NOT EXISTS bonus_wagering (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    grant_id            INTEGER NOT NULL REFERENCES bonus_grants(id) ON DELETE CASCADE,
    ref_id              INTEGER,
    source              TEXT    NOT NULL DEFAULT 'sportsbook',
    stake_sat           INTEGER NOT NULL,
    contribution_sat    INTEGER NOT NULL,
    at                  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bonus_wagering_grant ON bonus_wagering(grant_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bonus_wagering_once
    ON bonus_wagering(grant_id, source, ref_id);

-- ---------------------------------------------------------------------------
-- Manual cash adjustments
-- ---------------------------------------------------------------------------
-- Real money moved by a human. Anything at or above the configured threshold
-- needs a second person, and nobody can approve their own request.
CREATE TABLE IF NOT EXISTS manual_adjustments (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    amount_sat      INTEGER NOT NULL CHECK (amount_sat <> 0),
    reason          TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'applied', 'rejected')),
    requested_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    requested_at    TEXT    NOT NULL,
    decided_by      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    decided_at      TEXT,
    decision_note   TEXT    NOT NULL DEFAULT '',
    ledger_txn_id   INTEGER REFERENCES ledger_txns(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_manual_adjustments_status ON manual_adjustments(status, requested_at);
CREATE INDEX IF NOT EXISTS idx_manual_adjustments_user ON manual_adjustments(user_id);
