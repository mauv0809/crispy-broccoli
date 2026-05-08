package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
)

// Repository handles database operations for strategies
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a new strategy repository
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create inserts a new strategy authored by the given user and seeds v1 in
// strategy_versions, all within a single transaction. current_version_id is
// guaranteed to be set before the transaction commits.
func (r *Repository) Create(ctx context.Context, req CreateStrategyRequest, createdBy int64) (*Strategy, error) {
	rulesJSON, err := json.Marshal(req.Rules)
	if err != nil {
		return nil, fmt.Errorf("marshaling rules: %w", err)
	}

	versions := NewVersionsRepository(r.pool)

	var s Strategy
	err = dbutil.RunInTx(ctx, r.pool, func(tx dbutil.DBTX) error {
		// 1. Insert the strategy with current_version_id NULL (allowed by migration 025).
		if err := tx.QueryRow(ctx, `
			INSERT INTO strategies (name, description, rules, is_default, status, default_cadence, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, false, 'draft', $4, $5, NOW(), NOW())
			RETURNING id, name, description, rules, is_default, status, default_cadence, current_version_id, created_at, updated_at
		`, req.Name, req.Description, rulesJSON, req.DefaultCadence, createdBy).Scan(
			&s.ID, &s.Name, &s.Description, &s.Rules, &s.IsDefault,
			&s.Status, &s.DefaultCadence, &s.CurrentVersionID,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return fmt.Errorf("inserting strategy: %w", err)
		}

		// 2. Seed v1.
		v1, err := versions.CreateTx(ctx, tx, int64(s.ID), rulesJSON, createdBy)
		if err != nil {
			return fmt.Errorf("seeding v1: %w", err)
		}

		// 3. Point strategies.current_version_id at v1.
		if _, err := tx.Exec(ctx, `UPDATE strategies SET current_version_id = $1 WHERE id = $2`, v1.ID, s.ID); err != nil {
			return fmt.Errorf("setting current_version_id: %w", err)
		}
		s.CurrentVersionID = &v1.ID
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("creating strategy: %w", err)
	}
	return &s, nil
}

// GetByID retrieves a strategy by ID
func (r *Repository) GetByID(ctx context.Context, id int64) (*Strategy, error) {
	var s Strategy
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, description, rules, is_default, status, default_cadence, current_version_id, created_at, updated_at
		FROM strategies
		WHERE id = $1
	`, id).Scan(
		&s.ID, &s.Name, &s.Description, &s.Rules, &s.IsDefault,
		&s.Status, &s.DefaultCadence, &s.CurrentVersionID,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("getting strategy: %w", err)
	}

	return &s, nil
}

// List retrieves all strategies ordered by creation date
func (r *Repository) List(ctx context.Context) ([]Strategy, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, description, rules, is_default, status, default_cadence, current_version_id, created_at, updated_at
		FROM strategies
		ORDER BY is_default DESC, created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing strategies: %w", err)
	}
	defer rows.Close()

	var strategies []Strategy
	for rows.Next() {
		var s Strategy
		if err := rows.Scan(
			&s.ID, &s.Name, &s.Description, &s.Rules, &s.IsDefault,
			&s.Status, &s.DefaultCadence, &s.CurrentVersionID,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning strategy: %w", err)
		}
		strategies = append(strategies, s)
	}

	return strategies, rows.Err()
}

// Update updates an existing strategy
func (r *Repository) Update(ctx context.Context, id int64, req UpdateStrategyRequest) (*Strategy, error) {
	rulesJSON, err := json.Marshal(req.Rules)
	if err != nil {
		return nil, fmt.Errorf("marshaling rules: %w", err)
	}

	var s Strategy
	err = r.pool.QueryRow(ctx, `
		UPDATE strategies
		SET name = $2, description = $3, rules = $4, updated_at = NOW()
		WHERE id = $1
		RETURNING id, name, description, rules, is_default, status, default_cadence, current_version_id, created_at, updated_at
	`, id, req.Name, req.Description, rulesJSON).Scan(
		&s.ID, &s.Name, &s.Description, &s.Rules, &s.IsDefault,
		&s.Status, &s.DefaultCadence, &s.CurrentVersionID,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("updating strategy: %w", err)
	}

	return &s, nil
}

// Delete removes a strategy by ID
func (r *Repository) Delete(ctx context.Context, id int64) error {
	result, err := r.pool.Exec(ctx, `DELETE FROM strategies WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("deleting strategy: %w", err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("strategy not found")
	}
	return nil
}

// SaveRun saves a strategy execution run authored by the given user.
func (r *Repository) SaveRun(ctx context.Context, run *StrategyRun, createdBy int64) error {
	resultsJSON, err := json.Marshal(run.Results)
	if err != nil {
		return fmt.Errorf("marshaling results: %w", err)
	}

	err = r.pool.QueryRow(ctx, `
		INSERT INTO strategy_runs (strategy_id, run_at, results, execution_time_ms, stocks_screened, stocks_matched, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, run.StrategyID, run.RunAt, resultsJSON, run.ExecutionTimeMs, run.StocksScreened, run.StocksMatched, createdBy).Scan(&run.ID)
	if err != nil {
		return fmt.Errorf("saving strategy run: %w", err)
	}
	return nil
}

// GetRuns retrieves execution history for a strategy
func (r *Repository) GetRuns(ctx context.Context, strategyID int64, limit int) ([]StrategyRun, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id, strategy_id, run_at, results, execution_time_ms, stocks_screened, stocks_matched
		FROM strategy_runs
		WHERE strategy_id = $1
		ORDER BY run_at DESC
		LIMIT $2
	`, strategyID, limit)
	if err != nil {
		return nil, fmt.Errorf("getting strategy runs: %w", err)
	}
	defer rows.Close()

	var runs []StrategyRun
	for rows.Next() {
		var run StrategyRun
		var resultsJSON []byte
		if err := rows.Scan(
			&run.ID, &run.StrategyID, &run.RunAt, &resultsJSON, &run.ExecutionTimeMs, &run.StocksScreened, &run.StocksMatched,
		); err != nil {
			return nil, fmt.Errorf("scanning strategy run: %w", err)
		}
		// Handle null/empty results JSON gracefully
		if len(resultsJSON) > 0 {
			if err := json.Unmarshal(resultsJSON, &run.Results); err != nil {
				return nil, fmt.Errorf("unmarshaling results: %w", err)
			}
		}
		runs = append(runs, run)
	}

	return runs, rows.Err()
}

// GetLatestRun returns the most recent run for a strategy
func (r *Repository) GetLatestRun(ctx context.Context, strategyID int64) (*StrategyRun, error) {
	var run StrategyRun
	var resultsJSON []byte

	err := r.pool.QueryRow(ctx, `
		SELECT id, strategy_id, run_at, results, execution_time_ms, stocks_screened, stocks_matched
		FROM strategy_runs
		WHERE strategy_id = $1
		ORDER BY run_at DESC
		LIMIT 1
	`, strategyID).Scan(
		&run.ID, &run.StrategyID, &run.RunAt, &resultsJSON, &run.ExecutionTimeMs, &run.StocksScreened, &run.StocksMatched,
	)
	if err != nil {
		return nil, fmt.Errorf("getting latest run: %w", err)
	}

	// Handle null/empty results JSON gracefully
	if len(resultsJSON) > 0 {
		if err := json.Unmarshal(resultsJSON, &run.Results); err != nil {
			return nil, fmt.Errorf("unmarshaling results: %w", err)
		}
	}

	return &run, nil
}

// errSkip is an internal sentinel used by CreateDefaultStrategy to
// short-circuit the transaction when the strategy already exists, matching
// the original ON CONFLICT DO NOTHING / "return nil, nil" semantics.
var errSkip = errors.New("skip: existing default")

// CreateDefaultStrategy creates a default strategy if it doesn't already
// exist (checked by name). Both the strategy row and its v1 version row are
// inserted transactionally, and current_version_id is set before commit.
func (r *Repository) CreateDefaultStrategy(ctx context.Context, name, description string, rules Rules) (*Strategy, error) {
	rulesJSON, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("marshaling rules: %w", err)
	}
	versions := NewVersionsRepository(r.pool)

	var s Strategy
	err = dbutil.RunInTx(ctx, r.pool, func(tx dbutil.DBTX) error {
		// Guard: skip if this name already exists.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM strategies WHERE name = $1)`, name).Scan(&exists); err != nil {
			return fmt.Errorf("checking existence: %w", err)
		}
		if exists {
			return errSkip
		}

		// Look up the synthetic system user (inserted by migration 015).
		var sysUserID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE email = 'system@deepvalue.local'`).Scan(&sysUserID); err != nil {
			return fmt.Errorf("system user lookup: %w", err)
		}

		// Insert strategy with current_version_id NULL (migration 025 allows this).
		if err := tx.QueryRow(ctx, `
			INSERT INTO strategies (name, description, rules, is_default, status, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, true, 'verified', $4, NOW(), NOW())
			RETURNING id, name, description, rules, is_default, status, default_cadence, current_version_id, created_at, updated_at
		`, name, description, rulesJSON, sysUserID).Scan(
			&s.ID, &s.Name, &s.Description, &s.Rules, &s.IsDefault,
			&s.Status, &s.DefaultCadence, &s.CurrentVersionID,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return fmt.Errorf("inserting default strategy: %w", err)
		}

		v1, err := versions.CreateTx(ctx, tx, int64(s.ID), rulesJSON, sysUserID)
		if err != nil {
			return fmt.Errorf("seeding v1 for default: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE strategies SET current_version_id = $1 WHERE id = $2`, v1.ID, s.ID); err != nil {
			return fmt.Errorf("setting current_version_id: %w", err)
		}
		s.CurrentVersionID = &v1.ID
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("creating default strategy: %w", err)
	}
	return &s, nil
}

// Count returns the total number of strategies
func (r *Repository) Count(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM strategies").Scan(&count)
	return count, err
}

// GetDefaultStrategies returns all default strategies
func (r *Repository) GetDefaultStrategies(ctx context.Context) ([]Strategy, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, description, rules, is_default, status, default_cadence, current_version_id, created_at, updated_at
		FROM strategies
		WHERE is_default = true
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("getting default strategies: %w", err)
	}
	defer rows.Close()

	var strategies []Strategy
	for rows.Next() {
		var s Strategy
		if err := rows.Scan(
			&s.ID, &s.Name, &s.Description, &s.Rules, &s.IsDefault,
			&s.Status, &s.DefaultCadence, &s.CurrentVersionID,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning strategy: %w", err)
		}
		strategies = append(strategies, s)
	}

	return strategies, rows.Err()
}

// RunStats returns aggregate stats for strategy runs
type RunStats struct {
	TotalRuns     int       `json:"total_runs"`
	AvgExecTimeMs float64   `json:"avg_execution_time_ms"`
	LastRunAt     time.Time `json:"last_run_at"`
	AvgMatched    float64   `json:"avg_stocks_matched"`
}

// GetRunStats returns aggregate statistics for a strategy's runs
func (r *Repository) GetRunStats(ctx context.Context, strategyID int64) (*RunStats, error) {
	var stats RunStats
	err := r.pool.QueryRow(ctx, `
		SELECT
			COUNT(*),
			COALESCE(AVG(execution_time_ms), 0),
			COALESCE(MAX(run_at), '1970-01-01'),
			COALESCE(AVG(stocks_matched), 0)
		FROM strategy_runs
		WHERE strategy_id = $1
	`, strategyID).Scan(&stats.TotalRuns, &stats.AvgExecTimeMs, &stats.LastRunAt, &stats.AvgMatched)
	if err != nil {
		return nil, fmt.Errorf("getting run stats: %w", err)
	}
	return &stats, nil
}

// GetRecentRuns returns recent runs across all strategies
func (r *Repository) GetRecentRuns(ctx context.Context, limit int) ([]StrategyRun, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id, strategy_id, run_at, results, execution_time_ms, stocks_screened, stocks_matched
		FROM strategy_runs
		ORDER BY run_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("getting recent runs: %w", err)
	}
	defer rows.Close()

	var runs []StrategyRun
	for rows.Next() {
		var run StrategyRun
		var resultsJSON []byte
		if err := rows.Scan(
			&run.ID, &run.StrategyID, &run.RunAt, &resultsJSON, &run.ExecutionTimeMs, &run.StocksScreened, &run.StocksMatched,
		); err != nil {
			return nil, fmt.Errorf("scanning strategy run: %w", err)
		}
		// Handle null/empty results JSON gracefully
		if len(resultsJSON) > 0 {
			if err := json.Unmarshal(resultsJSON, &run.Results); err != nil {
				return nil, fmt.Errorf("unmarshaling results: %w", err)
			}
		}
		runs = append(runs, run)
	}

	return runs, rows.Err()
}
