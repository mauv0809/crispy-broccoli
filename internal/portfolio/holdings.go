package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
)

// ErrHoldingNotFound is returned when a holding lookup or sell-side ApplyTrade
// targets a (portfolio, ticker) row that doesn't exist.
var ErrHoldingNotFound = errors.New("holding not found")

// Holdings owns reads and writes against the holdings projection table.
// ApplyTrade is the single entry point for incremental updates from the
// proposal acceptor; Rebuild (added in C4) recomputes from the trade ledger.
type Holdings struct {
	pool *pgxpool.Pool
}

func NewHoldings(pool *pgxpool.Pool) *Holdings {
	return &Holdings{pool: pool}
}

// TradeApplication is the input shape for ApplyTrade. It mirrors a row in
// executed_trades but lives in this package's vocabulary so callers can build
// it without importing internal pgx types. Action must be "buy" or "sell".
type TradeApplication struct {
	PortfolioID int64
	Ticker      string
	Action      string
	Shares      decimal.Decimal
	Price       decimal.Decimal
	Fee         decimal.Decimal
	ExecutedAt  time.Time
}

// ApplyTrade updates the holdings projection for a single trade. Accepts DBTX
// so callers (notably proposal.Acceptor) can compose this with their own
// inserts inside one transaction. The caller is responsible for inserting the
// matching executed_trades row — ApplyTrade only updates the projection.
func (h *Holdings) ApplyTrade(ctx context.Context, db dbutil.DBTX, t TradeApplication) error {
	switch t.Action {
	case "buy":
		return h.applyBuy(ctx, db, t)
	case "sell":
		return h.applySell(ctx, db, t)
	default:
		return fmt.Errorf("invalid trade action: %q", t.Action)
	}
}

// applyBuy adds shares + cost_basis (shares*price + fee) to the holdings row,
// upserting if it doesn't exist. last_trade_at advances to the latest of the
// existing value and the new trade's executed_at (so out-of-order inserts
// don't rewind it).
func (h *Holdings) applyBuy(ctx context.Context, db dbutil.DBTX, t TradeApplication) error {
	cost := t.Shares.Mul(t.Price).Add(t.Fee)
	_, err := db.Exec(ctx, `
		INSERT INTO holdings (portfolio_id, ticker, shares, cost_basis, last_trade_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (portfolio_id, ticker) DO UPDATE
		SET shares = holdings.shares + EXCLUDED.shares,
		    cost_basis = holdings.cost_basis + EXCLUDED.cost_basis,
		    last_trade_at = GREATEST(holdings.last_trade_at, EXCLUDED.last_trade_at)
	`, t.PortfolioID, t.Ticker, t.Shares, cost, t.ExecutedAt)
	if err != nil {
		return fmt.Errorf("applying buy: %w", err)
	}
	return nil
}

// applySell reduces shares and cost_basis. cost_basis reduces *proportionally*
// — the remaining cost basis is (current_cost_basis * remaining_shares /
// current_shares). Fees on a sell don't add to cost_basis (cost_basis tracks
// acquisition cost only). When all shares are sold, the row is deleted —
// holdings has no zero-share placeholder rows.
func (h *Holdings) applySell(ctx context.Context, db dbutil.DBTX, t TradeApplication) error {
	var current Holding
	err := db.QueryRow(ctx, `
		SELECT portfolio_id, ticker, shares, cost_basis, last_trade_at
		FROM holdings WHERE portfolio_id = $1 AND ticker = $2
	`, t.PortfolioID, t.Ticker).Scan(&current.PortfolioID, &current.Ticker, &current.Shares, &current.CostBasis, &current.LastTradeAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("cannot sell %s: %w", t.Ticker, ErrHoldingNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading holding for sell: %w", err)
	}

	if t.Shares.GreaterThan(current.Shares) {
		return fmt.Errorf("cannot sell %s shares of %s; holding has %s",
			t.Shares, t.Ticker, current.Shares)
	}

	if t.Shares.Equal(current.Shares) {
		_, err := db.Exec(ctx,
			`DELETE FROM holdings WHERE portfolio_id = $1 AND ticker = $2`,
			t.PortfolioID, t.Ticker)
		if err != nil {
			return fmt.Errorf("deleting fully-sold holding: %w", err)
		}
		return nil
	}

	// Partial sell. Scale cost_basis by the ratio of remaining shares.
	remaining := current.Shares.Sub(t.Shares)
	ratio := remaining.Div(current.Shares)
	newCostBasis := current.CostBasis.Mul(ratio).Round(2)

	_, err = db.Exec(ctx, `
		UPDATE holdings
		SET shares = $1,
		    cost_basis = $2,
		    last_trade_at = GREATEST(last_trade_at, $3)
		WHERE portfolio_id = $4 AND ticker = $5
	`, remaining, newCostBasis, t.ExecutedAt, t.PortfolioID, t.Ticker)
	if err != nil {
		return fmt.Errorf("updating partially-sold holding: %w", err)
	}
	return nil
}

// Get returns the holding for a (portfolio, ticker), or ErrHoldingNotFound.
func (h *Holdings) Get(ctx context.Context, portfolioID int64, ticker string) (*Holding, error) {
	var hd Holding
	err := h.pool.QueryRow(ctx, `
		SELECT portfolio_id, ticker, shares, cost_basis, last_trade_at
		FROM holdings WHERE portfolio_id = $1 AND ticker = $2
	`, portfolioID, ticker).Scan(&hd.PortfolioID, &hd.Ticker, &hd.Shares, &hd.CostBasis, &hd.LastTradeAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrHoldingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting holding: %w", err)
	}
	return &hd, nil
}

// ListByPortfolio returns all current holdings for a portfolio, alphabetical
// by ticker. Returns an empty slice (not nil) when there are no holdings.
func (h *Holdings) ListByPortfolio(ctx context.Context, portfolioID int64) ([]Holding, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT portfolio_id, ticker, shares, cost_basis, last_trade_at
		FROM holdings WHERE portfolio_id = $1 ORDER BY ticker ASC
	`, portfolioID)
	if err != nil {
		return nil, fmt.Errorf("listing holdings: %w", err)
	}
	defer rows.Close()

	out := make([]Holding, 0)
	for rows.Next() {
		var hd Holding
		if err := rows.Scan(&hd.PortfolioID, &hd.Ticker, &hd.Shares, &hd.CostBasis, &hd.LastTradeAt); err != nil {
			return nil, fmt.Errorf("scanning holding: %w", err)
		}
		out = append(out, hd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing holdings: %w", err)
	}
	return out, nil
}
