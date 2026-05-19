package portfolio_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// seedPriceForCompany inserts a daily_prices row used by the Holdings ×
// latest_close lookup in Performance.Current. Idempotent on (ticker, date).
func seedPriceForCompany(t *testing.T, pool any, ticker string, date time.Time, close int64) {
	t.Helper()
	p := testutil.PoolFrom(pool)
	_, err := p.Exec(context.Background(), `
		INSERT INTO daily_prices (ticker, date, open, high, low, close, volume)
		VALUES ($1, $2, $3, $3, $3, $3, 1000)
		ON CONFLICT (ticker, date) DO UPDATE SET close = EXCLUDED.close
	`, ticker, date, close)
	if err != nil {
		t.Fatalf("seed price for %s: %v", ticker, err)
	}
}

func TestPerformance_NewPortfolioReturnsStartingCapital(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool) // starting_capital = 10000, no trades, no holdings

	perf := portfolio.NewPerformance(pool)
	snap, err := perf.Current(ctx, p.ID)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	want := decimal.NewFromInt(10000)
	if !snap.Cash.Equal(want) {
		t.Errorf("Cash = %s, want %s", snap.Cash, want)
	}
	if !snap.HoldingsValue.Equal(decimal.Zero) {
		t.Errorf("HoldingsValue = %s, want 0", snap.HoldingsValue)
	}
	if !snap.MarketValue.Equal(want) {
		t.Errorf("MarketValue = %s, want %s", snap.MarketValue, want)
	}
	if !snap.NetInvested.Equal(want) {
		t.Errorf("NetInvested = %s, want %s", snap.NetInvested, want)
	}
	if !snap.ReturnAmount.Equal(decimal.Zero) {
		t.Errorf("ReturnAmount = %s, want 0", snap.ReturnAmount)
	}
	if !snap.ReturnPct.Equal(decimal.Zero) {
		t.Errorf("ReturnPct = %s, want 0", snap.ReturnPct)
	}
}

func TestPerformance_AfterBuyAndPriceIncrease(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool) // starting_capital = 10000
	seedTicker(t, pool, "AAPL")
	now := time.Now().UTC().Truncate(24 * time.Hour)
	seedPriceForCompany(t, pool, "AAPL", now, 200) // current close = $200

	// Bought 50 shares @ $180, fee $0.
	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'AAPL', 'buy', 50, 180, 0, $2)
	`, p.ID, now); err != nil {
		t.Fatalf("seed trade: %v", err)
	}
	if err := portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	perf := portfolio.NewPerformance(pool)
	snap, err := perf.Current(ctx, p.ID)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	// Cash = 10000 - (50*180) = 1000.
	wantCash := decimal.NewFromInt(1000)
	if !snap.Cash.Equal(wantCash) {
		t.Errorf("Cash = %s, want %s", snap.Cash, wantCash)
	}
	// HoldingsValue = 50 * 200 = 10000.
	wantHoldings := decimal.NewFromInt(10000)
	if !snap.HoldingsValue.Equal(wantHoldings) {
		t.Errorf("HoldingsValue = %s, want %s", snap.HoldingsValue, wantHoldings)
	}
	// MarketValue = 1000 + 10000 = 11000.
	wantMarket := decimal.NewFromInt(11000)
	if !snap.MarketValue.Equal(wantMarket) {
		t.Errorf("MarketValue = %s, want %s", snap.MarketValue, wantMarket)
	}
	// NetInvested = 10000 (no capital events).
	if !snap.NetInvested.Equal(decimal.NewFromInt(10000)) {
		t.Errorf("NetInvested = %s, want 10000", snap.NetInvested)
	}
	// ReturnAmount = 11000 - 10000 = 1000.
	if !snap.ReturnAmount.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("ReturnAmount = %s, want 1000", snap.ReturnAmount)
	}
	// ReturnPct = 1000 / 10000 * 100 = 10%.
	if !snap.ReturnPct.Equal(decimal.NewFromInt(10)) {
		t.Errorf("ReturnPct = %s, want 10", snap.ReturnPct)
	}
}

func TestPerformance_FeeReducesCash(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")
	now := time.Now().UTC().Truncate(24 * time.Hour)
	seedPriceForCompany(t, pool, "AAPL", now, 100)

	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'AAPL', 'buy', 10, 50, 5, $2)
	`, p.ID, now); err != nil {
		t.Fatalf("seed trade: %v", err)
	}
	_ = portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID)

	perf := portfolio.NewPerformance(pool)
	snap, _ := perf.Current(ctx, p.ID)

	// Cash = 10000 - (10*50) - 5 = 9495.
	wantCash := decimal.NewFromInt(9495)
	if !snap.Cash.Equal(wantCash) {
		t.Errorf("Cash = %s, want %s", snap.Cash, wantCash)
	}
}

func TestPerformance_CapitalEventBumpsNetInvested(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool) // starting_capital = 10000
	now := time.Now().UTC()

	// Deposit $5000.
	if _, err := pool.Exec(ctx, `
		INSERT INTO capital_events (portfolio_id, amount, occurred_at)
		VALUES ($1, 5000, $2)
	`, p.ID, now); err != nil {
		t.Fatalf("seed deposit: %v", err)
	}

	perf := portfolio.NewPerformance(pool)
	snap, _ := perf.Current(ctx, p.ID)
	want := decimal.NewFromInt(15000)
	if !snap.NetInvested.Equal(want) {
		t.Errorf("NetInvested = %s, want %s", snap.NetInvested, want)
	}
	if !snap.Cash.Equal(want) {
		t.Errorf("Cash = %s, want %s (deposit increases cash too)", snap.Cash, want)
	}
	// Withdrawal of $2000.
	if _, err := pool.Exec(ctx, `
		INSERT INTO capital_events (portfolio_id, amount, occurred_at)
		VALUES ($1, -2000, $2)
	`, p.ID, now); err != nil {
		t.Fatalf("seed withdrawal: %v", err)
	}
	snap2, _ := perf.Current(ctx, p.ID)
	want2 := decimal.NewFromInt(13000)
	if !snap2.NetInvested.Equal(want2) {
		t.Errorf("NetInvested after withdrawal = %s, want %s", snap2.NetInvested, want2)
	}
}

func TestPerformance_NegativeReturn(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")
	now := time.Now().UTC().Truncate(24 * time.Hour)
	seedPriceForCompany(t, pool, "AAPL", now, 150) // bought at 200, current 150

	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'AAPL', 'buy', 50, 200, 0, $2)
	`, p.ID, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID)

	perf := portfolio.NewPerformance(pool)
	snap, _ := perf.Current(ctx, p.ID)
	// Cash = 10000 - (50*200) = 0.
	// HoldingsValue = 50*150 = 7500.
	// MarketValue = 7500.
	// NetInvested = 10000.
	// Return = -2500. Pct = -25%.
	if !snap.MarketValue.Equal(decimal.NewFromInt(7500)) {
		t.Errorf("MarketValue = %s, want 7500", snap.MarketValue)
	}
	if !snap.ReturnAmount.Equal(decimal.NewFromInt(-2500)) {
		t.Errorf("ReturnAmount = %s, want -2500", snap.ReturnAmount)
	}
	if !snap.ReturnPct.Equal(decimal.NewFromInt(-25)) {
		t.Errorf("ReturnPct = %s, want -25", snap.ReturnPct)
	}
}

func seedSPY(t *testing.T, pool any, date time.Time, close int64) {
	t.Helper()
	p := testutil.PoolFrom(pool)
	// Ensure the SPY benchmark row exists.
	_, _ = p.Exec(context.Background(),
		`INSERT INTO benchmarks (ticker, name) VALUES ('SPY', 'S&P 500 ETF') ON CONFLICT (ticker) DO NOTHING`)
	_, err := p.Exec(context.Background(), `
		INSERT INTO benchmark_prices (ticker, date, open, high, low, close, volume)
		VALUES ('SPY', $1, $2, $2, $2, $2, 1000)
		ON CONFLICT (ticker, date) DO UPDATE SET close = EXCLUDED.close
	`, date, close)
	if err != nil {
		t.Fatalf("seed SPY: %v", err)
	}
}

func TestPerformance_TimeSeriesNormalisedAgainstSPY(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool) // starting_capital = 10000
	seedTicker(t, pool, "AAPL")

	day0 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	day1 := day0.AddDate(0, 0, 1)
	day2 := day0.AddDate(0, 0, 2)
	seedPriceForCompany(t, pool, "AAPL", day0, 180)
	seedPriceForCompany(t, pool, "AAPL", day1, 200)
	seedPriceForCompany(t, pool, "AAPL", day2, 210)
	seedSPY(t, pool, day0, 100)
	seedSPY(t, pool, day1, 110)
	seedSPY(t, pool, day2, 105)

	// Trade on day0: bought 50 shares of AAPL @ 180. Cash = 10000 - 9000 = 1000.
	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'AAPL', 'buy', 50, 180, 0, $2)
	`, p.ID, day0); err != nil {
		t.Fatalf("seed trade: %v", err)
	}

	perf := portfolio.NewPerformance(pool)
	series, err := perf.TimeSeries(ctx, p.ID, day0, day2)
	if err != nil {
		t.Fatalf("time series: %v", err)
	}
	if len(series.Points) != 3 {
		t.Fatalf("len = %d, want 3", len(series.Points))
	}

	// day0 value: 1000 cash + 50 × 180 = 10000.
	wantDay0 := decimal.NewFromInt(10000)
	if !series.Points[0].PortfolioValue.Equal(wantDay0) {
		t.Errorf("day0 value = %s, want %s", series.Points[0].PortfolioValue, wantDay0)
	}
	// Normalised: day0 = 1.0.
	if !series.Points[0].PortfolioNormalised.Equal(decimal.NewFromInt(1)) {
		t.Errorf("day0 normalised = %s, want 1", series.Points[0].PortfolioNormalised)
	}
	if !series.Points[0].SPYNormalised.Equal(decimal.NewFromInt(1)) {
		t.Errorf("day0 SPY normalised = %s, want 1", series.Points[0].SPYNormalised)
	}

	// day1 value: cash 1000 + 50 × 200 = 11000. Normalised = 11000/10000 = 1.1.
	wantDay1 := decimal.NewFromInt(11000)
	if !series.Points[1].PortfolioValue.Equal(wantDay1) {
		t.Errorf("day1 value = %s, want %s", series.Points[1].PortfolioValue, wantDay1)
	}
	wantDay1Norm := decimal.NewFromFloat(1.1)
	if !series.Points[1].PortfolioNormalised.Equal(wantDay1Norm) {
		t.Errorf("day1 portfolio_normalised = %s, want %s", series.Points[1].PortfolioNormalised, wantDay1Norm)
	}
	// SPY day1 normalised = 110/100 = 1.1.
	if !series.Points[1].SPYNormalised.Equal(wantDay1Norm) {
		t.Errorf("day1 SPY normalised = %s, want %s", series.Points[1].SPYNormalised, wantDay1Norm)
	}

	// day2 value: cash 1000 + 50 × 210 = 11500. Normalised = 1.15.
	wantDay2 := decimal.NewFromInt(11500)
	if !series.Points[2].PortfolioValue.Equal(wantDay2) {
		t.Errorf("day2 value = %s, want %s", series.Points[2].PortfolioValue, wantDay2)
	}
	wantDay2Norm := decimal.NewFromFloat(1.15)
	if !series.Points[2].PortfolioNormalised.Equal(wantDay2Norm) {
		t.Errorf("day2 portfolio_normalised = %s, want %s", series.Points[2].PortfolioNormalised, wantDay2Norm)
	}
	// SPY day2 = 105/100 = 1.05.
	wantSPYDay2 := decimal.NewFromFloat(1.05)
	if !series.Points[2].SPYNormalised.Equal(wantSPYDay2) {
		t.Errorf("day2 SPY normalised = %s, want %s", series.Points[2].SPYNormalised, wantSPYDay2)
	}
}

func TestPerformance_TimeSeries_HandlesMissingPriceWithLastSeen(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool)
	// Use MSFT (not AAPL) to avoid cross-test price pollution: other tests seed
	// AAPL prices for the same 2026-01-02..04 window.
	seedTicker(t, pool, "MSFT")

	day0 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	day1 := day0.AddDate(0, 0, 1) // missing price — LastSeen should fall back to day0
	day2 := day0.AddDate(0, 0, 2) // missing price — LastSeen should fall back to day0
	seedPriceForCompany(t, pool, "MSFT", day0, 100)
	seedSPY(t, pool, day0, 100)

	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'MSFT', 'buy', 10, 100, 0, $2)
	`, p.ID, day0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	perf := portfolio.NewPerformance(pool)
	series, err := perf.TimeSeries(ctx, p.ID, day0, day2)
	if err != nil {
		t.Fatalf("time series: %v", err)
	}
	if len(series.Points) != 3 {
		t.Fatalf("len = %d, want 3", len(series.Points))
	}
	// All three days should value MSFT at 100 (last-seen price from day0).
	want := decimal.NewFromInt(10000) // cash 9000 + 10 × 100 = 10000
	for i, pt := range series.Points {
		if !pt.PortfolioValue.Equal(want) {
			t.Errorf("day %d value = %s, want %s (last-seen price)", i, pt.PortfolioValue, want)
		}
	}
	_ = day1
	_ = day2
}
