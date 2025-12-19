package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
		log.Printf("Fetching next page (cursor: %s...)", (*cursorID)[:min(20, len(*cursorID))])
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
			log.Printf("Retry attempt %d after %v", attempt, backoff)
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

		log.Printf("Request failed (attempt %d): %v", attempt+1, lastErr)
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

// FetchSF1 fetches fundamentals from SHARADAR/SF1.
// If tickers is empty, fetches all. If since is zero, fetches all history.
func (c *Client) FetchSF1(ctx context.Context, tickers []string, dimension string, since time.Time) ([]SF1Row, error) {
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

// FetchDaily fetches daily prices from SHARADAR/DAILY.
// tickers is required (at least one ticker).
func (c *Client) FetchDaily(ctx context.Context, tickers []string, since time.Time) ([]DailyRow, error) {
	if len(tickers) == 0 {
		return nil, fmt.Errorf("at least one ticker required for daily fetch")
	}

	params := map[string]string{
		"ticker": strings.Join(tickers, ","),
	}

	if !since.IsZero() {
		params["lastupdated.gte"] = since.Format("2006-01-02")
	}

	resp, err := c.FetchTable(ctx, "SHARADAR/DAILY", params)
	if err != nil {
		return nil, fmt.Errorf("fetching daily: %w", err)
	}

	return ParseDaily(resp)
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