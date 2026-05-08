package portfolio

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Performance computes portfolio-level analytics: a current snapshot today,
// daily time-series replay (Phase I2). All reads — never writes; safe to use
// from any goroutine that has a pool reference.
type Performance struct {
	pool *pgxpool.Pool
}

// NewPerformance wires the pool. Production wires this in cmd/app/main.go;
// the portfolios handler calls Current to render the value/return summary.
func NewPerformance(pool *pgxpool.Pool) *Performance {
	return &Performance{pool: pool}
}

// Snapshot is a point-in-time view of a portfolio's value and return. The
// six numeric fields satisfy the canonical "what's it worth, how much have
// I put in, what's the return" question. AsOf is set to time.Now() at
// computation time — useful for cache invalidation later.
type Snapshot struct {
	PortfolioID   int64
	AsOf          time.Time
	Cash          decimal.Decimal
	HoldingsValue decimal.Decimal
	MarketValue   decimal.Decimal // = Cash + HoldingsValue
	NetInvested   decimal.Decimal // starting_capital + Σ(capital_events.amount)
	ReturnAmount  decimal.Decimal // MarketValue − NetInvested
	ReturnPct     decimal.Decimal // ReturnAmount / NetInvested × 100, rounded(2)
}

// Current computes the snapshot in one Postgres round-trip. Holdings are
// valued at the most recent close from daily_prices. Tickers without price
// data contribute 0 to HoldingsValue (silently — production would surface
// this somewhere visible, but the v1 UX is that "no price = treat as zero
// position" since an unpriced ticker is itself a data-quality issue).
func (p *Performance) Current(ctx context.Context, portfolioID int64) (*Snapshot, error) {
	const q = `
WITH params AS (
    SELECT
        p.id,
        p.starting_capital
    FROM portfolios p
    WHERE p.id = $1
),
capital AS (
    SELECT COALESCE(SUM(amount), 0) AS amount
    FROM capital_events
    WHERE portfolio_id = $1
),
trade_flow AS (
    SELECT
        COALESCE(SUM(CASE WHEN action='sell' THEN shares*price ELSE -shares*price END), 0) AS net,
        COALESCE(SUM(fee), 0) AS fees
    FROM executed_trades
    WHERE portfolio_id = $1
),
holdings_value AS (
    SELECT COALESCE(SUM(h.shares * dp.close), 0) AS amount
    FROM holdings h
    LEFT JOIN LATERAL (
        SELECT close FROM daily_prices
        WHERE ticker = h.ticker
        ORDER BY date DESC LIMIT 1
    ) dp ON true
    WHERE h.portfolio_id = $1
)
SELECT
    p.starting_capital + c.amount + t.net - t.fees      AS cash,
    h.amount                                            AS holdings_value,
    p.starting_capital + c.amount                       AS net_invested
FROM params p, capital c, trade_flow t, holdings_value h
`
	var snap Snapshot
	snap.PortfolioID = portfolioID
	snap.AsOf = time.Now().UTC()

	err := p.pool.QueryRow(ctx, q, portfolioID).Scan(
		&snap.Cash, &snap.HoldingsValue, &snap.NetInvested,
	)
	if err != nil {
		return nil, fmt.Errorf("computing portfolio snapshot: %w", err)
	}
	snap.MarketValue = snap.Cash.Add(snap.HoldingsValue)
	snap.ReturnAmount = snap.MarketValue.Sub(snap.NetInvested)
	if snap.NetInvested.IsZero() {
		snap.ReturnPct = decimal.Zero
	} else {
		snap.ReturnPct = snap.ReturnAmount.Div(snap.NetInvested).
			Mul(decimal.NewFromInt(100)).Round(2)
	}
	return &snap, nil
}
