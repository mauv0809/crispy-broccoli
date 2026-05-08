package portfolio_test

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

func newServiceFixture(t *testing.T) (*portfolio.Service, *strategy.Repository) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	return portfolio.NewService(pRepo, sRepo), sRepo
}

func TestService_CreatePortfolio_VerifiedStrategySucceeds(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	svc, sRepo := newServiceFixture(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	cadence := strategy.CadenceQuarterly
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name(), Rules: rules, DefaultCadence: &cadence}, systemUserID(t, pool))
	if err := sRepo.Verify(ctx, int64(s.ID)); err != nil {
		t.Fatalf("verify: %v", err)
	}

	p, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID:          systemUserID(t, pool),
		Name:            "Test Portfolio",
		StartingCapital: decimal.NewFromInt(10000),
		StrategyID:      int64(s.ID),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.StrategyVersionID == 0 {
		t.Error("expected pinned StrategyVersionID")
	}
	if p.Cadence != strategy.CadenceQuarterly {
		t.Errorf("cadence = %s, want quarterly (from strategy default)", p.Cadence)
	}
}

func TestService_CreatePortfolio_DraftStrategyFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	svc, sRepo := newServiceFixture(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name(), Rules: rules}, systemUserID(t, pool))
	// status remains 'draft' — don't verify.

	_, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: systemUserID(t, pool), Name: "T",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID),
	})
	if !errors.Is(err, portfolio.ErrStrategyNotVerified) {
		t.Errorf("err = %v, want ErrStrategyNotVerified", err)
	}
}

func TestService_CreatePortfolio_ArchivedStrategyFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	svc, sRepo := newServiceFixture(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name(), Rules: rules}, systemUserID(t, pool))
	_ = sRepo.Verify(ctx, int64(s.ID))
	_ = sRepo.Archive(ctx, int64(s.ID))

	_, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: systemUserID(t, pool), Name: "T",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID),
	})
	if !errors.Is(err, portfolio.ErrStrategyNotVerified) {
		t.Errorf("err = %v, want ErrStrategyNotVerified (archived treated like unverified)", err)
	}
}

func TestService_CreatePortfolio_OverrideCadence(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	svc, sRepo := newServiceFixture(t)

	defC := strategy.CadenceQuarterly
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name(), Rules: rules, DefaultCadence: &defC}, systemUserID(t, pool))
	_ = sRepo.Verify(ctx, int64(s.ID))

	override := strategy.CadenceMonthly
	p, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: systemUserID(t, pool), Name: t.Name(),
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID),
		CadenceOverride: &override,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Cadence != strategy.CadenceMonthly {
		t.Errorf("cadence = %s, want monthly (override beats default)", p.Cadence)
	}
}

func TestService_CreatePortfolio_NoCadenceFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	svc, sRepo := newServiceFixture(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name(), Rules: rules}, systemUserID(t, pool)) // no DefaultCadence
	_ = sRepo.Verify(ctx, int64(s.ID))

	_, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: systemUserID(t, pool), Name: t.Name(),
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID),
		// no CadenceOverride and no DefaultCadence on strategy
	})
	if !errors.Is(err, portfolio.ErrCadenceMissing) {
		t.Errorf("err = %v, want ErrCadenceMissing", err)
	}
}

func TestService_SetStatus_Passthrough(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	svc, sRepo := newServiceFixture(t)

	cadence := strategy.CadenceMonthly
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name(), Rules: rules, DefaultCadence: &cadence}, systemUserID(t, pool))
	_ = sRepo.Verify(ctx, int64(s.ID))

	p, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: systemUserID(t, pool), Name: t.Name(),
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.SetStatus(ctx, p.ID, portfolio.StatusPaused); err != nil {
		t.Fatalf("set status: %v", err)
	}

	pRepo := portfolio.NewRepository(pool)
	got, _ := pRepo.GetByID(ctx, p.ID)
	if got.Status != portfolio.StatusPaused {
		t.Errorf("status = %s, want paused", got.Status)
	}
}
