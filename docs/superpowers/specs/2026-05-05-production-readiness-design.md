# DeepValue Production Readiness — Design

**Date:** 2026-05-05
**Status:** Draft for review
**Owner:** mauv0809

## Goal

Take DeepValue from a working local-dev MVP to a state where it can be deployed and run continuously on a Hetzner VPS, accessed by a small set of trusted users (admin-provisioned, no public signup), with enough operational hygiene to survive day-to-day use without surprises.

Out of scope: high availability, public multi-tenancy, formal SLOs, off-VPS database backups (deferred), domain/DNS setup (handled manually by the operator).

## Constraints and decisions made during brainstorming

| Topic | Decision |
|---|---|
| Hosting | Single Hetzner VPS, all services managed by Coolify |
| Deployment trigger | Push to `main` → Coolify auto-builds and deploys from git |
| Database | Coolify-managed Postgres on the same VPS |
| Backups | None at this time (accepted risk) |
| Auth | Google OAuth only (initially); no public signup; admin provisions users |
| User isolation | Not required now, but `created_by` attribution is required so isolation can be added later as a one-line filter |
| Roles | Two tiers: regular user and admin (`is_admin` boolean) |
| Observability | Structured JSON logs always; optional Sentry/GlitchTip via `SENTRY_DSN` env var |
| External services | Avoid where possible — prefer self-hosted libs and Coolify-managed services |
| Domain / TLS | Out of scope (operator handles DNS, Coolify auto-issues Let's Encrypt) |

## Architecture

```
                                      Hetzner VPS
                  ┌──────────────────────────────────────────┐
                  │  Coolify (manages all of below)          │
                  │                                          │
   user ── DNS ──▶│  Caddy (Coolify-managed, TLS)            │
                  │     │                                    │
                  │     ▼                                    │
                  │  deepvalue app  (Go binary in Docker)    │
                  │     │                                    │
                  │     ├──▶  Postgres (Coolify-managed)     │
                  │     │                                    │
                  │     ├──▶  Google OAuth (external)        │
                  │     ├──▶  Nasdaq Data Link (external)    │
                  │     ├──▶  Tiingo (external)              │
                  │     │                                    │
                  │     └──▶  GlitchTip [optional]           │
                  │             (Coolify-managed, same VPS)  │
                  └──────────────────────────────────────────┘
```

**Deploy flow:** PR → GitHub Actions (lint, fresh-files check, tests, build) → branch protection requires green → merge to `main` → Coolify webhook fires → Coolify pulls, builds Dockerfile, deploys with zero-downtime container swap. Env vars and secrets live in Coolify's project settings.

## 1. Containerization

Generated files (templ `.go`, Tailwind `output.css`, swagger docs) remain committed to git. The Docker build only needs Go; CI guarantees no drift.

### Dockerfile (multi-stage, distroless, non-root)

```dockerfile
# ---- build stage ----
FROM golang:1.24-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_SHA=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux \
    go build \
      -ldflags="-s -w -X main.buildSHA=${BUILD_SHA} -X main.buildTime=${BUILD_TIME}" \
      -o /out/app ./cmd/app

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /out/app /app/app
COPY --from=builder /src/assets /app/assets

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/app"]
```

### .dockerignore

```
.git
.github
node_modules
bin
tmp
pgdata
.env
.env.local
*.md
docker-compose.yml
docs/superpowers
```

### Coolify configuration

- Build pack: **Dockerfile**
- Dockerfile path: `Dockerfile`
- Healthcheck: HTTP GET `http://localhost:8080/health`, expect 200
- Restart policy: `unless-stopped`
- Resource limits: tune in Coolify UI; not specified in code

## 2. CI/CD pipeline

Single workflow: `.github/workflows/ci.yml`. Triggers on PRs targeting `main` and pushes to `main`.

### Jobs

| Job | Steps | Failure means |
|---|---|---|
| `lint` | `go vet ./...`, `golangci-lint run` (config in `.golangci.yml`, lean ruleset) | Code style/correctness issue |
| `generated-go-fresh` | `go install templ`, `go install swag`, `templ generate`, `swag init -g cmd/app/main.go --parseDependency --parseInternal`, `git diff --exit-code` | Committed `.templ.go` or swagger docs are stale relative to source |
| `generated-css-fresh` | `npm ci`, `npm run css:build`, `git diff --exit-code -- assets/css/output.css` | Committed Tailwind output is stale |
| `test` | Spin up Postgres 18 service container, run `go test ./...` with `DATABASE_URL` pointed at it | Test or migration failure |
| `build` | `go build -o /tmp/app ./cmd/app` | Compile failure |

All jobs use `actions/setup-go@v5` with `cache: true` and the standard module cache.

### Branch protection on `main`

- Require pull request before merging
- Require all 5 jobs above to pass
- Require branch to be up to date before merge
- Require linear history (squash or rebase merges)

### Coolify hookup

- Coolify "Auto Deploy" enabled, watching `main`
- Webhook secret stored in Coolify (one-time setup)
- On webhook fire: Coolify pulls, builds the Dockerfile, deploys
- On build failure inside Coolify: previous container keeps running (no downtime)

### Tool version pinning

The `generated-*-fresh` jobs work by re-running code generators and asserting `git diff` is empty. They will produce false-positive failures if a local developer ran a generator with a different version than CI uses. To prevent drift:

- **Go tooling versions are declared in a single source of truth: `tools.go`** (a build-tagged file at the repo root) listing `_ "github.com/a-h/templ/cmd/templ"` and `_ "github.com/swaggo/swag/cmd/swag"`. Versions are then locked in `go.mod` like any other dependency. CI installs them via `go install <tool>@<version-from-go.mod>` (using `go run` or `go install` against the module-pinned version), and the `Makefile`'s existing `tools` target is updated to use the same pinned versions.
- **Node tooling versions are pinned via `package.json`** + `package-lock.json`. CI uses `npm ci` (already in the spec) which respects the lock file. Tailwind, postcss, and any plugins are locked there.
- **The README gains a short "Tooling versions" subsection** documenting how to update a tool: bump in `tools.go` (or `package.json`), regenerate, commit. Single workflow for both contributors and CI.

Effect: CI and local both `go install` (or `npm ci`) the same exact version, so generator output is byte-identical. The drift mode is closed.

### Out of scope (deliberately)

No deploy step in GitHub Actions. No Docker build/push from CI. No release tagging or changelog automation. No security scanning workflow (Dependabot can be enabled via GitHub UI without a workflow file).

## 3. Authentication, users, and roles

### Libraries

- `github.com/markbates/goth` + `github.com/markbates/goth/providers/google` — OAuth flow
- `github.com/alexedwards/scs/v2` + `github.com/alexedwards/scs/postgresstore` — server-side sessions, Postgres-backed
- (Future, if password is added) `golang.org/x/crypto/bcrypt`

### Schema (one Goose migration)

```sql
-- 014_auth.sql

-- Canonical user identity
CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    name            TEXT,
    is_admin        BOOLEAN NOT NULL DEFAULT FALSE,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at   TIMESTAMPTZ
);

-- One row per (user, auth method). One user can have many.
CREATE TABLE auth_identities (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider     TEXT NOT NULL,        -- 'google', later: 'password', 'magiclink'
    provider_id  TEXT NOT NULL,        -- Google's stable subject ID; for password rows: email
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_id)
);
CREATE INDEX auth_identities_user_id_idx ON auth_identities (user_id);

-- scs/postgresstore standard schema
CREATE TABLE sessions (
    token  TEXT PRIMARY KEY,
    data   BYTEA NOT NULL,
    expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- Seed initial admin from env (idempotent).
-- The migration reads INITIAL_ADMIN_EMAIL at runtime via Goose's Go migration
-- (not pure SQL) so the value is not baked into the migration file.
```

The seeding step is implemented as a Go migration (Goose supports both SQL and Go migrations), reading `INITIAL_ADMIN_EMAIL` from the environment and inserting `(email, is_admin=true, is_active=true)` with `ON CONFLICT (email) DO NOTHING`. Re-running is safe.

### Login flow

1. Unauthenticated request to a protected route → middleware redirects to `/auth/google/login`
2. `goth` redirects to Google's consent screen
3. Google redirects back to `/auth/google/callback`
4. Callback handler:
   a. `goth` verifies the response, returns `(email, name, provider_id)`
   b. Lookup `auth_identities WHERE provider='google' AND provider_id=?`
      - **Found:** load the user. If `is_active=false`, render a 403 page reading "Your account is disabled. Contact the administrator." Stop.
      - **Not found:** lookup `users WHERE email=?`
        - **Found:** the admin pre-created this user by email. Insert a new `auth_identities` row linking this user to Google. Continue.
        - **Not found:** render a 403 page reading "This Google account is not authorized. Contact the administrator." Stop.
   c. Update `users.last_login_at = NOW()`
   d. Put `user_id` into the scs session
   e. Redirect to `/` (or to the originally-requested URL, captured pre-login)

### Middleware

Two Echo middlewares applied as global except where exempt:

- `RequireAuth` — reads `user_id` from the scs session, loads the user, attaches it to `echo.Context`. If session missing or user `!is_active`, redirects to `/auth/google/login` (for HTML requests) or returns 401 JSON (for API/HTMX requests detected via `Accept` or `HX-Request` header).
- `RequireAdmin` — runs after `RequireAuth`. Returns 403 if the loaded user has `!is_admin`.

**Exemptions from `RequireAuth`:** `/auth/*`, `/health`, `/assets/*`.

**`RequireAdmin` applied additionally to:** `/admin/*` and `/admin/ingest/*`.

### Admin user provisioning

Two supported mechanisms; both insert directly into `users`. The `auth_identities` row is created on the user's first successful Google login (the email-based linking step above).

1. **Initial admin via env var.** On startup, after migrations, if `INITIAL_ADMIN_EMAIL` is set, upsert a user row with that email, `is_admin=true`, `is_active=true`. Idempotent.
2. **CLI flag.** `./app --add-user EMAIL [--admin]` inserts a user row and exits 0. Run via Coolify's "Run a command in container" feature, or as a one-shot container.

Removing or disabling a user: a CLI flag `--disable-user EMAIL` flips `is_active=false`. Their next request fails the auth middleware. (Existing sessions are not invalidated immediately because scs lookups don't re-check `is_active`; however the middleware re-loads the user from `users` on each request, so the flag takes effect on the next request.)

### Attribution: `created_by`

Add `created_by BIGINT REFERENCES users(id)` columns to:

- `strategies` — user-authored strategy definitions
- `portfolio` — current portfolio holdings (note: singular, per existing schema)
- `strategy_runs` — backtest invocations are user-triggered actions worth attributing

The remaining tables (`companies`, `financial_metrics`, `daily_prices`, `benchmarks`, `benchmark_prices`, `sp500_membership`) hold ingested reference data, not user-authored content, and do not need `created_by`.

For pre-existing rows: backfill with the initial admin's `id` during the same migration that adds the column (the initial admin is guaranteed to exist by then because the seed step in the auth migration runs first; we enforce that ordering by giving the auth migration a lower number than the `created_by` migration). New inserts in handlers must populate `created_by` from the authenticated user's `id`.

Reads remain global for now (every authenticated user sees every strategy and portfolio). When isolation is later required, the change is `WHERE created_by = $current_user_id` in the relevant queries plus an admin override path.

### Out-of-band setup (one-time, manual)

1. In Google Cloud Console: create a project, create an OAuth 2.0 Client ID (Web application).
2. Authorized redirect URI: `https://<your-domain>/auth/google/callback`.
3. Copy Client ID and Client Secret into Coolify env vars (`GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`).
4. Set `INITIAL_ADMIN_EMAIL` in Coolify to the operator's email before the first deploy.

## 4. Observability

### Logging

`log/slog` (stdlib) configured at startup:

```go
var handler slog.Handler
opts := &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("LOG_LEVEL"))}
if env == "production" {
    handler = slog.NewJSONHandler(os.Stdout, opts)
} else {
    handler = slog.NewTextHandler(os.Stdout, opts)
}
slog.SetDefault(slog.New(handler))
```

- `LOG_LEVEL` env var, default `info`. Coolify UI can flip to `debug` without a deploy.
- Echo's request logger swapped for `middleware.RequestLoggerWithConfig`, emitting JSON via slog. Each request log includes: `method`, `path`, `status`, `latency_ms`, `request_id`, `user_id` (when authenticated).
- A request-scoped `*slog.Logger` is attached to `echo.Context`; handlers access it via a helper and add fields with `.With(...)`. Request ID flows through automatically.
- Existing `log.Println` / `log.Printf` calls are replaced with `slog` calls during implementation.

### Optional error tracking

`github.com/getsentry/sentry-go`. Initialized only when `SENTRY_DSN` is non-empty:

```go
if dsn := os.Getenv("SENTRY_DSN"); dsn != "" {
    sentry.Init(sentry.ClientOptions{
        Dsn:              dsn,
        Environment:      env,
        Release:          buildSHA,
        TracesSampleRate: 0,
    })
    defer sentry.Flush(2 * time.Second)
}
```

- Echo error middleware: when a handler returns an error, log via slog AND, if Sentry is initialized, call `sentry.CaptureException` with user ID, request ID, and route as tags.
- GlitchTip is wire-compatible with Sentry's protocol. To enable: deploy GlitchTip as another Coolify service on the same VPS, copy its DSN into the app's `SENTRY_DSN`, redeploy. No code change.
- When `SENTRY_DSN` is empty: behavior is identical to today (just the slog output).

### Out of scope

No metrics export (Prometheus / StatsD). No distributed tracing. No external log aggregation. Coolify's resource and log UIs are sufficient at this scale.

## 5. Operational hardening

### Graceful shutdown

`main` runs `e.Start(addr)` in a goroutine and blocks on `signal.Notify` for `SIGINT` and `SIGTERM`. On signal: `e.Shutdown(ctx)` with a 10-second deadline, then close the pgx pool, then exit. Required for clean Coolify redeploys (every push triggers a redeploy).

### Healthcheck that actually checks

`/health` becomes:

```go
func (h *Handler) Health(c echo.Context) error {
    ctx, cancel := context.WithTimeout(c.Request().Context(), 1*time.Second)
    defer cancel()
    if err := h.pool.Ping(ctx); err != nil {
        return c.JSON(503, map[string]any{
            "status":     "unhealthy",
            "build_sha":  buildSHA,
            "build_time": buildTime,
            "error":      "database unreachable",
        })
    }
    return c.JSON(200, map[string]any{
        "status":     "ok",
        "build_sha":  buildSHA,
        "build_time": buildTime,
    })
}
```

Coolify's container healthcheck calls this; an unhealthy container is restarted automatically.

### Echo production middleware additions

- `e.HideBanner = true`, `e.HidePort = true`
- `middleware.RequestID()` (used by the logging layer)
- `middleware.Secure()` (default security headers)
- `middleware.BodyLimit("1M")`
- `middleware.CSRFWithConfig` on state-changing routes (POST/PUT/DELETE on `/strategies`, `/portfolios`, `/admin/*`); HTMX picks up the token from a meta tag added to the layout template
- `middleware.Recover()` is already present — keep, ensure it logs panics via slog and reports to Sentry

### Migration safety on startup

Goose continues to run on app boot. Changes:

- Log each migration applied (name + duration) via slog at `info` level.
- If any migration errors, log at `error` level and exit non-zero before starting the HTTP server. Container will restart; if the failure is persistent, Coolify will mark unhealthy and stop redeploying after retries.
- Migrations run before OAuth provider registration (Google credentials are read after migrations succeed, which means a missing `users` table cannot poison auth setup).

### Build metadata

Build with:

```
-ldflags="-s -w \
  -X main.buildSHA=$BUILD_SHA \
  -X main.buildTime=$BUILD_TIME"
```

Surfaces in `/health` JSON and as Sentry's `Release` tag. `BUILD_SHA` and `BUILD_TIME` are passed as Docker build args; Coolify supplies the git SHA automatically.

### Tests added as part of this work

Bare minimum for the new code paths, not a coverage push:

- Integration test for OAuth callback flow against an ephemeral Postgres (mock the `goth` user response, assert user creation, identity linking, session establishment).
- Test for `RequireAuth` middleware (no session → redirect; session for inactive user → 401/redirect; valid session → handler called with user in context).
- Test for `RequireAdmin` middleware (non-admin → 403; admin → handler called).
- Test for `/health` returning 503 when the DB is unreachable (use a closed pool).

## 6. Environment variables (canonical list)

Maintained in `.env.example`. Used by the app via `os.Getenv` (godotenv in dev, Coolify-injected in prod).

| Var | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | yes | — | Postgres DSN; Coolify auto-injects when app is linked to its DB service |
| `PORT` | no | `8080` | HTTP listen port |
| `BASE_URL` | yes (prod) | — | Public URL of the app, used for OAuth redirect URIs and absolute links |
| `ENV` | no | `development` | `development` or `production`; switches log handler |
| `LOG_LEVEL` | no | `info` | `debug` / `info` / `warn` / `error` |
| `SESSION_KEY` | yes (prod) | — | 32-byte random string used by `goth`'s gothic store for the OAuth state cookie. (scs uses opaque random session tokens and does not require its own signing key.) |
| `GOOGLE_CLIENT_ID` | yes | — | Google OAuth client ID |
| `GOOGLE_CLIENT_SECRET` | yes | — | Google OAuth client secret |
| `INITIAL_ADMIN_EMAIL` | yes (first deploy) | — | Seeds the first admin row on startup |
| `NASDAQ_API_KEY` | yes for ingest | — | Nasdaq Data Link / Sharadar |
| `TIINGO_API_KEY` | yes for ingest | — | Tiingo ETF prices |
| `SENTRY_DSN` | no | — | If set, errors flow to Sentry/GlitchTip |

## 7. Implementation order (rough)

The plan skill will produce the actual sequenced steps. As a sanity check that these pieces fit together:

1. Dockerfile + .dockerignore + graceful shutdown + real /health (deployable scaffold)
2. CI workflow + branch protection (so all later PRs are gated)
3. Structured logging migration (slog) + request ID + log middleware
4. Optional Sentry wiring
5. Auth schema migration + goth + scs + login/callback/logout routes
6. RequireAuth + RequireAdmin middleware, applied to existing routes
7. `created_by` columns + handler updates to populate them
8. CLI user-management flags + initial-admin seeding
9. Echo hardening middlewares (Secure, BodyLimit, CSRF)
10. Tests for the new paths
11. Update `.env.example` and a short `DEPLOYMENT.md` for the operator

## 8. Risks and accepted compromises

- **No backups.** Operator-accepted. Postgres data lives only on the VPS volume. Loss of the VPS = loss of all data. Re-ingesting Sharadar fundamentals is possible but slow and consumes API quota.
- **Single VPS, no redundancy.** Standard for this scale; not addressed.
- **Sessions not invalidated on disable.** When `is_active` is flipped to false, existing sessions die on the next request (because middleware re-loads the user). A session that never makes another request technically remains valid until expiry. Acceptable for trusted-user model.
- **scs sessions stored in Postgres.** A DB outage takes down auth. Same DB the app already depends on, so blast radius is unchanged.
- **OAuth redirect URI is environment-specific.** Local dev (`http://localhost:8080/auth/google/callback`) and prod must both be registered in Google Cloud Console. Standard OAuth pain.
