-- +goose Up

-- Synthetic system user. Owns rows that predate per-user attribution and
-- rows authored by background processes (default-strategy seeding). Cannot
-- log in: is_active=false, and no auth_identities row will ever be created
-- for it (the email is reserved by UNIQUE on users.email).
INSERT INTO users (email, name, is_admin, is_active)
VALUES ('system@deepvalue.local', 'System', FALSE, FALSE)
ON CONFLICT (email) DO NOTHING;

-- Add the column nullable first so the backfill UPDATE has somewhere to write.
ALTER TABLE strategies     ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE RESTRICT;
ALTER TABLE strategy_runs  ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE RESTRICT;
ALTER TABLE portfolio      ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE RESTRICT;

-- Backfill every existing row to the system user.
UPDATE strategies     SET created_by = (SELECT id FROM users WHERE email = 'system@deepvalue.local') WHERE created_by IS NULL;
UPDATE strategy_runs  SET created_by = (SELECT id FROM users WHERE email = 'system@deepvalue.local') WHERE created_by IS NULL;
UPDATE portfolio      SET created_by = (SELECT id FROM users WHERE email = 'system@deepvalue.local') WHERE created_by IS NULL;

-- Now enforce NOT NULL. Every new write must populate created_by.
ALTER TABLE strategies     ALTER COLUMN created_by SET NOT NULL;
ALTER TABLE strategy_runs  ALTER COLUMN created_by SET NOT NULL;
ALTER TABLE portfolio      ALTER COLUMN created_by SET NOT NULL;

CREATE INDEX strategies_created_by_idx     ON strategies     (created_by);
CREATE INDEX strategy_runs_created_by_idx  ON strategy_runs  (created_by);
CREATE INDEX portfolio_created_by_idx      ON portfolio      (created_by);

-- +goose Down
DROP INDEX IF EXISTS portfolio_created_by_idx;
DROP INDEX IF EXISTS strategy_runs_created_by_idx;
DROP INDEX IF EXISTS strategies_created_by_idx;
ALTER TABLE portfolio      DROP COLUMN IF EXISTS created_by;
ALTER TABLE strategy_runs  DROP COLUMN IF EXISTS created_by;
ALTER TABLE strategies     DROP COLUMN IF EXISTS created_by;
DELETE FROM users WHERE email = 'system@deepvalue.local';
