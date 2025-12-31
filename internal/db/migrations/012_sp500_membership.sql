-- +goose Up
-- S&P 500 membership history for point-in-time backtesting
-- Action values from API: 'added' (joined index), 'removed' (left index), 'current' (current member)
CREATE TABLE IF NOT EXISTS sp500_membership (
    id SERIAL PRIMARY KEY,
    ticker VARCHAR(20) NOT NULL,
    date DATE NOT NULL,
    action VARCHAR(10) NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(ticker, date, action)
);

CREATE INDEX idx_sp500_membership_ticker ON sp500_membership(ticker);
CREATE INDEX idx_sp500_membership_date ON sp500_membership(date);

-- +goose Down
DROP TABLE IF EXISTS sp500_membership;
