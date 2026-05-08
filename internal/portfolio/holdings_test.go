package portfolio_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// seedPortfolio creates a verified-strategy-backed portfolio for holdings tests.
func seedPortfolio(t *testing.T, pool any) *portfolio.Portfolio {
	t.Helper()
	repo := portfolio.NewRepository(testutil.PoolFrom(pool))
	s, vID := seedStrategy(t, pool)
	p, err := repo.Create(context.Background(), portfolio.CreatePortfolioRequest{
		UserID:            systemUserID(t, pool),
		Name:              t.Name() + "-pf",
		StartingCapital:   decimal.NewFromInt(10000),
		StrategyID:        int64(s.ID),
		StrategyVersionID: vID,
		Cadence:           strategy.CadenceQuarterly,
	})
	if err != nil {
		t.Fatalf("seed portfolio: %v", err)
	}
	return p
}

// seedTicker ensures a row exists in companies(ticker) so the holdings FK is
// satisfied. Idempotent.
func seedTicker(t *testing.T, pool any, ticker string) {
	t.Helper()
	_, err := testutil.PoolFrom(pool).Exec(context.Background(),
		`INSERT INTO companies (ticker, name, sector, industry, active)
		 VALUES ($1, $1, '', '', true)
		 ON CONFLICT (ticker) DO NOTHING`,
		ticker)
	if err != nil {
		t.Fatalf("seed ticker %s: %v", ticker, err)
	}
}

func TestApplyTrade_BuyCreatesHolding(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now().UTC()
	err := holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180), Fee: decimal.NewFromInt(2),
		ExecutedAt: now,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	h, err := holdings.Get(ctx, p.ID, "AAPL")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !h.Shares.Equal(decimal.NewFromInt(10)) {
		t.Errorf("shares = %s, want 10", h.Shares)
	}
	wantCost := decimal.NewFromInt(180*10 + 2)
	if !h.CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", h.CostBasis, wantCost)
	}
}

func TestApplyTrade_BuyAddsToExistingHolding(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now().UTC()
	for _, b := range []struct{ shares, price int64 }{{10, 180}, {5, 200}} {
		if err := holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
			PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
			Shares: decimal.NewFromInt(b.shares), Price: decimal.NewFromInt(b.price),
			Fee: decimal.Zero, ExecutedAt: now,
		}); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	h, _ := holdings.Get(ctx, p.ID, "AAPL")
	if !h.Shares.Equal(decimal.NewFromInt(15)) {
		t.Errorf("shares = %s, want 15", h.Shares)
	}
	wantCost := decimal.NewFromInt(180*10 + 200*5)
	if !h.CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", h.CostBasis, wantCost)
	}
}

func TestApplyTrade_SellReducesSharesAndCostBasisProportionally(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now().UTC()
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180),
		Fee: decimal.Zero, ExecutedAt: now,
	})
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "sell",
		Shares: decimal.NewFromInt(4), Price: decimal.NewFromInt(190),
		Fee: decimal.Zero, ExecutedAt: now,
	})

	h, _ := holdings.Get(ctx, p.ID, "AAPL")
	if !h.Shares.Equal(decimal.NewFromInt(6)) {
		t.Errorf("shares = %s, want 6", h.Shares)
	}
	// Cost basis reduces proportionally to remaining shares: 1800 * (6/10) = 1080.
	wantCost := decimal.NewFromInt(1080)
	if !h.CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", h.CostBasis, wantCost)
	}
}

func TestApplyTrade_SellAllRemovesHolding(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now().UTC()
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180),
		Fee: decimal.Zero, ExecutedAt: now,
	})
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "sell",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(190),
		Fee: decimal.Zero, ExecutedAt: now,
	})

	_, err := holdings.Get(ctx, p.ID, "AAPL")
	if err != portfolio.ErrHoldingNotFound {
		t.Errorf("err = %v, want ErrHoldingNotFound", err)
	}
}

func TestApplyTrade_SellWithoutHoldingFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	err := holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "sell",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180),
		Fee: decimal.Zero, ExecutedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Error("expected error selling unheld ticker")
	}
}

func TestApplyTrade_SellMoreThanHeldFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now().UTC()
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(5), Price: decimal.NewFromInt(180),
		Fee: decimal.Zero, ExecutedAt: now,
	})
	err := holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "sell",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180),
		Fee: decimal.Zero, ExecutedAt: now,
	})
	if err == nil {
		t.Error("expected error selling more than held")
	}
}

func TestApplyTrade_InvalidActionFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	err := holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "bogus",
		Shares: decimal.NewFromInt(1), Price: decimal.NewFromInt(1),
		ExecutedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Error("expected error for invalid action")
	}
}

func TestApplyTrade_ListByPortfolio(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	for _, tk := range []string{"AAPL", "MSFT"} {
		seedTicker(t, pool, tk)
		if err := holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
			PortfolioID: p.ID, Ticker: tk, Action: "buy",
			Shares: decimal.NewFromInt(5), Price: decimal.NewFromInt(100),
			Fee: decimal.Zero, ExecutedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("apply %s: %v", tk, err)
		}
	}
	got, err := holdings.ListByPortfolio(ctx, p.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}
