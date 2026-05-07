-- +goose Up

-- Nullable on purpose: pre-existing rows have no owner. New writes populate it.
ALTER TABLE strategies     ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE strategy_runs  ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE portfolio      ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX strategies_created_by_idx     ON strategies     (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX strategy_runs_created_by_idx  ON strategy_runs  (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX portfolio_created_by_idx      ON portfolio      (created_by) WHERE created_by IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS portfolio_created_by_idx;
DROP INDEX IF EXISTS strategy_runs_created_by_idx;
DROP INDEX IF EXISTS strategies_created_by_idx;
ALTER TABLE portfolio      DROP COLUMN IF EXISTS created_by;
ALTER TABLE strategy_runs  DROP COLUMN IF EXISTS created_by;
ALTER TABLE strategies     DROP COLUMN IF EXISTS created_by;
