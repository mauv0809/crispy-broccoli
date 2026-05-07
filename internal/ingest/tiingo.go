package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

const (
	tiingoBaseURL = "https://api.tiingo.com/tiingo/daily"

	// Tiingo free tier limits
	tiingoHourlyLimit  = 50
	tiingoDailyLimit   = 1000
	tiingoMonthlyLimit = 500 // unique symbols
)

// TiingoRateLimits tracks current rate limit status
type TiingoRateLimits struct {
	HourlyRemaining  int
	DailyRemaining   int
	MonthlyRemaining int // unique symbols remaining
	IsRateLimited    bool
	ResetTime        time.Time // when hourly limit resets
}

// TiingoClient is a client for Tiingo API (used for ETF benchmark data).
type TiingoClient struct {
	apiKey     string
	httpClient *http.Client

	// Rate limit tracking
	mu             sync.Mutex
	hourlyRequests []time.Time     // timestamps of requests in last hour
	dailyRequests  []time.Time     // timestamps of requests in last 24 hours
	uniqueSymbols  map[string]bool // symbols fetched this month
	monthStart     time.Time       // start of current tracking month
	rateLimited    bool            // currently rate limited
	rateLimitReset time.Time       // when rate limit resets
}

// NewTiingoClient creates a new Tiingo API client.
func NewTiingoClient(apiKey string) *TiingoClient {
	now := time.Now()
	return &TiingoClient{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		hourlyRequests: make([]time.Time, 0),
		dailyRequests:  make([]time.Time, 0),
		uniqueSymbols:  make(map[string]bool),
		monthStart:     time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
	}
}

// GetRateLimits returns current rate limit status
func (c *TiingoClient) GetRateLimits() TiingoRateLimits {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupOldRequests()
	c.resetMonthlyIfNeeded()

	hourlyUsed := len(c.hourlyRequests)
	dailyUsed := len(c.dailyRequests)
	monthlyUsed := len(c.uniqueSymbols)

	return TiingoRateLimits{
		HourlyRemaining:  max(0, tiingoHourlyLimit-hourlyUsed),
		DailyRemaining:   max(0, tiingoDailyLimit-dailyUsed),
		MonthlyRemaining: max(0, tiingoMonthlyLimit-monthlyUsed),
		IsRateLimited:    c.rateLimited && time.Now().Before(c.rateLimitReset),
		ResetTime:        c.rateLimitReset,
	}
}

// CanFetch returns true if we can make a request for the given ticker
func (c *TiingoClient) CanFetch(ticker string) bool {
	limits := c.GetRateLimits()

	if limits.IsRateLimited {
		return false
	}
	if limits.HourlyRemaining <= 0 || limits.DailyRemaining <= 0 {
		return false
	}
	// Check monthly symbol limit only for new symbols
	c.mu.Lock()
	isNewSymbol := !c.uniqueSymbols[ticker]
	c.mu.Unlock()

	if isNewSymbol && limits.MonthlyRemaining <= 0 {
		return false
	}
	return true
}

// recordRequest records a request for rate limit tracking
func (c *TiingoClient) recordRequest(ticker string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.hourlyRequests = append(c.hourlyRequests, now)
	c.dailyRequests = append(c.dailyRequests, now)
	c.uniqueSymbols[ticker] = true
}

// cleanupOldRequests removes requests older than the tracking window
func (c *TiingoClient) cleanupOldRequests() {
	now := time.Now()
	hourAgo := now.Add(-1 * time.Hour)
	dayAgo := now.Add(-24 * time.Hour)

	// Clean hourly
	newHourly := make([]time.Time, 0)
	for _, t := range c.hourlyRequests {
		if t.After(hourAgo) {
			newHourly = append(newHourly, t)
		}
	}
	c.hourlyRequests = newHourly

	// Clean daily
	newDaily := make([]time.Time, 0)
	for _, t := range c.dailyRequests {
		if t.After(dayAgo) {
			newDaily = append(newDaily, t)
		}
	}
	c.dailyRequests = newDaily
}

// resetMonthlyIfNeeded resets monthly tracking if we're in a new month
func (c *TiingoClient) resetMonthlyIfNeeded() {
	now := time.Now()
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if currentMonthStart.After(c.monthStart) {
		c.uniqueSymbols = make(map[string]bool)
		c.monthStart = currentMonthStart
	}
}

// setRateLimited marks the client as rate limited
func (c *TiingoClient) setRateLimited() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rateLimited = true
	c.rateLimitReset = time.Now().Add(1 * time.Hour) // assume 1 hour reset

	// Fill up hourly requests to show 0 remaining
	now := time.Now()
	c.hourlyRequests = make([]time.Time, tiingoHourlyLimit)
	for i := 0; i < tiingoHourlyLimit; i++ {
		c.hourlyRequests[i] = now
	}
	slog.Warn("tiingo rate limited by API", "reset_at", c.rateLimitReset.Format("15:04:05"))
}

// TiingoPriceRow represents a single day's price data from Tiingo.
type TiingoPriceRow struct {
	Date        time.Time       `json:"date"`
	Open        decimal.Decimal `json:"open"`
	High        decimal.Decimal `json:"high"`
	Low         decimal.Decimal `json:"low"`
	Close       decimal.Decimal `json:"close"`
	Volume      int64           `json:"volume"`
	AdjOpen     decimal.Decimal `json:"adjOpen"`
	AdjHigh     decimal.Decimal `json:"adjHigh"`
	AdjLow      decimal.Decimal `json:"adjLow"`
	AdjClose    decimal.Decimal `json:"adjClose"`
	AdjVolume   int64           `json:"adjVolume"`
	DivCash     decimal.Decimal `json:"divCash"`
	SplitFactor decimal.Decimal `json:"splitFactor"`
}

// ErrRateLimited is returned when Tiingo rate limit is exceeded
var ErrRateLimited = fmt.Errorf("tiingo rate limit exceeded")

// FetchDaily fetches daily prices for a ticker from Tiingo.
func (c *TiingoClient) FetchDaily(ctx context.Context, ticker string, startDate, endDate time.Time) ([]DailyRow, error) {
	// Check rate limits before making request
	if !c.CanFetch(ticker) {
		limits := c.GetRateLimits()
		slog.Warn("tiingo rate limited", "hourly_remaining", limits.HourlyRemaining, "daily_remaining", limits.DailyRemaining, "monthly_remaining", limits.MonthlyRemaining)
		return nil, ErrRateLimited
	}

	// Build URL
	u, _ := url.Parse(fmt.Sprintf("%s/%s/prices", tiingoBaseURL, ticker))
	q := u.Query()
	q.Set("token", c.apiKey)
	q.Set("format", "json")
	if !startDate.IsZero() {
		q.Set("startDate", startDate.Format("2006-1-2"))
	}
	if !endDate.IsZero() {
		q.Set("endDate", endDate.Format("2006-1-2"))
	}
	u.RawQuery = q.Encode()

	slog.Info("tiingo fetching daily prices", "ticker", ticker, "start", startDate.Format("2006-01-02"), "end", endDate.Format("2006-01-02"))

	// Make request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	// Handle rate limiting from server
	if resp.StatusCode == http.StatusTooManyRequests {
		c.setRateLimited()
		return nil, ErrRateLimited
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	// Record successful request for rate limit tracking
	c.recordRequest(ticker)

	// Parse JSON response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var tiingoPrices []TiingoPriceRow
	if err := json.Unmarshal(body, &tiingoPrices); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	// Convert to DailyRow format
	rows := make([]DailyRow, len(tiingoPrices))
	for i, tp := range tiingoPrices {
		adjClose := tp.AdjClose
		closeUnadj := tp.Close
		open := tp.AdjOpen
		high := tp.AdjHigh
		low := tp.AdjLow
		volume := tp.AdjVolume
		dividend := tp.DivCash

		rows[i] = DailyRow{
			Ticker:     ticker,
			Date:       tp.Date,
			Open:       &open,
			High:       &high,
			Low:        &low,
			Close:      &adjClose, // Use adjusted close for returns
			CloseUnadj: &closeUnadj,
			Volume:     &volume,
			Dividends:  &dividend,
		}
	}

	slog.Info("tiingo fetched daily prices", "count", len(rows), "ticker", ticker)
	return rows, nil
}
