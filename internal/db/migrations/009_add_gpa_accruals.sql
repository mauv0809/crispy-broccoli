-- +goose Up
-- Add calculated fields from Python strategy
ALTER TABLE financial_metrics ADD COLUMN IF NOT EXISTS gp_a DECIMAL(10, 4);     -- gross_profit / assets
ALTER TABLE financial_metrics ADD COLUMN IF NOT EXISTS accruals DECIMAL(10, 4); -- fcf / net_income

-- +goose Down
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS accruals;
ALTER TABLE financial_metrics DROP COLUMN IF EXISTS gp_a;
