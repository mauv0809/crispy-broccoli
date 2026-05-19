package portfolio

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// Status is the lifecycle state of a portfolio.
type Status string

const (
	// StatusActive — portfolio is live; scheduler will generate proposals on cadence.
	StatusActive Status = "active"
	// StatusPaused — portfolio is on hold; scheduler skips it. User can resume.
	StatusPaused Status = "paused"
	// StatusArchived — terminal state. Portfolio is hidden from default lists.
	StatusArchived Status = "archived"
)

// Portfolio is a per-user investment vehicle that pins a specific strategy
// version and rebalances on a chosen cadence.
type Portfolio struct {
	ID                int64            `json:"id"`
	UserID            int64            `json:"user_id"`
	Name              string           `json:"name"`
	StartingCapital   decimal.Decimal  `json:"starting_capital"`
	StrategyID        int64            `json:"strategy_id"`
	StrategyVersionID int64            `json:"strategy_version_id"`
	Cadence           strategy.Cadence `json:"cadence"`
	NextRebalanceDue  *time.Time       `json:"next_rebalance_due,omitempty"`
	Status            Status           `json:"status"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

// Holding is a current-state projection of a portfolio's position in a single
// ticker, computed by replaying executed_trades. Rows are deleted when shares
// drop to zero (no zero-share placeholder rows).
type Holding struct {
	PortfolioID int64           `json:"portfolio_id"`
	Ticker      string          `json:"ticker"`
	Shares      decimal.Decimal `json:"shares"`
	CostBasis   decimal.Decimal `json:"cost_basis"`
	LastTradeAt time.Time       `json:"last_trade_at"`
}

// CapitalEvent records a deposit (positive amount) or withdrawal (negative
// amount) into/from a portfolio. Append-only.
type CapitalEvent struct {
	ID          int64           `json:"id"`
	PortfolioID int64           `json:"portfolio_id"`
	ProposalID  *int64          `json:"proposal_id,omitempty"`
	Amount      decimal.Decimal `json:"amount"`
	OccurredAt  time.Time       `json:"occurred_at"`
	RecordedAt  time.Time       `json:"recorded_at"`
	Notes       *string         `json:"notes,omitempty"`
}

// ExecutedTrade is one row in the append-only trade ledger. Action is "buy" or
// "sell" — pick-action labels (buy/sell/add/trim/hold) are normalized to one
// of these by the proposal acceptor before insertion.
type ExecutedTrade struct {
	ID          int64           `json:"id"`
	PortfolioID int64           `json:"portfolio_id"`
	ProposalID  *int64          `json:"proposal_id,omitempty"`
	Ticker      string          `json:"ticker"`
	Action      string          `json:"action"` // "buy" | "sell"
	Shares      decimal.Decimal `json:"shares"`
	Price       decimal.Decimal `json:"price"`
	Fee         decimal.Decimal `json:"fee"`
	ExecutedAt  time.Time       `json:"executed_at"`
	RecordedAt  time.Time       `json:"recorded_at"`
	Notes       *string         `json:"notes,omitempty"`
}
