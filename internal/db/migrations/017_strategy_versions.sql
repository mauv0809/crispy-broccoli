-- +goose Up
CREATE TABLE strategy_versions (
    id              BIGSERIAL PRIMARY KEY,
    strategy_id     BIGINT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    version_number  INT NOT NULL,
    rules           JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by      BIGINT REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (strategy_id, version_number)
);

CREATE INDEX strategy_versions_strategy_idx
    ON strategy_versions(strategy_id, version_number DESC);

-- Backfill: create v1 for every existing strategy from its current rules.
INSERT INTO strategy_versions (strategy_id, version_number, rules, created_at, created_by)
SELECT s.id, 1, s.rules, s.created_at, s.created_by
FROM strategies s;

-- +goose Down
DROP TABLE IF EXISTS strategy_versions;
