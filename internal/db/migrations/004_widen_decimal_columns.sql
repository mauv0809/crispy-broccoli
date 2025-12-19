-- +goose Up

-- Widen ratio columns to handle extreme values (e.g., very high PE ratios)
ALTER TABLE financial_metrics ALTER COLUMN roic TYPE DECIMAL(18, 4);
ALTER TABLE financial_metrics ALTER COLUMN pe_ratio TYPE DECIMAL(18, 4);
ALTER TABLE financial_metrics ALTER COLUMN ev_ebit TYPE DECIMAL(18, 4);
ALTER TABLE financial_metrics ALTER COLUMN pb_ratio TYPE DECIMAL(18, 4);
ALTER TABLE financial_metrics ALTER COLUMN debt_to_equity TYPE DECIMAL(18, 4);

-- +goose Down
ALTER TABLE financial_metrics ALTER COLUMN roic TYPE DECIMAL(10, 4);
ALTER TABLE financial_metrics ALTER COLUMN pe_ratio TYPE DECIMAL(10, 4);
ALTER TABLE financial_metrics ALTER COLUMN ev_ebit TYPE DECIMAL(10, 4);
ALTER TABLE financial_metrics ALTER COLUMN pb_ratio TYPE DECIMAL(10, 4);
ALTER TABLE financial_metrics ALTER COLUMN debt_to_equity TYPE DECIMAL(10, 4);
