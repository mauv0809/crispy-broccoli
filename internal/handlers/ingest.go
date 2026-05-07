package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mauv0809/crispy-broccoli/internal/db"
	"github.com/mauv0809/crispy-broccoli/internal/ingest"
)

// IngestHandler handles data ingestion endpoints.
type IngestHandler struct {
	client       *ingest.Client
	tiingoClient *ingest.TiingoClient
	repo         *db.Repository
}

// NewIngestHandler creates a new ingest handler.
func NewIngestHandler(client *ingest.Client, tiingoClient *ingest.TiingoClient, repo *db.Repository) *IngestHandler {
	return &IngestHandler{
		client:       client,
		tiingoClient: tiingoClient,
		repo:         repo,
	}
}

// IngestResponse is the JSON response for ingestion endpoints.
type IngestResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`
}

// IngestTickers handles POST /admin/ingest/tickers
// @Summary Ingest company tickers
// @Description Fetches company metadata from SHARADAR/TICKERS and upserts into the companies table
// @Tags ingestion
// @Accept json
// @Produce json
// @Param ticker query string false "Comma-separated tickers (defaults to all)"
// @Success 200 {object} IngestResponse
// @Failure 500 {object} IngestResponse
// @Router /admin/ingest/tickers [post]
func (h *IngestHandler) IngestTickers(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	// Parse optional ticker filter
	var tickerFilter []string
	if tickerParam := c.QueryParam("ticker"); tickerParam != "" {
		tickerFilter = strings.Split(tickerParam, ",")
		for i := range tickerFilter {
			tickerFilter[i] = strings.TrimSpace(tickerFilter[i])
		}
		slog.Info("starting ticker ingestion", "tickers", tickerFilter)
	} else {
		slog.Info("starting ticker ingestion for all tickers")
	}

	// Fetch tickers from API
	tickers, err := h.client.FetchTickers(ctx, tickerFilter)
	if err != nil {
		slog.Error("failed to fetch tickers", "error", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to fetch tickers: %v", err),
		})
	}

	slog.Info("fetched tickers from API", "count", len(tickers))

	// Upsert to database
	count, err := h.repo.UpsertCompanies(ctx, tickers)
	if err != nil {
		slog.Error("failed to upsert companies", "error", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to upsert companies: %v", err),
		})
	}

	elapsed := time.Since(start)
	slog.Info("ticker ingestion complete", "companies", count, "elapsed", elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d companies", count),
		Count:   count,
		Elapsed: elapsed.String(),
	})
}

// IngestFundamentals handles POST /admin/ingest/fundamentals
// @Summary Ingest financial metrics
// @Description Fetches fundamental data from SHARADAR/SF1 and upserts into financial_metrics table
// @Tags ingestion
// @Accept json
// @Produce json
// @Param ticker query string false "Comma-separated tickers (defaults to all companies in DB)"
// @Param dimension query string false "Comma-separated dimensions" default(ARQ,MRQ)
// @Param full query boolean false "Fetch all history (default: incremental)"
// @Success 200 {object} IngestResponse
// @Failure 400 {object} IngestResponse
// @Failure 500 {object} IngestResponse
// @Router /admin/ingest/fundamentals [post]
func (h *IngestHandler) IngestFundamentals(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	// Parse ticker filter - default to companies we have in DB
	var tickerFilter []string
	if tickerParam := c.QueryParam("ticker"); tickerParam != "" {
		tickerFilter = strings.Split(tickerParam, ",")
		for i := range tickerFilter {
			tickerFilter[i] = strings.TrimSpace(tickerFilter[i])
		}
	} else {
		// Default to all companies in our database
		var err error
		tickerFilter, err = h.repo.GetAllTickers(ctx)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, IngestResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to get tickers: %v", err),
			})
		}
	}

	if len(tickerFilter) == 0 {
		return c.JSON(http.StatusBadRequest, IngestResponse{
			Success: false,
			Message: "No companies in database. Run /admin/ingest/tickers first.",
		})
	}

	// Parse query params
	// Sharadar SF1 dimensions:
	// ARQ/ARY = As Reported (Quarterly/Yearly)
	// MRQ/MRY = Most Recent (Quarterly/Yearly)
	// ART/MRT = Trailing Twelve Months (As Reported/Most Recent)
	dimensionParam := c.QueryParam("dimension")
	if dimensionParam == "" {
		dimensionParam = "ARQ,ARY,MRQ,MRY,ART,MRT"
	}
	dimensions := strings.Split(dimensionParam, ",")

	fullFetch := c.QueryParam("full") == "true"

	slog.Info("starting fundamentals ingestion", "ticker_count", len(tickerFilter), "dimensions", dimensions, "full_fetch", fullFetch)

	// Check if we have companies first
	companyCount, err := h.repo.GetCompanyCount(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to check companies: %v", err),
		})
	}

	if companyCount == 0 {
		return c.JSON(http.StatusBadRequest, IngestResponse{
			Success: false,
			Message: "No companies in database. Run /admin/ingest/tickers first.",
		})
	}

	var totalCount atomic.Int64

	for _, dimension := range dimensions {
		dimension = strings.TrimSpace(dimension)
		if dimension == "" {
			continue
		}

		// Determine since date for incremental fetch
		var since time.Time
		if !fullFetch {
			since, _ = h.repo.GetLastSharadarUpdate(ctx, "financial_metrics")
			slog.Info("incremental fundamentals fetch", "dimension", dimension, "since", since)
		}

		// Stream batches with parallel API fetches (5 concurrent) and parallel upserts (3 concurrent)
		const maxAPIParallel = 5
		batchCh := h.client.FetchSF1Stream(ctx, tickerFilter, dimension, since, maxAPIParallel)

		var wg sync.WaitGroup
		var fetchErr error
		sem := make(chan struct{}, 3) // Limit concurrent DB writes

		for batch := range batchCh {
			if batch.Error != nil {
				fetchErr = batch.Error
				slog.Error("failed to fetch SF1 batch", "dimension", dimension, "error", batch.Error)
				break
			}

			if len(batch.Rows) == 0 {
				continue
			}

			sem <- struct{}{} // Acquire slot (blocks if 3 upserts running)

			wg.Add(1)
			go func(rows []ingest.SF1Row) {
				defer wg.Done()
				defer func() { <-sem }()

				count, err := h.repo.UpsertFinancialMetrics(ctx, rows)
				if err != nil {
					slog.Error("failed to upsert metrics batch", "dimension", dimension, "error", err)
				}
				totalCount.Add(int64(count))
				slog.Info("upserted metrics batch", "count", count, "dimension", dimension)
			}(batch.Rows)
		}

		// Wait for all upserts to complete before moving to next dimension
		wg.Wait()

		if fetchErr != nil {
			return c.JSON(http.StatusInternalServerError, IngestResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to fetch SF1 (%s): %v", dimension, fetchErr),
			})
		}
	}

	elapsed := time.Since(start)
	count := int(totalCount.Load())
	slog.Info("fundamentals ingestion complete", "metrics", count, "elapsed", elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d financial metrics", count),
		Count:   count,
		Elapsed: elapsed.String(),
	})
}

// IngestStatus handles GET /admin/ingest/status
// @Summary Get ingestion status
// @Description Returns current data counts and last update timestamps
// @Tags ingestion
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /admin/ingest/status [get]
func (h *IngestHandler) IngestStatus(c echo.Context) error {
	ctx := c.Request().Context()

	companyCount, _ := h.repo.GetCompanyCount(ctx)
	metricCount, _ := h.repo.GetMetricCount(ctx)
	priceCount, _ := h.repo.GetDailyPriceCount(ctx)
	benchmarkCount, _ := h.repo.GetBenchmarkPriceCount(ctx)
	sp500Count, _ := h.repo.GetSP500MembershipCount(ctx)

	lastMetricUpdate, _ := h.repo.GetLastSharadarUpdate(ctx, "financial_metrics")
	lastPriceUpdate, _ := h.repo.GetLastSharadarUpdate(ctx, "daily_prices")
	lastBenchmarkUpdate, _ := h.repo.GetLastBenchmarkUpdate(ctx)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"companies":             companyCount,
		"metrics":               metricCount,
		"prices":                priceCount,
		"benchmark_prices":      benchmarkCount,
		"sp500_membership":      sp500Count,
		"last_metric_update":    lastMetricUpdate.Format("2006-01-02"),
		"last_price_update":     lastPriceUpdate.Format("2006-01-02"),
		"last_benchmark_update": lastBenchmarkUpdate.Format("2006-01-02"),
	})
}

// IngestSP500 handles POST /admin/ingest/sp500
// @Summary Ingest S&P 500 membership history
// @Description Fetches S&P 500 membership history from SHARADAR/SP500 for point-in-time backtesting
// @Tags ingestion
// @Accept json
// @Produce json
// @Success 200 {object} IngestResponse
// @Failure 500 {object} IngestResponse
// @Router /admin/ingest/sp500 [post]
func (h *IngestHandler) IngestSP500(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	slog.Info("starting SP500 membership ingestion")

	// Fetch full membership history from API
	rows, err := h.client.FetchSP500History(ctx)
	if err != nil {
		slog.Error("failed to fetch SP500 history", "error", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to fetch S&P 500 history: %v", err),
		})
	}

	slog.Info("fetched SP500 membership records from API", "count", len(rows))

	// Upsert to database
	count, err := h.repo.UpsertSP500Membership(ctx, rows)
	if err != nil {
		slog.Error("failed to upsert SP500 membership", "error", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to upsert S&P 500 membership: %v", err),
		})
	}

	elapsed := time.Since(start)
	slog.Info("SP500 membership ingestion complete", "records", count, "elapsed", elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d S&P 500 membership records", count),
		Count:   count,
		Elapsed: elapsed.String(),
	})
}

// IngestBenchmark handles POST /admin/ingest/benchmark
// @Summary Ingest benchmark prices
// @Description Fetches ETF benchmark prices (SPY, QQQ, etc.) from Tiingo - incremental updates
// @Tags ingestion
// @Accept json
// @Produce json
// @Param ticker query string false "Comma-separated tickers (defaults to SPY)"
// @Success 200 {object} IngestResponse
// @Failure 500 {object} IngestResponse
// @Router /admin/ingest/benchmark [post]
func (h *IngestHandler) IngestBenchmark(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	if h.tiingoClient == nil {
		return c.JSON(http.StatusServiceUnavailable, IngestResponse{
			Success: false,
			Message: "Tiingo API not configured (TIINGO_API_KEY not set)",
		})
	}

	// Parse ticker filter - default to SPY
	tickerParam := c.QueryParam("ticker")
	if tickerParam == "" {
		tickerParam = "SPY"
	}
	tickers := strings.Split(tickerParam, ",")
	for i := range tickers {
		tickers[i] = strings.TrimSpace(tickers[i])
	}

	// Check rate limits
	limits := h.tiingoClient.GetRateLimits()
	if limits.IsRateLimited {
		return c.JSON(http.StatusTooManyRequests, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Rate limited. Resets at %s", limits.ResetTime.Format("15:04:05")),
		})
	}

	slog.Info("starting benchmark ingestion", "tickers", tickers, "hourly_remaining", limits.HourlyRemaining)

	var totalCount int
	var skipped int
	var errors []string
	endDate := time.Now()

	for _, ticker := range tickers {
		if !h.tiingoClient.CanFetch(ticker) {
			errors = append(errors, fmt.Sprintf("%s: rate limited", ticker))
			continue
		}

		// Check last date we have for this benchmark - incremental fetch
		lastDate, err := h.repo.GetLastBenchmarkPriceDateForTicker(ctx, ticker)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: failed to check last date: %v", ticker, err))
			continue
		}

		// If we have data from today or yesterday, skip (already up to date)
		if lastDate.After(endDate.AddDate(0, 0, -2)) {
			slog.Info("benchmark already up to date", "ticker", ticker, "last_date", lastDate.Format("2006-01-02"))
			skipped++
			continue
		}

		// Start from day after last date we have, or 1993 if no data
		startDate := lastDate.AddDate(0, 0, 1)
		if lastDate.Year() < 1990 {
			startDate = time.Date(1993, 1, 1, 0, 0, 0, 0, time.UTC)
		}

		slog.Info("fetching benchmark prices", "ticker", ticker, "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))

		rows, err := h.tiingoClient.FetchDaily(ctx, ticker, startDate, endDate)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", ticker, err))
			continue
		}

		count, err := h.repo.UpsertBenchmarkPrices(ctx, rows)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: upsert failed: %v", ticker, err))
			continue
		}

		totalCount += count
		slog.Info("ingested benchmark prices", "count", count, "ticker", ticker)
	}

	elapsed := time.Since(start)

	if len(errors) > 0 && totalCount == 0 && skipped == 0 {
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed: %s", strings.Join(errors, "; ")),
		})
	}

	msg := fmt.Sprintf("Ingested %d benchmark prices", totalCount)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d already up-to-date", skipped)
	}
	if len(errors) > 0 {
		msg += fmt.Sprintf(" (errors: %s)", strings.Join(errors, "; "))
	}

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: msg,
		Count:   totalCount,
		Elapsed: elapsed.String(),
	})
}

// IngestPrices handles POST /admin/ingest/prices
// @Summary Ingest daily stock prices
// @Description Fetches daily prices for stocks from Tiingo (rate limited: 50/hour) - incremental updates
// @Tags ingestion
// @Accept json
// @Produce json
// @Param ticker query string false "Comma-separated tickers (defaults to batch of stocks needing prices)"
// @Param limit query int false "Max tickers to fetch (default 10)"
// @Param stale_days query int false "Consider data stale after N days (default 3)"
// @Param retry_days query int false "Days to wait before retrying tickers with no data (default 7)"
// @Success 200 {object} IngestResponse
// @Failure 500 {object} IngestResponse
// @Router /admin/ingest/prices [post]
func (h *IngestHandler) IngestPrices(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	if h.tiingoClient == nil {
		return c.JSON(http.StatusServiceUnavailable, IngestResponse{
			Success: false,
			Message: "Tiingo API not configured (TIINGO_API_KEY not set)",
		})
	}

	// Check rate limits first
	limits := h.tiingoClient.GetRateLimits()
	if limits.IsRateLimited {
		return c.JSON(http.StatusTooManyRequests, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Rate limited. Resets at %s", limits.ResetTime.Format("15:04:05")),
		})
	}

	if limits.HourlyRemaining <= 0 {
		return c.JSON(http.StatusTooManyRequests, IngestResponse{
			Success: false,
			Message: "Hourly rate limit reached (50/hour). Try again later.",
		})
	}

	// Parse limit param
	batchLimit := 10
	if limitParam := c.QueryParam("limit"); limitParam != "" {
		fmt.Sscanf(limitParam, "%d", &batchLimit)
	}
	if batchLimit > limits.HourlyRemaining {
		batchLimit = limits.HourlyRemaining
	}

	// Parse stale_days param (default 3 days)
	staleDays := 3
	if staleParam := c.QueryParam("stale_days"); staleParam != "" {
		fmt.Sscanf(staleParam, "%d", &staleDays)
	}

	// Parse retry_days param (default 7 days - how long to wait before retrying tickers with no data)
	retryDays := 7
	if retryParam := c.QueryParam("retry_days"); retryParam != "" {
		fmt.Sscanf(retryParam, "%d", &retryDays)
	}

	// Parse ticker filter or get tickers needing updates
	type tickerInfo struct {
		ticker   string
		lastDate time.Time
	}
	var tickersToFetch []tickerInfo

	if tickerParam := c.QueryParam("ticker"); tickerParam != "" {
		// Explicit tickers - check their last dates
		tickers := strings.Split(tickerParam, ",")
		for _, t := range tickers {
			t = strings.TrimSpace(t)
			lastDate, _ := h.repo.GetLastPriceDateForTicker(ctx, t)
			tickersToFetch = append(tickersToFetch, tickerInfo{ticker: t, lastDate: lastDate})
		}
	} else {
		// Get tickers that need price updates (no data or stale)
		statuses, err := h.repo.GetTickersNeedingPriceUpdate(ctx, batchLimit, staleDays, retryDays)
		if err != nil {
			slog.Error("failed to get tickers needing price update", "error", err)
			return c.JSON(http.StatusInternalServerError, IngestResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to get tickers: %v", err),
			})
		}
		slog.Info("found tickers needing price update", "count", len(statuses))
		for _, s := range statuses {
			tickersToFetch = append(tickersToFetch, tickerInfo{ticker: s.Ticker, lastDate: s.LastDate})
		}
	}

	if len(tickersToFetch) == 0 {
		return c.JSON(http.StatusOK, IngestResponse{
			Success: true,
			Message: "All tickers are up to date",
			Count:   0,
		})
	}

	// Limit to available rate limit
	if len(tickersToFetch) > limits.HourlyRemaining {
		tickersToFetch = tickersToFetch[:limits.HourlyRemaining]
	}

	slog.Info("starting price ingestion", "ticker_count", len(tickersToFetch), "hourly_remaining", limits.HourlyRemaining)

	var totalCount int
	var fetched int
	var noDataCount int
	var errors []string
	var attemptedTickers []string
	endDate := time.Now()

	for _, ti := range tickersToFetch {
		if !h.tiingoClient.CanFetch(ti.ticker) {
			slog.Warn("rate limit reached, stopping price ingestion", "ticker", ti.ticker)
			break
		}

		// Incremental: start from day after last date, or 2010 if no data
		startDate := ti.lastDate.AddDate(0, 0, 1)
		if ti.lastDate.Year() < 2000 {
			startDate = time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
		}

		slog.Info("fetching ticker prices", "ticker", ti.ticker, "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))

		rows, err := h.tiingoClient.FetchDaily(ctx, ti.ticker, startDate, endDate)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", ti.ticker, err))
			if err == ingest.ErrRateLimited {
				break
			}
			continue
		}

		// Track that we attempted this ticker (even if no data returned)
		attemptedTickers = append(attemptedTickers, ti.ticker)

		if len(rows) == 0 {
			// Tiingo returned 200 but no data - ticker not available in their database
			slog.Info("no price data available from Tiingo", "ticker", ti.ticker)
			noDataCount++
			continue
		}

		count, err := h.repo.UpsertDailyPrices(ctx, rows)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: upsert failed: %v", ti.ticker, err))
			continue
		}

		totalCount += count
		fetched++
		slog.Info("ingested ticker prices", "count", count, "ticker", ti.ticker, "fetched", fetched, "total", len(tickersToFetch))
	}

	// Mark all attempted tickers so we don't retry them immediately
	if len(attemptedTickers) > 0 {
		if err := h.repo.MarkPriceFetchAttempted(ctx, attemptedTickers); err != nil {
			slog.Error("failed to mark tickers as attempted", "error", err)
		}
	}

	elapsed := time.Since(start)

	// Get updated rate limits
	newLimits := h.tiingoClient.GetRateLimits()

	// Check if we hit rate limit during this batch
	if newLimits.IsRateLimited || newLimits.HourlyRemaining == 0 {
		msg := fmt.Sprintf("Ingested %d prices for %d tickers. RATE LIMITED - resets at %s",
			totalCount, fetched, newLimits.ResetTime.Format("15:04:05"))
		return c.JSON(http.StatusOK, IngestResponse{
			Success: totalCount > 0,
			Message: msg,
			Count:   totalCount,
			Elapsed: elapsed.String(),
		})
	}

	msg := fmt.Sprintf("Ingested %d prices for %d tickers. Rate limit: %d/hour remaining",
		totalCount, fetched, newLimits.HourlyRemaining)
	if noDataCount > 0 {
		msg += fmt.Sprintf(", %d tickers with no data (will retry in %d days)", noDataCount, retryDays)
	}
	if len(errors) > 0 {
		msg += fmt.Sprintf(" (errors: %d)", len(errors))
	}

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: msg,
		Count:   totalCount,
		Elapsed: elapsed.String(),
	})
}

