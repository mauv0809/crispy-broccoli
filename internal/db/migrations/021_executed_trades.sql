-- +goose Up
CREATE TABLE executed_trades (
    id            BIGSERIAL PRIMARY KEY,
    portfolio_id  BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    proposal_id   BIGINT REFERENCES proposals(id) ON DELETE SET NULL,
    ticker        TEXT NOT NULL REFERENCES companies(ticker) ON DELETE RESTRICT,
    action        TEXT NOT NULL CHECK (action IN ('buy','sell')),
    shares        NUMERIC(18,6) NOT NULL CHECK (shares > 0),
    price         NUMERIC(18,4) NOT NULL CHECK (price > 0),
    fee           NUMERIC(18,2) NOT NULL DEFAULT 0 CHECK (fee >= 0),
    executed_at   TIMESTAMPTZ NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notes         TEXT
);

CREATE INDEX executed_trades_portfolio_idx
    ON executed_trades(portfolio_id, executed_at DESC);
CREATE INDEX executed_trades_proposal_idx ON executed_trades(proposal_id);

-- +goose Down
DROP TABLE IF EXISTS executed_trades;
