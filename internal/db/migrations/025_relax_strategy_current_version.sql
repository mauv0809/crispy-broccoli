-- +goose Up
-- Relax NOT NULL on strategies.current_version_id so the new transactional
-- Create flow can insert a strategies row before its v1 row exists. The
-- invariant ("every strategy has a current_version_id once Create returns")
-- is maintained in Repository.Create and Repository.CreateDefaultStrategy
-- instead of at the database level.
ALTER TABLE strategies ALTER COLUMN current_version_id DROP NOT NULL;

-- +goose Down
-- Cannot reliably re-tighten without ensuring all rows are populated; left
-- as a no-op. The constraint can be re-added manually after backfill.
