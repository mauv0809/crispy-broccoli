-- +goose Up

-- Track when we last attempted to fetch prices for a ticker
-- This prevents repeatedly fetching prices for tickers that Tiingo doesn't have data for
ALTER TABLE companies ADD COLUMN price_fetch_attempted_at TIMESTAMP;

-- Index to speed up the query for finding tickers needing price updates
CREATE INDEX idx_companies_price_fetch_attempted ON companies(price_fetch_attempted_at);

-- +goose Down

DROP INDEX IF EXISTS idx_companies_price_fetch_attempted;
ALTER TABLE companies DROP COLUMN IF EXISTS price_fetch_attempted_at;
