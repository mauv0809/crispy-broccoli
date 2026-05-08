package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
)

// Version is a snapshot of a strategy's rules at a point in time. Editing a
// strategy creates a new Version row; portfolios pin a specific version_id so
// edits never alter the rules under which a portfolio is being rebalanced.
type Version struct {
	ID            int64           `json:"id"`
	StrategyID    int64           `json:"strategy_id"`
	VersionNumber int             `json:"version_number"`
	Rules         json.RawMessage `json:"rules"`
	CreatedAt     time.Time       `json:"created_at"`
	CreatedBy     *int64          `json:"created_by,omitempty"`
}

// ErrVersionNotFound is returned by Get when the version id doesn't exist.
var ErrVersionNotFound = errors.New("strategy version not found")

// VersionsRepository handles inserts/lookups against strategy_versions.
type VersionsRepository struct {
	pool *pgxpool.Pool
}

// NewVersionsRepository creates a new VersionsRepository backed by pool.
func NewVersionsRepository(pool *pgxpool.Pool) *VersionsRepository {
	return &VersionsRepository{pool: pool}
}

// Create inserts a new strategy_versions row, auto-incrementing version_number
// per strategy. Convenience wrapper around CreateTx using the pool.
func (r *VersionsRepository) Create(ctx context.Context, strategyID int64, rules json.RawMessage, createdBy int64) (*Version, error) {
	return r.CreateTx(ctx, r.pool, strategyID, rules, createdBy)
}

// CreateTx is the transaction-aware variant. The proposal acceptor and strategy
// edit flow call this inside a parent transaction so a new version + the
// strategy update commit atomically.
func (r *VersionsRepository) CreateTx(ctx context.Context, db dbutil.DBTX, strategyID int64, rules json.RawMessage, createdBy int64) (*Version, error) {
	var v Version
	err := db.QueryRow(ctx, `
		INSERT INTO strategy_versions (strategy_id, version_number, rules, created_by)
		VALUES ($1,
		        COALESCE((SELECT MAX(version_number) FROM strategy_versions WHERE strategy_id = $1), 0) + 1,
		        $2,
		        $3)
		RETURNING id, strategy_id, version_number, rules, created_at, created_by
	`, strategyID, rules, createdBy).Scan(&v.ID, &v.StrategyID, &v.VersionNumber, &v.Rules, &v.CreatedAt, &v.CreatedBy)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Get retrieves a single version by its id.
func (r *VersionsRepository) Get(ctx context.Context, id int64) (*Version, error) {
	var v Version
	err := r.pool.QueryRow(ctx, `
		SELECT id, strategy_id, version_number, rules, created_at, created_by
		FROM strategy_versions WHERE id = $1
	`, id).Scan(&v.ID, &v.StrategyID, &v.VersionNumber, &v.Rules, &v.CreatedAt, &v.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListByStrategy returns every version for a strategy, newest first.
func (r *VersionsRepository) ListByStrategy(ctx context.Context, strategyID int64) ([]Version, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, strategy_id, version_number, rules, created_at, created_by
		FROM strategy_versions
		WHERE strategy_id = $1
		ORDER BY version_number DESC
	`, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.StrategyID, &v.VersionNumber, &v.Rules, &v.CreatedAt, &v.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
