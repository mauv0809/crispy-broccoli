-- +goose Up

-- Widen ratio columns in daily_prices to handle extreme values
ALTER TABLE daily_prices ALTER COLUMN pe_ratio TYPE DECIMAL(18, 4);
ALTER TABLE daily_prices ALTER COLUMN pb_ratio TYPE DECIMAL(18, 4);

-- +goose Down
ALTER TABLE daily_prices ALTER COLUMN pe_ratio TYPE DECIMAL(10, 4);
ALTER TABLE daily_prices ALTER COLUMN pb_ratio TYPE DECIMAL(10, 4);