package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strings"
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
// Refreshes the company list from SHARADAR/TICKERS.
// Query params:
// - ticker: comma-separated tickers (optional, defaults to all)
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
// Fetches SF1 data. Query params:
// - ticker: comma-separated tickers (optional, defaults to all known companies)
// - dimension: comma-separated dimensions (default: ARQ,MRQ)
// - full: if "true", fetch all history (default: incremental)
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

	totalCount := 0

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

		// Fetch from API
		rows, err := h.client.FetchSF1(ctx, tickerFilter, dimension, since)
		if err != nil {
			log.Printf("Error fetching SF1 (%s): %v", dimension, err)
			return c.JSON(http.StatusInternalServerError, IngestResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to fetch SF1 (%s): %v", dimension, err),
			})
		}

		log.Printf("Fetched %d rows for dimension %s", len(rows), dimension)

		// Upsert to database
		count, err := h.repo.UpsertFinancialMetrics(ctx, rows)
		if err != nil {
			log.Printf("Error upserting metrics (%s): %v", dimension, err)
			return c.JSON(http.StatusInternalServerError, IngestResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to upsert metrics (%s): %v", dimension, err),
			})
		}

		totalCount += count
		log.Printf("Upserted %d metrics for dimension %s", count, dimension)
	}

	elapsed := time.Since(start)
	log.Printf("Fundamentals ingestion complete: %d metrics in %v", totalCount, elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d financial metrics", totalCount),
		Count:   totalCount,
		Elapsed: elapsed.String(),
	})
}

// IngestDaily handles POST /admin/ingest/daily
// Fetches daily price data. Query params:
// - ticker: comma-separated tickers (required)
// - full: if "true", fetch all history (default: incremental)
func (h *IngestHandler) IngestDaily(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	// Parse query params
	tickerParam := c.QueryParam("ticker")
	if tickerParam == "" {
		return c.JSON(http.StatusBadRequest, IngestResponse{
			Success: false,
			Message: "ticker parameter is required (e.g., ?ticker=SPY,AAPL)",
		})
	}
	tickers := strings.Split(tickerParam, ",")
	for i := range tickers {
		tickers[i] = strings.TrimSpace(tickers[i])
	}

	fullFetch := c.QueryParam("full") == "true"

	log.Printf("Starting daily price ingestion (tickers: %v, full: %v)...", tickers, fullFetch)

	// Determine since date for incremental fetch
	var since time.Time
	if !fullFetch {
		since, _ = h.repo.GetLastSharadarUpdate(ctx, "daily_prices")
		log.Printf("Incremental fetch since %v", since)
	}

	// Fetch from API
	rows, err := h.client.FetchDaily(ctx, tickers, since)
	if err != nil {
		log.Printf("Error fetching daily prices: %v", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to fetch daily prices: %v", err),
		})
	}

	log.Printf("Fetched %d daily price rows", len(rows))

	// Filter rows to only include tickers that exist in companies table
	// This handles the case where we fetch prices for tickers not in SF1
	validRows := make([]ingest.DailyRow, 0, len(rows))
	for _, row := range rows {
		exists, err := h.repo.CompanyExists(ctx, row.Ticker)
		if err != nil {
			log.Printf("Error checking ticker %s: %v", row.Ticker, err)
			continue
		}
		if exists {
			validRows = append(validRows, row)
		}
	}

	if len(validRows) < len(rows) {
		log.Printf("Filtered to %d rows (some tickers not in companies table)", len(validRows))
	}

	// Upsert to database
	count, err := h.repo.UpsertDailyPrices(ctx, validRows)
	if err != nil {
		log.Printf("Error upserting daily prices: %v", err)
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to upsert daily prices: %v", err),
		})
	}

	elapsed := time.Since(start)
	log.Printf("Daily price ingestion complete: %d prices in %v", count, elapsed)

	return c.JSON(http.StatusOK, IngestResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully ingested %d daily prices", count),
		Count:   count,
		Elapsed: elapsed.String(),
	})
}

// IngestStatus handles GET /admin/ingest/status
// Returns current ingestion status and counts.
func (h *IngestHandler) IngestStatus(c echo.Context) error {
	ctx := c.Request().Context()

	companyCount, _ := h.repo.GetCompanyCount(ctx)
	metricCount, _ := h.repo.GetMetricCount(ctx)
	priceCount, _ := h.repo.GetDailyPriceCount(ctx)

	lastMetricUpdate, _ := h.repo.GetLastSharadarUpdate(ctx, "financial_metrics")
	lastPriceUpdate, _ := h.repo.GetLastSharadarUpdate(ctx, "daily_prices")

	return c.JSON(http.StatusOK, map[string]interface{}{
		"companies": companyCount,
		"metrics":   metricCount,
		"prices":    priceCount,
		"last_metric_update": lastMetricUpdate.Format("2006-01-02"),
		"last_price_update":  lastPriceUpdate.Format("2006-01-02"),
	})
}

// IngestTest handles GET /admin/ingest/test
// Makes a minimal API call to verify the connection works.
func (h *IngestHandler) IngestTest(c echo.Context) error {
	ctx := c.Request().Context()
	start := time.Now()

	// Fetch just AAPL ticker info (minimal data, one row)
	rows, err := h.client.FetchSF1(ctx, []string{"AAPL"}, "MRY", time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, IngestResponse{
			Success: false,
			Message: fmt.Sprintf("API test failed: %v", err),
		})
	}

	elapsed := time.Since(start)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"success":     true,
		"message":     "API connection successful",
		"rows_fetched": len(rows),
		"elapsed":     elapsed.String(),
		"sample":      rows[0].Ticker + " - " + rows[0].CalendarDate.Format("2006-01-02"),
	})
}