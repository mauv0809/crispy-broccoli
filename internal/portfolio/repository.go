package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// ErrNotFound is returned when a portfolio lookup misses.
var ErrNotFound = errors.New("portfolio not found")

// CreatePortfolioRequest carries the inputs needed to create a portfolio. The
// caller (typically portfolio.Service) is responsible for resolving
// strategy_version_id from the strategy's current_version_id and validating
// that the strategy is in 'verified' state.
type CreatePortfolioRequest struct {
	UserID            int64
	Name              string
	StartingCapital   decimal.Decimal
	StrategyID        int64
	StrategyVersionID int64
	Cadence           strategy.Cadence
}

// Repository handles portfolios CRUD and lifecycle queries.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create inserts a new portfolio. status defaults to 'active' and
// next_rebalance_due is left null until the first proposal is resolved.
func (r *Repository) Create(ctx context.Context, req CreatePortfolioRequest) (*Portfolio, error) {
	var p Portfolio
	err := r.pool.QueryRow(ctx, `
		INSERT INTO portfolios (user_id, name, starting_capital, strategy_id, strategy_version_id, cadence)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, user_id, name, starting_capital, strategy_id, strategy_version_id,
		          cadence, next_rebalance_due, status, created_at, updated_at
	`, req.UserID, req.Name, req.StartingCapital, req.StrategyID, req.StrategyVersionID, req.Cadence).Scan(
		&p.ID, &p.UserID, &p.Name, &p.StartingCapital, &p.StrategyID, &p.StrategyVersionID,
		&p.Cadence, &p.NextRebalanceDue, &p.Status, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("creating portfolio: %w", err)
	}
	return &p, nil
}

// GetByID looks up a portfolio. Returns ErrNotFound if no row exists.
func (r *Repository) GetByID(ctx context.Context, id int64) (*Portfolio, error) {
	return r.getByIDFrom(ctx, r.pool, id)
}

// GetByIDTx is the transaction-aware variant. The proposal acceptor reads the
// portfolio inside its transaction so cadence advancement happens atomically.
func (r *Repository) GetByIDTx(ctx context.Context, db dbutil.DBTX, id int64) (*Portfolio, error) {
	return r.getByIDFrom(ctx, db, id)
}

func (r *Repository) getByIDFrom(ctx context.Context, db dbutil.DBTX, id int64) (*Portfolio, error) {
	var p Portfolio
	err := db.QueryRow(ctx, `
		SELECT id, user_id, name, starting_capital, strategy_id, strategy_version_id,
		       cadence, next_rebalance_due, status, created_at, updated_at
		FROM portfolios WHERE id = $1
	`, id).Scan(
		&p.ID, &p.UserID, &p.Name, &p.StartingCapital, &p.StrategyID, &p.StrategyVersionID,
		&p.Cadence, &p.NextRebalanceDue, &p.Status, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting portfolio: %w", err)
	}
	return &p, nil
}

// ListByUser returns the user's non-archived portfolios, newest first.
// Archived portfolios are hidden from default views; the handler can opt in to
// including them via a separate listing method later if needed.
func (r *Repository) ListByUser(ctx context.Context, userID int64) ([]Portfolio, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, user_id, name, starting_capital, strategy_id, strategy_version_id,
		       cadence, next_rebalance_due, status, created_at, updated_at
		FROM portfolios
		WHERE user_id = $1 AND status <> 'archived'
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing portfolios: %w", err)
	}
	defer rows.Close()

	var out []Portfolio
	for rows.Next() {
		var p Portfolio
		if err := rows.Scan(
			&p.ID, &p.UserID, &p.Name, &p.StartingCapital, &p.StrategyID, &p.StrategyVersionID,
			&p.Cadence, &p.NextRebalanceDue, &p.Status, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning portfolio: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing portfolios: %w", err)
	}
	return out, nil
}

// SetStatus changes a portfolio's status (active / paused / archived).
func (r *Repository) SetStatus(ctx context.Context, portfolioID int64, status Status) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE portfolios SET status = $1, updated_at = NOW() WHERE id = $2
	`, status, portfolioID)
	if err != nil {
		return fmt.Errorf("setting status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNextRebalanceDue updates next_rebalance_due. Used by the proposal
// acceptor (after acceptance/skip advances cadence) and by the scheduler if
// it needs to bump portfolios manually. Accepts DBTX so it can run inside the
// acceptor's transaction.
func (r *Repository) SetNextRebalanceDue(ctx context.Context, db dbutil.DBTX, portfolioID int64, due time.Time) error {
	tag, err := db.Exec(ctx, `
		UPDATE portfolios SET next_rebalance_due = $1, updated_at = NOW() WHERE id = $2
	`, due, portfolioID)
	if err != nil {
		return fmt.Errorf("setting next_rebalance_due: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FindDueForRebalance returns the IDs of active portfolios whose
// next_rebalance_due has passed. The query uses FOR UPDATE SKIP LOCKED so
// concurrent scheduler ticks (or future multi-instance deploys) don't double-
// process any portfolio. Caller MUST run inside a transaction; pass that tx
// as db. limit caps the batch size per tick.
func (r *Repository) FindDueForRebalance(ctx context.Context, db dbutil.DBTX, now time.Time, limit int) ([]int64, error) {
	rows, err := db.Query(ctx, `
		SELECT id FROM portfolios
		WHERE status = 'active'
		  AND next_rebalance_due IS NOT NULL
		  AND next_rebalance_due <= $1
		ORDER BY next_rebalance_due ASC
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("finding due portfolios: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning portfolio id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding due portfolios: %w", err)
	}
	return ids, nil
}
