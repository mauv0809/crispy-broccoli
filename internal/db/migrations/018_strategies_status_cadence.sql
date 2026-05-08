-- +goose Up
ALTER TABLE strategies
    ADD COLUMN status              TEXT NOT NULL DEFAULT 'draft',
    ADD COLUMN default_cadence     TEXT,
    ADD COLUMN current_version_id  BIGINT REFERENCES strategy_versions(id);

-- Backfill: every existing strategy is considered verified (they exist and have been used).
UPDATE strategies SET status = 'verified';

-- Point current_version_id at the v1 row created in migration 017.
UPDATE strategies s
SET current_version_id = sv.id
FROM strategy_versions sv
WHERE sv.strategy_id = s.id AND sv.version_number = 1;

-- After backfill, current_version_id should never be null.
ALTER TABLE strategies
    ALTER COLUMN current_version_id SET NOT NULL,
    ADD CONSTRAINT strategies_status_check
        CHECK (status IN ('draft', 'verified', 'archived'));

-- +goose Down
ALTER TABLE strategies
    DROP CONSTRAINT IF EXISTS strategies_status_check,
    DROP COLUMN IF EXISTS current_version_id,
    DROP COLUMN IF EXISTS default_cadence,
    DROP COLUMN IF EXISTS status;
