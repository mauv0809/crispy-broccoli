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

	// Wipe auth-related tables. Don't touch reference data (companies,
	// financial_metrics) — slow to repopulate and not what these tests touch.
	_, err = pool.Exec(context.Background(),
		`TRUNCATE auth_identities, sessions, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}
