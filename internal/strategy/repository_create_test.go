package strategy_test

import (
	"context"
	"testing"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

func TestCreate_SeedsV1AndStartsAsDraft(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	versions := strategy.NewVersionsRepository(pool)
	uid := systemUserID(t, pool)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "Auto v1", Rules: rules}, uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.Status != strategy.StatusDraft {
		t.Errorf("status = %s, want draft", s.Status)
	}
	if s.CurrentVersionID == nil {
		t.Fatal("CurrentVersionID is nil; want pointer to v1 id")
	}
	all, err := versions.ListByStrategy(ctx, s.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(all) != 1 || all[0].VersionNumber != 1 {
		t.Errorf("expected one v1, got %+v", all)
	}
	if all[0].ID != *s.CurrentVersionID {
		t.Errorf("CurrentVersionID = %d, want %d", *s.CurrentVersionID, all[0].ID)
	}
}

func TestCreate_WithDefaultCadenceStores(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	uid := systemUserID(t, pool)

	cadence := strategy.CadenceQuarterly
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "Quarterly S", Rules: rules, DefaultCadence: &cadence}, uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.CurrentVersionID == nil {
		t.Error("CurrentVersionID nil after Create")
	}
	if s.DefaultCadence == nil || *s.DefaultCadence != strategy.CadenceQuarterly {
		t.Errorf("default_cadence not preserved: %v", s.DefaultCadence)
	}
}

func TestGetByID_ReadsNewColumns(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	uid := systemUserID(t, pool)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	created, err := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "RT", Rules: rules}, uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != strategy.StatusDraft {
		t.Errorf("status = %s, want draft", got.Status)
	}
	if got.CurrentVersionID == nil || *got.CurrentVersionID != *created.CurrentVersionID {
		t.Errorf("CurrentVersionID = %v, want %v", got.CurrentVersionID, created.CurrentVersionID)
	}
}
