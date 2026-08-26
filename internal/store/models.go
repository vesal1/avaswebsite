package store

import "time"

// Role values.
const (
	RoleCustomer   = "customer"
	RoleTrader     = "trader"
	RoleCompliance = "compliance"
	RoleAdmin      = "admin"
)

// User status values.
const (
	UserActive    = "active"
	UserSuspended = "suspended"
	UserClosed    = "closed"
)

// KYC status values.
const (
	KYCNone     = "none"
	KYCPending  = "pending"
	KYCVerified = "verified"
	KYCRejected = "rejected"
)

// Event status values.
const (
	EventScheduled = "scheduled"
	EventLive      = "live"
	EventSuspended = "suspended"
	EventFinished  = "finished"
	EventSettled   = "settled"
	EventCancelled = "cancelled"
	EventPostponed = "postponed"
)

// Market status values.
const (
	MarketOpen      = "open"
	MarketSuspended = "suspended"
	MarketClosed    = "closed"
	MarketSettled   = "settled"
	MarketVoid      = "void"
)

// Selection, leg and bet outcome values. A selection and the legs and bets
// riding on it share this vocabulary so settlement can propagate directly.
const (
	OutcomeOpen      = "open"
	OutcomeSuspended = "suspended"
	OutcomeWon       = "won"
	OutcomeLost      = "lost"
	OutcomeVoid      = "void"
	OutcomeHalfWon   = "half_won"
	OutcomeHalfLost  = "half_lost"
)

// Bet kinds.
const (
	BetSingle  = "single"
	BetParlay  = "parlay"
	BetEachWay = "each_way"
)

// Ledger accounts.
const (
	AccountUserCash           = "user_cash"
	AccountUserBonus          = "user_bonus"
	AccountBetEscrow          = "bet_escrow"
	AccountHouseRevenue       = "house_revenue"
	AccountHouseFees          = "house_fees"
	AccountHousePromotions    = "house_promotions"
	AccountDepositSuspense    = "deposit_suspense"
	AccountWithdrawalSuspense = "withdrawal_suspense"
	AccountExternalBitcoin    = "external_bitcoin"
	AccountPokerTable         = "poker_table"
	AccountHouseRake          = "house_rake"
)

// Ledger transaction kinds.
const (
	TxnDeposit            = "deposit"
	TxnWithdrawal         = "withdrawal"
	TxnWithdrawalReversal = "withdrawal_reversal"
	TxnBetStake           = "bet_stake"
	TxnBetPayout          = "bet_payout"
	TxnBetVoid            = "bet_void"
	TxnAdjustment         = "adjustment"
	TxnFee                = "fee"
	TxnBonus              = "bonus"
	TxnPokerBuyIn         = "poker_buy_in"
	TxnPokerCashOut       = "poker_cash_out"
	TxnPokerRake          = "poker_rake"
	TxnCasinoStake        = "casino_stake"
	TxnCasinoPayout       = "casino_payout"
)

// Deposit status values.
const (
	DepositPending   = "pending"
	DepositConfirmed = "confirmed"
	DepositCredited  = "credited"
	DepositOrphaned  = "orphaned"
	DepositRejected  = "rejected"
	DepositFrozen    = "frozen"
)

// Withdrawal status values.
const (
	WithdrawalRequested = "requested"
	WithdrawalReview    = "review"
	WithdrawalApproved  = "approved"
	WithdrawalBroadcast = "broadcast"
	WithdrawalConfirmed = "confirmed"
	WithdrawalRejected  = "rejected"
	WithdrawalCancelled = "cancelled"
	WithdrawalFailed    = "failed"
)

// Player limit kinds.
const (
	LimitDepositDaily   = "deposit_daily"
	LimitDepositWeekly  = "deposit_weekly"
	LimitDepositMonthly = "deposit_monthly"
	LimitLossDaily      = "loss_daily"
	LimitLossWeekly     = "loss_weekly"
	LimitStakeDaily     = "stake_daily"
	LimitSessionMinutes = "session_minutes"
)

// Exclusion kinds.
const (
	ExclusionCoolOff       = "cool_off"
	ExclusionSelfExclusion = "self_exclusion"
)

// User is a registered account.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	DisplayName  string
	DateOfBirth  string
	Country      string
	Role         string
	Status       string
	KYCStatus    string
	OddsFormat   string
	CreatedAt    time.Time
	LastLoginAt  time.Time
}

// IsStaff reports whether the account can reach any part of the back office.
func (u User) IsStaff() bool {
	return u.Role == RoleTrader || u.Role == RoleCompliance || u.Role == RoleAdmin
}

// CanTrade reports whether the account may create events and move prices.
func (u User) CanTrade() bool { return u.Role == RoleTrader || u.Role == RoleAdmin }

// CanReviewCompliance reports whether the account may release withdrawals and
// clear compliance flags.
func (u User) CanReviewCompliance() bool {
	return u.Role == RoleCompliance || u.Role == RoleAdmin
}

// Session is a logged-in browser session.
type Session struct {
	TokenHash      string
	UserID         int64
	CreatedAt      time.Time
	ExpiresAt      time.Time
	LastSeenAt     time.Time
	RealityCheckAt time.Time
	IP             string
	UserAgent      string
}

// Competition groups events within a sport.
type Competition struct {
	ID       int64
	SportKey string
	Name     string
	Region   string
}

// Event is a single bettable contest.
type Event struct {
	ID            int64
	SportKey      string
	CompetitionID int64
	Competition   string
	Name          string
	Format        string
	StartsAt      time.Time
	Status        string
	Venue         string
	SettledAt     time.Time
}

// IsOpenForBetting reports whether new bets may be struck on this event.
func (e Event) IsOpenForBetting(now time.Time) bool {
	switch e.Status {
	case EventScheduled, EventLive:
		return e.Status == EventLive || now.Before(e.StartsAt)
	default:
		return false
	}
}

// Participant is a competitor in an event.
type Participant struct {
	ID             int64
	EventID        int64
	Name           string
	ShortName      string
	Nationality    string
	HomeAway       string
	SortOrder      int
	Score          int64
	HasScore       bool
	FinishPosition int
	Withdrawn      bool
}

// Market is a set of mutually exclusive selections on an event.
type Market struct {
	ID         int64
	EventID    int64
	Kind       string
	Title      string
	LineX100   int64
	HasLine    bool
	Period     string
	SubjectID  int64
	Status     string
	MarginBps  int64
	SettledAt  time.Time
	Selections []Selection
}

// Selection is one bettable outcome within a market.
type Selection struct {
	ID            int64
	MarketID      int64
	ParticipantID int64
	Name          string
	OutcomeCode   string
	OddsMilli     int64
	Status        string
	SortOrder     int
}

// Bet is a struck wager: one leg for a single, many for a parlay.
type Bet struct {
	ID                 int64
	UserID             int64
	Kind               string
	StakeSat           int64
	OddsMilli          int64
	PotentialPayoutSat int64
	PayoutSat          int64
	Status             string
	PlacedAt           time.Time
	SettledAt          time.Time
	IP                 string
	AcceptedOddsChange bool
	Legs               []BetLeg
}

// BetLeg is one selection inside a bet, with its price frozen at placement and
// its description denormalised so the bet reads correctly forever.
type BetLeg struct {
	ID            int64
	BetID         int64
	SelectionID   int64
	OddsMilli     int64
	Status        string
	EventName     string
	MarketTitle   string
	SelectionName string
	SportKey      string
	StartsAt      time.Time
}

// LedgerEntry is one side of a balanced money movement.
type LedgerEntry struct {
	ID        int64
	TxnID     int64
	Account   string
	UserID    int64
	AmountSat int64
	At        time.Time
}

// LedgerTxn is a complete money movement whose entries sum to zero.
type LedgerTxn struct {
	ID        int64
	Kind      string
	At        time.Time
	Memo      string
	RefType   string
	RefID     int64
	CreatedBy int64
	Entries   []LedgerEntry
}

// DepositAddress is a bitcoin address issued to one customer.
type DepositAddress struct {
	ID          int64
	UserID      int64
	Address     string
	Provider    string
	ProviderRef string
	Network     string
	CreatedAt   time.Time
	Active      bool
}

// Deposit is an inbound on-chain payment.
type Deposit struct {
	ID            int64
	UserID        int64
	Address       string
	TxID          string
	Vout          int
	AmountSat     int64
	Confirmations int
	Status        string
	FirstSeenAt   time.Time
	CreditedAt    time.Time
	LedgerTxnID   int64
}

// Withdrawal is an outbound payment request.
type Withdrawal struct {
	ID          int64
	UserID      int64
	Address     string
	AmountSat   int64
	FeeSat      int64
	Status      string
	TxID        string
	RequestedAt time.Time
	DecidedAt   time.Time
	DecidedBy   int64
	Reason      string
	LedgerTxnID int64
}

// TotalDebitSat is what leaves the customer's balance: the amount sent plus
// the network fee they cover.
func (w Withdrawal) TotalDebitSat() int64 { return w.AmountSat + w.FeeSat }

// PlayerLimit is a self-imposed cap.
type PlayerLimit struct {
	ID            int64
	UserID        int64
	Kind          string
	Amount        int64
	EffectiveFrom time.Time
	RequestedAt   time.Time
	RevokedAt     time.Time
}

// Exclusion is a cool-off or self-exclusion period.
type Exclusion struct {
	ID        int64
	UserID    int64
	Kind      string
	StartsAt  time.Time
	EndsAt    time.Time
	Permanent bool
	CreatedAt time.Time
	Reason    string
}

// Active reports whether the exclusion bars play at the given moment.
func (e Exclusion) Active(now time.Time) bool {
	if now.Before(e.StartsAt) {
		return false
	}
	return e.Permanent || now.Before(e.EndsAt)
}

// ComplianceFlag is something a compliance officer has to look at.
type ComplianceFlag struct {
	ID         int64
	UserID     int64
	Kind       string
	Severity   string
	Detail     string
	RaisedAt   time.Time
	ResolvedAt time.Time
	ResolvedBy int64
	Resolution string
}

// AuditEntry is an append-only record of a significant action.
type AuditEntry struct {
	ID            int64
	At            time.Time
	ActorUserID   int64
	SubjectUserID int64
	Action        string
	Detail        string
	IP            string
}

// KYCDocument references a document held in an external vault. The document
// itself is never stored here.
type KYCDocument struct {
	ID          int64
	UserID      int64
	Kind        string
	Reference   string
	Status      string
	SubmittedAt time.Time
	ReviewedAt  time.Time
	ReviewedBy  int64
	Note        string
}
