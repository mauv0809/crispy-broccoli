package proposal

import (
	"time"

	"github.com/shopspring/decimal"
)

// Status is the lifecycle state of a proposal. Once a proposal leaves
// 'pending' it becomes immutable.
type Status string

const (
	// StatusPending — proposal is open for the user to act on. Mutable
	// (the picks JSONB can be regenerated when capital_change changes).
	StatusPending Status = "pending"
	// StatusAccepted — every non-hold pick was either accepted or implicitly
	// satisfied. Trades and (optional) capital event have been emitted.
	StatusAccepted Status = "accepted"
	// StatusPartiallyAccepted — some picks were skipped on acceptance. Cadence
	// still advances; portfolio drift is the user's tolerated outcome.
	StatusPartiallyAccepted Status = "partially_accepted"
	// StatusSkipped — user chose to skip the entire rebalance. No trades
	// emitted; cadence still advances by one period (no bunching).
	StatusSkipped Status = "skipped"
	// StatusExpired — auto-applied when the next cadence ticks while a prior
	// proposal is still pending. Acceptor refuses to act on expired rows.
	StatusExpired Status = "expired"
)

// Action is the per-row label assigned at generation time. It describes how
// the pick relates to the portfolio's current holdings — the acceptor
// normalises this into a 'buy' or 'sell' executed_trades row (or no row at
// all for 'hold').
type Action string

const (
	// ActionBuy — ticker is not currently held; buy fresh.
	ActionBuy Action = "buy"
	// ActionSell — ticker is held but not in the new picks; sell entirely.
	ActionSell Action = "sell"
	// ActionAdd — ticker is held; target shares > current shares.
	ActionAdd Action = "add"
	// ActionTrim — ticker is held; target shares < current shares.
	ActionTrim Action = "trim"
	// ActionHold — ticker is held at exactly the target share count. No
	// trade row is emitted at acceptance time.
	ActionHold Action = "hold"
)

// Pick is one row in a proposal's picks JSONB array. The whole picks slice
// is read and written as a unit; we never query individual picks. Whole-array
// replacement on capital_change recompute is the only mutation while pending.
type Pick struct {
	Ticker          string          `json:"ticker"`
	Action          Action          `json:"action"`
	TargetWeight    decimal.Decimal `json:"target_weight"`     // 0..1
	TargetShares    decimal.Decimal `json:"target_shares"`     // floor((weight × deploy_amount) / price)
	CurrentShares   decimal.Decimal `json:"current_shares"`    // 0 if not currently held
	PriceAtProposal decimal.Decimal `json:"price_at_proposal"` // latest close at generation time
}

// Proposal is the central aggregate of the rebalance loop. Created by the
// scheduler (or by Service.CreatePortfolio's first-proposal hook), accepted
// or skipped by the user via handlers. Append-only after the status leaves
// 'pending'.
type Proposal struct {
	ID                    int64           `json:"id"`
	PortfolioID           int64           `json:"portfolio_id"`
	StrategyVersionID     int64           `json:"strategy_version_id"`
	GeneratedAt           time.Time       `json:"generated_at"`
	MarketValueAtProposal decimal.Decimal `json:"market_value_at_proposal"`
	CapitalChange         decimal.Decimal `json:"capital_change"` // signed; negative = withdrawal
	DeployAmount          decimal.Decimal `json:"deploy_amount"`  // = market_value + capital_change
	Picks                 []Pick          `json:"picks"`
	Status                Status          `json:"status"`
	ResolvedAt            *time.Time      `json:"resolved_at,omitempty"`
	NotificationSentAt    *time.Time      `json:"notification_sent_at,omitempty"`
	ReminderSentAt        *time.Time      `json:"reminder_sent_at,omitempty"`
}
