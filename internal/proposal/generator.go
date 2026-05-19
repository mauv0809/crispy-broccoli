package proposal

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// StrategyExecutor abstracts the strategy-running surface used by the
// Generator. The real implementation in internal/strategy executes the
// configured rules against the latest financial_metrics + daily_prices and
// returns ranked recommendations.
type StrategyExecutor interface {
	RunWithRules(ctx context.Context, rules []byte) ([]strategy.Recommendation, error)
}

// HoldingsLister returns the portfolio's current holdings. Satisfied by
// *portfolio.Holdings in production.
type HoldingsLister interface {
	ListByPortfolio(ctx context.Context, portfolioID int64) ([]portfolio.Holding, error)
}

// PriceLookup returns the latest close price for a ticker. The proposal
// generator uses this for both the new picks (sizing the buy) and any sells
// (sourcing PriceAtProposal for the executed_trade later).
type PriceLookup interface {
	Latest(ctx context.Context, ticker string) (decimal.Decimal, error)
}

// Generator turns strategy recommendations into a ready-to-store proposal.
// Pure with respect to its dependencies — no DB access of its own; everything
// is mediated through the three injected interfaces.
type Generator struct {
	executor StrategyExecutor
	holdings HoldingsLister
	prices   PriceLookup
}

// NewGenerator wires the three dependencies. Call sites in production live in
// cmd/app/main.go (Phase H); tests pass stubs.
func NewGenerator(e StrategyExecutor, h HoldingsLister, p PriceLookup) *Generator {
	return &Generator{executor: e, holdings: h, prices: p}
}

// GenerateInput is what the scheduler/handler passes per generation. The
// MarketValue + CapitalChange are computed by the caller (scheduler computes
// from holdings + prices; recompute handler echoes back the user-edited
// CapitalChange). StrategyLimit truncates the executor's output (matches the
// strategy.rules.limit field).
type GenerateInput struct {
	PortfolioID   int64
	Rules         []byte
	MarketValue   decimal.Decimal
	CapitalChange decimal.Decimal
	StrategyLimit int
	// Weights, if supplied, must align with the truncated recs slice. If nil,
	// equal weight is used.
	Weights []decimal.Decimal
}

// GeneratePicks runs the executor, sizes shares against deploy_amount, and
// labels each pick's action by diffing against current holdings. Returns the
// full slice (buys + sells + adds + trims + holds) ready to be stored.
//
// The diff logic emits one row per pick from the executor, plus one extra
// 'sell' row per held ticker that doesn't appear in the new picks.
func (g *Generator) GeneratePicks(ctx context.Context, in GenerateInput) ([]Pick, error) {
	recs, err := g.executor.RunWithRules(ctx, in.Rules)
	if err != nil {
		return nil, fmt.Errorf("strategy executor: %w", err)
	}
	if in.StrategyLimit > 0 && len(recs) > in.StrategyLimit {
		recs = recs[:in.StrategyLimit]
	}

	deploy := in.MarketValue.Add(in.CapitalChange)

	current, err := g.holdings.ListByPortfolio(ctx, in.PortfolioID)
	if err != nil {
		return nil, fmt.Errorf("listing holdings: %w", err)
	}
	currentByTicker := make(map[string]decimal.Decimal, len(current))
	for _, h := range current {
		currentByTicker[h.Ticker] = h.Shares
	}

	weights := normaliseWeights(in.Weights, len(recs))
	out := make([]Pick, 0, len(recs)+len(current))
	picked := make(map[string]struct{}, len(recs))

	for i, r := range recs {
		price, err := g.prices.Latest(ctx, r.Ticker)
		if err != nil {
			return nil, fmt.Errorf("price for %s: %w", r.Ticker, err)
		}
		if price.IsZero() {
			return nil, fmt.Errorf("zero price for %s", r.Ticker)
		}

		alloc := deploy.Mul(weights[i])
		target := alloc.Div(price).Floor() // whole-share rounding

		curr := currentByTicker[r.Ticker]
		var action Action
		switch {
		case curr.IsZero():
			action = ActionBuy
		case target.GreaterThan(curr):
			action = ActionAdd
		case target.LessThan(curr):
			action = ActionTrim
		default:
			action = ActionHold
		}

		out = append(out, Pick{
			Ticker:          r.Ticker,
			Action:          action,
			TargetWeight:    weights[i],
			TargetShares:    target,
			CurrentShares:   curr,
			PriceAtProposal: price,
		})
		picked[r.Ticker] = struct{}{}
	}

	// Anything currently held but not picked → sell entirely.
	for _, h := range current {
		if _, ok := picked[h.Ticker]; ok {
			continue
		}
		price, err := g.prices.Latest(ctx, h.Ticker)
		if err != nil {
			return nil, fmt.Errorf("price for %s (sell): %w", h.Ticker, err)
		}
		out = append(out, Pick{
			Ticker:          h.Ticker,
			Action:          ActionSell,
			TargetWeight:    decimal.Zero,
			TargetShares:    decimal.Zero,
			CurrentShares:   h.Shares,
			PriceAtProposal: price,
		})
	}

	return out, nil
}

// normaliseWeights returns the input weights if they match n; otherwise an
// equal-weight slice. Empty n returns nil (caller has no picks to weight).
func normaliseWeights(in []decimal.Decimal, n int) []decimal.Decimal {
	if n == 0 {
		return nil
	}
	if len(in) == n {
		return in
	}
	w := decimal.NewFromInt(1).Div(decimal.NewFromInt(int64(n)))
	out := make([]decimal.Decimal, n)
	for i := range out {
		out[i] = w
	}
	return out
}
