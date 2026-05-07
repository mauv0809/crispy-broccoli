package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mauv0809/crispy-broccoli/internal/ingest"
	"github.com/shopspring/decimal"
)

const dbBatchSize = 1000 // Rows per database batch for resilience

// Repository handles database operations for ingested data.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a new repository.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// UpsertCompanies inserts or updates companies from ticker data.
// Returns the number of rows affected.
func (r *Repository) UpsertCompanies(ctx context.Context, tickers []ingest.TickerRow) (int, error) {
	if len(tickers) == 0 {
		return 0, nil
	}

	batch := &pgx.Batch{}
	for _, t := range tickers {
		batch.Queue(`
			INSERT INTO companies (ticker, name, sector, industry, active, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (ticker) DO UPDATE SET
				name = EXCLUDED.name,
				sector = EXCLUDED.sector,
				industry = EXCLUDED.industry,
				active = EXCLUDED.active,
				updated_at = NOW()
		`, t.Ticker, t.Name, t.Sector, t.Industry, !t.IsDelisted)
	}

	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()

	count := 0
	for range tickers {
		_, err := br.Exec()
		if err != nil {
			return count, fmt.Errorf("upserting company: %w", err)
		}
		count++
	}

	return count, nil
}

// UpsertFinancialMetrics inserts or updates financial metrics from SF1 data.
// Processes in batches for resilience - a single bad row won't fail the entire import.
func (r *Repository) UpsertFinancialMetrics(ctx context.Context, rows []ingest.SF1Row) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	totalCount := 0
	var lastErr error

	for i := 0; i < len(rows); i += dbBatchSize {
		end := i + dbBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		batchRows := rows[i:end]

		count, err := r.upsertFinancialMetricsBatch(ctx, batchRows)
		totalCount += count
		if err != nil {
			slog.Error("metrics batch upsert error", "batch_start", i, "batch_end", end, "error", err, "inserted_before_error", count)
			lastErr = err
			// Continue with next batch instead of failing entirely
		}
	}

	if lastErr != nil && totalCount == 0 {
		return 0, lastErr
	}

	return totalCount, nil
}

func (r *Repository) upsertFinancialMetricsBatch(ctx context.Context, rows []ingest.SF1Row) (int, error) {
	batch := &pgx.Batch{}
	for _, row := range rows {
		reportPeriod := row.DateKey
		if row.ReportPeriod != nil {
			reportPeriod = *row.ReportPeriod
		}

		// Calculate ROE = net_income / equity (proper ROE formula)
		var roe interface{}
		if row.NetIncome != nil && row.Equity != nil && !row.Equity.IsZero() {
			roeVal := row.NetIncome.Div(*row.Equity)
			roe = sanitizeDecimal(&roeVal, "roe", row.Ticker, 4)
		}

		// Calculate ROA = net_income / assets (called "roe" in Python strategy)
		var roa interface{}
		if row.NetIncome != nil && row.Assets != nil && !row.Assets.IsZero() {
			roaVal := row.NetIncome.Div(*row.Assets)
			roa = sanitizeDecimal(&roaVal, "roa", row.Ticker, 4)
		}

		// Calculate GP/A = gross_profit / assets (quality metric from Python strategy)
		var gpA interface{}
		if row.GrossProfit != nil && row.Assets != nil && !row.Assets.IsZero() {
			gpAVal := row.GrossProfit.Div(*row.Assets)
			gpA = sanitizeDecimal(&gpAVal, "gp_a", row.Ticker, 4)
		}

		// Calculate Accruals = fcf / net_income (earnings quality from Python strategy)
		var accruals interface{}
		if row.FCF != nil && row.NetIncome != nil && !row.NetIncome.IsZero() {
			accrualsVal := row.FCF.Div(*row.NetIncome)
			accruals = sanitizeDecimal(&accrualsVal, "accruals", row.Ticker, 4)
		}

		batch.Queue(`
			INSERT INTO financial_metrics (
				ticker, dimension, date_key, report_period,
				revenue, net_income, ebitda, fcf,
				roic, pe_ratio, ev_ebit, pb_ratio, debt_to_equity,
				market_cap, enterprise_value, price,
				assets, gross_profit, roe, roa, gp_a, accruals,
				last_updated, updated_at
			) VALUES (
				$1, $2, $3, $4,
				$5, $6, $7, $8,
				$9, $10, $11, $12, $13,
				$14, $15, $16,
				$17, $18, $19, $20, $21, $22,
				$23, NOW()
			)
			ON CONFLICT (ticker, date_key, dimension) DO UPDATE SET
				report_period = EXCLUDED.report_period,
				revenue = EXCLUDED.revenue,
				net_income = EXCLUDED.net_income,
				ebitda = EXCLUDED.ebitda,
				fcf = EXCLUDED.fcf,
				roic = EXCLUDED.roic,
				pe_ratio = EXCLUDED.pe_ratio,
				ev_ebit = EXCLUDED.ev_ebit,
				pb_ratio = EXCLUDED.pb_ratio,
				debt_to_equity = EXCLUDED.debt_to_equity,
				market_cap = EXCLUDED.market_cap,
				enterprise_value = EXCLUDED.enterprise_value,
				price = EXCLUDED.price,
				assets = EXCLUDED.assets,
				gross_profit = EXCLUDED.gross_profit,
				roe = EXCLUDED.roe,
				roa = EXCLUDED.roa,
				gp_a = EXCLUDED.gp_a,
				accruals = EXCLUDED.accruals,
				last_updated = EXCLUDED.last_updated,
				updated_at = NOW()
		`,
			row.Ticker, row.Dimension, row.DateKey, reportPeriod,
			sanitizeDecimal(row.Revenue, "revenue", row.Ticker, 2),
			sanitizeDecimal(row.NetIncome, "net_income", row.Ticker, 2),
			sanitizeDecimal(row.EBITDA, "ebitda", row.Ticker, 2),
			sanitizeDecimal(row.FCF, "fcf", row.Ticker, 2),
			sanitizeDecimal(row.ROIC, "roic", row.Ticker, 4),
			sanitizeDecimal(row.PE, "pe_ratio", row.Ticker, 4),
			sanitizeDecimal(row.EVEBIT, "ev_ebit", row.Ticker, 4),
			sanitizeDecimal(row.PB, "pb_ratio", row.Ticker, 4),
			sanitizeDecimal(row.DE, "debt_to_equity", row.Ticker, 4),
			sanitizeDecimal(row.MarketCap, "market_cap", row.Ticker, 2),
			sanitizeDecimal(row.EV, "enterprise_value", row.Ticker, 2),
			sanitizeDecimal(row.Price, "price", row.Ticker, 6),
			sanitizeDecimal(row.Assets, "assets", row.Ticker, 2),
			sanitizeDecimal(row.GrossProfit, "gross_profit", row.Ticker, 2),
			roe,
			roa,
			gpA,
			accruals,
			row.LastUpdated,
		)
	}

	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()

	count := 0
	for range rows {
		_, err := br.Exec()
		if err != nil {
			return count, fmt.Errorf("upserting financial metric: %w", err)
		}
		count++
	}

	return count, nil
}

// UpsertDailyPrices inserts or updates daily price data.
// Processes in batches for resilience - a single bad row won't fail the entire import.
func (r *Repository) UpsertDailyPrices(ctx context.Context, rows []ingest.DailyRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	totalCount := 0
	var lastErr error

	for i := 0; i < len(rows); i += dbBatchSize {
		end := i + dbBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		batchRows := rows[i:end]

		count, err := r.upsertDailyPricesBatch(ctx, batchRows)
		totalCount += count
		if err != nil {
			slog.Error("daily prices batch upsert error", "batch_start", i, "batch_end", end, "error", err, "inserted_before_error", count)
			lastErr = err
		}
	}

	if lastErr != nil && totalCount == 0 {
		return 0, lastErr
	}

	return totalCount, nil
}

func (r *Repository) upsertDailyPricesBatch(ctx context.Context, rows []ingest.DailyRow) (int, error) {
	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(`
			INSERT INTO daily_prices (
				ticker, date, open, high, low, close, volume,
				dividends, close_unadj, market_cap, enterprise_value,
				pe_ratio, pb_ratio, last_updated
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9, $10, $11,
				$12, $13, $14
			)
			ON CONFLICT (ticker, date) DO UPDATE SET
				open = EXCLUDED.open,
				high = EXCLUDED.high,
				low = EXCLUDED.low,
				close = EXCLUDED.close,
				volume = EXCLUDED.volume,
				dividends = EXCLUDED.dividends,
				close_unadj = EXCLUDED.close_unadj,
				market_cap = EXCLUDED.market_cap,
				enterprise_value = EXCLUDED.enterprise_value,
				pe_ratio = EXCLUDED.pe_ratio,
				pb_ratio = EXCLUDED.pb_ratio,
				last_updated = EXCLUDED.last_updated
		`,
			row.Ticker, row.Date,
			decimalPtr(row.Open), decimalPtr(row.High), decimalPtr(row.Low), decimalPtr(row.Close),
			row.Volume,
			decimalPtr(row.Dividends), decimalPtr(row.CloseUnadj),
			sanitizeDecimal(row.MarketCap, "market_cap", row.Ticker, 2),
			sanitizeDecimal(row.EV, "enterprise_value", row.Ticker, 2),
			sanitizeDecimal(row.PE, "pe_ratio", row.Ticker, 4),
			sanitizeDecimal(row.PB, "pb_ratio", row.Ticker, 4),
			row.LastUpdated,
		)
	}

	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()

	count := 0
	for range rows {
		_, err := br.Exec()
		if err != nil {
			return count, fmt.Errorf("upserting daily price: %w", err)
		}
		count++
	}

	return count, nil
}

// GetLastSharadarUpdate returns the most recent update timestamp for a table.
// For financial_metrics, returns MAX(last_updated) since we use lastupdated.gte for API filtering.
// For daily_prices, returns MAX(date) since we use date.gte for API filtering.
func (r *Repository) GetLastSharadarUpdate(ctx context.Context, table string) (time.Time, error) {
	var query string
	switch table {
	case "financial_metrics":
		query = "SELECT COALESCE(MAX(last_updated), '1970-01-01'::timestamp) FROM financial_metrics"
	case "daily_prices":
		// Use MAX(date) not MAX(last_updated) because Sharadar updates last_updated daily for ALL rows
		query = "SELECT COALESCE(MAX(date), '1970-01-01'::date)::timestamp FROM daily_prices"
	default:
		return time.Time{}, fmt.Errorf("unknown table: %s", table)
	}

	var lastUpdate time.Time
	err := r.pool.QueryRow(ctx, query).Scan(&lastUpdate)
	if err != nil {
		return time.Time{}, fmt.Errorf("querying last update: %w", err)
	}

	return lastUpdate, nil
}

// GetCompanyCount returns the number of companies in the database.
func (r *Repository) GetCompanyCount(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM companies").Scan(&count)
	return count, err
}

// CompanyExists checks if a company exists in the database.
func (r *Repository) CompanyExists(ctx context.Context, ticker string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM companies WHERE ticker = $1)", ticker).Scan(&exists)
	return exists, err
}

// Limits for DECIMAL columns (digits before decimal point)
var (
	maxDecimal18_2 = decimal.NewFromInt(1).Shift(16) // 10^16 for DECIMAL(18,2)
	maxDecimal18_4 = decimal.NewFromInt(1).Shift(14) // 10^14 for DECIMAL(18,4)
	maxDecimal18_6 = decimal.NewFromInt(1).Shift(12) // 10^12 for DECIMAL(18,6)
)

// decimalPtr converts a *decimal.Decimal to interface{} for database insertion.
func decimalPtr(d *decimal.Decimal) interface{} {
	if d == nil {
		return nil
	}
	return *d
}

// sanitizeDecimal checks if value fits in column, logs and returns nil if overflow
func sanitizeDecimal(d *decimal.Decimal, field, ticker string, scale int) interface{} {
	if d == nil {
		return nil
	}

	var limit decimal.Decimal
	switch scale {
	case 2:
		limit = maxDecimal18_2
	case 4:
		limit = maxDecimal18_4
	case 6:
		limit = maxDecimal18_6
	default:
		limit = maxDecimal18_4
	}

	abs := d.Abs()
	if abs.GreaterThan(limit) {
		slog.Warn("decimal overflow, skipping value", "ticker", ticker, "field", field, "value", d.String(), "scale", scale)
		return nil // Skip this value instead of failing
	}
	return *d
}

// GetAllTickers returns all tickers from the companies table.
func (r *Repository) GetAllTickers(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, "SELECT ticker FROM companies WHERE active = true ORDER BY ticker")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tickers []string
	for rows.Next() {
		var ticker string
		if err := rows.Scan(&ticker); err != nil {
			return nil, err
		}
		tickers = append(tickers, ticker)
	}

	return tickers, rows.Err()
}

// GetMetricCount returns the number of financial metrics in the database.
func (r *Repository) GetMetricCount(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM financial_metrics").Scan(&count)
	return count, err
}

// GetDailyPriceCount returns the number of daily prices in the database.
func (r *Repository) GetDailyPriceCount(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM daily_prices").Scan(&count)
	return count, err
}

// GetBenchmarkTickers returns all benchmark tickers.
func (r *Repository) GetBenchmarkTickers(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, "SELECT ticker FROM benchmarks ORDER BY ticker")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tickers []string
	for rows.Next() {
		var ticker string
		if err := rows.Scan(&ticker); err != nil {
			return nil, err
		}
		tickers = append(tickers, ticker)
	}

	return tickers, rows.Err()
}

// UpsertBenchmarkPrices inserts or updates benchmark price data.
func (r *Repository) UpsertBenchmarkPrices(ctx context.Context, rows []ingest.DailyRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(`
			INSERT INTO benchmark_prices (
				ticker, date, open, high, low, close, volume,
				dividends, close_unadj, last_updated
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10
			)
			ON CONFLICT (ticker, date) DO UPDATE SET
				open = EXCLUDED.open,
				high = EXCLUDED.high,
				low = EXCLUDED.low,
				close = EXCLUDED.close,
				volume = EXCLUDED.volume,
				dividends = EXCLUDED.dividends,
				close_unadj = EXCLUDED.close_unadj,
				last_updated = EXCLUDED.last_updated
		`,
			row.Ticker, row.Date,
			decimalPtr(row.Open), decimalPtr(row.High), decimalPtr(row.Low), decimalPtr(row.Close),
			row.Volume,
			decimalPtr(row.Dividends), decimalPtr(row.CloseUnadj),
			row.LastUpdated,
		)
	}

	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()

	count := 0
	for range rows {
		_, err := br.Exec()
		if err != nil {
			return count, fmt.Errorf("upserting benchmark price: %w", err)
		}
		count++
	}

	return count, nil
}

// GetLastBenchmarkUpdate returns the most recent date for benchmark prices.
// Uses MAX(date) not MAX(last_updated) because Sharadar updates last_updated daily for ALL rows.
func (r *Repository) GetLastBenchmarkUpdate(ctx context.Context) (time.Time, error) {
	var lastUpdate time.Time
	err := r.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(date), '1970-01-01'::date)::timestamp FROM benchmark_prices",
	).Scan(&lastUpdate)
	if err != nil {
		return time.Time{}, fmt.Errorf("querying last benchmark update: %w", err)
	}
	return lastUpdate, nil
}

// GetBenchmarkPriceCount returns the number of benchmark prices in the database.
func (r *Repository) GetBenchmarkPriceCount(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM benchmark_prices").Scan(&count)
	return count, err
}

// PricePoint represents a single price data point for backtesting.
type PricePoint struct {
	Date  time.Time
	Close float64
}

// GetPricesForPeriod fetches daily closing prices for multiple tickers within a date range.
// Returns a map of ticker -> []PricePoint sorted by date.
func (r *Repository) GetPricesForPeriod(ctx context.Context, tickers []string, start, end time.Time) (map[string][]PricePoint, error) {
	if len(tickers) == 0 {
		return map[string][]PricePoint{}, nil
	}

	rows, err := r.pool.Query(ctx, `
		SELECT ticker, date, close
		FROM daily_prices
		WHERE ticker = ANY($1)
		  AND date >= $2
		  AND date <= $3
		  AND close IS NOT NULL
		ORDER BY ticker, date
	`, tickers, start, end)
	if err != nil {
		return nil, fmt.Errorf("querying prices for period: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]PricePoint)
	for rows.Next() {
		var ticker string
		var date time.Time
		var close float64

		if err := rows.Scan(&ticker, &date, &close); err != nil {
			return nil, fmt.Errorf("scanning price row: %w", err)
		}

		result[ticker] = append(result[ticker], PricePoint{Date: date, Close: close})
	}

	return result, rows.Err()
}

// GetBenchmarkPricesForPeriod fetches benchmark (e.g., SPY) closing prices for a date range.
func (r *Repository) GetBenchmarkPricesForPeriod(ctx context.Context, ticker string, start, end time.Time) ([]PricePoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT date, close
		FROM benchmark_prices
		WHERE ticker = $1
		  AND date >= $2
		  AND date <= $3
		  AND close IS NOT NULL
		ORDER BY date
	`, ticker, start, end)
	if err != nil {
		return nil, fmt.Errorf("querying benchmark prices: %w", err)
	}
	defer rows.Close()

	var result []PricePoint
	for rows.Next() {
		var date time.Time
		var close float64

		if err := rows.Scan(&date, &close); err != nil {
			return nil, fmt.Errorf("scanning benchmark price row: %w", err)
		}

		result = append(result, PricePoint{Date: date, Close: close})
	}

	return result, rows.Err()
}

// GetPriceOnOrBefore gets the closest price on or before the given date for a ticker.
// Useful for getting entry/exit prices at rebalance dates.
func (r *Repository) GetPriceOnOrBefore(ctx context.Context, ticker string, date time.Time) (*PricePoint, error) {
	var result PricePoint
	err := r.pool.QueryRow(ctx, `
		SELECT date, close
		FROM daily_prices
		WHERE ticker = $1
		  AND date <= $2
		  AND close IS NOT NULL
		ORDER BY date DESC
		LIMIT 1
	`, ticker, date).Scan(&result.Date, &result.Close)

	if err != nil {
		return nil, err // Could be pgx.ErrNoRows
	}
	return &result, nil
}

// GetBenchmarkPriceOnOrBefore gets the closest benchmark price on or before the given date.
func (r *Repository) GetBenchmarkPriceOnOrBefore(ctx context.Context, ticker string, date time.Time) (*PricePoint, error) {
	var result PricePoint
	err := r.pool.QueryRow(ctx, `
		SELECT date, close
		FROM benchmark_prices
		WHERE ticker = $1
		  AND date <= $2
		  AND close IS NOT NULL
		ORDER BY date DESC
		LIMIT 1
	`, ticker, date).Scan(&result.Date, &result.Close)

	if err != nil {
		return nil, err
	}
	return &result, nil
}

// GetDailyPriceCountForTicker returns the number of daily prices for a specific ticker.
func (r *Repository) GetDailyPriceCountForTicker(ctx context.Context, ticker string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM daily_prices WHERE ticker = $1", ticker).Scan(&count)
	return count, err
}

// GetBenchmarkPriceCountForTicker returns the number of benchmark prices for a specific ticker.
func (r *Repository) GetBenchmarkPriceCountForTicker(ctx context.Context, ticker string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM benchmark_prices WHERE ticker = $1", ticker).Scan(&count)
	return count, err
}

// UpsertSP500Membership inserts or updates S&P 500 membership records.
func (r *Repository) UpsertSP500Membership(ctx context.Context, rows []ingest.SP500Row) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(`
			INSERT INTO sp500_membership (ticker, date, action)
			VALUES ($1, $2, $3)
			ON CONFLICT (ticker, date, action) DO NOTHING
		`, row.Ticker, row.Date, row.Action)
	}

	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()

	count := 0
	for range rows {
		_, err := br.Exec()
		if err != nil {
			slog.Error("failed to upsert SP500 membership row", "error", err)
			continue
		}
		count++
	}

	return count, nil
}

// GetSP500MembersAtDate returns all tickers that were members of the S&P 500 on a given date.
// Uses point-in-time logic: a ticker is a member if it was added before the date and
// either never removed or removed after the date.
// Note: API action values are 'added' and 'removed' (not 'add'/'drop')
func (r *Repository) GetSP500MembersAtDate(ctx context.Context, date time.Time) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		WITH membership AS (
			SELECT
				ticker,
				MIN(CASE WHEN action = 'added' THEN date END) as add_date,
				MAX(CASE WHEN action = 'removed' THEN date END) as remove_date
			FROM sp500_membership
			GROUP BY ticker
		)
		SELECT ticker
		FROM membership
		WHERE add_date IS NOT NULL
		  AND add_date <= $1
		  AND (remove_date IS NULL OR remove_date > $1)
		ORDER BY ticker
	`, date)
	if err != nil {
		return nil, fmt.Errorf("querying SP500 members: %w", err)
	}
	defer rows.Close()

	var tickers []string
	for rows.Next() {
		var ticker string
		if err := rows.Scan(&ticker); err != nil {
			return nil, err
		}
		tickers = append(tickers, ticker)
	}

	return tickers, rows.Err()
}

// GetSP500MembershipCount returns the number of membership records in the database.
func (r *Repository) GetSP500MembershipCount(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM sp500_membership").Scan(&count)
	return count, err
}

// GetTickersWithoutPrices returns tickers that have no daily price data.
func (r *Repository) GetTickersWithoutPrices(ctx context.Context, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.ticker
		FROM companies c
		WHERE c.active = true
		  AND NOT EXISTS (
			SELECT 1 FROM daily_prices dp WHERE dp.ticker = c.ticker
		  )
		ORDER BY c.ticker
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tickers []string
	for rows.Next() {
		var ticker string
		if err := rows.Scan(&ticker); err != nil {
			return nil, err
		}
		tickers = append(tickers, ticker)
	}

	return tickers, rows.Err()
}

// TickerPriceStatus represents a ticker and its last price date.
type TickerPriceStatus struct {
	Ticker   string
	LastDate time.Time
}

// GetTickersNeedingPriceUpdate returns tickers that need price updates.
// Priority: 1) No prices at all (and not recently attempted), 2) Stale prices (older than staleDays)
// retryDays controls how long to wait before retrying tickers that had no data from Tiingo
func (r *Repository) GetTickersNeedingPriceUpdate(ctx context.Context, limit int, staleDays int, retryDays int) ([]TickerPriceStatus, error) {
	rows, err := r.pool.Query(ctx, `
		WITH ticker_status AS (
			SELECT
				c.ticker,
				c.price_fetch_attempted_at,
				MAX(dp.date) as last_date
			FROM companies c
			LEFT JOIN daily_prices dp ON c.ticker = dp.ticker
			WHERE c.active = true
			GROUP BY c.ticker, c.price_fetch_attempted_at
		)
		SELECT ticker, COALESCE(last_date, '1900-01-01'::date) as last_date
		FROM ticker_status
		WHERE (
			-- Has prices but they're stale
			last_date IS NOT NULL AND last_date < (CURRENT_DATE - ($1::integer))
		) OR (
			-- No prices and either never attempted or attempted long ago
			last_date IS NULL AND (
				price_fetch_attempted_at IS NULL
				OR price_fetch_attempted_at < (NOW() - ($3::integer * INTERVAL '1 day'))
			)
		)
		ORDER BY last_date ASC NULLS FIRST, ticker
		LIMIT $2
	`, staleDays, limit, retryDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []TickerPriceStatus
	for rows.Next() {
		var ts TickerPriceStatus
		if err := rows.Scan(&ts.Ticker, &ts.LastDate); err != nil {
			return nil, err
		}
		results = append(results, ts)
	}

	return results, rows.Err()
}

// GetLastPriceDateForTicker returns the most recent price date for a ticker.
func (r *Repository) GetLastPriceDateForTicker(ctx context.Context, ticker string) (time.Time, error) {
	var lastDate time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(date), '1900-01-01'::date)
		FROM daily_prices WHERE ticker = $1
	`, ticker).Scan(&lastDate)
	return lastDate, err
}

// GetLastBenchmarkPriceDateForTicker returns the most recent benchmark price date for a ticker.
func (r *Repository) GetLastBenchmarkPriceDateForTicker(ctx context.Context, ticker string) (time.Time, error) {
	var lastDate time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(date), '1900-01-01'::date)
		FROM benchmark_prices WHERE ticker = $1
	`, ticker).Scan(&lastDate)
	return lastDate, err
}

// MarkPriceFetchAttempted marks tickers as having had a price fetch attempt.
// This prevents repeatedly fetching prices for tickers that Tiingo doesn't have data for.
func (r *Repository) MarkPriceFetchAttempted(ctx context.Context, tickers []string) error {
	if len(tickers) == 0 {
		return nil
	}

	_, err := r.pool.Exec(ctx, `
		UPDATE companies
		SET price_fetch_attempted_at = NOW()
		WHERE ticker = ANY($1)
	`, tickers)
	return err
}