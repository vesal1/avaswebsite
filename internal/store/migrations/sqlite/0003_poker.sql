-- Poker tables, hands and hand histories (SQLite).
--
-- Everything a player would need to audit a hand is stored: the deck
-- commitment published before the deal, the seed revealed afterwards, the
-- seats, the actions in order and the payouts. A hand history that cannot be
-- checked is a hand history nobody should trust.

-- Chips at a poker table are customer money parked in a seat, so they get
-- their own ledger account rather than being conjured at the table. Poker
-- buy-ins, cash-outs and rake are also movements the original transaction
-- kinds do not describe.
--
-- SQLite cannot alter a CHECK constraint, so both ledger tables are rebuilt.
-- ledger_entries has a foreign key onto ledger_txns, so the order matters: the
-- new child is pointed at the new parent before either old table is dropped,
-- which keeps every foreign key satisfied throughout and lets the whole thing
-- run inside one transaction. Renaming a table rewrites the references to it,
-- so the final names come out right.
--
-- Every row is carried across, ids included: a ledger is append-only history
-- and losing or renumbering an entry would destroy the audit trail.
CREATE TABLE ledger_txns_v3 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    kind            TEXT    NOT NULL
                            CHECK (kind IN ('deposit', 'withdrawal', 'withdrawal_reversal',
                                            'bet_stake', 'bet_payout', 'bet_void',
                                            'adjustment', 'fee', 'bonus',
                                            'poker_buy_in', 'poker_cash_out', 'poker_rake',
                                            'casino_stake', 'casino_payout')),
    at              TEXT    NOT NULL,
    memo            TEXT    NOT NULL DEFAULT '',
    ref_type        TEXT    NOT NULL DEFAULT '',
    ref_id          INTEGER,
    created_by      INTEGER REFERENCES users(id) ON DELETE SET NULL
);

INSERT INTO ledger_txns_v3 (id, kind, at, memo, ref_type, ref_id, created_by)
    SELECT id, kind, at, memo, ref_type, ref_id, created_by FROM ledger_txns;

CREATE TABLE ledger_entries_v3 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    txn_id          INTEGER NOT NULL REFERENCES ledger_txns_v3(id) ON DELETE CASCADE,
    account         TEXT    NOT NULL
                            CHECK (account IN ('user_cash', 'user_bonus', 'bet_escrow',
                                               'house_revenue', 'house_fees',
                                               'house_promotions', 'deposit_suspense',
                                               'withdrawal_suspense', 'external_bitcoin',
                                               'poker_table', 'house_rake')),
    user_id         INTEGER REFERENCES users(id) ON DELETE RESTRICT,
    amount_sat      INTEGER NOT NULL,
    at              TEXT    NOT NULL,
    CHECK ((account IN ('user_cash', 'user_bonus') AND user_id IS NOT NULL)
        OR (account NOT IN ('user_cash', 'user_bonus')))
);

INSERT INTO ledger_entries_v3 (id, txn_id, account, user_id, amount_sat, at)
    SELECT id, txn_id, account, user_id, amount_sat, at FROM ledger_entries;

DROP TABLE ledger_entries;
DROP TABLE ledger_txns;

ALTER TABLE ledger_txns_v3 RENAME TO ledger_txns;
ALTER TABLE ledger_entries_v3 RENAME TO ledger_entries;

CREATE INDEX IF NOT EXISTS idx_ledger_txns_at ON ledger_txns(at);
CREATE INDEX IF NOT EXISTS idx_ledger_txns_ref ON ledger_txns(ref_type, ref_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_txn ON ledger_entries(txn_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_user ON ledger_entries(user_id, account);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account ON ledger_entries(account);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_user_account
    ON ledger_entries(user_id, account, id);

CREATE TABLE IF NOT EXISTS poker_tables (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT    NOT NULL,
    variant             TEXT    NOT NULL DEFAULT 'holdem' CHECK (variant IN ('holdem')),
    small_blind_sat     INTEGER NOT NULL CHECK (small_blind_sat > 0),
    big_blind_sat       INTEGER NOT NULL CHECK (big_blind_sat > 0),
    ante_sat            INTEGER NOT NULL DEFAULT 0 CHECK (ante_sat >= 0),
    min_buy_in_sat      INTEGER NOT NULL CHECK (min_buy_in_sat > 0),
    max_buy_in_sat      INTEGER NOT NULL CHECK (max_buy_in_sat > 0),
    max_seats           INTEGER NOT NULL DEFAULT 6 CHECK (max_seats BETWEEN 2 AND 9),
    -- Rake is stored on the table so it is visible in the lobby before anybody
    -- sits down, not discovered from a hand history afterwards.
    rake_bps            INTEGER NOT NULL DEFAULT 250 CHECK (rake_bps >= 0 AND rake_bps <= 1000),
    rake_cap_sat        INTEGER NOT NULL DEFAULT 0 CHECK (rake_cap_sat >= 0),
    no_flop_no_drop     INTEGER NOT NULL DEFAULT 1,
    action_seconds      INTEGER NOT NULL DEFAULT 30 CHECK (action_seconds BETWEEN 5 AND 300),
    -- Whether camera and microphone are offered at this table at all.
    video_enabled       INTEGER NOT NULL DEFAULT 1,
    active              INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_poker_tables_active ON poker_tables(active, big_blind_sat);

-- A seat occupancy: who is sitting where, and what they brought.
CREATE TABLE IF NOT EXISTS poker_seats (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    table_id        INTEGER NOT NULL REFERENCES poker_tables(id) ON DELETE CASCADE,
    seat_number     INTEGER NOT NULL,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    stack_sat       INTEGER NOT NULL DEFAULT 0 CHECK (stack_sat >= 0),
    status          TEXT    NOT NULL DEFAULT 'sitting_out'
                            CHECK (status IN ('active', 'sitting_out', 'left')),
    -- The player's own contribution to the shuffle.
    client_seed     TEXT    NOT NULL DEFAULT '',
    -- Consent is per seat and per session: sitting down does not turn a camera
    -- on, and it is never remembered as a standing permission.
    video_consent   INTEGER NOT NULL DEFAULT 0,
    joined_at       TEXT    NOT NULL,
    left_at         TEXT,
    -- One live occupancy per seat, and a player cannot occupy two seats at a
    -- table at once.
    UNIQUE (table_id, seat_number, joined_at)
);
CREATE INDEX IF NOT EXISTS idx_poker_seats_table ON poker_seats(table_id, status);
CREATE INDEX IF NOT EXISTS idx_poker_seats_user ON poker_seats(user_id, status);

CREATE TABLE IF NOT EXISTS poker_hands (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    table_id            INTEGER NOT NULL REFERENCES poker_tables(id) ON DELETE CASCADE,
    hand_number         INTEGER NOT NULL,
    button_seat         INTEGER NOT NULL,
    -- Published before the deal.
    commitment          TEXT    NOT NULL,
    client_seed         TEXT    NOT NULL,
    -- Written only once the hand is over.
    server_seed         TEXT    NOT NULL DEFAULT '',
    board               TEXT    NOT NULL DEFAULT '',
    pot_sat             INTEGER NOT NULL DEFAULT 0,
    rake_sat            INTEGER NOT NULL DEFAULT 0,
    stage               TEXT    NOT NULL DEFAULT 'pre-flop',
    started_at          TEXT    NOT NULL,
    finished_at         TEXT,
    history             TEXT    NOT NULL DEFAULT '',
    UNIQUE (table_id, hand_number)
);
CREATE INDEX IF NOT EXISTS idx_poker_hands_table ON poker_hands(table_id, hand_number DESC);

CREATE TABLE IF NOT EXISTS poker_hand_players (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    hand_id         INTEGER NOT NULL REFERENCES poker_hands(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    seat_number     INTEGER NOT NULL,
    starting_stack_sat BIGINT NOT NULL,
    committed_sat   INTEGER NOT NULL DEFAULT 0,
    won_sat         INTEGER NOT NULL DEFAULT 0,
    -- Hole cards are written only for hands that reached showdown, or for the
    -- player's own history. A folded hand's cards are nobody else's business.
    hole_cards      TEXT    NOT NULL DEFAULT '',
    shown           INTEGER NOT NULL DEFAULT 0,
    result          TEXT    NOT NULL DEFAULT '',
    UNIQUE (hand_id, seat_number)
);
CREATE INDEX IF NOT EXISTS idx_poker_hand_players_user ON poker_hand_players(user_id, hand_id DESC);

CREATE TABLE IF NOT EXISTS poker_actions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    hand_id         INTEGER NOT NULL REFERENCES poker_hands(id) ON DELETE CASCADE,
    sequence        INTEGER NOT NULL,
    seat_number     INTEGER NOT NULL,
    stage           TEXT    NOT NULL,
    action          TEXT    NOT NULL,
    amount_sat      INTEGER NOT NULL DEFAULT 0,
    at              TEXT    NOT NULL,
    UNIQUE (hand_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_poker_actions_hand ON poker_actions(hand_id, sequence);
