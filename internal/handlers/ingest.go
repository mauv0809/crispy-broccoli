package handlers

import (
	"fmt"
	"log"
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
	client *ingest.Client
	repo   *db.Repository
}

// NewIngestHandler creates a new ingest handler.
func NewIngestHandler(client *ingest.Client, repo *db.Repository) *IngestHandler {
	return &IngestHandler{
		client: client,
		repo:   repo,
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
		log.Printf("Starting ticker ingestion for: %v", tickerFilter)
	} else {
		log.Println("Starting ticker ingestion (all tickers)...")
	}

	// Fetch tickers from API
	tickers, err := h.client.FetchTickers(ctx, tickerFilter)
	if err != nil {
		log.Printf("Error fetching tickers: %v", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to fetch tickers: %v", err),
		})
	}

	log.Printf("Fetched %d tickers from API", len(tickers))

	// Upsert to database
	count, err := h.repo.UpsertCompanies(ctx, tickers)
	if err != nil {
		log.Printf("Error upserting companies: %v", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to upsert companies: %v", err),
		})
	}

	elapsed := time.Since(start)
	log.Printf("Ticker ingestion complete: %d companies in %v", count, elapsed)

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
	dimensionParam := c.QueryParam("dimension")
	if dimensionParam == "" {
		dimensionParam = "ARQ,MRQ"
	}
	dimensions := strings.Split(dimensionParam, ",")

	fullFetch := c.QueryParam("full") == "true"

	log.Printf("Starting fundamentals ingestion (tickers: %d, dimensions: %v, full: %v)...", len(tickerFilter), dimensions, fullFetch)

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
			log.Printf("Incremental fetch for %s since %v", dimension, since)
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
				log.Printf("Error fetching SF1 batch (%s): %v", dimension, batch.Error)
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
					log.Printf("Error upserting metrics batch (%s): %v", dimension, err)
				}
				totalCount.Add(int64(count))
				log.Printf("Upserted %d metrics for %s", count, dimension)
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
	log.Printf("Fundamentals ingestion complete: %d metrics in %v", count, elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d financial metrics", count),
		Count:   count,
		Elapsed: elapsed.String(),
	})
}

// IngestDaily handles POST /admin/ingest/daily
// @Summary Ingest daily prices
// @Description Fetches daily price/fundamental data from SHARADAR/DAILY. If no ticker specified, fetches for all DB companies.
// @Tags ingestion
// @Accept json
// @Produce json
// @Param ticker query string false "Comma-separated tickers (defaults to all companies in DB)"
// @Param full query boolean false "Fetch all history (default: incremental)"
// @Success 200 {object} IngestResponse
// @Failure 400 {object} IngestResponse
// @Failure 500 {object} IngestResponse
// @Router /admin/ingest/daily [post]
func (h *IngestHandler) IngestDaily(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	// Parse ticker filter - default to all companies in DB
	var tickers []string
	var fetchAllFromDB bool
	if tickerParam := c.QueryParam("ticker"); tickerParam != "" {
		tickers = strings.Split(tickerParam, ",")
		for i := range tickers {
			tickers[i] = strings.TrimSpace(tickers[i])
		}
	} else {
		// Fetch all tickers from database
		var err error
		tickers, err = h.repo.GetAllTickers(ctx)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, IngestResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to get tickers: %v", err),
			})
		}
		fetchAllFromDB = true
	}

	if len(tickers) == 0 {
		return c.JSON(http.StatusBadRequest, IngestResponse{
			Success: false,
			Message: "No companies in database. Run /admin/ingest/tickers first.",
		})
	}

	fullFetch := c.QueryParam("full") == "true"

	log.Printf("Starting daily price ingestion (tickers: %d, full: %v)...", len(tickers), fullFetch)

	// Determine since date for incremental fetch
	var since time.Time
	if !fullFetch {
		since, _ = h.repo.GetLastSharadarUpdate(ctx, "daily_prices")
		log.Printf("Incremental fetch since %v", since)
	}

	// Stream batches with parallel API fetches (5 concurrent) and parallel upserts (3 concurrent)
	const maxAPIParallel = 5
	const maxDBParallel = 3

	batchCh := h.client.FetchDailyStream(ctx, tickers, since, maxAPIParallel)

	var totalCount atomic.Int64
	var wg sync.WaitGroup
	var fetchErr error
	sem := make(chan struct{}, maxDBParallel)

	for batch := range batchCh {
		if batch.Error != nil {
			fetchErr = batch.Error
			log.Printf("Error fetching daily batch: %v", batch.Error)
			continue // Don't stop - try other batches
		}

		if len(batch.Rows) == 0 {
			continue
		}

		// Filter to valid tickers only if we're fetching specific tickers (not all from DB)
		rowsToUpsert := batch.Rows
		if !fetchAllFromDB {
			validRows := make([]ingest.DailyRow, 0, len(batch.Rows))
			for _, row := range batch.Rows {
				exists, err := h.repo.CompanyExists(ctx, row.Ticker)
				if err == nil && exists {
					validRows = append(validRows, row)
				}
			}
			rowsToUpsert = validRows
		}

		if len(rowsToUpsert) == 0 {
			continue
		}

		sem <- struct{}{} // Acquire DB slot

		wg.Add(1)
		go func(rows []ingest.DailyRow) {
			defer wg.Done()
			defer func() { <-sem }()

			count, err := h.repo.UpsertDailyPrices(ctx, rows)
			if err != nil {
				log.Printf("Error upserting daily prices: %v", err)
			}
			totalCount.Add(int64(count))
			log.Printf("Upserted %d daily prices", count)
		}(rowsToUpsert)
	}

	wg.Wait()

	count := int(totalCount.Load())
	elapsed := time.Since(start)

	if fetchErr != nil && count == 0 {
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to fetch daily prices: %v", fetchErr),
		})
	}

	log.Printf("Daily price ingestion complete: %d prices in %v", count, elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d daily prices", count),
		Count:   count,
		Elapsed: elapsed.String(),
	})
}

// IngestBenchmarks handles POST /admin/ingest/benchmarks
// @Summary Ingest benchmark prices
// @Description Fetches daily data for configured benchmarks (e.g., SPY) from SHARADAR/DAILY
// @Tags ingestion
// @Accept json
// @Produce json
// @Param full query boolean false "Fetch all history (default: incremental)"
// @Success 200 {object} IngestResponse
// @Failure 400 {object} IngestResponse
// @Failure 500 {object} IngestResponse
// @Router /admin/ingest/benchmarks [post]
func (h *IngestHandler) IngestBenchmarks(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	// Get benchmark tickers from database
	tickers, err := h.repo.GetBenchmarkTickers(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to get benchmark tickers: %v", err),
		})
	}

	if len(tickers) == 0 {
		return c.JSON(http.StatusBadRequest, IngestResponse{
			Success: false,
			Message: "No benchmarks configured in database",
		})
	}

	fullFetch := c.QueryParam("full") == "true"

	log.Printf("Starting benchmark ingestion (tickers: %v, full: %v)...", tickers, fullFetch)

	// Determine since date for incremental fetch
	var since time.Time
	if !fullFetch {
		since, _ = h.repo.GetLastBenchmarkUpdate(ctx)
		log.Printf("Incremental fetch since %v", since)
	}

	// Fetch from API (using same SHARADAR/DAILY endpoint)
	rows, err := h.client.FetchDaily(ctx, tickers, since)
	if err != nil {
		log.Printf("Error fetching benchmark prices: %v", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to fetch benchmark prices: %v", err),
		})
	}

	log.Printf("Fetched %d benchmark price rows", len(rows))

	// Upsert to benchmark_prices table
	count, err := h.repo.UpsertBenchmarkPrices(ctx, rows)
	if err != nil {
		log.Printf("Error upserting benchmark prices: %v", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to upsert benchmark prices: %v", err),
		})
	}

	elapsed := time.Since(start)
	log.Printf("Benchmark ingestion complete: %d prices in %v", count, elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d benchmark prices", count),
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

	lastMetricUpdate, _ := h.repo.GetLastSharadarUpdate(ctx, "financial_metrics")
	lastPriceUpdate, _ := h.repo.GetLastSharadarUpdate(ctx, "daily_prices")
	lastBenchmarkUpdate, _ := h.repo.GetLastBenchmarkUpdate(ctx)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"companies":             companyCount,
		"metrics":               metricCount,
		"prices":                priceCount,
		"benchmark_prices":      benchmarkCount,
		"last_metric_update":    lastMetricUpdate.Format("2006-01-02"),
		"last_price_update":     lastPriceUpdate.Format("2006-01-02"),
		"last_benchmark_update": lastBenchmarkUpdate.Format("2006-01-02"),
	})
}

