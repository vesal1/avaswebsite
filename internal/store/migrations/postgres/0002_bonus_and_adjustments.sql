-- Bonuses, comps and manual adjustments (PostgreSQL).
--
-- Bonus money is not the same thing as customer cash and is deliberately kept
-- in its own ledger account. Only `user_cash` is withdrawable; a bonus becomes
-- cash by meeting its wagering requirement, and never any other way.

-- ---------------------------------------------------------------------------
-- Two new ledger accounts
-- ---------------------------------------------------------------------------
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_account_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_account_check
    CHECK (account IN ('user_cash', 'user_bonus', 'bet_escrow', 'house_revenue',
                       'house_fees', 'house_promotions', 'deposit_suspense',
                       'withdrawal_suspense', 'external_bitcoin'));

-- Bonus balances are per customer, so they carry a user_id for the same reason
-- cash does: money that cannot be attributed to somebody is not money, it is a
-- reconciliation break waiting to happen.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_attributable
    CHECK ((account IN ('user_cash', 'user_bonus') AND user_id IS NOT NULL)
        OR (account NOT IN ('user_cash', 'user_bonus')));

CREATE INDEX IF NOT EXISTS idx_ledger_entries_user_account
    ON ledger_entries(user_id, account, id);

-- ---------------------------------------------------------------------------
-- Offers: reusable templates a bonus can be granted from
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bonus_offers (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code                TEXT    NOT NULL UNIQUE,
    name                TEXT    NOT NULL,
    description         TEXT    NOT NULL DEFAULT '',
    kind                TEXT    NOT NULL
                                CHECK (kind IN ('deposit_match', 'free_credit', 'comp', 'test_credit')),
    -- A fixed award, or a percentage of a qualifying deposit, or both with the
    -- percentage capped by max_amount_sat.
    amount_sat          BIGINT  NOT NULL DEFAULT 0 CHECK (amount_sat >= 0),
    match_bps           BIGINT  NOT NULL DEFAULT 0 CHECK (match_bps >= 0),
    max_amount_sat      BIGINT  NOT NULL DEFAULT 0 CHECK (max_amount_sat >= 0),
    min_deposit_sat     BIGINT  NOT NULL DEFAULT 0 CHECK (min_deposit_sat >= 0),
    -- Wagering multiplier x100: 500 means the bonus must be staked 5 times.
    wagering_x100       BIGINT  NOT NULL DEFAULT 0 CHECK (wagering_x100 >= 0),
    -- Stakes at shorter prices than this do not count toward wagering.
    min_odds_milli      BIGINT  NOT NULL DEFAULT 0,
    valid_days          BIGINT  NOT NULL DEFAULT 30 CHECK (valid_days > 0),
    max_per_user        BIGINT  NOT NULL DEFAULT 1 CHECK (max_per_user > 0),
    active              BIGINT  NOT NULL DEFAULT 1,
    created_at          TEXT    NOT NULL,
    created_by          BIGINT  REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_bonus_offers_active ON bonus_offers(active);

-- ---------------------------------------------------------------------------
-- Grants: a bonus actually awarded to one customer
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bonus_grants (
    id                      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id                 BIGINT  NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    offer_id                BIGINT  REFERENCES bonus_offers(id) ON DELETE SET NULL,
    code                    TEXT    NOT NULL DEFAULT '',
    kind                    TEXT    NOT NULL
                                    CHECK (kind IN ('deposit_match', 'free_credit', 'comp', 'test_credit')),
    amount_sat              BIGINT  NOT NULL CHECK (amount_sat > 0),
    wagering_required_sat   BIGINT  NOT NULL DEFAULT 0 CHECK (wagering_required_sat >= 0),
    wagering_done_sat       BIGINT  NOT NULL DEFAULT 0 CHECK (wagering_done_sat >= 0),
    min_odds_milli          BIGINT  NOT NULL DEFAULT 0,
    status                  TEXT    NOT NULL DEFAULT 'active'
                                    CHECK (status IN ('active', 'completed', 'forfeited', 'expired', 'cancelled')),
    -- Test money never coexists with real money in production; the service
    -- refuses to grant it there and this column makes any that exists obvious.
    is_test                 BIGINT  NOT NULL DEFAULT 0,
    note                    TEXT    NOT NULL DEFAULT '',
    granted_by              BIGINT  REFERENCES users(id) ON DELETE SET NULL,
    granted_at              TEXT    NOT NULL,
    expires_at              TEXT    NOT NULL,
    closed_at               TEXT,
    close_reason            TEXT    NOT NULL DEFAULT '',
    ledger_txn_id           BIGINT  REFERENCES ledger_txns(id) ON DELETE SET NULL
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
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    grant_id            BIGINT  NOT NULL REFERENCES bonus_grants(id) ON DELETE CASCADE,
    ref_id              BIGINT,
    source              TEXT    NOT NULL DEFAULT 'sportsbook',
    stake_sat           BIGINT  NOT NULL,
    contribution_sat    BIGINT  NOT NULL,
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
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id         BIGINT  NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    amount_sat      BIGINT  NOT NULL CHECK (amount_sat <> 0),
    reason          TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'applied', 'rejected')),
    requested_by    BIGINT  REFERENCES users(id) ON DELETE SET NULL,
    requested_at    TEXT    NOT NULL,
    decided_by      BIGINT  REFERENCES users(id) ON DELETE SET NULL,
    decided_at      TEXT,
    decision_note   TEXT    NOT NULL DEFAULT '',
    ledger_txn_id   BIGINT  REFERENCES ledger_txns(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_manual_adjustments_status ON manual_adjustments(status, requested_at);
CREATE INDEX IF NOT EXISTS idx_manual_adjustments_user ON manual_adjustments(user_id);
