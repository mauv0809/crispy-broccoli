package db

import (
	"context"
	"fmt"
	"log"
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
			log.Printf("Error in metrics batch %d-%d: %v (inserted %d before error)", i, end, err, count)
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

		batch.Queue(`
			INSERT INTO financial_metrics (
				ticker, dimension, date_key, report_period,
				revenue, net_income, ebitda, fcf,
				roic, pe_ratio, ev_ebit, pb_ratio, debt_to_equity,
				market_cap, enterprise_value, price,
				last_updated, updated_at
			) VALUES (
				$1, $2, $3, $4,
				$5, $6, $7, $8,
				$9, $10, $11, $12, $13,
				$14, $15, $16,
				$17, NOW()
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
			log.Printf("Error in daily batch %d-%d: %v (inserted %d before error)", i, end, err, count)
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
		log.Printf("OVERFLOW: %s.%s = %s (exceeds DECIMAL(18,%d) limit)", ticker, field, d.String(), scale)
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