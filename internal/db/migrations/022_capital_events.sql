-- +goose Up
CREATE TABLE capital_events (
    id            BIGSERIAL PRIMARY KEY,
    portfolio_id  BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    proposal_id   BIGINT REFERENCES proposals(id) ON DELETE SET NULL,
    amount        NUMERIC(18,2) NOT NULL,
    occurred_at   TIMESTAMPTZ NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notes         TEXT,
    CHECK (amount <> 0)
);

CREATE INDEX capital_events_portfolio_idx
    ON capital_events(portfolio_id, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS capital_events;
