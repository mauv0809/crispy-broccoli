package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mauv0809/crispy-broccoli/internal/ingest"
	"github.com/shopspring/decimal"
)

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
func (r *Repository) UpsertFinancialMetrics(ctx context.Context, rows []ingest.SF1Row) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

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
			decimalPtr(row.Revenue), decimalPtr(row.NetIncome), decimalPtr(row.EBITDA), decimalPtr(row.FCF),
			decimalPtr(row.ROIC), decimalPtr(row.PE), decimalPtr(row.EVEBIT), decimalPtr(row.PB), decimalPtr(row.DE),
			decimalPtr(row.MarketCap), decimalPtr(row.EV), decimalPtr(row.Price),
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
func (r *Repository) UpsertDailyPrices(ctx context.Context, rows []ingest.DailyRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

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
			decimalPtr(row.MarketCap), decimalPtr(row.EV),
			decimalPtr(row.PE), decimalPtr(row.PB),
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

// GetLastSharadarUpdate returns the most recent lastupdated timestamp for a table.
func (r *Repository) GetLastSharadarUpdate(ctx context.Context, table string) (time.Time, error) {
	var query string
	switch table {
	case "financial_metrics":
		query = "SELECT COALESCE(MAX(last_updated), '1970-01-01'::timestamp) FROM financial_metrics"
	case "daily_prices":
		query = "SELECT COALESCE(MAX(last_updated), '1970-01-01'::timestamp) FROM daily_prices"
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

// decimalPtr converts a *decimal.Decimal to interface{} for database insertion.
func decimalPtr(d *decimal.Decimal) interface{} {
	if d == nil {
		return nil
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