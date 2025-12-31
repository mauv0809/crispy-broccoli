-- +goose Up

-- Strategy definitions
CREATE TABLE strategies (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    rules JSONB NOT NULL,  -- The composable rules structure
    is_default BOOLEAN DEFAULT FALSE,  -- Ship with app
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Strategy execution history
CREATE TABLE strategy_runs (
    id SERIAL PRIMARY KEY,
    strategy_id INT REFERENCES strategies(id) ON DELETE CASCADE,
    run_at TIMESTAMP DEFAULT NOW(),
    results JSONB,  -- Array of recommendations
    execution_time_ms INT,
    stocks_screened INT,
    stocks_matched INT
);

-- Indexes for fast lookups
CREATE INDEX idx_strategy_runs_strategy_id ON strategy_runs(strategy_id);
CREATE INDEX idx_strategy_runs_run_at ON strategy_runs(run_at DESC);

-- +goose Down
DROP TABLE IF EXISTS strategy_runs;
DROP TABLE IF EXISTS strategies;