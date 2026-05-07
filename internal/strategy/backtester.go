package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mauv0809/crispy-broccoli/internal/db"
	"github.com/mauv0809/crispy-broccoli/internal/ingest"
)

// Backtester runs historical simulations of strategies
type Backtester struct {
	executor     *Executor
	repo         *db.Repository
	nasdaqClient *ingest.Client       // Nasdaq Data Link for SEP equity prices
	tiingoClient *ingest.TiingoClient // Tiingo for ETF benchmark prices (SPY, etc.)
}

// NewBacktester creates a new backtester
func NewBacktester(executor *Executor, repo *db.Repository, nasdaqClient *ingest.Client, tiingoClient *ingest.TiingoClient) *Backtester {
	return &Backtester{
		executor:     executor,
		repo:         repo,
		nasdaqClient: nasdaqClient,
		tiingoClient: tiingoClient,
	}
}

// Run executes a backtest with the given configuration
func (b *Backtester) Run(ctx context.Context, strategy *Strategy, config BacktestConfig) (*BacktestResult, error) {
	startTime := time.Now()

	// Apply defaults
	if config.LagDays == 0 {
		config.LagDays = 60
	}
	if config.InitialCapital == 0 {
		config.InitialCapital = 10000
	}
	if config.RebalanceFreq == "" {
		config.RebalanceFreq = "semi-annual"
	}

	// Generate rebalance dates
	rebalanceDates := b.generateRebalanceDates(config.StartDate, config.EndDate, config.RebalanceFreq)
	if len(rebalanceDates) == 0 {
		return nil, fmt.Errorf("no rebalance dates generated for period %s to %s", config.StartDate.Format("2006-01-02"), config.EndDate.Format("2006-01-02"))
	}

	slog.Info("backtest rebalance dates", "count", len(rebalanceDates), "start", rebalanceDates[0].Format("2006-01-02"), "end", rebalanceDates[len(rebalanceDates)-1].Format("2006-01-02"))

	// Run strategy at each rebalance date and track portfolios
	var periods []BacktestPeriod
	var portfolioCurve []CurvePoint
	cumulativeValue := 1.0 // Start at 1.0 (normalized)

	for i, rebDate := range rebalanceDates {
		// Determine period end date
		var periodEnd time.Time
		if i < len(rebalanceDates)-1 {
			periodEnd = rebalanceDates[i+1]
		} else {
			periodEnd = config.EndDate
		}

		// Run strategy as of rebalance date
		result, err := b.executor.ExecuteAsOf(ctx, strategy, rebDate, config.LagDays)
		if err != nil {
			slog.Warn("backtest strategy execution error", "date", rebDate.Format("2006-01-02"), "error", err)
			continue
		}

		if len(result.Recommendations) == 0 {
			slog.Warn("backtest no recommendations at rebalance date", "date", rebDate.Format("2006-01-02"))
			continue
		}

		// Get tickers, weights, and fallback info
		tickers := make([]string, len(result.Recommendations))
		fallbackFlags := make([]bool, len(result.Recommendations))
		for j, rec := range result.Recommendations {
			tickers[j] = rec.Ticker
			fallbackFlags[j] = rec.IsFallback
		}
		weights := GetWeightsWithCustom(len(tickers), strategy.Rules.Weights)

		// Calculate period returns
		periodReturn, holdings, err := b.calculatePeriodReturns(ctx, tickers, weights, fallbackFlags, rebDate, periodEnd)
		if err != nil {
			slog.Warn("backtest period return calculation error", "period_start", rebDate.Format("2006-01-02"), "period_end", periodEnd.Format("2006-01-02"), "error", err)
			// Use 0 return if we can't calculate
			periodReturn = 0
		}

		// Chain the returns
		cumulativeValue *= (1 + periodReturn)

		period := BacktestPeriod{
			StartDate:     rebDate,
			EndDate:       periodEnd,
			Holdings:      holdings,
			PeriodReturn:  periodReturn,
			CumulativeVal: cumulativeValue * config.InitialCapital,
		}
		periods = append(periods, period)

		// Add curve points
		portfolioCurve = append(portfolioCurve, CurvePoint{
			Date:  rebDate,
			Value: cumulativeValue,
		})

		slog.Info("backtest period result", "period_start", rebDate.Format("2006-01-02"), "period_end", periodEnd.Format("2006-01-02"), "holdings", len(holdings), "return_pct", periodReturn*100, "cumulative_value", cumulativeValue)
	}

	// Build daily portfolio curve from period holdings
	dailyPortfolioCurve, err := b.buildDailyPortfolioCurve(ctx, periods)
	if err != nil {
		slog.Warn("backtest daily curve build error, falling back to period points", "error", err)
		// Fall back to period-based curve
		if len(periods) > 0 {
			lastPeriod := periods[len(periods)-1]
			portfolioCurve = append(portfolioCurve, CurvePoint{
				Date:  lastPeriod.EndDate,
				Value: cumulativeValue,
			})
		}
	} else if len(dailyPortfolioCurve) > 0 {
		portfolioCurve = dailyPortfolioCurve
	}

	// Calculate benchmark returns
	benchmarkCurve, benchmarkReturn, err := b.getBenchmarkCurve(ctx, config.StartDate, config.EndDate)
	if err != nil {
		slog.Warn("backtest benchmark retrieval error", "error", err)
		benchmarkReturn = 0
	}

	// Sample benchmark to match portfolio dates for chart alignment
	// Only needed if portfolio has fewer points than benchmark (rebalance-only mode)
	if len(benchmarkCurve) > 0 && len(portfolioCurve) > 0 && len(portfolioCurve) < len(benchmarkCurve)/2 {
		benchmarkCurve = sampleBenchmarkToMatchPortfolio(portfolioCurve, benchmarkCurve)
	}

	// Calculate total returns
	totalReturn := (cumulativeValue - 1) * 100 // As percentage

	// Calculate annualized returns
	years := config.EndDate.Sub(config.StartDate).Hours() / 24 / 365.25
	annualizedReturn := 0.0
	benchmarkAnnualized := 0.0
	if years > 0 {
		annualizedReturn = (math.Pow(cumulativeValue, 1/years) - 1) * 100
		if benchmarkReturn > -1 {
			benchmarkAnnualized = (math.Pow(1+benchmarkReturn, 1/years) - 1) * 100
		}
	}

	// Calculate max drawdown
	maxDrawdown := b.calculateMaxDrawdown(portfolioCurve)

	executionTimeMs := time.Since(startTime).Milliseconds()

	return &BacktestResult{
		Config:              config,
		Periods:             periods,
		TotalReturn:         totalReturn,
		AnnualizedReturn:    annualizedReturn,
		BenchmarkReturn:     benchmarkReturn * 100,
		BenchmarkAnnualized: benchmarkAnnualized,
		Alpha:               annualizedReturn - benchmarkAnnualized,
		MaxDrawdown:         maxDrawdown,
		PortfolioCurve:      portfolioCurve,
		BenchmarkCurve:      benchmarkCurve,
		ExecutionTimeMs:     executionTimeMs,
	}, nil
}

// generateRebalanceDates creates rebalance dates based on frequency
func (b *Backtester) generateRebalanceDates(start, end time.Time, freq string) []time.Time {
	var dates []time.Time

	switch freq {
	case "monthly":
		// First of each month
		current := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
		if current.Before(start) {
			current = current.AddDate(0, 1, 0)
		}
		for !current.After(end) {
			dates = append(dates, current)
			current = current.AddDate(0, 1, 0)
		}

	case "quarterly":
		// Jan 1, Apr 1, Jul 1, Oct 1
		months := []time.Month{time.January, time.April, time.July, time.October}
		year := start.Year()
		for {
			for _, month := range months {
				d := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
				if d.Before(start) {
					continue
				}
				if d.After(end) {
					return dates
				}
				dates = append(dates, d)
			}
			year++
			if year > end.Year()+1 {
				break
			}
		}

	case "semi-annual":
		// Apr 1 and Oct 1 (following Python example)
		months := []time.Month{time.April, time.October}
		year := start.Year()
		for {
			for _, month := range months {
				d := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
				if d.Before(start) {
					continue
				}
				if d.After(end) {
					return dates
				}
				dates = append(dates, d)
			}
			year++
			if year > end.Year()+1 {
				break
			}
		}

	case "annual":
		// January 1
		year := start.Year()
		if start.After(time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)) {
			year++
		}
		for {
			d := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
			if d.After(end) {
				break
			}
			dates = append(dates, d)
			year++
		}

	default:
		// Default to semi-annual
		return b.generateRebalanceDates(start, end, "semi-annual")
	}

	return dates
}

// calculatePeriodReturns calculates weighted returns for a period
func (b *Backtester) calculatePeriodReturns(ctx context.Context, tickers []string, weights []float64, fallbackFlags []bool, start, end time.Time) (float64, []Holding, error) {
	holdings := make([]Holding, 0, len(tickers))
	totalReturn := 0.0
	totalWeight := 0.0

	for i, ticker := range tickers {
		if i >= len(weights) {
			break
		}
		weight := weights[i]
		isFallback := i < len(fallbackFlags) && fallbackFlags[i]

		// Get entry price (on or before start date)
		entryPrice, err := b.repo.GetPriceOnOrBefore(ctx, ticker, start)
		if err != nil {
			if err == pgx.ErrNoRows && b.nasdaqClient != nil {
				// Try to fetch prices from Nasdaq Data Link
				if fetchErr := b.fetchAndStorePrices(ctx, ticker, start, end); fetchErr != nil {
					slog.Warn("backtest failed to fetch prices", "ticker", ticker, "error", fetchErr)
					continue
				}
				// Retry after fetch
				entryPrice, err = b.repo.GetPriceOnOrBefore(ctx, ticker, start)
				if err != nil {
					slog.Warn("backtest still no entry price after fetch", "ticker", ticker, "date", start.Format("2006-01-02"))
					continue
				}
			} else {
				slog.Warn("backtest no entry price", "ticker", ticker, "date", start.Format("2006-01-02"))
				continue
			}
		}

		// Get exit price (on or before end date)
		exitPrice, err := b.repo.GetPriceOnOrBefore(ctx, ticker, end)
		if err != nil {
			slog.Warn("backtest no exit price", "ticker", ticker, "date", end.Format("2006-01-02"))
			continue
		}

		// Calculate return
		stockReturn := (exitPrice.Close - entryPrice.Close) / entryPrice.Close

		holding := Holding{
			Ticker:     ticker,
			Rank:       i + 1,
			Weight:     weight,
			EntryPrice: entryPrice.Close,
			ExitPrice:  exitPrice.Close,
			Return:     stockReturn,
			IsFallback: isFallback,
		}
		holdings = append(holdings, holding)

		totalReturn += weight * stockReturn
		totalWeight += weight
	}

	// Normalize if we don't have all tickers
	if totalWeight > 0 && totalWeight < 1 {
		totalReturn = totalReturn / totalWeight
	}

	return totalReturn, holdings, nil
}

// fetchAndStorePrices fetches historical prices, trying Tiingo first, then SEP as fallback
func (b *Backtester) fetchAndStorePrices(ctx context.Context, ticker string, startDate, endDate time.Time) error {
	var rows []ingest.DailyRow
	var err error

	// Try Tiingo first (better coverage for all stocks)
	if b.tiingoClient != nil {
		slog.Info("backtest fetching prices from Tiingo", "ticker", ticker, "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))
		rows, err = b.tiingoClient.FetchDaily(ctx, ticker, startDate, endDate)
		if err == nil && len(rows) > 0 {
			count, upsertErr := b.repo.UpsertDailyPrices(ctx, rows)
			if upsertErr != nil {
				return fmt.Errorf("upserting Tiingo prices: %w", upsertErr)
			}
			slog.Info("backtest stored Tiingo prices", "count", count, "ticker", ticker)
			return nil
		}
		if err != nil {
			slog.Warn("backtest Tiingo fetch failed, trying SEP fallback", "ticker", ticker, "error", err)
		}
	}

	// Fallback to Nasdaq SEP
	if b.nasdaqClient != nil {
		slog.Info("backtest fetching SEP prices", "ticker", ticker, "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))
		rows, err = b.nasdaqClient.FetchSEP(ctx, []string{ticker}, startDate, endDate)
		if err != nil {
			return fmt.Errorf("SEP fetch failed for %s: %w", ticker, err)
		}
		if len(rows) == 0 {
			slog.Warn("backtest no SEP data found", "ticker", ticker)
			return fmt.Errorf("no price data for %s", ticker)
		}
		count, upsertErr := b.repo.UpsertDailyPrices(ctx, rows)
		if upsertErr != nil {
			return fmt.Errorf("upserting SEP prices: %w", upsertErr)
		}
		slog.Info("backtest stored SEP prices", "count", count, "ticker", ticker)
		return nil
	}

	return fmt.Errorf("no data source configured")
}

// fetchAndStoreBenchmarkPrices fetches benchmark prices
// Uses Tiingo for ETF benchmarks (SPY, QQQ, etc.), Nasdaq SEP for equity benchmarks
func (b *Backtester) fetchAndStoreBenchmarkPrices(ctx context.Context, ticker string, startDate, endDate time.Time) error {
	// Common ETF benchmarks - use Tiingo (has free access to major ETFs)
	isETF := ticker == "SPY" || ticker == "QQQ" || ticker == "IWM" || ticker == "DIA" || ticker == "VTI" || ticker == "VOO" || ticker == "IVV"

	var rows []ingest.DailyRow
	var err error

	if isETF && b.tiingoClient != nil {
		// Use Tiingo for ETF benchmarks
		slog.Info("backtest fetching ETF benchmark from Tiingo", "ticker", ticker, "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))
		rows, err = b.tiingoClient.FetchDaily(ctx, ticker, startDate, endDate)
		if err != nil {
			return fmt.Errorf("fetching benchmark %s from Tiingo: %w", ticker, err)
		}
	} else if b.nasdaqClient != nil {
		// Use SEP for equity benchmarks
		slog.Info("backtest fetching SEP benchmark prices", "ticker", ticker, "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))
		rows, err = b.nasdaqClient.FetchSEP(ctx, []string{ticker}, startDate, endDate)
		if err != nil {
			return fmt.Errorf("fetching benchmark %s from SEP: %w", ticker, err)
		}
	} else {
		return fmt.Errorf("no data source configured for benchmark %s", ticker)
	}

	if len(rows) == 0 {
		return fmt.Errorf("no benchmark data for %s", ticker)
	}

	count, err := b.repo.UpsertBenchmarkPrices(ctx, rows)
	if err != nil {
		return fmt.Errorf("upserting benchmark prices: %w", err)
	}
	slog.Info("backtest stored benchmark prices", "count", count, "ticker", ticker)
	return nil
}

// getBenchmarkCurve fetches benchmark performance curve
// Returns nil curve and 0 return if benchmark data unavailable (graceful degradation)
func (b *Backtester) getBenchmarkCurve(ctx context.Context, start, end time.Time) ([]CurvePoint, float64, error) {
	prices, err := b.repo.GetBenchmarkPricesForPeriod(ctx, "SPY", start, end)
	if err != nil {
		return nil, 0, err
	}

	// Try to fetch benchmark prices if none exist
	if len(prices) == 0 && (b.tiingoClient != nil || b.nasdaqClient != nil) {
		slog.Info("backtest no benchmark prices found, fetching SPY")
		if fetchErr := b.fetchAndStoreBenchmarkPrices(ctx, "SPY", start, end); fetchErr != nil {
			slog.Warn("backtest benchmark unavailable", "error", fetchErr)
			// Return gracefully - backtest can proceed without benchmark
			return nil, 0, nil
		}
		// Retry after fetch
		prices, err = b.repo.GetBenchmarkPricesForPeriod(ctx, "SPY", start, end)
		if err != nil {
			return nil, 0, err
		}
	}

	if len(prices) == 0 {
		slog.Info("backtest proceeding without benchmark data")
		return nil, 0, nil // Graceful - no error, just no benchmark
	}

	// Normalize to start price
	startPrice := prices[0].Close
	curve := make([]CurvePoint, len(prices))
	for i, p := range prices {
		curve[i] = CurvePoint{
			Date:  p.Date,
			Value: p.Close / startPrice,
		}
	}

	// Calculate total return
	endPrice := prices[len(prices)-1].Close
	totalReturn := (endPrice - startPrice) / startPrice

	return curve, totalReturn, nil
}

// buildDailyPortfolioCurve builds a daily portfolio value curve from period holdings
func (b *Backtester) buildDailyPortfolioCurve(ctx context.Context, periods []BacktestPeriod) ([]CurvePoint, error) {
	if len(periods) == 0 {
		return nil, nil
	}

	// Collect all unique tickers across all periods
	allTickers := make(map[string]bool)
	for _, period := range periods {
		for _, h := range period.Holdings {
			allTickers[h.Ticker] = true
		}
	}

	tickers := make([]string, 0, len(allTickers))
	for t := range allTickers {
		tickers = append(tickers, t)
	}

	if len(tickers) == 0 {
		return nil, nil
	}

	// Fetch all daily prices for the full date range
	startDate := periods[0].StartDate
	endDate := periods[len(periods)-1].EndDate
	priceMap, err := b.repo.GetPricesForPeriod(ctx, tickers, startDate, endDate)
	if err != nil {
		return nil, fmt.Errorf("fetching prices for daily curve: %w", err)
	}

	// Build daily curve
	var curve []CurvePoint
	cumulativeValue := 1.0

	for _, period := range periods {
		if len(period.Holdings) == 0 {
			continue
		}

		// Get entry prices for this period
		entryPrices := make(map[string]float64)
		for _, h := range period.Holdings {
			entryPrices[h.Ticker] = h.EntryPrice
		}

		// Calculate daily values for this period
		// Find all unique dates in this period
		dateSet := make(map[time.Time]bool)
		for _, h := range period.Holdings {
			prices := priceMap[h.Ticker]
			for _, p := range prices {
				if !p.Date.Before(period.StartDate) && !p.Date.After(period.EndDate) {
					dateSet[p.Date] = true
				}
			}
		}

		// Sort dates
		dates := make([]time.Time, 0, len(dateSet))
		for d := range dateSet {
			dates = append(dates, d)
		}
		sortDates(dates)

		// Calculate portfolio value for each date
		for _, date := range dates {
			dayReturn := 0.0
			totalWeight := 0.0

			for _, h := range period.Holdings {
				prices := priceMap[h.Ticker]
				// Find price for this date
				var dayPrice float64
				for _, p := range prices {
					if p.Date.Equal(date) {
						dayPrice = p.Close
						break
					}
				}

				if dayPrice > 0 && entryPrices[h.Ticker] > 0 {
					stockReturn := (dayPrice - entryPrices[h.Ticker]) / entryPrices[h.Ticker]
					dayReturn += h.Weight * stockReturn
					totalWeight += h.Weight
				}
			}

			if totalWeight > 0 {
				// Normalize return by weight and calculate value relative to period start
				dayReturn = dayReturn / totalWeight
				dayValue := cumulativeValue * (1 + dayReturn)

				curve = append(curve, CurvePoint{
					Date:  date,
					Value: dayValue,
				})
			}
		}

		// Update cumulative for next period
		cumulativeValue = cumulativeValue * (1 + period.PeriodReturn)
	}

	return curve, nil
}

// sortDates sorts a slice of times in ascending order
func sortDates(dates []time.Time) {
	for i := 0; i < len(dates)-1; i++ {
		for j := i + 1; j < len(dates); j++ {
			if dates[j].Before(dates[i]) {
				dates[i], dates[j] = dates[j], dates[i]
			}
		}
	}
}

// sampleBenchmarkToMatchPortfolio samples the benchmark curve to match portfolio dates
// This ensures both curves have the same x-axis points for charting
func sampleBenchmarkToMatchPortfolio(portfolio, benchmark []CurvePoint) []CurvePoint {
	if len(portfolio) == 0 || len(benchmark) == 0 {
		return benchmark
	}

	sampled := make([]CurvePoint, 0, len(portfolio))

	for _, pPoint := range portfolio {
		// Find the closest benchmark point on or before this date
		var closest *CurvePoint
		for i := range benchmark {
			if benchmark[i].Date.After(pPoint.Date) {
				break
			}
			closest = &benchmark[i]
		}
		if closest != nil {
			sampled = append(sampled, CurvePoint{
				Date:  pPoint.Date, // Use portfolio date for alignment
				Value: closest.Value,
			})
		}
	}

	return sampled
}

// calculateMaxDrawdown calculates the maximum peak-to-trough decline
func (b *Backtester) calculateMaxDrawdown(curve []CurvePoint) float64 {
	if len(curve) == 0 {
		return 0
	}

	maxVal := curve[0].Value
	maxDrawdown := 0.0

	for _, p := range curve {
		if p.Value > maxVal {
			maxVal = p.Value
		}
		drawdown := (maxVal - p.Value) / maxVal
		if drawdown > maxDrawdown {
			maxDrawdown = drawdown
		}
	}

	return maxDrawdown * 100 // As percentage
}
