-- +goose Up
DROP TABLE IF EXISTS portfolio;

-- +goose Down
-- Recreate the old portfolio table (best-effort restoration; data is not preserved).
CREATE TABLE portfolio (
    id SERIAL PRIMARY KEY,
    ticker TEXT NOT NULL REFERENCES companies(ticker),
    shares_owned DECIMAL(18, 6) NOT NULL DEFAULT 0,
    cost_basis DECIMAL(18, 2),
    target_weight DECIMAL(5, 4),
    acquired_date DATE,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW(),
    created_by BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX idx_portfolio_ticker ON portfolio(ticker);
CREATE INDEX portfolio_created_by_idx ON portfolio(created_by);
