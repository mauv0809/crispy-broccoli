// Package prices owns latest-close lookups against the daily_prices table.
// It satisfies proposal.PriceLookup so the proposal generator can size shares
// against current market prices without depending on the broader strategy
// executor infrastructure.
package prices

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// ErrNotFound is returned when the ticker has no rows in daily_prices.
var ErrNotFound = errors.New("no price for ticker")

// Lookup returns the most recent close price for a ticker. Production wires
// this in cmd/app/main.go; the proposal generator depends on it via the
// proposal.PriceLookup interface.
type Lookup struct {
	pool *pgxpool.Pool
}

func NewLookup(pool *pgxpool.Pool) *Lookup { return &Lookup{pool: pool} }

// Latest returns the most recent close price for the ticker, or ErrNotFound
// if no rows exist. Used by proposal.Generator to size new picks.
func (l *Lookup) Latest(ctx context.Context, ticker string) (decimal.Decimal, error) {
	var p decimal.Decimal
	err := l.pool.QueryRow(ctx, `
		SELECT close FROM daily_prices
		WHERE ticker = $1
		ORDER BY date DESC
		LIMIT 1
	`, ticker).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, fmt.Errorf("%w: %s", ErrNotFound, ticker)
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("getting latest price for %s: %w", ticker, err)
	}
	return p, nil
}
