-- +goose Up
-- Settings table is unused - portfolio_size is now stored per-strategy in rules.limit
DROP TABLE IF EXISTS settings;

-- +goose Down
CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
INSERT INTO settings (key, value) VALUES ('portfolio_size', '6');
