package portfolio

import (
	"context"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// ErrStrategyNotVerified is returned when CreatePortfolio is called with a
// strategy that isn't in 'verified' status. The handler layer surfaces this
// as a 400 to the user with a meaningful message.
var ErrStrategyNotVerified = errors.New("strategy must be verified to attach to a portfolio")

// ErrCadenceMissing is returned when neither CadenceOverride nor the
// strategy's DefaultCadence supplies a cadence value.
var ErrCadenceMissing = errors.New("no cadence supplied and strategy has no default")

// Service is the application layer over Repository. It enforces business rules
// that the storage layer doesn't (e.g., strategy must be verified before
// attachment) and resolves derived values (pinned version id, cadence
// fallback).
type Service struct {
	portfolios *Repository
	strategies *strategy.Repository
}

func NewService(p *Repository, s *strategy.Repository) *Service {
	return &Service{portfolios: p, strategies: s}
}

// CreatePortfolioInput is the user-facing shape: just a strategy id (not a
// version id), with optional cadence override.
type CreatePortfolioInput struct {
	UserID          int64
	Name            string
	StartingCapital decimal.Decimal
	StrategyID      int64
	CadenceOverride *strategy.Cadence
}

// CreatePortfolio validates the strategy, pins its current version, resolves
// cadence, and creates the portfolio.
func (s *Service) CreatePortfolio(ctx context.Context, in CreatePortfolioInput) (*Portfolio, error) {
	strat, err := s.strategies.GetByID(ctx, int(in.StrategyID))
	if err != nil {
		return nil, fmt.Errorf("loading strategy: %w", err)
	}
	if strat.Status != strategy.StatusVerified {
		return nil, fmt.Errorf("%w (status=%s)", ErrStrategyNotVerified, strat.Status)
	}
	if strat.CurrentVersionID == nil {
		// Should be impossible given the Create flow always seeds v1, but guard
		// rather than panic if a stale row predates the versioning system.
		return nil, fmt.Errorf("strategy %d has no current_version_id", in.StrategyID)
	}

	cadence := in.CadenceOverride
	if cadence == nil {
		cadence = strat.DefaultCadence
	}
	if cadence == nil {
		return nil, ErrCadenceMissing
	}

	return s.portfolios.Create(ctx, CreatePortfolioRequest{
		UserID:            in.UserID,
		Name:              in.Name,
		StartingCapital:   in.StartingCapital,
		StrategyID:        int64(strat.ID),
		StrategyVersionID: *strat.CurrentVersionID,
		Cadence:           *cadence,
	})
}

// SetStatus is a thin pass-through used by handlers (pause / resume / archive).
// Lives here so handlers depend on Service rather than reaching directly into
// Repository.
func (s *Service) SetStatus(ctx context.Context, portfolioID int64, status Status) error {
	return s.portfolios.SetStatus(ctx, portfolioID, status)
}
