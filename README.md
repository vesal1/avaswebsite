# Avas Sportsbook

A sports betting platform in Go, covering **168 sports** across 17 categories,
funded in bitcoin, running on PostgreSQL or SQLite.

It is a complete working system: an odds board, a bet slip, singles and
multiples, a settlement engine, a bitcoin wallet built on a double-entry
ledger, a trading and compliance back office, and the regulatory controls a
betting licence requires.

---

## Before you run this in public

This is real-money gambling software. Operating it is not a technical decision:

- **You need a licence.** Taking bets from a jurisdiction you are not licensed
  in is a criminal matter in many countries, not a terms-of-service problem.
  The country allowlist here fails closed — an unconfigured server serves
  nobody — and `AVAS_ALLOWED_COUNTRIES` is the only thing that opens it.
- **You need real KYC/AML.** The identity flow here records a *reference* to a
  document held by a verification provider. It does not verify anyone. Wire it
  to a real provider, and to sanctions and PEP screening, before launch.
- **You must not hold customer bitcoin on a hot key you wrote yourself.** The
  wallet layer deliberately delegates custody to BTCPay Server or Bitcoin Core.
  The `mock` provider is refused in production.
- **Safer-gambling tools are a licence condition**, not a feature. Deposit,
  loss and stake limits, cool-offs, self-exclusion and reality checks are all
  implemented and all enforced server-side.

`avas serve` refuses to start with `AVAS_ENV=production` while any unsafe
default remains. Run `avas check-ledger` on a schedule.

---

## Installing against your own database

The schema lives in versioned `.sql` files you can read, review and run:

```
internal/store/migrations/postgres/0001_initial_schema.sql
internal/store/migrations/postgres/0002_bonus_and_adjustments.sql
internal/store/migrations/sqlite/...
```

Point `AVAS_DB` at your server and let the app apply them:

```bash
createdb avas
export AVAS_DB="postgres://avas:secret@localhost:5432/avas?sslmode=require"

./avas migrate-status     # what is pending
./avas migrate            # apply it
```

Or apply them by hand if you would rather your DBA drove:

```bash
psql "$AVAS_DB" -v ON_ERROR_STOP=1 -f internal/store/migrations/postgres/0001_initial_schema.sql
psql "$AVAS_DB" -v ON_ERROR_STOP=1 -f internal/store/migrations/postgres/0002_bonus_and_adjustments.sql
```

`avas serve` refuses to start against a database that is behind the binary. A
missing column should be a clear error at boot, not a failed bet at 2am.

Migrations are recorded with a checksum. If an already-applied migration file
is edited, the next run is refused rather than silently ignored — two
environments believing they share a schema when they do not is worse than a
loud failure.

**A note on the schema.** Timestamps are `TEXT` holding RFC3339 UTC, and
booleans are `INTEGER` 0/1, in both databases. That is deliberately
unidiomatic for PostgreSQL: keeping one representation across both supported
databases removes a class of timezone and type drift between environments.
Reporting queries can cast with `col::timestamptz`.

## Quick start

```bash
go build -o avas ./cmd/avas

export AVAS_DB=avas.sqlite3           # or a postgres:// URL
export AVAS_ALLOWED_COUNTRIES=GB,IE   # jurisdictions you are licensed in
export AVAS_GEO_DEFAULT=GB            # development only; see below

./avas migrate                        # create the schema
./avas seed -events 200               # a board across every sport
./avas create-user -email you@example.com -password 'a long passphrase' -verified
./avas create-user -email boss@example.com -password 'a long passphrase' -role admin
./avas serve                          # http://127.0.0.1:8000
```

In development the wallet is simulated, so the whole deposit → bet → settle →
withdraw loop can be walked without a bitcoin node: the wallet page has a
"simulate a deposit" control that only exists on the mock provider.

### Commands

| Command | What it does |
|---|---|
| `avas migrate` | Apply pending schema migrations |
| `avas migrate-status` | Show which migrations have run |
| `avas serve` | Run the web server and the background reconciliation jobs |
| `avas seed -events N` | Load a deterministic demonstration board |
| `avas create-user` | Create an account, optionally with a staff role |
| `avas sync-deposits` | One deposit reconciliation pass |
| `avas check-ledger` | Verify the ledger invariants and exit non-zero if broken |

---

## How it is put together

```
cmd/avas              entrypoint and CLI
internal/money        satoshi and odds arithmetic — integers only
internal/bonus        promotions, wagering requirements, comps
internal/treasury     manual cash adjustments with two-person approval
internal/catalog      the 168 sports and every market kind
internal/store        SQLite schema and all SQL
internal/auth         Argon2id credentials, sessions, CSRF
internal/bitcoin      wallet providers: mock, BTCPay Server, Bitcoin Core
internal/compliance   geo, age, KYC, limits, self-exclusion, AML
internal/wallet       deposits, withdrawals, the double-entry ledger
internal/betting      bet placement, grading, settlement
internal/web          HTTP handlers, templates, JSON API
internal/seed         demonstration content
```

### Money is never a float

Balances are `int64` satoshi. Prices are decimal odds stored as integer
thousandths, so 2.50 is `2500`. Every payout truncates toward the book: a
fractional satoshi is never paid out. Fiat appears only as a display
conversion, never as a stored balance.

### Balances are derived, not stored

There is no `balance` column. A customer's balance is the sum of their entries
in a double-entry ledger whose every transaction must sum to exactly zero — the
store refuses to write one that does not. That means any balance can be
explained line by line, and a bug shows up as a broken invariant rather than as
quietly wrong money.

`avas check-ledger` and `/healthz` both verify three things: the whole ledger
sums to zero, every individual transaction sums to zero, and no customer
balance has gone negative.

### Deposits cannot be double-credited

A deposit row is unique on `(txid, vout)`, and crediting is guarded by a status
check inside the transaction that posts the ledger entries. A webhook replayed
five times credits once. Deposits are only credited after the configured
confirmation depth, and a deposit that would breach a customer's own limit is
held and flagged rather than credited.

### Withdrawals debit immediately

Requesting a withdrawal debits the balance there and then, so the same funds
cannot also be staked. A rejection or cancellation reverses the debit, fee
included. A send that fails ambiguously is **never** retried automatically: it
raises a critical flag for a human, because a retried payout is a duplicated
payout.

### Bonus money is not cash

Promotional money lives in its own ledger account. It can be staked but not
withdrawn, and becomes withdrawable cash only by meeting its wagering
requirement. The rules are on the `/promotions` page rather than buried in
terms, because promotions nobody can understand are how disputes start:

- Staking draws on the customer's **own cash first**, so a bonus they later
  give up costs them as little as possible. The other order quietly protects
  the house.
- Contribution rates differ by product (slots 100%, sports 50%, poker 20%) and
  are published. A bonus clearing at full rate on near-even-money bets is a
  promotion to arbitrage, not to play.
- Stakes below an offer's minimum price do not clear it.
- While a bonus is active, withdrawals are paused. The customer can finish the
  wagering **or forfeit the bonus in one click** — their own money is never
  touched either way. Trapping somebody's own funds behind a promotion they no
  longer want is not acceptable.
- Wagering credit is idempotent per stake, so replayed settlement cannot clear
  a bonus that was not earned.

### Manual adjustments are deliberately awkward

Adjustments mint or destroy customer balance with no deposit or bet behind
them, which makes them the most dangerous operation in the system. So: every
one carries a written reason, anything at or above `AVAS_MANUAL_APPROVAL_SAT`
needs a second person, nobody can approve their own, and a debit cannot
overdraw. All of it lands in the audit trail.

**Test credits** (`AVAS_ALLOW_TEST_CREDITS`) let an admin mint play money to
exercise the system. They are refused in production by default, and enabling
them there is reported by the config validator, because test money sitting
alongside real customer money cannot be reconciled against a bank.

### Settlement

Markets are graded from the posted result where a result can decide them:
1X2, two-way, draw-no-bet, double chance, handicaps (including Asian quarter
lines that split a stake), totals, team totals, both-teams-to-score, odd/even,
clean sheet, correct score, winning margin, and field markets on finishing
position.

Anything a score line cannot decide — first scorer, method of victory,
half-time/full-time — is reported for a trader rather than guessed at.
Settling late is recoverable; settling wrongly is not.

Two rules in the grader protect the customer:

- An exact score or margin band that was **never priced** voids the market
  rather than losing every selection. Charging for an outcome that could not be
  backed is not a settlement.
- A withdrawn competitor's stake comes back.

### Markets follow the shape of a contest, not its name

A tennis match and a darts match are different sports and the same *shape*: two
individuals, no draw, scored in sets. The catalog prices on shape, so adding a
sport is a single table entry and it inherits the right markets, the right
totals wording ("Total Goals" vs "Total Points" vs "Total Runs") and the right
settlement rules.

The catalog's tests enforce the invariants that matter: a sport that can draw
must offer a draw-no-bet alternative, a sport that runs races must be able to
price an outright, and every sport must carry a headline winner market.

---

## Configuration

Everything is environment-driven. Defaults are development defaults.

### Core

| Variable | Default | Notes |
|---|---|---|
| `AVAS_ENV` | `development` | `production` turns on the safety checks |
| `AVAS_SECRET_KEY` | dev placeholder | Must be ≥32 unique chars in production |
| `AVAS_DB` | `avas.sqlite3` | PostgreSQL URL or SQLite file path |
| `AVAS_HOST` / `AVAS_PORT` | `127.0.0.1` / `8000` | |
| `AVAS_BRAND` | `Avas Sportsbook` | |
| `AVAS_LICENCE` | *(unset)* | Shown in the footer; required in production |
| `AVAS_SUPPORT_EMAIL` | — | |

### Jurisdiction and player protection

| Variable | Default | Notes |
|---|---|---|
| `AVAS_ALLOWED_COUNTRIES` | *(empty)* | **Empty blocks everyone.** e.g. `GB,IE` |
| `AVAS_BLOCKED_COUNTRIES` | *(empty)* | Beats the allowlist |
| `AVAS_GEO_HEADER` | `CF-IPCountry` | Geolocation header from your edge |
| `AVAS_GEO_DEFAULT` | *(empty)* | Dev only; refused in production |
| `AVAS_MIN_AGE` | `18` | |
| `AVAS_KYC_FOR_BETTING` | `true` | Refused if false in production |
| `AVAS_REALITY_CHECK_MIN` | `60` | Minutes between reality checks |
| `AVAS_AML_REPORTING_SAT` | `100000000` | Flag threshold |

### Betting limits

| Variable | Default |
|---|---|
| `AVAS_MIN_STAKE_SAT` | `1000` |
| `AVAS_MAX_STAKE_SAT` | `50000000` |
| `AVAS_MAX_PAYOUT_SAT` | `500000000` |
| `AVAS_MAX_PARLAY_LEGS` | `15` |
| `AVAS_MAX_LIABILITY_SAT` | `2000000000` |
| `AVAS_MARGIN_BPS` | `500` (a 5% overround) |

### Bonuses and manual money

| Variable | Default | Notes |
|---|---|---|
| `AVAS_ALLOW_TEST_CREDITS` | `false` | Lets staff mint play money; refused in production by default |
| `AVAS_MANUAL_APPROVAL_SAT` | `10000000` | Adjustments at or above this need a second approver |
| `AVAS_MAX_MANUAL_ADJUST_SAT` | `1000000000` | Hard ceiling on any single adjustment |
| `AVAS_DEFAULT_WAGERING_X100` | `500` | Default wagering multiplier, 500 = 5× |

### Bitcoin

| Variable | Default | Notes |
|---|---|---|
| `AVAS_BTC_PROVIDER` | `mock` | `btcpay`, `bitcoind`, or `mock` (dev only) |
| `AVAS_BTC_NETWORK` | `regtest` | Must be `mainnet` in production |
| `AVAS_BTC_CONFIRMATIONS` | `2` | Confirmations before crediting |
| `AVAS_MIN_DEPOSIT_SAT` | `20000` | |
| `AVAS_MIN_WITHDRAWAL_SAT` | `50000` | |
| `AVAS_WITHDRAWAL_FEE_SAT` | `2500` | Charged on top of the amount |
| `AVAS_MANUAL_REVIEW_SAT` | `100000000` | Payouts at or above need a human |

**BTCPay Server:** `AVAS_BTCPAY_URL`, `AVAS_BTCPAY_STORE_ID`, `AVAS_BTCPAY_API_KEY`
**Bitcoin Core:** `AVAS_BITCOIND_URL`, `AVAS_BITCOIND_USER`, `AVAS_BITCOIND_PASSWORD`, `AVAS_BITCOIND_WALLET`

---

## Addresses are checked properly

A payout to a mistyped address is unrecoverable, so addresses are validated
against real checksums rather than a regex: bech32 and bech32m (BIP-173,
BIP-350) with witness-version and program-length rules, and base58check with
network version bytes. A mainnet address offered to a testnet wallet is
rejected *and told* which network it belongs to.

---

## Roles

| Role | Can |
|---|---|
| `customer` | Bet, deposit, withdraw, manage their own limits |
| `trader` | Create events, move prices, settle markets |
| `compliance` | Release and reject withdrawals, clear flags, decide KYC |
| `admin` | Everything |

Staff roles are granted only via `avas create-user` or by another admin. There
is no customer-facing path to one. A customer who reaches a back-office URL is
told the page does not exist.

---

## JSON API

Read-only, by design. Bets are struck through the form flow, which carries the
CSRF token, the compliance checks and the price-change consent; a second,
weaker path to the same money is not worth having.

```
GET /api/sports                      every sport with its markets and open event count
GET /api/sports/{sport}/events       open events for a sport
GET /api/events/{id}                 one event with all markets and prices
GET /healthz                         health, including ledger invariants
```

Prices come back in all three formats plus `odds_milli`, the canonical integer.

---

## Security

- Argon2id password hashing (64 MiB, t=3, p=4) with a per-password salt.
- Session cookies are random 256-bit tokens; only their SHA-256 hash is stored.
- CSRF tokens are derived per session and verified on every state change.
- Login throttling: 8 failures in 15 minutes locks sign-in.
- Sign-in failures are indistinguishable whether or not the account exists.
- CSP forbids inline script; `HttpOnly`, `SameSite=Lax`, `Secure` in production.
- Suspended, closed and self-excluded accounts lose live sessions immediately.
- Post-login redirects are constrained to this site.

---

## Testing

```bash
go test ./...
go test -race ./internal/betting/ ./internal/wallet/

# Run the suite against a real PostgreSQL server too. Each test gets its own
# schema, so dialect drift is caught here rather than in production.
AVAS_TEST_POSTGRES="postgres://postgres@localhost:5432/avas?sslmode=disable" \
  go test ./internal/store/ -count=1
```

Every money-moving test asserts the ledger invariants afterwards. A wallet that
produces the right balance through an unbalanced ledger is still broken.

---

## Before going live

- [ ] Hold a licence in every jurisdiction in `AVAS_ALLOWED_COUNTRIES`
- [ ] Set `AVAS_ENV=production` and let the config validator pass
- [ ] Real geolocation at the edge; `AVAS_GEO_DEFAULT` unset
- [ ] Real KYC provider, plus sanctions and PEP screening
- [ ] Custody on BTCPay or Core, with cold storage and a hot-wallet float policy
- [ ] `avas check-ledger` on a schedule, alerting on non-zero exit
- [ ] Breached-password checking on registration
- [ ] Run on PostgreSQL, not SQLite, if you expect concurrent writers
- [ ] `AVAS_ALLOW_TEST_CREDITS` unset, and no test grants in the ledger
- [ ] Promotion terms on `/promotions` reviewed against what you advertise
- [ ] Deposit/withdrawal reconciliation against on-chain state
- [ ] Independent review of the settlement rules against your published terms
- [ ] Self-exclusion shared with any national scheme you are required to join
