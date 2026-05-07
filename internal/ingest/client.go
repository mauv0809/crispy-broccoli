package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	baseURL        = "https://data.nasdaq.com/api/v3/datatables"
	defaultTimeout = 60 * time.Second
	rateLimit      = 2 // requests per second (conservative for authenticated users)
)

// Client is a rate-limited client for Nasdaq Data Link Tables API.
type Client struct {
	apiKey     string
	httpClient *http.Client
	limiter    *rateLimiter
}

// rateLimiter implements a simple token bucket rate limiter.
type rateLimiter struct {
	mu       sync.Mutex
	lastCall time.Time
	interval time.Duration
}

func newRateLimiter(requestsPerSecond int) *rateLimiter {
	return &rateLimiter{
		interval: time.Second / time.Duration(requestsPerSecond),
	}
}

func (r *rateLimiter) Wait() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(r.lastCall)
	if elapsed < r.interval {
		time.Sleep(r.interval - elapsed)
	}
	r.lastCall = time.Now()
}

// NewClient creates a new Sharadar API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
		limiter: newRateLimiter(rateLimit),
	}
}

// FetchTable fetches data from a table with the given parameters.
// Handles pagination automatically and returns all rows.
func (c *Client) FetchTable(ctx context.Context, table string, params map[string]string) (*Response, error) {
	allData := &Response{}
	var cursorID *string

	for {
		resp, err := c.fetchPage(ctx, table, params, cursorID)
		if err != nil {
			return nil, err
		}

		// Merge columns (only needed on first page)
		if len(allData.Datatable.Columns) == 0 {
			allData.Datatable.Columns = resp.Datatable.Columns
		}

		// Append data
		allData.Datatable.Data = append(allData.Datatable.Data, resp.Datatable.Data...)

		// Check for more pages
		if resp.Meta.NextCursorID == nil || *resp.Meta.NextCursorID == "" {
			break
		}
		cursorID = resp.Meta.NextCursorID
		slog.Info("fetching next page", "cursor_prefix", (*cursorID)[:min(20, len(*cursorID))])
	}

	return allData, nil
}

// fetchPage fetches a single page of data.
func (c *Client) fetchPage(ctx context.Context, table string, params map[string]string, cursorID *string) (*Response, error) {
	// Build URL
	u, err := url.Parse(fmt.Sprintf("%s/%s.json", baseURL, table))
	if err != nil {
		return nil, fmt.Errorf("invalid table name: %w", err)
	}

	q := u.Query()
	q.Set("api_key", c.apiKey)
	for k, v := range params {
		q.Set(k, v)
	}
	if cursorID != nil {
		q.Set("qopts.cursor_id", *cursorID)
	}
	u.RawQuery = q.Encode()

	// Rate limit
	c.limiter.Wait()

	// Make request with retries
	var resp *Response
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second
			slog.Info("retrying request", "attempt", attempt, "backoff", backoff)
			time.Sleep(backoff)
		}

		resp, lastErr = c.doRequest(ctx, u.String())
		if lastErr == nil {
			return resp, nil
		}

		// Don't retry on context cancellation
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		slog.Warn("request failed", "attempt", attempt+1, "error", lastErr)
	}

	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func (c *Client) doRequest(ctx context.Context, urlStr string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if httpResp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited (429)")
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(body))
	}

	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &resp, nil
}

// FetchTickers fetches tickers from SHARADAR/TICKERS for SF1 table.
// If tickers slice is empty, fetches all tickers.
func (c *Client) FetchTickers(ctx context.Context, tickers []string) ([]TickerRow, error) {
	params := map[string]string{
		"table": "SF1",
	}

	if len(tickers) > 0 {
		params["ticker"] = strings.Join(tickers, ",")
	}

	resp, err := c.FetchTable(ctx, "SHARADAR/TICKERS", params)
	if err != nil {
		return nil, fmt.Errorf("fetching tickers: %w", err)
	}

	return ParseTickers(resp)
}

// SF1Batch represents a batch of SF1 rows from the API.
type SF1Batch struct {
	Rows  []SF1Row
	Error error
}

const apiBatchSize = 100 // Tickers per API request to avoid 414 errors

// FetchSF1Stream fetches SF1 data with parallel API requests.
// Uses up to maxParallel concurrent fetchers, streaming results to channel.
func (c *Client) FetchSF1Stream(ctx context.Context, tickers []string, dimension string, since time.Time, maxParallel int) <-chan SF1Batch {
	ch := make(chan SF1Batch, maxParallel)

	go func() {
		defer close(ch)

		// If small batch, send directly
		if len(tickers) <= apiBatchSize {
			rows, err := c.fetchSF1Batch(ctx, tickers, dimension, since)
			select {
			case ch <- SF1Batch{Rows: rows, Error: err}:
			case <-ctx.Done():
			}
			return
		}

		// Parallel fetch with semaphore
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxParallel)

		for i := 0; i < len(tickers); i += apiBatchSize {
			if ctx.Err() != nil {
				break
			}

			end := i + apiBatchSize
			if end > len(tickers) {
				end = len(tickers)
			}
			batch := tickers[i:end]
			batchNum := i/apiBatchSize + 1

			sem <- struct{}{} // Acquire slot
			wg.Add(1)

			go func(batch []string, num int) {
				defer wg.Done()
				defer func() { <-sem }() // Release slot

				slog.Info("fetching SF1 batch", "batch", num, "ticker_count", len(batch), "dimension", dimension)
				rows, err := c.fetchSF1Batch(ctx, batch, dimension, since)

				select {
				case ch <- SF1Batch{Rows: rows, Error: err}:
				case <-ctx.Done():
				}
			}(batch, batchNum)
		}

		wg.Wait()
	}()

	return ch
}

// fetchSF1Batch fetches a single batch of SF1 data.
func (c *Client) fetchSF1Batch(ctx context.Context, tickers []string, dimension string, since time.Time) ([]SF1Row, error) {
	params := make(map[string]string)

	if len(tickers) > 0 {
		params["ticker"] = strings.Join(tickers, ",")
	}

	if dimension != "" {
		params["dimension"] = dimension
	}

	if !since.IsZero() {
		params["lastupdated.gte"] = since.Format("2006-01-02")
	}

	resp, err := c.FetchTable(ctx, "SHARADAR/SF1", params)
	if err != nil {
		return nil, fmt.Errorf("fetching SF1: %w", err)
	}

	return ParseSF1(resp)
}

// DailyBatch represents a batch of daily rows from the API.
type DailyBatch struct {
	Rows  []DailyRow
	Error error
}

// FetchDaily fetches daily prices from SHARADAR/DAILY for a small set of tickers.
func (c *Client) FetchDaily(ctx context.Context, tickers []string, since time.Time) ([]DailyRow, error) {
	if len(tickers) == 0 {
		return nil, fmt.Errorf("at least one ticker required for daily fetch")
	}

	return c.fetchDailyBatch(ctx, tickers, since)
}

// fetchDailyBatch fetches a single batch of daily data.
func (c *Client) fetchDailyBatch(ctx context.Context, tickers []string, since time.Time) ([]DailyRow, error) {
	params := map[string]string{
		"ticker": strings.Join(tickers, ","),
	}

	if !since.IsZero() {
		params["date.gte"] = since.Format("2006-01-02")
	}

	resp, err := c.FetchTable(ctx, "SHARADAR/DAILY", params)
	if err != nil {
		return nil, fmt.Errorf("fetching daily: %w", err)
	}

	return ParseDaily(resp)
}

// FetchDailyStream fetches daily data with parallel API requests.
// Uses up to maxParallel concurrent fetchers, streaming results to channel.
func (c *Client) FetchDailyStream(ctx context.Context, tickers []string, since time.Time, maxParallel int) <-chan DailyBatch {
	ch := make(chan DailyBatch, maxParallel)

	go func() {
		defer close(ch)

		if len(tickers) == 0 {
			return
		}

		// Small batch - fetch directly
		if len(tickers) <= apiBatchSize {
			rows, err := c.fetchDailyBatch(ctx, tickers, since)
			select {
			case ch <- DailyBatch{Rows: rows, Error: err}:
			case <-ctx.Done():
			}
			return
		}

		// Parallel fetch with semaphore
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxParallel)

		for i := 0; i < len(tickers); i += apiBatchSize {
			if ctx.Err() != nil {
				break
			}

			end := i + apiBatchSize
			if end > len(tickers) {
				end = len(tickers)
			}
			batch := tickers[i:end]
			batchNum := i/apiBatchSize + 1

			sem <- struct{}{} // Acquire slot
			wg.Add(1)

			go func(batch []string, num int) {
				defer wg.Done()
				defer func() { <-sem }() // Release slot

				slog.Info("fetching daily batch", "batch", num, "ticker_count", len(batch))
				rows, err := c.fetchDailyBatch(ctx, batch, since)

				select {
				case ch <- DailyBatch{Rows: rows, Error: err}:
				case <-ctx.Done():
				}
			}(batch, batchNum)
		}

		wg.Wait()
	}()

	return ch
}

// SEPBatch represents a batch of SEP rows from the API.
type SEPBatch struct {
	Rows  []DailyRow
	Error error
}

// FetchSEP fetches daily prices from SHARADAR/SEP (Sharadar Equity Prices).
// This table provides adjusted close prices (split + dividend adjusted) for accurate backtesting.
// Date range is optional - if startDate or endDate is zero, they are not included in the query.
func (c *Client) FetchSEP(ctx context.Context, tickers []string, startDate, endDate time.Time) ([]DailyRow, error) {
	if len(tickers) == 0 {
		return nil, fmt.Errorf("at least one ticker required for SEP fetch")
	}

	params := map[string]string{
		"ticker": strings.Join(tickers, ","),
	}

	if !startDate.IsZero() {
		params["date.gte"] = startDate.Format("2006-01-02")
	}
	if !endDate.IsZero() {
		params["date.lte"] = endDate.Format("2006-01-02")
	}

	slog.Info("fetching SEP prices", "ticker_count", len(tickers), "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))

	resp, err := c.FetchTable(ctx, "SHARADAR/SEP", params)
	if err != nil {
		return nil, fmt.Errorf("fetching SEP: %w", err)
	}

	return ParseSEP(resp)
}

// FetchSEPStream fetches SEP data with parallel API requests for many tickers.
func (c *Client) FetchSEPStream(ctx context.Context, tickers []string, startDate, endDate time.Time, maxParallel int) <-chan SEPBatch {
	ch := make(chan SEPBatch, maxParallel)

	go func() {
		defer close(ch)

		if len(tickers) == 0 {
			return
		}

		// Small batch - fetch directly
		if len(tickers) <= apiBatchSize {
			rows, err := c.FetchSEP(ctx, tickers, startDate, endDate)
			select {
			case ch <- SEPBatch{Rows: rows, Error: err}:
			case <-ctx.Done():
			}
			return
		}

		// Parallel fetch with semaphore
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxParallel)

		for i := 0; i < len(tickers); i += apiBatchSize {
			if ctx.Err() != nil {
				break
			}

			end := i + apiBatchSize
			if end > len(tickers) {
				end = len(tickers)
			}
			batch := tickers[i:end]
			batchNum := i/apiBatchSize + 1

			sem <- struct{}{} // Acquire slot
			wg.Add(1)

			go func(batch []string, num int) {
				defer wg.Done()
				defer func() { <-sem }() // Release slot

				slog.Info("fetching SEP batch", "batch", num, "ticker_count", len(batch))
				rows, err := c.FetchSEP(ctx, batch, startDate, endDate)

				select {
				case ch <- SEPBatch{Rows: rows, Error: err}:
				case <-ctx.Done():
				}
			}(batch, batchNum)
		}

		wg.Wait()
	}()

	return ch
}

// FetchSFP fetches daily prices from SHARADAR/SFP (Sharadar Fund Prices).
// This table covers ETFs, CEFs, ETNs, and ETD securities (not individual equities).
// Use this for benchmarks like SPY, QQQ, IWM, etc.
func (c *Client) FetchSFP(ctx context.Context, tickers []string, startDate, endDate time.Time) ([]DailyRow, error) {
	if len(tickers) == 0 {
		return nil, fmt.Errorf("at least one ticker required for SFP fetch")
	}

	params := map[string]string{
		"ticker": strings.Join(tickers, ","),
	}

	if !startDate.IsZero() {
		params["date.gte"] = startDate.Format("2006-01-02")
	}
	if !endDate.IsZero() {
		params["date.lte"] = endDate.Format("2006-01-02")
	}

	slog.Info("fetching SFP fund prices", "ticker_count", len(tickers), "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))

	resp, err := c.FetchTable(ctx, "SHARADAR/SFP", params)
	if err != nil {
		return nil, fmt.Errorf("fetching SFP: %w", err)
	}

	// SFP has same structure as SEP, so we can reuse the parser
	return ParseSEP(resp)
}

// FetchSP500Current fetches current S&P 500 constituents.
func (c *Client) FetchSP500Current(ctx context.Context) ([]string, error) {
	params := map[string]string{
		"action": "current",
	}

	resp, err := c.FetchTable(ctx, "SHARADAR/SP500", params)
	if err != nil {
		return nil, fmt.Errorf("fetching SP500: %w", err)
	}

	rows, err := ParseSP500(resp)
	if err != nil {
		return nil, err
	}

	tickers := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Ticker != "" {
			tickers = append(tickers, row.Ticker)
		}
	}

	return tickers, nil
}

// FetchSP500History fetches full S&P 500 membership history (all add/drop events).
// This is used for point-in-time backtesting to know which stocks were in the index at any date.
func (c *Client) FetchSP500History(ctx context.Context) ([]SP500Row, error) {
	// Fetch all membership changes (no action filter = get all)
	resp, err := c.FetchTable(ctx, "SHARADAR/SP500", nil)
	if err != nil {
		return nil, fmt.Errorf("fetching SP500 history: %w", err)
	}

	rows, err := ParseSP500(resp)
	if err != nil {
		return nil, err
	}

	slog.Info("fetched SP500 membership records", "count", len(rows))
	return rows, nil
}
