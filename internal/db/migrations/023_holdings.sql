-- +goose Up
CREATE TABLE holdings (
    portfolio_id   BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    ticker         TEXT NOT NULL REFERENCES companies(ticker) ON DELETE RESTRICT,
    shares         NUMERIC(18,6) NOT NULL CHECK (shares > 0),
    cost_basis     NUMERIC(18,2) NOT NULL CHECK (cost_basis >= 0),
    last_trade_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (portfolio_id, ticker)
);

-- +goose Down
DROP TABLE IF EXISTS holdings;
