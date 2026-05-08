package strategy_test

import (
	"context"
	"testing"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

func TestUpdateRules_CreatesNewVersionAndDemotes(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	versions := strategy.NewVersionsRepository(pool)
	uid := systemUserID(t, pool)

	original := strategy.Rules{
		Filters:   []strategy.Filter{},
		Ranking:   []strategy.Ranking{},
		Limit:     6,
		Dimension: "MRQ",
	}
	s, err := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "Demote test", Rules: original}, uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Promote to verified so we can observe the demotion.
	if err := repo.Verify(ctx, int64(s.ID)); err != nil {
		t.Fatalf("verify: %v", err)
	}

	newRules := strategy.Rules{
		Filters:   []strategy.Filter{{Field: "pe_ratio", Operator: "<", Value: 15.0}},
		Ranking:   []strategy.Ranking{},
		Limit:     6,
		Dimension: "MRQ",
	}
	if err := repo.UpdateRules(ctx, s.ID, newRules, uid); err != nil {
		t.Fatalf("update rules: %v", err)
	}

	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != strategy.StatusDraft {
		t.Errorf("status = %s, want draft (demoted by edit)", got.Status)
	}
	if got.CurrentVersionID == nil {
		t.Fatal("CurrentVersionID is nil after update")
	}

	all, err := versions.ListByStrategy(ctx, int64(s.ID))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(all))
	}
	if all[0].VersionNumber != 2 {
		t.Errorf("latest version_number = %d, want 2 (DESC order)", all[0].VersionNumber)
	}
	if *got.CurrentVersionID != all[0].ID {
		t.Errorf("CurrentVersionID = %d, want %d (latest)", *got.CurrentVersionID, all[0].ID)
	}
}

func TestUpdateRules_OnDraftStrategyStillWorks(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	uid := systemUserID(t, pool)

	original := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "Draft edit", Rules: original}, uid)
	// Don't verify — leave as draft.

	newRules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 10, Dimension: "MRQ"}
	if err := repo.UpdateRules(ctx, s.ID, newRules, uid); err != nil {
		t.Fatalf("update rules on draft: %v", err)
	}
	got, _ := repo.GetByID(ctx, s.ID)
	if got.Status != strategy.StatusDraft {
		t.Errorf("status = %s, want still draft", got.Status)
	}
	if got.Rules.Limit != 10 {
		t.Errorf("rules.Limit = %d, want 10", got.Rules.Limit)
	}
}

func TestUpdateRules_OnArchivedStrategyAlsoWorks(t *testing.T) {
	// Editing an archived strategy creates a new version and demotes the
	// strategy back to draft. Whether to allow editing archived strategies
	// at all is a UI/handler concern; the repo permits it for now.
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	uid := systemUserID(t, pool)

	original := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "Arch edit", Rules: original}, uid)
	_ = repo.Archive(ctx, int64(s.ID))

	newRules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 12, Dimension: "MRQ"}
	if err := repo.UpdateRules(ctx, s.ID, newRules, uid); err != nil {
		t.Fatalf("update rules on archived: %v", err)
	}
	got, _ := repo.GetByID(ctx, s.ID)
	if got.Status != strategy.StatusDraft {
		t.Errorf("status = %s, want draft (demoted)", got.Status)
	}
}
