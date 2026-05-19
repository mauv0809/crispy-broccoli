-- +goose Up
-- Strategy names are treated as unique by SeedDefaultStrategies and
-- CreateDefaultStrategy. Make the schema match.
CREATE UNIQUE INDEX strategies_name_unique ON strategies(name);

-- +goose Down
DROP INDEX IF EXISTS strategies_name_unique;
