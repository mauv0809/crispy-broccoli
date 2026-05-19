-- +goose Up
CREATE TABLE portfolios (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name                TEXT NOT NULL,
    starting_capital    NUMERIC(18,2) NOT NULL CHECK (starting_capital > 0),
    strategy_id         BIGINT NOT NULL REFERENCES strategies(id) ON DELETE RESTRICT,
    strategy_version_id BIGINT NOT NULL REFERENCES strategy_versions(id) ON DELETE RESTRICT,
    cadence             TEXT NOT NULL CHECK (cadence IN ('monthly','quarterly','semi_annual','annual')),
    next_rebalance_due  TIMESTAMPTZ,
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active','paused','archived')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX portfolios_user_idx ON portfolios(user_id);
CREATE INDEX portfolios_due_idx
    ON portfolios(next_rebalance_due)
    WHERE status = 'active' AND next_rebalance_due IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS portfolios;
