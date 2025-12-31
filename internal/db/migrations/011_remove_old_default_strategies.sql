-- +goose Up
-- Remove old default strategies "Magic Formula" and "Deep Value"
-- Keep only "Quality Value (Python Backtest)"
DELETE FROM strategies WHERE name IN ('Magic Formula', 'Deep Value') AND is_default = true;

-- +goose Down
-- Re-insert the strategies if needed (optional rollback)
-- Note: This won't restore the exact original data, just placeholders
INSERT INTO strategies (name, description, rules, is_default, created_at, updated_at)
VALUES
    ('Magic Formula', 'Joel Greenblatt''s Magic Formula: High ROIC + Low EV/EBIT. Equal weight ranking.', '{"filters":[{"field":"market_cap","operator":">=","value":500000000},{"field":"roic","operator":">","value":0},{"field":"ev_ebit","operator":">","value":0},{"field":"sector","operator":"not_in","value":["Financial Services","Utilities"]}],"ranking":[{"field":"roic","direction":"desc","weight":50},{"field":"ev_ebit","direction":"asc","weight":50}],"dimension":"MRQ","limit":6}', true, NOW(), NOW()),
    ('Deep Value', 'Aggressive value: very low EV/EBIT with minimal quality floor. Higher risk, higher potential reward.', '{"filters":[{"field":"market_cap","operator":">=","value":100000000},{"field":"ev_ebit","operator":">","value":0},{"field":"ev_ebit","operator":"<=","value":10},{"field":"roe","operator":">","value":0.05},{"field":"sector","operator":"not_in","value":["Financial Services"]}],"ranking":[{"field":"ev_ebit","direction":"asc","weight":80},{"field":"roe","direction":"desc","weight":20}],"dimension":"MRQ","limit":6}', true, NOW(), NOW())
ON CONFLICT (name) DO NOTHING;
