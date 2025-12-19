-- +goose Up

-- Benchmark tickers (SPY, QQQ, etc.) - separate from companies
CREATE TABLE benchmarks (
    ticker TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Insert common benchmarks
INSERT INTO benchmarks (ticker, name, description) VALUES
    ('SPY', 'SPDR S&P 500 ETF Trust', 'S&P 500 Index ETF');

-- Benchmark daily prices (no FK to companies)
CREATE TABLE benchmark_prices (
    id SERIAL PRIMARY KEY,
    ticker TEXT NOT NULL REFERENCES benchmarks(ticker),
    date DATE NOT NULL,
    open DECIMAL(18, 6),
    high DECIMAL(18, 6),
    low DECIMAL(18, 6),
    close DECIMAL(18, 6),
    volume BIGINT,
    dividends DECIMAL(18, 6),
    close_unadj DECIMAL(18, 6),
    last_updated TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(ticker, date)
);

CREATE INDEX idx_benchmark_prices_ticker ON benchmark_prices(ticker);
CREATE INDEX idx_benchmark_prices_date ON benchmark_prices(date);

-- +goose Down
DROP TABLE IF EXISTS benchmark_prices;
DROP TABLE IF EXISTS benchmarks;