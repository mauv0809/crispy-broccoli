-- +goose Up

-- Add fields needed for ROE calculation
ALTER TABLE financial_metrics ADD COLUMN IF NOT EXISTS assets DECIMAL(18, 2);
ALTER TABLE financial_metrics ADD COLUMN IF NOT EXISTS gross_profit DECIMAL(18, 2);

-- ROE calculated as net_income / assets (following the Python strategy)
-- Stored as computed during ingestion for query performance
ALTER TABLE financial_metrics ADD COLUMN IF NOT EXISTS roe DECIMAL(10, 4);

-- +goose Down
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS roe;
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS gross_profit;
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS assets;
