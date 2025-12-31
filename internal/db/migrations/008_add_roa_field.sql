-- +goose Up
-- Add ROA (Return on Assets) = net_income / assets
ALTER TABLE financial_metrics ADD COLUMN IF NOT EXISTS roa DECIMAL(10, 4);

-- +goose Down
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS roa;
