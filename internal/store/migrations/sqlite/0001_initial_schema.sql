-- Avas Sportsbook: initial schema (SQLite).
--
-- Conventions that hold throughout:
--   * Every monetary column is an INTEGER count of satoshi. No floats.
--   * Every price column is milli-odds: decimal odds x 1000.
--   * Handicap and total lines are stored x100 so that quarter lines
--     (-0.25, 2.75) are exact integers.
--   * Timestamps are RFC3339 UTC strings, which sort lexicographically.

-- ---------------------------------------------------------------------------
-- Accounts and access
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    email           TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash   TEXT    NOT NULL,
    display_name    TEXT    NOT NULL,
    date_of_birth   TEXT    NOT NULL,               -- YYYY-MM-DD
    country         TEXT    NOT NULL,               -- ISO 3166-1 alpha-2
    role            TEXT    NOT NULL DEFAULT 'customer'
                            CHECK (role IN ('customer', 'trader', 'compliance', 'admin')),
    status          TEXT    NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active', 'suspended', 'closed')),
    kyc_status      TEXT    NOT NULL DEFAULT 'none'
                            CHECK (kyc_status IN ('none', 'pending', 'verified', 'rejected')),
    odds_format     TEXT    NOT NULL DEFAULT 'decimal'
                            CHECK (odds_format IN ('decimal', 'american', 'fractional')),
    created_at      TEXT    NOT NULL,
    last_login_at   TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash      TEXT    PRIMARY KEY,            -- SHA-256 of the cookie value
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at      TEXT    NOT NULL,
    expires_at      TEXT    NOT NULL,
    last_seen_at    TEXT    NOT NULL,
    reality_check_at TEXT   NOT NULL,               -- when the next reality check is due
    ip              TEXT    NOT NULL DEFAULT '',
    user_agent      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS login_attempts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    email           TEXT    NOT NULL COLLATE NOCASE,
    ip              TEXT    NOT NULL DEFAULT '',
    succeeded       INTEGER NOT NULL,
    at              TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_login_attempts ON login_attempts(email, at);

CREATE TABLE IF NOT EXISTS audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    at              TEXT    NOT NULL,
    actor_user_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    subject_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    action          TEXT    NOT NULL,
    detail          TEXT    NOT NULL DEFAULT '',
    ip              TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at);
CREATE INDEX IF NOT EXISTS idx_audit_subject ON audit_log(subject_user_id, at);

-- ---------------------------------------------------------------------------
-- Responsible gambling
-- ---------------------------------------------------------------------------
-- A limit change that loosens protection cannot take effect immediately; it is
-- written with a future effective_from and the tighter value keeps applying
-- until then. Tightening applies at once.
CREATE TABLE IF NOT EXISTS player_limits (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL
                            CHECK (kind IN ('deposit_daily', 'deposit_weekly', 'deposit_monthly',
                                            'loss_daily', 'loss_weekly', 'stake_daily',
                                            'session_minutes')),
    amount          INTEGER NOT NULL,               -- satoshi, or minutes for session_minutes
    effective_from  TEXT    NOT NULL,
    requested_at    TEXT    NOT NULL,
    revoked_at      TEXT
);
CREATE INDEX IF NOT EXISTS idx_limits_user ON player_limits(user_id, kind, effective_from);

CREATE TABLE IF NOT EXISTS exclusions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL CHECK (kind IN ('cool_off', 'self_exclusion')),
    starts_at       TEXT    NOT NULL,
    ends_at         TEXT,                           -- NULL means permanent
    created_at      TEXT    NOT NULL,
    reason          TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_exclusions_user ON exclusions(user_id, starts_at);

CREATE TABLE IF NOT EXISTS kyc_documents (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL CHECK (kind IN ('identity', 'address', 'source_of_funds')),
    reference       TEXT    NOT NULL,               -- external vault reference, never the document
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'accepted', 'rejected')),
    submitted_at    TEXT    NOT NULL,
    reviewed_at     TEXT,
    reviewed_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    note            TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_kyc_user ON kyc_documents(user_id);

-- ---------------------------------------------------------------------------
-- Sporting content
-- ---------------------------------------------------------------------------
-- sport_key is a catalog key compiled into the binary, not a foreign key.
CREATE TABLE IF NOT EXISTS competitions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    sport_key       TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    region          TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    UNIQUE (sport_key, name)
);
CREATE INDEX IF NOT EXISTS idx_competitions_sport ON competitions(sport_key);

CREATE TABLE IF NOT EXISTS events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    sport_key       TEXT    NOT NULL,
    competition_id  INTEGER REFERENCES competitions(id) ON DELETE SET NULL,
    name            TEXT    NOT NULL,
    format          TEXT    NOT NULL CHECK (format IN ('match', 'race', 'tournament')),
    starts_at       TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'scheduled'
                            CHECK (status IN ('scheduled', 'live', 'suspended', 'finished',
                                              'settled', 'cancelled', 'postponed')),
    venue           TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    settled_at      TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_sport_start ON events(sport_key, starts_at);
CREATE INDEX IF NOT EXISTS idx_events_status ON events(status, starts_at);

CREATE TABLE IF NOT EXISTS participants (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    short_name      TEXT    NOT NULL DEFAULT '',
    nationality     TEXT    NOT NULL DEFAULT '',
    -- home_away is meaningful for match events only; 'neutral' elsewhere.
    home_away       TEXT    NOT NULL DEFAULT 'neutral'
                            CHECK (home_away IN ('home', 'away', 'neutral')),
    sort_order      INTEGER NOT NULL DEFAULT 0,
    -- Populated at settlement.
    score           INTEGER,
    finish_position INTEGER,
    withdrawn       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_participants_event ON participants(event_id, sort_order);

CREATE TABLE IF NOT EXISTS markets (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL,               -- catalog.MarketKind
    title           TEXT    NOT NULL,
    -- line_x100 is the handicap or total, scaled by 100. NULL for markets
    -- that do not carry a line.
    line_x100       INTEGER,
    period          TEXT    NOT NULL DEFAULT '',    -- '' means the full contest
    -- Which participant a one-sided market refers to (team totals, clean sheet).
    subject_participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
    status          TEXT    NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open', 'suspended', 'closed', 'settled', 'void')),
    margin_bps      INTEGER NOT NULL DEFAULT 500,
    created_at      TEXT    NOT NULL,
    settled_at      TEXT
);
CREATE INDEX IF NOT EXISTS idx_markets_event ON markets(event_id);
CREATE INDEX IF NOT EXISTS idx_markets_status ON markets(status);

CREATE TABLE IF NOT EXISTS selections (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    market_id       INTEGER NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
    participant_id  INTEGER REFERENCES participants(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    -- outcome_code tells the settlement engine what this selection means,
    -- e.g. 'home', 'draw', 'away', 'over', 'under', 'yes', 'no', '2-1'.
    outcome_code    TEXT    NOT NULL DEFAULT '',
    odds_milli      INTEGER NOT NULL CHECK (odds_milli > 1000),
    status          TEXT    NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open', 'suspended', 'won', 'lost', 'void',
                                              'half_won', 'half_lost')),
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_selections_market ON selections(market_id, sort_order);

CREATE TABLE IF NOT EXISTS odds_history (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    selection_id    INTEGER NOT NULL REFERENCES selections(id) ON DELETE CASCADE,
    odds_milli      INTEGER NOT NULL,
    at              TEXT    NOT NULL,
    reason          TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_odds_history_sel ON odds_history(selection_id, at);

-- ---------------------------------------------------------------------------
-- Betting
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bets (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    kind            TEXT    NOT NULL CHECK (kind IN ('single', 'parlay', 'each_way')),
    stake_sat       INTEGER NOT NULL CHECK (stake_sat > 0),
    odds_milli      INTEGER NOT NULL CHECK (odds_milli > 1000),
    potential_payout_sat INTEGER NOT NULL CHECK (potential_payout_sat >= 0),
    payout_sat      INTEGER NOT NULL DEFAULT 0,
    status          TEXT    NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open', 'won', 'lost', 'void', 'half_won', 'half_lost')),
    placed_at       TEXT    NOT NULL,
    settled_at      TEXT,
    ip              TEXT    NOT NULL DEFAULT '',
    -- Snapshot of what the customer was shown, for dispute resolution.
    accepted_odds_change INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_bets_user ON bets(user_id, placed_at);
CREATE INDEX IF NOT EXISTS idx_bets_status ON bets(status);

CREATE TABLE IF NOT EXISTS bet_legs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    bet_id          INTEGER NOT NULL REFERENCES bets(id) ON DELETE CASCADE,
    selection_id    INTEGER NOT NULL REFERENCES selections(id) ON DELETE RESTRICT,
    -- The price is frozen at placement; later movement never changes a struck bet.
    odds_milli      INTEGER NOT NULL CHECK (odds_milli > 1000),
    status          TEXT    NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open', 'won', 'lost', 'void', 'half_won', 'half_lost')),
    -- Denormalised descriptions so a settled bet still reads correctly after
    -- the underlying event or market has been edited or removed.
    event_name      TEXT    NOT NULL,
    market_title    TEXT    NOT NULL,
    selection_name  TEXT    NOT NULL,
    sport_key       TEXT    NOT NULL,
    starts_at       TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bet_legs_bet ON bet_legs(bet_id);
CREATE INDEX IF NOT EXISTS idx_bet_legs_selection ON bet_legs(selection_id);

-- ---------------------------------------------------------------------------
-- Money: double-entry ledger
-- ---------------------------------------------------------------------------
-- Every movement of value is a transaction whose entries sum to exactly zero.
-- A customer's balance is derived from this ledger and is never stored as a
-- mutable column, so a balance can always be explained line by line.
CREATE TABLE IF NOT EXISTS ledger_txns (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    kind            TEXT    NOT NULL
                            CHECK (kind IN ('deposit', 'withdrawal', 'withdrawal_reversal',
                                            'bet_stake', 'bet_payout', 'bet_void',
                                            'adjustment', 'fee', 'bonus')),
    at              TEXT    NOT NULL,
    memo            TEXT    NOT NULL DEFAULT '',
    ref_type        TEXT    NOT NULL DEFAULT '',
    ref_id          INTEGER,
    created_by      INTEGER REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_ledger_txns_at ON ledger_txns(at);
CREATE INDEX IF NOT EXISTS idx_ledger_txns_ref ON ledger_txns(ref_type, ref_id);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    txn_id          INTEGER NOT NULL REFERENCES ledger_txns(id) ON DELETE CASCADE,
    -- account is one of the book's internal accounts; user accounts also carry
    -- a user_id so a customer's balance is a single indexed sum.
    account         TEXT    NOT NULL
                            CHECK (account IN ('user_cash', 'bet_escrow', 'house_revenue',
                                               'house_fees', 'deposit_suspense',
                                               'withdrawal_suspense', 'external_bitcoin')),
    user_id         INTEGER REFERENCES users(id) ON DELETE RESTRICT,
    amount_sat      INTEGER NOT NULL,
    at              TEXT    NOT NULL,
    -- A user account entry without a user_id would be unattributable money.
    CHECK ((account = 'user_cash' AND user_id IS NOT NULL)
        OR (account <> 'user_cash'))
);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_txn ON ledger_entries(txn_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_user ON ledger_entries(user_id, account);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account ON ledger_entries(account);

-- ---------------------------------------------------------------------------
-- Bitcoin
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS deposit_addresses (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    address         TEXT    NOT NULL UNIQUE,
    provider        TEXT    NOT NULL,
    provider_ref    TEXT    NOT NULL DEFAULT '',
    network         TEXT    NOT NULL,
    created_at      TEXT    NOT NULL,
    active          INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_deposit_addresses_user ON deposit_addresses(user_id, active);

CREATE TABLE IF NOT EXISTS deposits (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    address         TEXT    NOT NULL,
    txid            TEXT    NOT NULL,
    vout            INTEGER NOT NULL DEFAULT 0,
    amount_sat      INTEGER NOT NULL CHECK (amount_sat > 0),
    confirmations   INTEGER NOT NULL DEFAULT 0,
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'confirmed', 'credited',
                                              'orphaned', 'rejected', 'frozen')),
    first_seen_at   TEXT    NOT NULL,
    credited_at     TEXT,
    ledger_txn_id   INTEGER REFERENCES ledger_txns(id) ON DELETE SET NULL,
    -- One credit per on-chain output, enforced by the database rather than
    -- by application care: a replayed webhook must never double-credit.
    UNIQUE (txid, vout)
);
CREATE INDEX IF NOT EXISTS idx_deposits_user ON deposits(user_id, first_seen_at);
CREATE INDEX IF NOT EXISTS idx_deposits_status ON deposits(status);

CREATE TABLE IF NOT EXISTS withdrawals (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    address         TEXT    NOT NULL,
    amount_sat      INTEGER NOT NULL CHECK (amount_sat > 0),
    fee_sat         INTEGER NOT NULL DEFAULT 0 CHECK (fee_sat >= 0),
    status          TEXT    NOT NULL DEFAULT 'requested'
                            CHECK (status IN ('requested', 'review', 'approved', 'broadcast',
                                              'confirmed', 'rejected', 'cancelled', 'failed')),
    txid            TEXT    NOT NULL DEFAULT '',
    requested_at    TEXT    NOT NULL,
    decided_at      TEXT,
    decided_by      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    reason          TEXT    NOT NULL DEFAULT '',
    ledger_txn_id   INTEGER REFERENCES ledger_txns(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_withdrawals_user ON withdrawals(user_id, requested_at);
CREATE INDEX IF NOT EXISTS idx_withdrawals_status ON withdrawals(status);

-- Anything a compliance officer must look at lands here rather than being
-- buried in a log file.
CREATE TABLE IF NOT EXISTS compliance_flags (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL,
    severity        TEXT    NOT NULL DEFAULT 'info'
                            CHECK (severity IN ('info', 'warning', 'critical')),
    detail          TEXT    NOT NULL DEFAULT '',
    raised_at       TEXT    NOT NULL,
    resolved_at     TEXT,
    resolved_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    resolution      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_flags_open ON compliance_flags(resolved_at, raised_at);
CREATE INDEX IF NOT EXISTS idx_flags_user ON compliance_flags(user_id);
