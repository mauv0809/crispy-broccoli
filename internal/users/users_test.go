package users_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mauv0809/crispy-broccoli/internal/testutil"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

func TestUpsert_InsertsNewUser(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)

	u, err := repo.Upsert(context.Background(), "alice@example.com", "Alice", true)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if u.Email != "alice@example.com" || !u.IsAdmin || !u.IsActive {
		t.Errorf("unexpected user: %+v", u)
	}
}

func TestUpsert_IsIdempotentAndPreservesIsAdmin(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)
	ctx := context.Background()

	first, err := repo.Upsert(ctx, "alice@example.com", "Alice", true)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	second, err := repo.Upsert(ctx, "alice@example.com", "Alice Renamed", false)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("expected same id, got %d -> %d", first.ID, second.ID)
	}
	if !second.IsAdmin {
		t.Errorf("is_admin must be preserved across upserts; got false")
	}
}

func TestEnsureIdentity_LinksToPreProvisionedUser(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, "bob@example.com", "Bob", false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	u, err := repo.EnsureIdentity(ctx, "google", "google-sub-123", "bob@example.com")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if u.Email != "bob@example.com" {
		t.Errorf("got %q, want bob@example.com", u.Email)
	}

	u2, err := repo.EnsureIdentity(ctx, "google", "google-sub-123", "irrelevant@example.com")
	if err != nil {
		t.Fatalf("ensure identity 2: %v", err)
	}
	if u2.ID != u.ID {
		t.Errorf("expected same user, got %d vs %d", u2.ID, u.ID)
	}
}

func TestEnsureIdentity_RejectsUnknownEmail(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)

	_, err := repo.EnsureIdentity(context.Background(), "google", "sub-x", "stranger@example.com")
	if !errors.Is(err, users.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSetActive_ReturnsNotFoundForUnknownEmail(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)

	err := repo.SetActive(context.Background(), "ghost@example.com", false)
	if !errors.Is(err, users.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
