package auth_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// We can't drive a real Google OAuth round-trip in tests. Instead, exercise
// the post-callback bookkeeping (EnsureIdentity → TouchLastLogin → session
// PUT) directly by simulating what GoogleHandler.Callback does after gothic
// returns.
func TestCallbackBookkeeping_LinksAndStartsSession(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)

	// Pre-provision the user (admin path).
	if _, err := repo.Upsert(context.Background(), "alice@example.com", "Alice", false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Build a session manager backed by the same DB.
	sqlDB, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	defer sqlDB.Close()
	sm := scs.New()
	sm.Store = postgresstore.New(sqlDB)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/auth/google/callback", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	wrapped := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		c.SetRequest(r)
		c.Response().Writer = w

		u, err := repo.EnsureIdentity(r.Context(), "google", "google-sub-1", "alice@example.com")
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if err := repo.TouchLastLogin(r.Context(), u.ID); err != nil {
			t.Fatalf("touch: %v", err)
		}
		auth.PutUserID(sm, c, u.ID)
	}))
	wrapped.ServeHTTP(rec, req)

	if !called {
		t.Fatal("callback simulation did not run")
	}

	if cookies := rec.Result().Cookies(); len(cookies) == 0 {
		t.Errorf("expected session cookie to be set, got none")
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM auth_identities WHERE provider='google' AND provider_id='google-sub-1'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("auth_identities rows = %d, want 1", n)
	}

	var nullable bool
	if err := pool.QueryRow(context.Background(),
		`SELECT last_login_at IS NOT NULL FROM users WHERE email='alice@example.com'`,
	).Scan(&nullable); err != nil {
		t.Fatalf("scan last_login: %v", err)
	}
	if !nullable {
		t.Errorf("expected last_login_at to be set")
	}
}

func TestCallbackBookkeeping_RejectsUnknownEmail(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)

	_, err := repo.EnsureIdentity(context.Background(), "google", "sub-x", "stranger@example.com")
	if err == nil {
		t.Fatal("expected error for unknown email")
	}
	if !errorsIs(err, users.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func errorsIs(err, target error) bool {
	type unwrap interface{ Unwrap() error }
	for err != nil {
		if err == target {
			return true
		}
		if u, ok := err.(unwrap); ok {
			err = u.Unwrap()
			continue
		}
		return false
	}
	return false
}
