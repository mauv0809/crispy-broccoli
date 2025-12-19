-- +goose Up

-- Settings for configurable values (e.g., portfolio size)
CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

INSERT INTO settings (key, value) VALUES ('portfolio_size', '6');

-- Companies master list
CREATE TABLE companies (
    ticker TEXT PRIMARY KEY,
    name TEXT,
    sector TEXT,
    industry TEXT,
    active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Financial metrics from Sharadar SF1 dataset
CREATE TABLE financial_metrics (
    id SERIAL PRIMARY KEY,
    ticker TEXT NOT NULL REFERENCES companies(ticker),
    date_key DATE NOT NULL,
    report_period DATE NOT NULL,

    -- Fundamentals (DECIMAL for precision)
    revenue DECIMAL(18, 2),
    net_income DECIMAL(18, 2),
    ebitda DECIMAL(18, 2),
    fcf DECIMAL(18, 2),

    -- Valuation ratios
    roic DECIMAL(10, 4),
    pe_ratio DECIMAL(10, 4),
    ev_ebit DECIMAL(10, 4),
    pb_ratio DECIMAL(10, 4),
    debt_to_equity DECIMAL(10, 4),

    -- Market data
    market_cap DECIMAL(18, 2),
    enterprise_value DECIMAL(18, 2),
    price DECIMAL(18, 6),

    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW(),

    UNIQUE(ticker, date_key)
);

-- Portfolio holdings
CREATE TABLE portfolio (
    id SERIAL PRIMARY KEY,
    ticker TEXT NOT NULL REFERENCES companies(ticker),
    shares_owned DECIMAL(18, 6) NOT NULL DEFAULT 0,
    cost_basis DECIMAL(18, 2),
    target_weight DECIMAL(5, 4),
    acquired_date DATE,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Indexes for common queries
CREATE INDEX idx_financial_metrics_ticker ON financial_metrics(ticker);
CREATE INDEX idx_financial_metrics_date_key ON financial_metrics(date_key);
CREATE INDEX idx_portfolio_ticker ON portfolio(ticker);

-- +goose Down
DROP TABLE IF EXISTS portfolio;
DROP TABLE IF EXISTS financial_metrics;
DROP TABLE IF EXISTS companies;
DROP TABLE IF EXISTS settings;
