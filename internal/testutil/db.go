// Package testutil holds shared test helpers. testutil.OpenTestDB skips
// the test if DATABASE_URL is not set, so unit-only `go test ./...` runs
// stay green on a workstation without Postgres.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mauv0809/crispy-broccoli/internal/db"
)

// PoolFrom is a convenience helper for test code that receives a pool as `any`
// (e.g. passed through a generic helper). It panics if v is not *pgxpool.Pool.
func PoolFrom(v any) *pgxpool.Pool {
	p, ok := v.(*pgxpool.Pool)
	if !ok {
		panic("testutil.PoolFrom: expected *pgxpool.Pool")
	}
	return p
}

// OpenTestDB runs migrations and returns a connected pool. Auth-related
// tables are truncated before the test runs.
//
// CI sets DATABASE_URL via the Postgres service container; locally,
// `make db-up && export DATABASE_URL=...` enables these tests.
func OpenTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	if err := db.RunMigrations(dsn); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Wipe auth-related tables. CASCADE also clears strategies/strategy_runs/
	// portfolio because their created_by FK references users(id). Reference
	// data (companies, financial_metrics) is left intact — slow to repopulate
	// and not what these tests touch.
	_, err = pool.Exec(context.Background(),
		`TRUNCATE auth_identities, sessions, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Re-insert the synthetic system user that migration 015 normally
	// guarantees. Default-strategy seeding and any code path that targets
	// the system user via SELECT ... WHERE email='system@deepvalue.local'
	// depends on this row existing.
	_, err = pool.Exec(context.Background(),
		`INSERT INTO users (email, name, is_admin, is_active)
		 VALUES ('system@deepvalue.local', 'System', FALSE, FALSE)`)
	if err != nil {
		t.Fatalf("re-insert system user: %v", err)
	}
	return pool
}
