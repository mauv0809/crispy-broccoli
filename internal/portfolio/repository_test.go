package portfolio_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// systemUserID looks up the synthetic system user inserted by migration 015.
// Mirrors the helper in internal/strategy/versions_test.go but lives in this
// package's test scope.
func systemUserID(t *testing.T, pool any) int64 {
	t.Helper()
	p := testutil.PoolFrom(pool)
	var id int64
	err := p.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'system@deepvalue.local'`).Scan(&id)
	if err != nil {
		t.Fatalf("system user lookup: %v", err)
	}
	return id
}

// seedStrategy creates and verifies a strategy, returning it along with its
// pinned version id. Used by repository tests as the FK target.
func seedStrategy(t *testing.T, pool any) (*strategy.Strategy, int64) {
	t.Helper()
	repo := strategy.NewRepository(testutil.PoolFrom(pool))
	uid := systemUserID(t, pool)
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := repo.Create(context.Background(), strategy.CreateStrategyRequest{Name: t.Name() + "-seed", Rules: rules}, uid)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if err := repo.Verify(context.Background(), int64(s.ID)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := repo.GetByID(context.Background(), s.ID)
	if got.CurrentVersionID == nil {
		t.Fatal("CurrentVersionID nil after Create+Verify")
	}
	return got, *got.CurrentVersionID
}

func TestPortfolio_CreateAndGet(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := portfolio.NewRepository(pool)
	s, vID := seedStrategy(t, pool)

	p, err := repo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID:            systemUserID(t, pool),
		Name:              "My Portfolio",
		StartingCapital:   decimal.NewFromInt(50000),
		StrategyID:        int64(s.ID),
		StrategyVersionID: vID,
		Cadence:           strategy.CadenceQuarterly,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == 0 {
		t.Fatal("expected non-zero ID")
	}
	if p.Status != portfolio.StatusActive {
		t.Errorf("status = %s, want active", p.Status)
	}
	if p.NextRebalanceDue != nil {
		t.Errorf("next_rebalance_due should be nil for new portfolio, got %v", p.NextRebalanceDue)
	}
	if !p.StartingCapital.Equal(decimal.NewFromInt(50000)) {
		t.Errorf("starting_capital = %s, want 50000", p.StartingCapital)
	}

	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "My Portfolio" {
		t.Errorf("name = %q, want %q", got.Name, "My Portfolio")
	}
}

func TestPortfolio_GetByIDNotFound(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := portfolio.NewRepository(pool)
	_, err := repo.GetByID(context.Background(), 999_999_999)
	if err != portfolio.ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPortfolio_ListByUser(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := portfolio.NewRepository(pool)
	s, vID := seedStrategy(t, pool)
	uid := systemUserID(t, pool)

	for _, name := range []string{"A", "B"} {
		_, err := repo.Create(ctx, portfolio.CreatePortfolioRequest{
			UserID: uid, Name: name,
			StartingCapital:   decimal.NewFromInt(1000),
			StrategyID:        int64(s.ID),
			StrategyVersionID: vID,
			Cadence:           strategy.CadenceMonthly,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	got, err := repo.ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestPortfolio_ListByUserExcludesArchived(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := portfolio.NewRepository(pool)
	s, vID := seedStrategy(t, pool)
	uid := systemUserID(t, pool)

	a, _ := repo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: "alive",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID), StrategyVersionID: vID,
		Cadence: strategy.CadenceMonthly,
	})
	arc, _ := repo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: "to-archive",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID), StrategyVersionID: vID,
		Cadence: strategy.CadenceMonthly,
	})
	if err := repo.SetStatus(ctx, arc.ID, portfolio.StatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}

	got, _ := repo.ListByUser(ctx, uid)
	if len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("expected only the live portfolio, got %+v", got)
	}
}

func TestPortfolio_SetNextRebalanceDue(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := portfolio.NewRepository(pool)
	s, vID := seedStrategy(t, pool)
	uid := systemUserID(t, pool)

	p, _ := repo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: "due",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID), StrategyVersionID: vID,
		Cadence: strategy.CadenceQuarterly,
	})

	due := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := repo.SetNextRebalanceDue(ctx, pool, p.ID, due); err != nil {
		t.Fatalf("set due: %v", err)
	}

	got, _ := repo.GetByID(ctx, p.ID)
	if got.NextRebalanceDue == nil {
		t.Fatal("NextRebalanceDue is nil after set")
	}
	if !got.NextRebalanceDue.Equal(due) {
		t.Errorf("NextRebalanceDue = %s, want %s", got.NextRebalanceDue, due)
	}
}

func TestPortfolio_FindDueForRebalance(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := portfolio.NewRepository(pool)
	s, vID := seedStrategy(t, pool)
	uid := systemUserID(t, pool)

	now := time.Now().UTC()

	// Three portfolios: one due in past, one due in future, one paused with past due.
	dueNow, _ := repo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: "due-now",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID), StrategyVersionID: vID,
		Cadence: strategy.CadenceQuarterly,
	})
	_ = repo.SetNextRebalanceDue(ctx, pool, dueNow.ID, now.Add(-time.Hour))

	dueFuture, _ := repo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: "due-future",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID), StrategyVersionID: vID,
		Cadence: strategy.CadenceQuarterly,
	})
	_ = repo.SetNextRebalanceDue(ctx, pool, dueFuture.ID, now.Add(time.Hour))

	pausedDue, _ := repo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: "paused-due",
		StartingCapital: decimal.NewFromInt(1000),
		StrategyID:      int64(s.ID), StrategyVersionID: vID,
		Cadence: strategy.CadenceQuarterly,
	})
	_ = repo.SetNextRebalanceDue(ctx, pool, pausedDue.ID, now.Add(-time.Hour))
	_ = repo.SetStatus(ctx, pausedDue.ID, portfolio.StatusPaused)

	ids, err := repo.FindDueForRebalance(ctx, pool, now, 100)
	if err != nil {
		t.Fatalf("find due: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("len = %d, want 1 (only active+past-due); got %v", len(ids), ids)
	}
	if ids[0] != dueNow.ID {
		t.Errorf("id = %d, want %d (the due one)", ids[0], dueNow.ID)
	}
}
