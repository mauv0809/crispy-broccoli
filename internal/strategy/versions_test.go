package strategy_test

import (
	"context"
	"testing"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// systemUserID looks up the synthetic system user inserted by migration 015.
// testutil's OpenTestDB ensures it exists; we read its id here so tests can
// satisfy the strategies.created_by FK without coupling to a real auth flow.
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

func TestVersions_CreateAndGet(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	versions := strategy.NewVersionsRepository(pool)
	uid := systemUserID(t, pool)

	// Insert a bare strategies row directly (current_version_id is nullable per
	// migration 025, so we omit it here and patch it after creating v1).
	var strategyID int64
	rules := []byte(`{"filters":[],"ranking":[],"limit":6,"dimension":"MRQ"}`)
	err := pool.QueryRow(ctx, `
		INSERT INTO strategies (name, description, rules, is_default, status, created_by, created_at, updated_at)
		VALUES ('Test', '', $1::jsonb, false, 'draft', $2, NOW(), NOW())
		RETURNING id
	`, rules, uid).Scan(&strategyID)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}

	v1, err := versions.Create(ctx, strategyID, rules, uid)
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if v1.VersionNumber != 1 {
		t.Errorf("v1.VersionNumber = %d, want 1", v1.VersionNumber)
	}
	if v1.StrategyID != strategyID {
		t.Errorf("v1.StrategyID = %d, want %d", v1.StrategyID, strategyID)
	}

	// Patch current_version_id to satisfy the FK we asserted above.
	if _, err := pool.Exec(ctx, `UPDATE strategies SET current_version_id = $1 WHERE id = $2`, v1.ID, strategyID); err != nil {
		t.Fatalf("patch current_version_id: %v", err)
	}

	rules2 := []byte(`{"filters":[],"ranking":[],"limit":10,"dimension":"MRQ"}`)
	v2, err := versions.Create(ctx, strategyID, rules2, uid)
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if v2.VersionNumber != 2 {
		t.Errorf("v2.VersionNumber = %d, want 2", v2.VersionNumber)
	}

	got, err := versions.Get(ctx, v1.ID)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if got.VersionNumber != 1 {
		t.Errorf("got.VersionNumber = %d, want 1", got.VersionNumber)
	}

	all, err := versions.ListByStrategy(ctx, strategyID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	// ListByStrategy returns DESC by version_number.
	if all[0].VersionNumber != 2 || all[1].VersionNumber != 1 {
		t.Errorf("expected [v2, v1], got [v%d, v%d]", all[0].VersionNumber, all[1].VersionNumber)
	}
}
