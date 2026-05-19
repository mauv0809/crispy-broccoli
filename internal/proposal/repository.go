package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
)

// ErrNotFound is returned when a proposal lookup misses.
var ErrNotFound = errors.New("proposal not found")

// InsertInput is the create payload. Picks is stored as JSONB; the repository
// marshals it at the storage boundary.
type InsertInput struct {
	PortfolioID           int64
	StrategyVersionID     int64
	MarketValueAtProposal decimal.Decimal
	CapitalChange         decimal.Decimal
	DeployAmount          decimal.Decimal
	Picks                 []Pick
}

// UpdatePendingInput carries the values that may change while a proposal is
// pending. Only the recompute-on-capital-change handler calls this; once a
// proposal is resolved the row is frozen.
type UpdatePendingInput struct {
	CapitalChange decimal.Decimal
	DeployAmount  decimal.Decimal
	Picks         []Pick
}

// Repository owns reads/writes against the proposals table. It enforces the
// pending-only mutation contract: UpdatePending and MarkResolved both require
// status='pending' in their WHERE clauses, and surface a non-nil error on
// no-op updates so callers can distinguish "missing" from "already resolved".
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Insert creates a new pending proposal. Accepts DBTX so the scheduler/handler
// can compose with adjacent inserts in one transaction (for example, expiring
// a previous pending proposal in the same tx that creates the next one).
func (r *Repository) Insert(ctx context.Context, db dbutil.DBTX, in InsertInput) (*Proposal, error) {
	picksJSON, err := json.Marshal(in.Picks)
	if err != nil {
		return nil, fmt.Errorf("marshaling picks: %w", err)
	}
	var (
		p        Proposal
		picksRaw []byte
	)
	err = db.QueryRow(ctx, `
		INSERT INTO proposals (portfolio_id, strategy_version_id, market_value_at_proposal,
		                       capital_change, deploy_amount, picks, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending')
		RETURNING id, portfolio_id, strategy_version_id, generated_at,
		          market_value_at_proposal, capital_change, deploy_amount,
		          picks, status, resolved_at, notification_sent_at, reminder_sent_at
	`, in.PortfolioID, in.StrategyVersionID, in.MarketValueAtProposal,
		in.CapitalChange, in.DeployAmount, picksJSON).Scan(
		&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
		&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
		&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting proposal: %w", err)
	}
	if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
		return nil, fmt.Errorf("unmarshaling picks: %w", err)
	}
	return &p, nil
}

// Get reads a proposal by id, returning ErrNotFound on miss.
func (r *Repository) Get(ctx context.Context, id int64) (*Proposal, error) {
	return r.getFrom(ctx, r.pool, id)
}

// GetTx is the transaction-aware variant. Used by the acceptor to read the
// proposal inside its parent transaction.
func (r *Repository) GetTx(ctx context.Context, db dbutil.DBTX, id int64) (*Proposal, error) {
	return r.getFrom(ctx, db, id)
}

func (r *Repository) getFrom(ctx context.Context, db dbutil.DBTX, id int64) (*Proposal, error) {
	var (
		p        Proposal
		picksRaw []byte
	)
	err := db.QueryRow(ctx, `
		SELECT id, portfolio_id, strategy_version_id, generated_at,
		       market_value_at_proposal, capital_change, deploy_amount,
		       picks, status, resolved_at, notification_sent_at, reminder_sent_at
		FROM proposals WHERE id = $1
	`, id).Scan(&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
		&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
		&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting proposal: %w", err)
	}
	if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
		return nil, fmt.Errorf("unmarshaling picks: %w", err)
	}
	return &p, nil
}

// GetPending returns the single pending proposal for a portfolio, or
// ErrNotFound if none. The proposals_pending_idx (partial unique index on
// status='pending') makes this fast.
func (r *Repository) GetPending(ctx context.Context, db dbutil.DBTX, portfolioID int64) (*Proposal, error) {
	var (
		p        Proposal
		picksRaw []byte
	)
	err := db.QueryRow(ctx, `
		SELECT id, portfolio_id, strategy_version_id, generated_at,
		       market_value_at_proposal, capital_change, deploy_amount,
		       picks, status, resolved_at, notification_sent_at, reminder_sent_at
		FROM proposals
		WHERE portfolio_id = $1 AND status = 'pending'
		LIMIT 1
	`, portfolioID).Scan(&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
		&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
		&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting pending proposal: %w", err)
	}
	if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
		return nil, fmt.Errorf("unmarshaling picks: %w", err)
	}
	return &p, nil
}

// ExpirePending sets any pending proposals for the portfolio to status=expired.
// Used right before generating a new proposal so we maintain the invariant of
// at-most-one pending proposal per portfolio.
func (r *Repository) ExpirePending(ctx context.Context, db dbutil.DBTX, portfolioID int64) error {
	_, err := db.Exec(ctx, `
		UPDATE proposals
		SET status = 'expired', resolved_at = NOW()
		WHERE portfolio_id = $1 AND status = 'pending'
	`, portfolioID)
	if err != nil {
		return fmt.Errorf("expiring pending proposals: %w", err)
	}
	return nil
}

// UpdatePending mutates a pending proposal in place — used when the user
// adjusts capital_change and the picks need to be regenerated. Errors if the
// proposal is no longer pending (or not found at all). The caller is
// responsible for regenerating the picks before calling.
func (r *Repository) UpdatePending(ctx context.Context, db dbutil.DBTX, id int64, in UpdatePendingInput) error {
	picksJSON, err := json.Marshal(in.Picks)
	if err != nil {
		return fmt.Errorf("marshaling picks: %w", err)
	}
	tag, err := db.Exec(ctx, `
		UPDATE proposals
		SET capital_change = $1, deploy_amount = $2, picks = $3
		WHERE id = $4 AND status = 'pending'
	`, in.CapitalChange, in.DeployAmount, picksJSON, id)
	if err != nil {
		return fmt.Errorf("updating pending proposal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("proposal %d not pending or not found", id)
	}
	return nil
}

// MarkResolved transitions a pending proposal to a terminal state (accepted,
// partially_accepted, or skipped) and stamps resolved_at. Errors if the row
// is not currently pending — protects against double-accept races.
func (r *Repository) MarkResolved(ctx context.Context, db dbutil.DBTX, id int64, status Status, at time.Time) error {
	tag, err := db.Exec(ctx, `
		UPDATE proposals SET status = $1, resolved_at = $2
		WHERE id = $3 AND status = 'pending'
	`, status, at, id)
	if err != nil {
		return fmt.Errorf("resolving proposal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("proposal %d not pending or not found", id)
	}
	return nil
}

// SetNotificationSent stamps notification_sent_at after a successful initial
// email send. Idempotent — re-stamping is fine; the scheduler retry path
// re-uses this method.
func (r *Repository) SetNotificationSent(ctx context.Context, id int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE proposals SET notification_sent_at = $1 WHERE id = $2`, at, id)
	if err != nil {
		return fmt.Errorf("setting notification_sent_at: %w", err)
	}
	return nil
}

// SetReminderSent stamps reminder_sent_at after a successful 3-day reminder.
func (r *Repository) SetReminderSent(ctx context.Context, id int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE proposals SET reminder_sent_at = $1 WHERE id = $2`, at, id)
	if err != nil {
		return fmt.Errorf("setting reminder_sent_at: %w", err)
	}
	return nil
}

// FindReminderCandidates returns pending proposals whose initial notification
// was sent more than `after` ago and have no reminder yet. The scheduler calls
// this once per tick; results are batched and ranged over by the worker.
func (r *Repository) FindReminderCandidates(ctx context.Context, after time.Duration) ([]Proposal, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, portfolio_id, strategy_version_id, generated_at,
		       market_value_at_proposal, capital_change, deploy_amount,
		       picks, status, resolved_at, notification_sent_at, reminder_sent_at
		FROM proposals
		WHERE status = 'pending'
		  AND notification_sent_at IS NOT NULL
		  AND notification_sent_at < NOW() - $1::INTERVAL
		  AND reminder_sent_at IS NULL
	`, intervalString(after))
	if err != nil {
		return nil, fmt.Errorf("finding reminder candidates: %w", err)
	}
	defer rows.Close()
	return scanProposals(rows)
}

// FindUnsentNotifications returns pending proposals where the initial email
// failed (notification_sent_at IS NULL) within the retry window — generated
// more than 5 minutes ago (so we don't race the initial send) but less than
// retryWindow ago (after which we give up).
func (r *Repository) FindUnsentNotifications(ctx context.Context, retryWindow time.Duration) ([]Proposal, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, portfolio_id, strategy_version_id, generated_at,
		       market_value_at_proposal, capital_change, deploy_amount,
		       picks, status, resolved_at, notification_sent_at, reminder_sent_at
		FROM proposals
		WHERE status = 'pending'
		  AND notification_sent_at IS NULL
		  AND generated_at < NOW() - INTERVAL '5 minutes'
		  AND generated_at > NOW() - $1::INTERVAL
	`, intervalString(retryWindow))
	if err != nil {
		return nil, fmt.Errorf("finding unsent notifications: %w", err)
	}
	defer rows.Close()
	return scanProposals(rows)
}

// intervalString formats a time.Duration as a Postgres INTERVAL literal that
// $N::INTERVAL can parse. We use seconds so any Duration round-trips losslessly.
func intervalString(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}

func scanProposals(rows pgx.Rows) ([]Proposal, error) {
	out := make([]Proposal, 0)
	for rows.Next() {
		var (
			p        Proposal
			picksRaw []byte
		)
		if err := rows.Scan(&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
			&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
			&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt); err != nil {
			return nil, fmt.Errorf("scanning proposal: %w", err)
		}
		if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
			return nil, fmt.Errorf("unmarshaling picks: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating proposals: %w", err)
	}
	return out, nil
}

// Compile-time assertion: *pgxpool.Pool satisfies dbutil.DBTX (verified in
// dbutil package, but referenced here to confirm the pool field is usable).
var _ dbutil.DBTX = (*pgxpool.Pool)(nil)
