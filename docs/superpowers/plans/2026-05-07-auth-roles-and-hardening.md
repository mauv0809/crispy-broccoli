# Auth, Roles & Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Google OAuth login (goth + scs/postgresstore), `users` / `auth_identities` / `sessions` schema, `RequireAuth` + `RequireAdmin` Echo middlewares, env-seeded initial admin + CLI user-management flags, `created_by` attribution on user-authored tables, and CSRF protection on state-changing routes — with HTMX picking up the token from a `<meta>` tag.

**Architecture:** Auth is opaque to handlers: middleware loads the session, fetches the user by ID, attaches `*users.User` to `echo.Context`. Initial admin is upserted at startup from `INITIAL_ADMIN_EMAIL`. CLI flags (`--add-user`, `--disable-user`) short-circuit before HTTP starts. `created_by` is nullable to keep the migration trivial; new inserts populate it from the request user. CSRF uses Echo's stock middleware (`X-CSRF-Token` header) and is exposed to HTMX via a meta tag + `htmx:configRequest` listener.

**Tech Stack:** Go 1.24, Echo v4, pgx v5, Goose (SQL only), `github.com/markbates/goth/providers/google`, `github.com/alexedwards/scs/v2` + `github.com/alexedwards/scs/postgresstore`, templ, HTMX.

**Spec:** `docs/superpowers/specs/2026-05-05-production-readiness-design.md` — Sections 3 (auth) and 5 hardening item *CSRF only*. Sections 1, 2, 4, 5 (other items), 6 are **out of scope** — already implemented in Plan 1.

**One deliberate spec deviation:** The spec's section 3 says the admin seed runs as a Go migration and `created_by` is backfilled during the migration that adds it. We instead seed at app startup (post-migration, idempotent upsert) and leave `created_by` **nullable** with no backfill. Reasoning: this codebase's Goose setup only embeds `*.sql` migrations; mixing in env-aware Go migrations adds complexity that doesn't pay off because the spec's read pattern is global (not per-user filtering). NULL `created_by` for legacy rows is acceptable — when isolation is later required, those rows can be assigned to the admin in a one-line UPDATE.

---

## File Structure

**New files**:
- `internal/db/migrations/014_auth.sql` — users, auth_identities, sessions
- `internal/db/migrations/015_created_by.sql` — `created_by` columns on strategies / strategy_runs / portfolio
- `internal/users/users.go` — `User` struct + `Repository` (Get / GetByEmail / Upsert / Disable / SetAdmin / EnsureIdentity)
- `internal/users/users_test.go` — repository integration tests (uses DATABASE_URL)
- `internal/users/cli.go` — `AddUser` and `DisableUser` functions called from main when CLI flags are passed
- `internal/auth/session.go` — scs session manager construction + middleware adapter for Echo
- `internal/auth/google.go` — goth provider registration + login/callback/logout handlers
- `internal/auth/middleware.go` — `RequireAuth` and `RequireAdmin` Echo middlewares + `UserFromContext` helper
- `internal/auth/middleware_test.go` — middleware unit tests (no DB)
- `internal/auth/google_test.go` — callback flow integration test (uses DATABASE_URL)
- `internal/views/csrf.go` — `CSRFFromContext(ctx context.Context) string` helper for templ
- `internal/testutil/db.go` — `OpenTestDB(t)` helper; skips if `DATABASE_URL` unset

**Modified files**:
- `cmd/app/main.go` — admin seed, CLI flag short-circuit, scs init, auth routes, RequireAuth wiring, CSRF middleware
- `internal/handlers/render.go` — copy CSRF token from echo to request context for templ to read
- `internal/handlers/strategies.go` — read user from context, pass `userID` into repo calls
- `internal/strategy/repository.go` — accept `createdBy *int64` on `Create`, `SaveRun`
- `internal/strategy/seeds.go` — pass nil `createdBy` for default-strategy seeding (system-owned)
- `internal/views/layout.templ` — `<meta name="csrf-token">` + `htmx:configRequest` JS listener
- `.env.example` — `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `INITIAL_ADMIN_EMAIL`, `SESSION_KEY`
- `go.mod`, `go.sum` — new deps

**Untouched packages:** `internal/observability`, `internal/buildinfo`, `internal/ingest`, `internal/db` (besides migrate.go is unchanged).

---

## Task 1: Add auth dependencies

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the modules**

Run:
```bash
go get github.com/markbates/goth@latest
go get github.com/markbates/goth/providers/google@latest
go get github.com/alexedwards/scs/v2@latest
go get github.com/alexedwards/scs/postgresstore@latest
go mod tidy
```

Expected: `go.mod` shows direct requires for `github.com/markbates/goth`, `github.com/alexedwards/scs/v2`, `github.com/alexedwards/scs/postgresstore`. (The `goth/providers/google` package lives inside the same module.)

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`

Expected: success. (No usages yet — we just want to confirm the modules resolve.)

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "build: add goth and scs dependencies for auth"
```

---

## Task 2: Migration 014 — auth schema

**Files:**
- Create: `internal/db/migrations/014_auth.sql`

- [ ] **Step 1: Write the migration**

Create `internal/db/migrations/014_auth.sql`:

```sql
-- +goose Up

CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    name            TEXT,
    is_admin        BOOLEAN NOT NULL DEFAULT FALSE,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at   TIMESTAMPTZ
);

CREATE TABLE auth_identities (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider     TEXT NOT NULL,
    provider_id  TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_id)
);
CREATE INDEX auth_identities_user_id_idx ON auth_identities (user_id);

-- scs/postgresstore canonical schema.
CREATE TABLE sessions (
    token  TEXT PRIMARY KEY,
    data   BYTEA NOT NULL,
    expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- +goose Down
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS auth_identities;
DROP TABLE IF EXISTS users;
```

- [ ] **Step 2: Verify the migration applies cleanly**

Make sure local Postgres is up (`make db-up`), then:

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -20
# Ctrl-C once you see "migrations completed".
```

Expected: log line `OK   014_auth.sql` or equivalent. Confirm with:

```bash
psql postgres://value_user:value_pass@localhost:5432/value_db -c "\dt users auth_identities sessions"
```

Expected: all three tables listed.

- [ ] **Step 3: Commit**

```bash
git add internal/db/migrations/014_auth.sql
git commit -m "feat(db): add users, auth_identities, sessions tables"
```

---

## Task 3: Migration 015 — `created_by` columns

**Files:**
- Create: `internal/db/migrations/015_created_by.sql`

- [ ] **Step 1: Write the migration**

Create `internal/db/migrations/015_created_by.sql`:

```sql
-- +goose Up

-- Nullable on purpose: pre-existing rows have no owner. New writes populate it.
ALTER TABLE strategies     ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE strategy_runs  ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE portfolio      ADD COLUMN created_by BIGINT REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX strategies_created_by_idx     ON strategies     (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX strategy_runs_created_by_idx  ON strategy_runs  (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX portfolio_created_by_idx      ON portfolio      (created_by) WHERE created_by IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS portfolio_created_by_idx;
DROP INDEX IF EXISTS strategy_runs_created_by_idx;
DROP INDEX IF EXISTS strategies_created_by_idx;
ALTER TABLE portfolio      DROP COLUMN IF EXISTS created_by;
ALTER TABLE strategy_runs  DROP COLUMN IF EXISTS created_by;
ALTER TABLE strategies     DROP COLUMN IF EXISTS created_by;
```

- [ ] **Step 2: Verify the migration applies**

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -20
# Ctrl-C after "migrations completed".

psql postgres://value_user:value_pass@localhost:5432/value_db \
  -c "\d strategies" -c "\d strategy_runs" -c "\d portfolio"
```

Expected: each table now has a `created_by | bigint` column.

- [ ] **Step 3: Commit**

```bash
git add internal/db/migrations/015_created_by.sql
git commit -m "feat(db): add created_by attribution columns"
```

---

## Task 4: Users repository — package skeleton + types

**Files:**
- Create: `internal/users/users.go`

- [ ] **Step 1: Write the package**

Create `internal/users/users.go`:

```go
package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID          int64
	Email       string
	Name        string
	IsAdmin     bool
	IsActive    bool
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

var ErrNotFound = errors.New("user not found")

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// GetByID returns the user with the given primary key. ErrNotFound when missing.
func (r *Repository) GetByID(ctx context.Context, id int64) (*User, error) {
	const q = `SELECT id, email, name, is_admin, is_active, created_at, last_login_at
	           FROM users WHERE id = $1`
	return r.scanOne(r.pool.QueryRow(ctx, q, id))
}

// GetByEmail returns the user with the given email. ErrNotFound when missing.
func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	const q = `SELECT id, email, name, is_admin, is_active, created_at, last_login_at
	           FROM users WHERE email = $1`
	return r.scanOne(r.pool.QueryRow(ctx, q, email))
}

// Upsert inserts a user or no-ops if the email already exists. Returns the
// resulting (or existing) user. is_admin and is_active on conflict are NOT
// changed; that lets the env-seeded admin survive a startup that runs after
// an admin has manually demoted themselves.
func (r *Repository) Upsert(ctx context.Context, email, name string, isAdmin bool) (*User, error) {
	const q = `
		INSERT INTO users (email, name, is_admin, is_active)
		VALUES ($1, $2, $3, TRUE)
		ON CONFLICT (email) DO UPDATE
		   SET name = COALESCE(NULLIF(EXCLUDED.name, ''), users.name)
		RETURNING id, email, name, is_admin, is_active, created_at, last_login_at`
	return r.scanOne(r.pool.QueryRow(ctx, q, email, name, isAdmin))
}

// SetActive flips is_active. Used by --disable-user.
func (r *Repository) SetActive(ctx context.Context, email string, active bool) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET is_active = $1 WHERE email = $2`, active, email)
	if err != nil {
		return fmt.Errorf("set active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastLogin sets last_login_at = NOW() for the given user.
func (r *Repository) TouchLastLogin(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET last_login_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("touch last_login_at: %w", err)
	}
	return nil
}

// EnsureIdentity finds the user linked to (provider, providerID). If the
// identity row does not yet exist but a user with `email` does, the
// identity is created and that user is returned (admin pre-provisioning
// path). If no user exists for that email, ErrNotFound is returned.
func (r *Repository) EnsureIdentity(ctx context.Context, provider, providerID, email string) (*User, error) {
	// 1) identity already linked?
	const idLookup = `
		SELECT u.id, u.email, u.name, u.is_admin, u.is_active, u.created_at, u.last_login_at
		FROM auth_identities ai
		JOIN users u ON u.id = ai.user_id
		WHERE ai.provider = $1 AND ai.provider_id = $2`
	if u, err := r.scanOne(r.pool.QueryRow(ctx, idLookup, provider, providerID)); err == nil {
		return u, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// 2) admin pre-provisioned by email?
	u, err := r.GetByEmail(ctx, email)
	if err != nil {
		return nil, err // ErrNotFound bubbles
	}

	// 3) link the identity to that user
	_, err = r.pool.Exec(ctx,
		`INSERT INTO auth_identities (user_id, provider, provider_id) VALUES ($1, $2, $3)`,
		u.ID, provider, providerID)
	if err != nil {
		return nil, fmt.Errorf("insert identity: %w", err)
	}
	return u, nil
}

func (r *Repository) scanOne(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return &u, nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/users/`

Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/users/users.go
git commit -m "feat(users): add Repository with Upsert, GetBy*, EnsureIdentity"
```

---

## Task 5: Users repository — integration tests

**Files:**
- Create: `internal/testutil/db.go`
- Create: `internal/users/users_test.go`

- [ ] **Step 1: Write the test helper**

Create `internal/testutil/db.go`:

```go
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

// OpenTestDB runs migrations and returns a connected pool. Each table is
// truncated before the test runs (cheap on small schemas).
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
```

- [ ] **Step 2: Write the failing tests**

Create `internal/users/users_test.go`:

```go
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

	// First call inserts as admin.
	first, err := repo.Upsert(ctx, "alice@example.com", "Alice", true)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Second call passes is_admin=false; the row's is_admin must stay true.
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

	// Calling again with the same (provider, provider_id) should return the
	// same user without inserting a second row.
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
```

- [ ] **Step 3: Run tests**

Make sure `make db-up` is up. Then:

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go test ./internal/users/ -v
```

Expected: all tests PASS. Without `DATABASE_URL`: tests SKIP (not fail).

- [ ] **Step 4: Commit**

```bash
git add internal/testutil/db.go internal/users/users_test.go
git commit -m "test(users): integration tests for Upsert and EnsureIdentity"
```

---

## Task 6: Seed initial admin at startup

**Files:**
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Wire the seed**

Edit `cmd/app/main.go`. Add to the import block:

```go
"github.com/mauv0809/crispy-broccoli/internal/users"
```

After the `slog.Info("database connected")` line and before the existing `// Setup Echo` comment, add:

```go
usersRepo := users.NewRepository(pool)
if email := os.Getenv("INITIAL_ADMIN_EMAIL"); email != "" {
	if _, err := usersRepo.Upsert(ctx, email, "", true); err != nil {
		slog.Error("initial admin upsert failed", "error", err)
		os.Exit(1)
	}
	slog.Info("initial admin ensured", "email", email)
}
```

- [ ] **Step 2: Verify behavior with the env var set**

```bash
INITIAL_ADMIN_EMAIL=admin@example.com \
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -10
# Ctrl-C after you see "initial admin ensured".

psql postgres://value_user:value_pass@localhost:5432/value_db \
  -c "SELECT email, is_admin, is_active FROM users"
```

Expected: one row, `admin@example.com | t | t`. Re-running the command produces the same single row (idempotent).

- [ ] **Step 3: Verify behavior with the env var unset**

```bash
unset INITIAL_ADMIN_EMAIL
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -10
```

Expected: no `initial admin ensured` log line. App starts normally.

- [ ] **Step 4: Commit**

```bash
git add cmd/app/main.go
git commit -m "feat(auth): upsert initial admin from INITIAL_ADMIN_EMAIL on startup"
```

---

## Task 7: scs session manager + Echo adapter

**Files:**
- Create: `internal/auth/session.go`

- [ ] **Step 1: Implement the session helper**

Create `internal/auth/session.go`:

```go
package auth

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	"github.com/labstack/echo/v4"
)

const sessionUserIDKey = "user_id"

// NewSessionManager returns an scs manager backed by a Postgres store.
// The schema (table `sessions`) is created by migration 014.
//
// db must be a *database/sql connection pool — scs's postgresstore wants
// the stdlib sql interface even though the rest of the app uses pgx.
// Open it via sql.Open("pgx", databaseURL).
func NewSessionManager(db *sql.DB) *scs.SessionManager {
	sm := scs.New()
	sm.Store = postgresstore.New(db)
	sm.Lifetime = 30 * 24 * time.Hour
	sm.Cookie.Name = "deepvalue_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Persist = true
	sm.Cookie.Secure = true // overridden by main when ENV != "production"
	return sm
}

// SessionMiddleware adapts scs's standard net/http middleware to Echo's
// pipeline so any handler downstream can read/write session data via
// sm.Get/Put on the request context.
func SessionMiddleware(sm *scs.SessionManager) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			var handlerErr error
			h := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.SetRequest(r)
				c.Response().Writer = w
				handlerErr = next(c)
			}))
			h.ServeHTTP(c.Response().Writer, c.Request())
			return handlerErr
		}
	}
}

// PutUserID writes the authenticated user's ID into the session.
func PutUserID(sm *scs.SessionManager, c echo.Context, id int64) {
	sm.Put(c.Request().Context(), sessionUserIDKey, id)
}

// UserID reads the authenticated user's ID from the session, or 0 if absent.
func UserID(sm *scs.SessionManager, c echo.Context) int64 {
	return sm.GetInt64(c.Request().Context(), sessionUserIDKey)
}

// Destroy clears the session — call on logout.
func Destroy(sm *scs.SessionManager, c echo.Context) error {
	return sm.Destroy(c.Request().Context())
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/auth/`

Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/auth/session.go
git commit -m "feat(auth): scs session manager + Echo middleware adapter"
```

---

## Task 8: RequireAuth + RequireAdmin middlewares (with tests)

**Files:**
- Create: `internal/auth/middleware.go`
- Create: `internal/auth/middleware_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/auth/middleware_test.go`:

```go
package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// fakeLoader implements auth.UserLoader for in-process middleware tests.
type fakeLoader struct {
	id   int64
	user *users.User
	err  error
}

func (f *fakeLoader) LoadUser(ctx context.Context, id int64) (*users.User, error) {
	f.id = id
	return f.user, f.err
}

// fakeSession implements auth.Session for in-process middleware tests.
type fakeSession struct{ id int64 }

func (f fakeSession) UserID(c echo.Context) int64 { return f.id }

func TestRequireAuth_NoSession_RedirectsHTML(t *testing.T) {
	e := echo.New()
	mw := auth.RequireAuth(fakeSession{id: 0}, &fakeLoader{})
	called := false
	h := mw(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("middleware err: %v", err)
	}
	if called {
		t.Error("downstream handler must not run")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/auth/google/login" {
		t.Errorf("location: got %q, want /auth/google/login", loc)
	}
}

func TestRequireAuth_NoSession_HXRequestReturns401(t *testing.T) {
	e := echo.New()
	mw := auth.RequireAuth(fakeSession{id: 0}, &fakeLoader{})
	h := mw(func(c echo.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/api/strategies", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestRequireAuth_InactiveUser_TreatedAsLoggedOut(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{user: &users.User{ID: 7, IsActive: false}}
	mw := auth.RequireAuth(fakeSession{id: 7}, loader)
	called := false
	h := mw(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("inactive user must not reach handler")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", rec.Code)
	}
}

func TestRequireAuth_ValidUser_AttachesToContext(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{user: &users.User{ID: 7, Email: "alice@example.com", IsActive: true}}
	mw := auth.RequireAuth(fakeSession{id: 7}, loader)

	var seen *users.User
	h := mw(func(c echo.Context) error {
		seen = auth.UserFromContext(c)
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if seen == nil || seen.ID != 7 {
		t.Errorf("UserFromContext: got %+v, want id=7", seen)
	}
	if loader.id != 7 {
		t.Errorf("loader called with %d, want 7", loader.id)
	}
}

func TestRequireAuth_LoaderNotFound_TreatedAsLoggedOut(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{err: users.ErrNotFound}
	mw := auth.RequireAuth(fakeSession{id: 99}, loader)
	called := false
	h := mw(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("handler must not run when user is not found")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", rec.Code)
	}
}

func TestRequireAuth_LoaderError_PropagatesAs500(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{err: errors.New("boom")}
	mw := auth.RequireAuth(fakeSession{id: 7}, loader)
	h := mw(func(c echo.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	rec := httptest.NewRecorder()
	err := h(e.NewContext(req, rec))
	if err == nil {
		t.Fatalf("expected error to be returned to Echo")
	}
}

func TestRequireAdmin_NonAdmin_Returns403(t *testing.T) {
	e := echo.New()
	called := false
	h := auth.RequireAdmin()(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/admin/ingest/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	auth.SetUserOnContext(c, &users.User{ID: 7, IsAdmin: false, IsActive: true})

	if err := h(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("non-admin must not reach handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", rec.Code)
	}
}

func TestRequireAdmin_Admin_PassesThrough(t *testing.T) {
	e := echo.New()
	called := false
	h := auth.RequireAdmin()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/ingest/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	auth.SetUserOnContext(c, &users.User{ID: 7, IsAdmin: true, IsActive: true})

	if err := h(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Error("admin handler must run")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
}

func TestRequireAdmin_NoUser_Returns403(t *testing.T) {
	e := echo.New()
	h := auth.RequireAdmin()(func(c echo.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/admin/ingest/status", nil)
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/auth/ -v`

Expected: FAIL — `auth.RequireAuth`, `auth.RequireAdmin`, `auth.UserFromContext`, `auth.SetUserOnContext`, `auth.Session`, `auth.UserLoader` not defined.

- [ ] **Step 3: Implement middleware.go**

Create `internal/auth/middleware.go`:

```go
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/users"
)

const contextKeyUser = "auth.user"

// Session abstracts the bits of scs the middleware actually needs, so
// tests don't have to construct a real scs.SessionManager.
type Session interface {
	UserID(c echo.Context) int64
}

// UserLoader fetches a user by ID. *users.Repository satisfies it.
type UserLoader interface {
	LoadUser(ctx context.Context, id int64) (*users.User, error)
}

// repoLoader adapts *users.Repository to UserLoader without changing its API.
type repoLoader struct{ R *users.Repository }

func (l repoLoader) LoadUser(ctx context.Context, id int64) (*users.User, error) {
	return l.R.GetByID(ctx, id)
}

// NewLoader is the production wiring for UserLoader.
func NewLoader(r *users.Repository) UserLoader { return repoLoader{R: r} }

// scsSession adapts *scs.SessionManager to the Session interface.
type scsSession struct{ M sessionLike }

type sessionLike interface {
	GetInt64(ctx context.Context, key string) int64
}

func (s scsSession) UserID(c echo.Context) int64 {
	return s.M.GetInt64(c.Request().Context(), sessionUserIDKey)
}

// NewSession is the production wiring for Session. Pass *scs.SessionManager.
func NewSession(m sessionLike) Session { return scsSession{M: m} }

// UserFromContext returns the *users.User attached by RequireAuth, or nil.
func UserFromContext(c echo.Context) *users.User {
	if v, ok := c.Get(contextKeyUser).(*users.User); ok {
		return v
	}
	return nil
}

// SetUserOnContext attaches a user — used by RequireAuth and by tests.
func SetUserOnContext(c echo.Context, u *users.User) {
	c.Set(contextKeyUser, u)
}

// RequireAuth ensures a session-bound, active user exists. HTML requests
// without a session redirect to the login route; HTMX/JSON requests get a
// 401 instead so the client can handle it without a page reload.
func RequireAuth(sess Session, loader UserLoader) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			id := sess.UserID(c)
			if id == 0 {
				return rejectUnauthenticated(c)
			}
			u, err := loader.LoadUser(c.Request().Context(), id)
			if errors.Is(err, users.ErrNotFound) {
				return rejectUnauthenticated(c)
			}
			if err != nil {
				slog.Error("auth: load user failed", "user_id", id, "error", err)
				return err
			}
			if !u.IsActive {
				return rejectUnauthenticated(c)
			}
			SetUserOnContext(c, u)
			return next(c)
		}
	}
}

// RequireAdmin must run after RequireAuth. Returns 403 when the user is
// missing or non-admin.
func RequireAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			u := UserFromContext(c)
			if u == nil || !u.IsAdmin {
				return c.String(http.StatusForbidden, "forbidden")
			}
			return next(c)
		}
	}
}

func rejectUnauthenticated(c echo.Context) error {
	if isAPI(c) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
	return c.Redirect(http.StatusSeeOther, "/auth/google/login")
}

func isAPI(c echo.Context) bool {
	if c.Request().Header.Get("HX-Request") == "true" {
		return true
	}
	accept := c.Request().Header.Get("Accept")
	if accept == "" {
		return false
	}
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		return true
	}
	if strings.HasPrefix(c.Request().URL.Path, "/api/") {
		return true
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/ -v`

Expected: all 9 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/middleware.go internal/auth/middleware_test.go
git commit -m "feat(auth): RequireAuth + RequireAdmin Echo middlewares"
```

---

## Task 9: Google OAuth provider + login/callback/logout handlers

**Files:**
- Create: `internal/auth/google.go`

- [ ] **Step 1: Implement google.go**

Create `internal/auth/google.go`:

```go
package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo/v4"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	"github.com/markbates/goth/providers/google"

	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// GoogleConfig wires the goth provider. Caller (main) reads env.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	BaseURL      string // e.g. https://deepvalue.example.com or http://localhost:8080
}

// RegisterGoogle installs the goth Google provider. Must be called once
// at startup before any login attempt. The redirect URI is derived from
// BaseURL: <BaseURL>/auth/google/callback.
func RegisterGoogle(cfg GoogleConfig) error {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return errors.New("google oauth: client id and secret required")
	}
	if cfg.BaseURL == "" {
		return errors.New("google oauth: BASE_URL required")
	}
	goth.UseProviders(google.New(
		cfg.ClientID,
		cfg.ClientSecret,
		fmt.Sprintf("%s/auth/google/callback", cfg.BaseURL),
		"email", "profile",
	))
	return nil
}

// NewGothicStore returns a gorilla CookieStore suitable for gothic's
// short-lived OAuth state cookie. Caller assigns it to gothic.Store.
func NewGothicStore(sessionKey []byte, secureCookie bool) *sessions.CookieStore {
	store := sessions.NewCookieStore(sessionKey)
	store.Options.HttpOnly = true
	store.Options.Secure = secureCookie
	store.Options.SameSite = http.SameSiteLaxMode
	store.Options.MaxAge = 60 * 10 // OAuth state cookie — 10 min is plenty.
	store.Options.Path = "/"
	return store
}

// GoogleHandler bundles the three OAuth endpoints.
type GoogleHandler struct {
	sm    *scs.SessionManager
	users *users.Repository
}

func NewGoogleHandler(sm *scs.SessionManager, repo *users.Repository) *GoogleHandler {
	return &GoogleHandler{sm: sm, users: repo}
}

// Login initiates the OAuth dance. goth/gothic stashes the OAuth state
// in a short-lived cookie and redirects to Google's consent screen.
func (h *GoogleHandler) Login(c echo.Context) error {
	// gothic reads `provider` from the request query string.
	q := c.Request().URL.Query()
	q.Set("provider", "google")
	c.Request().URL.RawQuery = q.Encode()

	// If the user is already authenticated upstream, just redirect home.
	if u, _ := gothic.CompleteUserAuth(c.Response().Writer, c.Request()); u.Email != "" {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	gothic.BeginAuthHandler(c.Response().Writer, c.Request())
	return nil
}

// Callback finalizes the OAuth dance.
func (h *GoogleHandler) Callback(c echo.Context) error {
	q := c.Request().URL.Query()
	q.Set("provider", "google")
	c.Request().URL.RawQuery = q.Encode()

	gothUser, err := gothic.CompleteUserAuth(c.Response().Writer, c.Request())
	if err != nil {
		slog.Warn("oauth callback failed", "error", err)
		return c.String(http.StatusBadRequest, "OAuth callback failed.")
	}

	ctx := c.Request().Context()
	u, err := h.users.EnsureIdentity(ctx, "google", gothUser.UserID, gothUser.Email)
	if errors.Is(err, users.ErrNotFound) {
		return c.String(http.StatusForbidden,
			"This Google account is not authorized. Contact the administrator.")
	}
	if err != nil {
		slog.Error("ensure identity failed", "error", err)
		return err
	}
	if !u.IsActive {
		return c.String(http.StatusForbidden,
			"Your account is disabled. Contact the administrator.")
	}

	if err := h.users.TouchLastLogin(ctx, u.ID); err != nil {
		// Non-fatal — log and continue.
		slog.Warn("touch last_login_at failed", "user_id", u.ID, "error", err)
	}

	if err := h.sm.RenewToken(ctx); err != nil {
		slog.Error("session renew failed", "error", err)
		return err
	}
	PutUserID(h.sm, c, u.ID)

	return c.Redirect(http.StatusSeeOther, "/")
}

// Logout clears the session and redirects home.
func (h *GoogleHandler) Logout(c echo.Context) error {
	if err := h.sm.Destroy(c.Request().Context()); err != nil {
		slog.Error("session destroy failed", "error", err)
		return err
	}
	return c.Redirect(http.StatusSeeOther, "/")
}

// Mount registers the auth routes onto the Echo instance. These must NOT
// be wrapped in RequireAuth.
func (h *GoogleHandler) Mount(e *echo.Echo) {
	e.GET("/auth/google/login", h.Login)
	e.GET("/auth/google/callback", h.Callback)
	e.POST("/auth/logout", h.Logout)
}
```

- [ ] **Step 2: Run `go mod tidy` so gorilla/sessions is a direct require**

Run: `go mod tidy`

Expected: `github.com/gorilla/sessions` shows as a direct require (it was previously transitive via goth).

- [ ] **Step 3: Verify it compiles**

Run: `go build ./internal/auth/`

Expected: success.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/google.go go.mod go.sum
git commit -m "feat(auth): Google OAuth handlers (login/callback/logout)"
```

---

## Task 10: Wire auth into main — sessions, Google provider, routes, middleware

**Files:**
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Add imports**

Edit `cmd/app/main.go`. Add to the import block:

```go
"database/sql"
"encoding/hex"

"github.com/markbates/goth/gothic"

"github.com/mauv0809/crispy-broccoli/internal/auth"
```

(`database/sql` is for the scs store; `encoding/hex` is for decoding `SESSION_KEY`.)

- [ ] **Step 2: Build the session manager and the google handler**

After the `usersRepo := users.NewRepository(pool)` block from Task 6, and before `// Setup Echo`, insert:

```go
// Sessions: scs uses database/sql, not pgx. Open a small companion pool.
sqlDB, err := sql.Open("pgx", databaseURL)
if err != nil {
	slog.Error("session db open failed", "error", err)
	os.Exit(1)
}
defer sqlDB.Close()
sessionManager := auth.NewSessionManager(sqlDB)
sessionManager.Cookie.Secure = env == "production"

// Gothic store (for the short-lived OAuth state cookie).
sessionKeyHex := os.Getenv("SESSION_KEY")
if env == "production" && sessionKeyHex == "" {
	slog.Error("SESSION_KEY required in production")
	os.Exit(1)
}
sessionKey, err := hex.DecodeString(sessionKeyHex)
if err != nil || len(sessionKey) < 32 {
	if env == "production" {
		slog.Error("SESSION_KEY must be hex-encoded and at least 32 bytes")
		os.Exit(1)
	}
	// dev fallback: stable per-process random key
	sessionKey = []byte("dev-session-key-not-for-production-use")
}
gothic.Store = auth.NewGothicStore(sessionKey, env == "production")

// Google OAuth provider.
googleConfigured := false
if cid, csec := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"); cid != "" && csec != "" {
	if err := auth.RegisterGoogle(auth.GoogleConfig{
		ClientID:     cid,
		ClientSecret: csec,
		BaseURL:      os.Getenv("BASE_URL"),
	}); err != nil {
		slog.Error("google oauth init failed", "error", err)
		os.Exit(1)
	}
	googleConfigured = true
	slog.Info("google oauth provider registered")
} else {
	slog.Warn("google oauth disabled; GOOGLE_CLIENT_ID/SECRET not set")
}

googleHandler := auth.NewGoogleHandler(sessionManager, usersRepo)
authMiddleware := auth.RequireAuth(auth.NewSession(sessionManager), auth.NewLoader(usersRepo))
adminMiddleware := auth.RequireAdmin()
```

- [ ] **Step 3: Add the global session middleware to Echo**

In the existing `e.Use(...)` block, insert one line right after `middleware.Recover()`:

```go
e.Use(auth.SessionMiddleware(sessionManager))
```

(Order matters: session must run after Recover so a panic doesn't poison the cookie write, and before any handler that reads/writes the session.)

- [ ] **Step 4: Mount auth routes (no RequireAuth)**

Right after `e.Static("/assets", "assets")` and before the existing `e.GET("/health", h.Health)`, mount:

```go
googleHandler.Mount(e)
_ = googleConfigured // referenced for log gating only
```

- [ ] **Step 5: Apply RequireAuth to protected routes**

Update the existing route registration in main:

- `e.GET("/health", h.Health)` — **unchanged, no auth**
- `e.GET("/", h.Index)` — **add** `, authMiddleware` as the second arg
- `e.GET("/docs", h.Docs)` — **add** `, authMiddleware`
- `e.GET("/api/openapi.json", ...)` — **unchanged, no auth** (keep it discoverable)

For the `api := e.Group("/api")` block, change to:

```go
api := e.Group("/api", authMiddleware)
```

For the page-level `/strategies*` routes (currently `e.GET(...)`), change each to include `authMiddleware`:

```go
e.GET("/strategies", strategyHandler.StrategiesPage, authMiddleware)
e.GET("/strategies/new", strategyHandler.NewStrategyPage, authMiddleware)
e.GET("/strategies/:id", strategyHandler.StrategyDetailPage, authMiddleware)
e.GET("/strategies/:id/edit", strategyHandler.EditStrategyPage, authMiddleware)
```

For the admin group:

```go
admin := e.Group("/admin", authMiddleware, adminMiddleware)
```

(The existing inner registrations on `admin.POST(...)` etc. stay as-is.)

- [ ] **Step 6: Verify it compiles**

Run: `go build ./...`

Expected: success.

- [ ] **Step 7: Smoke-test login redirect**

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
INITIAL_ADMIN_EMAIL=admin@example.com \
  go run ./cmd/app &
sleep 1
curl -sI http://localhost:8080/strategies | head -5
kill %1
```

Expected: `HTTP/1.1 303 See Other` and `Location: /auth/google/login`.

- [ ] **Step 8: Smoke-test that /health is unauthenticated**

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app &
sleep 1
curl -s http://localhost:8080/health
kill %1
```

Expected: 200, `{"status":"ok",...}` — no redirect.

- [ ] **Step 9: Commit**

```bash
git add cmd/app/main.go
git commit -m "feat(auth): wire scs sessions, Google OAuth, RequireAuth/Admin"
```

---

## Task 11: OAuth callback flow integration test

**Files:**
- Create: `internal/auth/google_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/auth/google_test.go`:

```go
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

	// Fake the request/response so scs has somewhere to put a session.
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/auth/google/callback", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	// scs requires its LoadAndSave wrapper to attach a session context.
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

	// Assert the cookie was set.
	if cookies := rec.Result().Cookies(); len(cookies) == 0 {
		t.Errorf("expected session cookie to be set, got none")
	}

	// Assert auth_identities row exists.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM auth_identities WHERE provider='google' AND provider_id='google-sub-1'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("auth_identities rows = %d, want 1", n)
	}

	// Assert last_login_at was bumped.
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
```

- [ ] **Step 2: Run the test**

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go test ./internal/auth/ -run TestCallbackBookkeeping -v
```

Expected: PASS for both tests.

- [ ] **Step 3: Commit**

```bash
git add internal/auth/google_test.go
git commit -m "test(auth): integration coverage for OAuth callback bookkeeping"
```

---

## Task 12: Thread `created_by` through strategy writes

**Files:**
- Modify: `internal/strategy/repository.go`
- Modify: `internal/strategy/seeds.go`
- Modify: `internal/handlers/strategies.go`

- [ ] **Step 1: Update the strategy repository signatures**

Edit `internal/strategy/repository.go`. Replace the `Create` method:

```go
// Create inserts a new strategy. createdBy may be nil for system-seeded rows.
func (r *Repository) Create(ctx context.Context, req CreateStrategyRequest, createdBy *int64) (*Strategy, error) {
	rulesJSON, err := json.Marshal(req.Rules)
	if err != nil {
		return nil, fmt.Errorf("marshaling rules: %w", err)
	}

	var s Strategy
	err = r.pool.QueryRow(ctx, `
		INSERT INTO strategies (name, description, rules, is_default, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, false, $4, NOW(), NOW())
		RETURNING id, name, description, rules, is_default, created_at, updated_at
	`, req.Name, req.Description, rulesJSON, createdBy).Scan(
		&s.ID, &s.Name, &s.Description, &s.Rules, &s.IsDefault, &s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("creating strategy: %w", err)
	}
	return &s, nil
}
```

Replace `SaveRun`:

```go
// SaveRun saves a strategy execution run. createdBy may be nil for system-triggered runs.
func (r *Repository) SaveRun(ctx context.Context, run *StrategyRun, createdBy *int64) error {
	resultsJSON, err := json.Marshal(run.Results)
	if err != nil {
		return fmt.Errorf("marshaling results: %w", err)
	}

	err = r.pool.QueryRow(ctx, `
		INSERT INTO strategy_runs (strategy_id, run_at, results, execution_time_ms, stocks_screened, stocks_matched, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, run.StrategyID, run.RunAt, resultsJSON, run.ExecutionTimeMs, run.StocksScreened, run.StocksMatched, createdBy).Scan(&run.ID)
	if err != nil {
		return fmt.Errorf("saving strategy run: %w", err)
	}
	return nil
}
```

(Leave `Update`, `CreateDefaultStrategy`, list/get methods untouched. Updates don't change ownership; default strategies are system-owned with NULL.)

- [ ] **Step 2: Confirm all call sites live in handlers (already verified at plan time)**

The call sites of `repo.Create` and `repo.SaveRun` are all in `internal/handlers/strategies.go`:
- Line 104 — `CreateStrategy` calls `h.repo.Create(...)`
- Line 226 — `RunStrategyHTMX` calls `h.repo.SaveRun(...)`
- Line 432 — `RunBacktestHTMX` calls `_ = h.repo.SaveRun(...)`

(`internal/strategy/seeds.go` calls `CreateDefaultStrategy`, which we left untouched — default strategies are system-owned with NULL `created_by`.)

Re-confirm the survey holds before editing:

```bash
grep -rn 'repo\.Create\|\.SaveRun(' internal/ cmd/ | grep -v _test.go | grep -v migrations | grep -v CreateDefault
```

Expected: exactly the 3 lines above.

- [ ] **Step 3: Update the handler call sites in strategies.go**

Edit `internal/handlers/strategies.go`. Add to imports:

```go
"github.com/mauv0809/crispy-broccoli/internal/auth"
```

Add a private helper just below the `NewStrategyHandler` function:

```go
// currentUserID returns the authenticated user's ID as a pointer suitable
// for nullable created_by columns. Returns nil if no user is on context
// (should not happen behind RequireAuth, but defensive — the column is
// nullable so a nil insert is fine).
func currentUserID(c echo.Context) *int64 {
	if u := auth.UserFromContext(c); u != nil {
		id := u.ID
		return &id
	}
	return nil
}
```

Change the three call sites:

```go
// Line ~104 (CreateStrategy):
s, err := h.repo.Create(c.Request().Context(), req, currentUserID(c))

// Line ~226 (RunStrategyHTMX):
if err := h.repo.SaveRun(c.Request().Context(), run, currentUserID(c)); err != nil {

// Line ~432 (RunBacktestHTMX):
_ = h.repo.SaveRun(c.Request().Context(), run, currentUserID(c))
```

- [ ] **Step 4: Run tests and the build**

Run: `go build ./... && go test ./...`

Expected: success.

- [ ] **Step 5: Commit**

```bash
git add internal/strategy/repository.go internal/handlers/strategies.go
git commit -m "feat(strategies): record created_by on strategy create + run"
```

---

## Task 13: CSRF middleware + meta tag + HTMX wiring

**Files:**
- Create: `internal/views/csrf.go`
- Modify: `internal/handlers/render.go`
- Modify: `internal/views/layout.templ`
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Add the CSRF context helper for templ**

Create `internal/views/csrf.go`:

```go
package views

import "context"

type csrfKey struct{}

// WithCSRFToken stashes the token on a context so templ templates can read it.
func WithCSRFToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, csrfKey{}, token)
}

// CSRFFromContext reads a token previously stashed by WithCSRFToken.
// Returns empty string when none is present.
func CSRFFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(csrfKey{}).(string); ok {
		return s
	}
	return ""
}
```

- [ ] **Step 2: Update Render to copy the token onto the request context**

Edit `internal/handlers/render.go`:

```go
package handlers

import (
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/views"
)

func Render(c echo.Context, statusCode int, t templ.Component) error {
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(statusCode)

	ctx := c.Request().Context()
	if tok, ok := c.Get("csrf").(string); ok {
		ctx = views.WithCSRFToken(ctx, tok)
	}
	return t.Render(ctx, c.Response())
}
```

- [ ] **Step 3: Inject the meta tag and HTMX listener into the layout**

Edit `internal/views/layout.templ`. Replace the entire `<head>` block (lines 6–24) with:

```templ
		<head>
			<meta charset="UTF-8"/>
			<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
			<meta name="csrf-token" content={ CSRFFromContext(ctx) }/>
			<title>{ title } | DeepValue</title>
			<link rel="icon" type="image/png" href="/assets/favicon.png"/>
			<link rel="stylesheet" href="/assets/css/output.css"/>
			<script src="https://unpkg.com/htmx.org@2.0.4"></script>
			<script src="https://unpkg.com/htmx-ext-json-enc@2.0.1/json-enc.js"></script>
			<script defer src="https://unpkg.com/alpinejs@3.15.3/dist/cdn.min.js"></script>
			<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"></script>
			<script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3.0.0/dist/chartjs-adapter-date-fns.bundle.min.js"></script>
			<script>
				// Apply saved theme before render to avoid flash
				(function() {
					const theme = localStorage.getItem('theme') || 'dark';
					document.documentElement.setAttribute('data-theme', theme);
				})();
				// Inject CSRF token into every HTMX request as X-CSRF-Token.
				document.addEventListener('htmx:configRequest', function(e) {
					var meta = document.querySelector('meta[name="csrf-token"]');
					if (meta) {
						e.detail.headers['X-CSRF-Token'] = meta.getAttribute('content');
					}
				});
			</script>
		</head>
```

(`ctx` is implicit in templ template bodies — that's how `CSRFFromContext(ctx)` resolves.)

- [ ] **Step 4: Regenerate templ output**

Run: `make tools && templ generate`

Expected: `internal/views/layout_templ.go` regenerated.

- [ ] **Step 5: Wire the CSRF middleware in main**

Edit `cmd/app/main.go`. Just before the `// Routes` comment (right after the `e.Use(auth.SessionMiddleware(...))` line from Task 10, step 3), add:

```go
e.Use(middleware.CSRFWithConfig(middleware.CSRFConfig{
	TokenLookup:    "header:X-CSRF-Token,form:_csrf",
	CookieName:     "_csrf",
	CookiePath:     "/",
	CookieHTTPOnly: false, // JS needs to read it indirectly via meta tag
	CookieSameSite: http.SameSiteLaxMode,
	CookieSecure:   env == "production",
	Skipper: func(c echo.Context) bool {
		// /health, /assets, and the OAuth provider's own callback don't take
		// a CSRF token. /api/openapi.json is a static read.
		p := c.Request().URL.Path
		switch {
		case p == "/health",
			p == "/api/openapi.json",
			strings.HasPrefix(p, "/assets/"),
			strings.HasPrefix(p, "/auth/"):
			return true
		}
		return false
	},
}))
```

Add to the import block: `"strings"` (if not already there).

- [ ] **Step 6: Run the full test suite**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 7: Smoke-test that GETs still work and POSTs without a token are rejected**

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app &
sleep 1

# GET to /health should be 200, no auth, no CSRF.
curl -s -o /dev/null -w "health=%{http_code}\n" http://localhost:8080/health

# POST to /api/strategies WITHOUT a CSRF token should fail (401 from auth middleware
# OR 403 from CSRF — depending on order; either is correct rejection).
curl -s -o /dev/null -w "strategies_post=%{http_code}\n" \
  -X POST -H "Content-Type: application/json" -d '{}' http://localhost:8080/api/strategies

kill %1
```

Expected: `health=200` and `strategies_post=` either `401` or `403`, never `200`.

- [ ] **Step 8: Commit**

```bash
git add internal/views/csrf.go internal/handlers/render.go internal/views/layout.templ internal/views/layout_templ.go cmd/app/main.go
git commit -m "feat(security): CSRF middleware on state-changing routes; HTMX picks up token from meta tag"
```

---

## Task 14: CLI flags for user provisioning

**Files:**
- Create: `internal/users/cli.go`
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Implement the CLI helpers**

Create `internal/users/cli.go`:

```go
package users

import (
	"context"
	"fmt"
)

// AddUser inserts (or no-ops) a user. Used by `app --add-user EMAIL [--admin]`.
// Prints the resulting user; returns nil on success.
func AddUser(ctx context.Context, repo *Repository, email string, admin bool) error {
	u, err := repo.Upsert(ctx, email, "", admin)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	fmt.Printf("user: id=%d email=%s is_admin=%t is_active=%t\n", u.ID, u.Email, u.IsAdmin, u.IsActive)
	return nil
}

// DisableUser flips is_active to false. Used by `app --disable-user EMAIL`.
func DisableUser(ctx context.Context, repo *Repository, email string) error {
	if err := repo.SetActive(ctx, email, false); err != nil {
		return fmt.Errorf("disable: %w", err)
	}
	fmt.Printf("disabled: %s\n", email)
	return nil
}
```

- [ ] **Step 2: Parse flags and short-circuit in main**

Edit `cmd/app/main.go`. Add to the import block:

```go
"flag"
```

At the **very top of `func main()`** (before any logging or env reading), insert:

```go
addUserEmail := flag.String("add-user", "", "Add a user with the given email and exit")
disableUserEmail := flag.String("disable-user", "", "Disable the user with the given email and exit")
addUserAdmin := flag.Bool("admin", false, "When used with --add-user, mark the user as an admin")
flag.Parse()
```

After the `usersRepo := users.NewRepository(pool)` line (Task 6 added it), and **before** the session manager setup (Task 10), insert:

```go
// CLI short-circuit: provisioning commands don't need to start the HTTP server.
if *addUserEmail != "" {
	if err := users.AddUser(ctx, usersRepo, *addUserEmail, *addUserAdmin); err != nil {
		slog.Error("add-user failed", "error", err)
		os.Exit(1)
	}
	return
}
if *disableUserEmail != "" {
	if err := users.DisableUser(ctx, usersRepo, *disableUserEmail); err != nil {
		slog.Error("disable-user failed", "error", err)
		os.Exit(1)
	}
	return
}
```

- [ ] **Step 3: Verify both flag paths work end-to-end**

```bash
# Add a regular user.
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app --add-user bob@example.com
# Expect: user: id=... email=bob@example.com is_admin=false is_active=true

# Promote (re-run with --admin keeps is_admin=true since Upsert preserves on conflict;
# but a brand-new user gets is_admin=true because of the new row).
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app --add-user carol@example.com --admin
# Expect: user: id=... email=carol@example.com is_admin=true is_active=true

# Disable.
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app --disable-user bob@example.com
# Expect: disabled: bob@example.com

# Confirm via SQL.
psql postgres://value_user:value_pass@localhost:5432/value_db \
  -c "SELECT email, is_admin, is_active FROM users ORDER BY id"
# Expect: bob is_active=f; carol is_admin=t.
```

- [ ] **Step 4: Commit**

```bash
git add internal/users/cli.go cmd/app/main.go
git commit -m "feat(users): --add-user and --disable-user CLI flags"
```

---

## Task 15: Update .env.example

**Files:**
- Modify: `.env.example`

- [ ] **Step 1: Append the auth-related variables**

Edit `.env.example`. After the `SENTRY_DSN=""` line (the current end of file), append:

```dotenv

# --- Authentication (Plan 2) ---

# Initial admin email. On first deploy, set this to the operator's email
# before bringing the app up. The user will be upserted as is_admin=true on
# every startup (idempotent).
INITIAL_ADMIN_EMAIL=""

# Hex-encoded 32-byte signing key for goth's OAuth state cookie.
# Generate with: openssl rand -hex 32
# Required in production; a fixed dev fallback is used otherwise.
SESSION_KEY=""

# Google OAuth credentials. Create at:
# https://console.cloud.google.com/apis/credentials
# Authorized redirect URI: ${BASE_URL}/auth/google/callback
GOOGLE_CLIENT_ID=""
GOOGLE_CLIENT_SECRET=""
```

- [ ] **Step 2: Commit**

```bash
git add .env.example
git commit -m "docs: document auth env vars in .env.example"
```

---

## Task 16: Final verification

- [ ] **Step 1: Full test suite (with DB)**

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go test ./... -race
```

Expected: PASS.

- [ ] **Step 2: Build**

Run: `go build ./...`

Expected: success.

- [ ] **Step 3: Boot the app with auth fully wired and confirm /health is open**

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
INITIAL_ADMIN_EMAIL=admin@example.com \
SESSION_KEY=$(openssl rand -hex 32) \
ENV=production \
BASE_URL=http://localhost:8080 \
GOOGLE_CLIENT_ID=fake \
GOOGLE_CLIENT_SECRET=fake \
  go run ./cmd/app &
sleep 1

# /health is open
curl -sf http://localhost:8080/health | head -c 200; echo
# Protected page redirects
curl -sI http://localhost:8080/strategies | grep -i 'location'
# /auth/google/login responds (will try to redirect to Google with our fake creds — that's OK)
curl -sI http://localhost:8080/auth/google/login | head -3

kill %1
```

Expected: `/health` returns 200 JSON; `/strategies` redirects to `/auth/google/login`; `/auth/google/login` returns 307 toward Google.

- [ ] **Step 4: Push and confirm CI is green**

```bash
git push -u origin mauv0809/auth-hardening
```

The CI workflow runs migrations against an ephemeral Postgres 18 service, so the new integration tests will exercise the full path. Confirm all five jobs pass.

- [ ] **Step 5: Open PR**

```bash
gh pr create --base main \
  --title "Auth, roles & CSRF hardening" \
  --body "Implements docs/superpowers/plans/2026-05-07-auth-roles-and-hardening.md.

- Migrations 014 (users/auth_identities/sessions) and 015 (created_by columns)
- internal/users repository + integration tests
- internal/auth: scs sessions, RequireAuth/RequireAdmin middlewares, Google OAuth handlers
- Initial-admin upsert from INITIAL_ADMIN_EMAIL on startup
- --add-user / --disable-user CLI flags
- created_by attribution on strategies + strategy_runs
- Echo CSRF middleware on state-changing routes; HTMX picks up the token from a meta tag
- Integration tests for OAuth callback bookkeeping

Builds on #1 (deployment & observability)."
```

Expected: PR opens, CI passes, ready for review.
