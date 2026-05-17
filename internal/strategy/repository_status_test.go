package strategy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

func seedStrategy(t *testing.T, pool any, name string) *strategy.Strategy {
	t.Helper()
	repo := strategy.NewRepository(testutil.PoolFrom(pool))
	uid := systemUserID(t, pool)
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := repo.Create(context.Background(), strategy.CreateStrategyRequest{Name: name, Rules: rules}, uid)
	if err != nil {
		t.Fatalf("seed strategy %q: %v", name, err)
	}
	return s
}

func TestVerify_FromDraftSucceeds(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	s := seedStrategy(t, pool, "VerifyDraft")

	if err := repo.Verify(ctx, s.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := repo.GetByID(ctx, s.ID)
	if got.Status != strategy.StatusVerified {
		t.Errorf("status = %s, want verified", got.Status)
	}
}

func TestVerify_FromVerifiedIsIdempotent(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	s := seedStrategy(t, pool, "VerifyTwice")
	_ = repo.Verify(ctx, s.ID)

	if err := repo.Verify(ctx, s.ID); err != nil {
		t.Errorf("second verify should be a no-op, got err: %v", err)
	}
}

func TestVerify_FromArchivedFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	s := seedStrategy(t, pool, "VerifyArchived")
	_ = repo.Archive(ctx, s.ID)

	err := repo.Verify(ctx, s.ID)
	if !errors.Is(err, strategy.ErrInvalidStatusTransition) {
		t.Errorf("err = %v, want ErrInvalidStatusTransition", err)
	}
}

func TestVerify_MissingStrategyFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)

	err := repo.Verify(ctx, 999_999_999)
	if !errors.Is(err, strategy.ErrInvalidStatusTransition) {
		t.Errorf("err = %v, want ErrInvalidStatusTransition (missing row treated as invalid transition)", err)
	}
}

func TestArchive_FromAnyStateSucceeds(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)

	// from draft
	s1 := seedStrategy(t, pool, "ArchDraft")
	if err := repo.Archive(ctx, s1.ID); err != nil {
		t.Errorf("archive from draft: %v", err)
	}
	got1, _ := repo.GetByID(ctx, s1.ID)
	if got1.Status != strategy.StatusArchived {
		t.Errorf("status = %s, want archived", got1.Status)
	}

	// from verified
	s2 := seedStrategy(t, pool, "ArchVerified")
	_ = repo.Verify(ctx, s2.ID)
	if err := repo.Archive(ctx, s2.ID); err != nil {
		t.Errorf("archive from verified: %v", err)
	}

	// already archived (idempotent)
	if err := repo.Archive(ctx, s1.ID); err != nil {
		t.Errorf("archive twice should be a no-op, got err: %v", err)
	}
}

func TestArchive_MissingStrategyFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)

	err := repo.Archive(ctx, 999_999_999)
	if err == nil {
		t.Error("expected error for missing strategy")
	}
}
