package proposal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// stubExecutor returns a fixed list of recommendations regardless of input.
type stubExecutor struct {
	recs []strategy.Recommendation
	err  error
}

func (s stubExecutor) RunWithRules(ctx context.Context, rules []byte) ([]strategy.Recommendation, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.recs, nil
}

// stubHoldings is a map keyed by ticker; iterates as portfolio.Holdings.
type stubHoldings map[string]decimal.Decimal

func (s stubHoldings) ListByPortfolio(ctx context.Context, portfolioID int64) ([]portfolio.Holding, error) {
	out := make([]portfolio.Holding, 0, len(s))
	for ticker, shares := range s {
		out = append(out, portfolio.Holding{PortfolioID: portfolioID, Ticker: ticker, Shares: shares})
	}
	return out, nil
}

// stubPrices map ticker → latest price; missing ticker → error.
type stubPrices map[string]decimal.Decimal

func (s stubPrices) Latest(ctx context.Context, ticker string) (decimal.Decimal, error) {
	p, ok := s[ticker]
	if !ok {
		return decimal.Zero, errors.New("no price for " + ticker)
	}
	return p, nil
}

func TestGenerator_NewPortfolioAllBuysEqualWeight(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{
			{Ticker: "AAPL", Score: 0.9},
			{Ticker: "MSFT", Score: 0.8},
		}},
		stubHoldings{},
		stubPrices{"AAPL": decimal.NewFromInt(180), "MSFT": decimal.NewFromInt(400)},
	)

	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID:   1,
		Rules:         []byte(`{}`),
		MarketValue:   decimal.NewFromInt(10000),
		CapitalChange: decimal.Zero,
		StrategyLimit: 2,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(picks) != 2 {
		t.Fatalf("len = %d, want 2", len(picks))
	}
	for _, p := range picks {
		if p.Action != proposal.ActionBuy {
			t.Errorf("%s: action = %s, want buy", p.Ticker, p.Action)
		}
	}
	// Equal weight: 0.5 each. Deploy = 10000.
	// AAPL: floor(5000/180) = 27. MSFT: floor(5000/400) = 12.
	for _, p := range picks {
		switch p.Ticker {
		case "AAPL":
			if !p.TargetShares.Equal(decimal.NewFromInt(27)) {
				t.Errorf("AAPL shares = %s, want 27", p.TargetShares)
			}
		case "MSFT":
			if !p.TargetShares.Equal(decimal.NewFromInt(12)) {
				t.Errorf("MSFT shares = %s, want 12", p.TargetShares)
			}
		}
	}
}

func TestGenerator_HoldingNotInPicksBecomesSell(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{
			{Ticker: "AAPL", Score: 0.9},
		}},
		stubHoldings{"GOOG": decimal.NewFromInt(5)},
		stubPrices{"AAPL": decimal.NewFromInt(180), "GOOG": decimal.NewFromInt(140)},
	)

	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID:   1,
		Rules:         []byte(`{}`),
		MarketValue:   decimal.NewFromInt(10000),
		CapitalChange: decimal.Zero,
		StrategyLimit: 1,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(picks) != 2 {
		t.Fatalf("len = %d, want 2 (1 buy + 1 sell)", len(picks))
	}

	bySymbol := map[string]proposal.Pick{}
	for _, p := range picks {
		bySymbol[p.Ticker] = p
	}
	if bySymbol["GOOG"].Action != proposal.ActionSell {
		t.Errorf("GOOG action = %s, want sell", bySymbol["GOOG"].Action)
	}
	if !bySymbol["GOOG"].CurrentShares.Equal(decimal.NewFromInt(5)) {
		t.Errorf("GOOG current_shares = %s, want 5", bySymbol["GOOG"].CurrentShares)
	}
	if bySymbol["AAPL"].Action != proposal.ActionBuy {
		t.Errorf("AAPL action = %s, want buy", bySymbol["AAPL"].Action)
	}
}

func TestGenerator_ActionAdd(t *testing.T) {
	// Held=50, target=100 → add (delta 50).
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{{Ticker: "A", Score: 1}}},
		stubHoldings{"A": decimal.NewFromInt(50)},
		stubPrices{"A": decimal.NewFromInt(100)},
	)
	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue: decimal.NewFromInt(10000), CapitalChange: decimal.Zero,
		StrategyLimit: 1,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if picks[0].Action != proposal.ActionAdd {
		t.Errorf("action = %s, want add", picks[0].Action)
	}
	if !picks[0].TargetShares.Equal(decimal.NewFromInt(100)) {
		t.Errorf("target_shares = %s, want 100", picks[0].TargetShares)
	}
}

func TestGenerator_ActionTrim(t *testing.T) {
	// Held=200, target=100 → trim.
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{{Ticker: "A", Score: 1}}},
		stubHoldings{"A": decimal.NewFromInt(200)},
		stubPrices{"A": decimal.NewFromInt(100)},
	)
	picks, _ := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue: decimal.NewFromInt(10000), CapitalChange: decimal.Zero,
		StrategyLimit: 1,
	})
	if picks[0].Action != proposal.ActionTrim {
		t.Errorf("action = %s, want trim", picks[0].Action)
	}
}

func TestGenerator_ActionHold(t *testing.T) {
	// Held=100, target=100 → hold (exact match).
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{{Ticker: "A", Score: 1}}},
		stubHoldings{"A": decimal.NewFromInt(100)},
		stubPrices{"A": decimal.NewFromInt(100)},
	)
	picks, _ := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue: decimal.NewFromInt(10000), CapitalChange: decimal.Zero,
		StrategyLimit: 1,
	})
	if picks[0].Action != proposal.ActionHold {
		t.Errorf("action = %s, want hold", picks[0].Action)
	}
}

func TestGenerator_CapitalChangeAdjustsDeployUp(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{{Ticker: "A", Score: 1}}},
		stubHoldings{},
		stubPrices{"A": decimal.NewFromInt(100)},
	)
	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue:   decimal.NewFromInt(5000),
		CapitalChange: decimal.NewFromInt(5000),
		StrategyLimit: 1,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Deploy = 5000 + 5000 = 10000. weight=1.0, price=100 → 100 shares.
	if !picks[0].TargetShares.Equal(decimal.NewFromInt(100)) {
		t.Errorf("target_shares = %s, want 100", picks[0].TargetShares)
	}
}

func TestGenerator_CapitalChangeAdjustsDeployDown(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{{Ticker: "A", Score: 1}}},
		stubHoldings{},
		stubPrices{"A": decimal.NewFromInt(100)},
	)
	picks, _ := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue:   decimal.NewFromInt(10000),
		CapitalChange: decimal.NewFromInt(-3000),
		StrategyLimit: 1,
	})
	// Deploy = 10000 - 3000 = 7000 → 70 shares.
	if !picks[0].TargetShares.Equal(decimal.NewFromInt(70)) {
		t.Errorf("target_shares = %s, want 70", picks[0].TargetShares)
	}
}

func TestGenerator_LimitTruncatesRecs(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{
			{Ticker: "A", Score: 1.0},
			{Ticker: "B", Score: 0.9},
			{Ticker: "C", Score: 0.8},
			{Ticker: "D", Score: 0.7},
		}},
		stubHoldings{},
		stubPrices{"A": decimal.NewFromInt(100), "B": decimal.NewFromInt(100)},
	)
	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue: decimal.NewFromInt(10000), CapitalChange: decimal.Zero,
		StrategyLimit: 2,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// 2 buys (truncated to first 2) + 0 sells = 2 picks.
	if len(picks) != 2 {
		t.Errorf("len = %d, want 2", len(picks))
	}
	tickers := map[string]bool{}
	for _, p := range picks {
		tickers[p.Ticker] = true
	}
	if !tickers["A"] || !tickers["B"] {
		t.Errorf("expected A and B; got %v", tickers)
	}
}

func TestGenerator_ExecutorErrorPropagates(t *testing.T) {
	want := errors.New("executor failure")
	g := proposal.NewGenerator(
		stubExecutor{err: want},
		stubHoldings{},
		stubPrices{},
	)
	_, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue:   decimal.NewFromInt(10000),
		StrategyLimit: 1,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want chain to include %v", err, want)
	}
}

func TestGenerator_MissingPriceFails(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{{Ticker: "X", Score: 1}}},
		stubHoldings{},
		stubPrices{}, // no prices
	)
	_, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID: 1, Rules: []byte(`{}`),
		MarketValue: decimal.NewFromInt(10000), StrategyLimit: 1,
	})
	if err == nil {
		t.Error("expected error for missing price")
	}
}
