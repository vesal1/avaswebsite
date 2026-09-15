-- Poker tables, hands and hand histories (PostgreSQL).
--
-- Everything a player would need to audit a hand is stored: the deck
-- commitment published before the deal, the seed revealed afterwards, the
-- seats, the actions in order and the payouts. A hand history that cannot be
-- checked is a hand history nobody should trust.

-- Chips at a poker table are customer money parked in a seat, so they get
-- their own ledger account rather than being conjured at the table.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_account_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_account_check
    CHECK (account IN ('user_cash', 'user_bonus', 'bet_escrow', 'house_revenue',
                       'house_fees', 'house_promotions', 'deposit_suspense',
                       'withdrawal_suspense', 'external_bitcoin',
                       'poker_table', 'house_rake'));

-- Poker buy-ins, cash-outs and rake are movements the original transaction
-- kinds do not describe, so the list is widened.
ALTER TABLE ledger_txns DROP CONSTRAINT IF EXISTS ledger_txns_kind_check;
ALTER TABLE ledger_txns ADD CONSTRAINT ledger_txns_kind_check
    CHECK (kind IN ('deposit', 'withdrawal', 'withdrawal_reversal',
                    'bet_stake', 'bet_payout', 'bet_void',
                    'adjustment', 'fee', 'bonus',
                    'poker_buy_in', 'poker_cash_out', 'poker_rake',
                    'casino_stake', 'casino_payout'));

CREATE TABLE IF NOT EXISTS poker_tables (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                TEXT    NOT NULL,
    variant             TEXT    NOT NULL DEFAULT 'holdem' CHECK (variant IN ('holdem')),
    small_blind_sat     BIGINT  NOT NULL CHECK (small_blind_sat > 0),
    big_blind_sat       BIGINT  NOT NULL CHECK (big_blind_sat > 0),
    ante_sat            BIGINT  NOT NULL DEFAULT 0 CHECK (ante_sat >= 0),
    min_buy_in_sat      BIGINT  NOT NULL CHECK (min_buy_in_sat > 0),
    max_buy_in_sat      BIGINT  NOT NULL CHECK (max_buy_in_sat > 0),
    max_seats           BIGINT  NOT NULL DEFAULT 6 CHECK (max_seats BETWEEN 2 AND 9),
    -- Rake is stored on the table so it is visible in the lobby before anybody
    -- sits down, not discovered from a hand history afterwards.
    rake_bps            BIGINT  NOT NULL DEFAULT 250 CHECK (rake_bps >= 0 AND rake_bps <= 1000),
    rake_cap_sat        BIGINT  NOT NULL DEFAULT 0 CHECK (rake_cap_sat >= 0),
    no_flop_no_drop     BIGINT  NOT NULL DEFAULT 1,
    action_seconds      BIGINT  NOT NULL DEFAULT 30 CHECK (action_seconds BETWEEN 5 AND 300),
    -- Whether camera and microphone are offered at this table at all.
    video_enabled       BIGINT  NOT NULL DEFAULT 1,
    active              BIGINT  NOT NULL DEFAULT 1,
    created_at          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_poker_tables_active ON poker_tables(active, big_blind_sat);

-- A seat occupancy: who is sitting where, and what they brought.
CREATE TABLE IF NOT EXISTS poker_seats (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    table_id        BIGINT  NOT NULL REFERENCES poker_tables(id) ON DELETE CASCADE,
    seat_number     BIGINT  NOT NULL,
    user_id         BIGINT  NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    stack_sat       BIGINT  NOT NULL DEFAULT 0 CHECK (stack_sat >= 0),
    status          TEXT    NOT NULL DEFAULT 'sitting_out'
                            CHECK (status IN ('active', 'sitting_out', 'left')),
    -- The player's own contribution to the shuffle.
    client_seed     TEXT    NOT NULL DEFAULT '',
    -- Consent is per seat and per session: sitting down does not turn a camera
    -- on, and it is never remembered as a standing permission.
    video_consent   BIGINT  NOT NULL DEFAULT 0,
    joined_at       TEXT    NOT NULL,
    left_at         TEXT,
    -- One live occupancy per seat, and a player cannot occupy two seats at a
    -- table at once.
    UNIQUE (table_id, seat_number, joined_at)
);
CREATE INDEX IF NOT EXISTS idx_poker_seats_table ON poker_seats(table_id, status);
CREATE INDEX IF NOT EXISTS idx_poker_seats_user ON poker_seats(user_id, status);

CREATE TABLE IF NOT EXISTS poker_hands (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    table_id            BIGINT  NOT NULL REFERENCES poker_tables(id) ON DELETE CASCADE,
    hand_number         BIGINT  NOT NULL,
    button_seat         BIGINT  NOT NULL,
    -- Published before the deal.
    commitment          TEXT    NOT NULL,
    client_seed         TEXT    NOT NULL,
    -- Written only once the hand is over.
    server_seed         TEXT    NOT NULL DEFAULT '',
    board               TEXT    NOT NULL DEFAULT '',
    pot_sat             BIGINT  NOT NULL DEFAULT 0,
    rake_sat            BIGINT  NOT NULL DEFAULT 0,
    stage               TEXT    NOT NULL DEFAULT 'pre-flop',
    started_at          TEXT    NOT NULL,
    finished_at         TEXT,
    history             TEXT    NOT NULL DEFAULT '',
    UNIQUE (table_id, hand_number)
);
CREATE INDEX IF NOT EXISTS idx_poker_hands_table ON poker_hands(table_id, hand_number DESC);

CREATE TABLE IF NOT EXISTS poker_hand_players (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    hand_id         BIGINT  NOT NULL REFERENCES poker_hands(id) ON DELETE CASCADE,
    user_id         BIGINT  NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    seat_number     BIGINT  NOT NULL,
    starting_stack_sat BIGINT NOT NULL,
    committed_sat   BIGINT  NOT NULL DEFAULT 0,
    won_sat         BIGINT  NOT NULL DEFAULT 0,
    -- Hole cards are written only for hands that reached showdown, or for the
    -- player's own history. A folded hand's cards are nobody else's business.
    hole_cards      TEXT    NOT NULL DEFAULT '',
    shown           BIGINT  NOT NULL DEFAULT 0,
    result          TEXT    NOT NULL DEFAULT '',
    UNIQUE (hand_id, seat_number)
);
CREATE INDEX IF NOT EXISTS idx_poker_hand_players_user ON poker_hand_players(user_id, hand_id DESC);

CREATE TABLE IF NOT EXISTS poker_actions (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    hand_id         BIGINT  NOT NULL REFERENCES poker_hands(id) ON DELETE CASCADE,
    sequence        BIGINT  NOT NULL,
    seat_number     BIGINT  NOT NULL,
    stage           TEXT    NOT NULL,
    action          TEXT    NOT NULL,
    amount_sat      BIGINT  NOT NULL DEFAULT 0,
    at              TEXT    NOT NULL,
    UNIQUE (hand_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_poker_actions_hand ON poker_actions(hand_id, sequence);
