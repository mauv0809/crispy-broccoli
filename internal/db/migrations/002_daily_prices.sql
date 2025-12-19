-- +goose Up

-- Add dimension and last_updated to financial_metrics
ALTER TABLE financial_metrics ADD COLUMN dimension TEXT NOT NULL DEFAULT 'ARQ';
ALTER TABLE financial_metrics ADD COLUMN last_updated TIMESTAMP;
ALTER TABLE financial_metrics DROP CONSTRAINT IF EXISTS financial_metrics_ticker_date_key_key;
ALTER TABLE financial_metrics ADD CONSTRAINT financial_metrics_ticker_date_dimension_key
    UNIQUE(ticker, date_key, dimension);

-- Daily price data from SHARADAR/DAILY
CREATE TABLE daily_prices (
    id SERIAL PRIMARY KEY,
    ticker TEXT NOT NULL REFERENCES companies(ticker),
    date DATE NOT NULL,
    open DECIMAL(18, 6),
    high DECIMAL(18, 6),
    low DECIMAL(18, 6),
    close DECIMAL(18, 6),
    volume BIGINT,
    dividends DECIMAL(18, 6),
    close_unadj DECIMAL(18, 6),
    market_cap DECIMAL(18, 2),
    enterprise_value DECIMAL(18, 2),
    pe_ratio DECIMAL(10, 4),
    pb_ratio DECIMAL(10, 4),
    last_updated TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(ticker, date)
);

CREATE INDEX idx_daily_prices_ticker ON daily_prices(ticker);
CREATE INDEX idx_daily_prices_date ON daily_prices(date);

-- +goose Down
DROP TABLE IF EXISTS daily_prices;
ALTER TABLE financial_metrics DROP CONSTRAINT IF EXISTS financial_metrics_ticker_date_dimension_key;
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS dimension;
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS last_updated;
CREATE UNIQUE INDEX financial_metrics_ticker_date_key_key ON financial_metrics(ticker, date_key);