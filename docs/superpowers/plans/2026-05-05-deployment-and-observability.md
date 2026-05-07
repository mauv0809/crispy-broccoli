# Deployment & Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take DeepValue from a local-dev MVP to a containerized Go service that builds in CI, deploys to Coolify on Hetzner, runs reliably (graceful shutdown, real health probe), and emits structured JSON logs with optional Sentry/GlitchTip error tracking.

**Architecture:** Multi-stage Dockerfile (distroless, non-root) → Coolify-managed app + Postgres → GitHub Actions CI gates merges to `main` → branch protection + Coolify watching `main` for auto-deploy. Logging via stdlib `log/slog`; error tracking via `getsentry/sentry-go`, both conditional on env vars so the same binary runs unchanged in dev and prod.

**Tech Stack:** Go 1.24, Echo v4, pgx v5, Goose, `log/slog` (stdlib), `getsentry/sentry-go`, GitHub Actions, Coolify, Docker, distroless `static-debian12:nonroot`.

**Spec:** `docs/superpowers/specs/2026-05-05-production-readiness-design.md` — Sections 1, 2, 4, 5, and 6 (env vars). Sections 3 (auth) and 7 (impl order beyond #4) are **out of scope for this plan** and covered by the separate "Auth, Roles, & Hardening" plan that builds on this one.

---

## File Structure

**New files**:
- `Dockerfile` — multi-stage build, distroless runtime
- `.dockerignore` — keep build context lean
- `tools.go` — module-pinned versions of templ + swag CLI tools
- `internal/observability/logging.go` — slog handler setup, level parsing
- `internal/observability/sentry.go` — conditional Sentry initialization
- `internal/observability/middleware.go` — Echo middleware for structured request logging + per-request logger attachment
- `internal/observability/context.go` — helpers to attach/retrieve `*slog.Logger` and request ID on `echo.Context`
- `internal/observability/logging_test.go` — tests for logger config
- `internal/observability/middleware_test.go` — tests for middleware behavior
- `internal/handlers/handlers_test.go` — covers the new Health endpoint
- `.github/workflows/ci.yml` — pipeline definition
- `DEPLOYMENT.md` — operator-facing deployment notes (Coolify setup, env vars, OAuth registration deferred to Plan 2)

**Modified files**:
- `cmd/app/main.go` — add slog/Sentry init, graceful shutdown, build metadata vars, swap logger middleware, fail-fast on migration errors
- `internal/handlers/handlers.go` — `Health` checks DB and returns build metadata
- `internal/handlers/handlers.go` — `Handler` struct gets a `*pgxpool.Pool` (currently empty)
- `internal/db/migrate.go` — add structured logging of migrations (optional, low priority)
- `Makefile` — `tools` target uses pinned versions; new `docker-build` target
- `.env.example` — adds `BASE_URL`, `ENV`, `LOG_LEVEL`, `SENTRY_DSN`
- `README.md` — adds "Tooling versions" section
- `package.json` — no change needed (already has Tailwind locked); `package-lock.json` already exists

**Untouched packages** (logging migration applies but no structural changes): `internal/ingest`, `internal/strategy`, `internal/db`.

---

## Task 1: Add Dockerfile and .dockerignore

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`

- [ ] **Step 1: Write the Dockerfile**

Create `Dockerfile` at repo root:

```dockerfile
# ---- build stage ----
FROM golang:1.24-alpine AS builder
WORKDIR /src

# Cache deps separately for fast rebuilds
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

- [ ] **Step 2: Write .dockerignore**

Create `.dockerignore` at repo root:

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

- [ ] **Step 3: Verify the build succeeds locally**

Run: `docker build --build-arg BUILD_SHA=test --build-arg BUILD_TIME=test -t deepvalue:test .`

Expected: build completes; final image around 20-30 MB. Confirm with `docker images deepvalue:test`.

- [ ] **Step 4: Verify the binary starts inside the container**

Run: `docker run --rm -e DATABASE_URL=postgres://invalid -e PORT=8080 -p 8080:8080 deepvalue:test 2>&1 | head -20`

Expected: app starts and either fails on DB connect (current behavior — that's fine for now; we change it in later tasks) or starts the HTTP listener. Important point: no "command not found" / no permission errors / runs as `nonroot`. Confirm with `docker run --rm --entrypoint id deepvalue:test` → `uid=65532(nonroot)`.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile .dockerignore
git commit -m "feat: add multi-stage Dockerfile with distroless runtime

Builds a static Go binary; final image runs as nonroot on distroless
base. Build args BUILD_SHA and BUILD_TIME injected via ldflags into
main.buildSHA / main.buildTime (consumed by /health and Sentry release
tag in subsequent tasks)."
```

---

## Task 2: Add build metadata variables to main package

**Files:**
- Modify: `cmd/app/main.go` (top of file)

- [ ] **Step 1: Add the build metadata variables**

Edit `cmd/app/main.go`. Add after the imports block (between `import (...)` and the `// @title` swagger comment):

```go
// Build metadata, populated via -ldflags at build time.
// See Dockerfile for the build args wiring.
var (
	buildSHA  = "dev"
	buildTime = "unknown"
)
```

- [ ] **Step 2: Verify with a quick local build**

Run:
```bash
go build -ldflags="-X main.buildSHA=abc123 -X main.buildTime=2026-01-01" -o /tmp/dv ./cmd/app
/tmp/dv 2>&1 &
PID=$!
sleep 1
curl -s localhost:8080/health
kill $PID
```

Expected: `/health` still returns the existing `{"status":"ok"}` payload (we'll wire build metadata into it in Task 4). The point of this step is just to confirm the binary compiles with the new vars present.

- [ ] **Step 3: Commit**

```bash
git add cmd/app/main.go
git commit -m "feat: add buildSHA/buildTime vars for ldflags injection"
```

---

## Task 3: Make Handler aware of the database pool

The current `Handler` struct is empty. To check DB health from `/health`, the handler needs access to the pool.

**Files:**
- Modify: `internal/handlers/handlers.go`
- Modify: `cmd/app/main.go` (where `handlers.New()` is called)

- [ ] **Step 1: Update the Handler struct and constructor**

Edit `internal/handlers/handlers.go`. Replace the `Handler` struct and `New` function:

```go
package handlers

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/mauv0809/crispy-broccoli/internal/views"
)

type Handler struct {
	pool *pgxpool.Pool
}

// New constructs a Handler. pool may be nil — the Health handler
// reports unhealthy if the pool is nil or the ping fails.
func New(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}
```

(Leave the existing `Health`, `Index`, `Docs` methods alone for now; we update `Health` in Task 4.)

- [ ] **Step 2: Update the call site**

Edit `cmd/app/main.go`. Find the line `h := handlers.New()` and change to:

```go
h := handlers.New(pool)
```

The variable `pool` already exists in `main` (line ~47 in current main.go). When DB connect fails, `pool` is `nil`; that's fine — the Health handler will treat nil-pool as unhealthy in Task 4.

- [ ] **Step 3: Verify it compiles**

Run: `go build ./...`

Expected: success, no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/handlers/handlers.go cmd/app/main.go
git commit -m "refactor: thread pgxpool through Handler for upcoming health check"
```

---

## Task 4: Health endpoint pings DB and returns build metadata

**Files:**
- Modify: `internal/handlers/handlers.go`
- Create: `internal/handlers/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/handlers/handlers_test.go`:

```go
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

func TestHealth_NilPool_Returns503(t *testing.T) {
	e := echo.New()
	h := New(nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.Health(c); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if body["status"] != "unhealthy" {
		t.Errorf(`body["status"] = %v, want "unhealthy"`, body["status"])
	}
	if _, ok := body["build_sha"]; !ok {
		t.Errorf("body missing build_sha")
	}
}

func TestHealth_ClosedPool_Returns503(t *testing.T) {
	// A pool that has been closed will fail Ping.
	cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	pool.Close()

	e := echo.New()
	h := New(pool)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.Health(c); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/handlers/ -run TestHealth -v`

Expected: FAIL — current `Health` returns 200 unconditionally.

- [ ] **Step 3: Implement the new Health handler**

Edit `internal/handlers/handlers.go`. Add an exported package-level reference to the build metadata so handlers can read it:

Replace the existing `Health` method:

```go
// BuildSHA and BuildTime are populated from main at startup.
// They mirror the ldflags-injected values; main calls SetBuildInfo.
var (
	BuildSHA  = "dev"
	BuildTime = "unknown"
)

// SetBuildInfo lets cmd/app/main wire build metadata into the handlers package.
func SetBuildInfo(sha, ts string) {
	BuildSHA = sha
	BuildTime = ts
}

// Health returns 200 when the database is reachable, 503 otherwise.
// @Summary Health check
// @Description Returns the health status of the application
// @Tags system
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 503 {object} map[string]any
// @Router /health [get]
func (h *Handler) Health(c echo.Context) error {
	resp := map[string]any{
		"build_sha":  BuildSHA,
		"build_time": BuildTime,
	}
	if h.pool == nil {
		resp["status"] = "unhealthy"
		resp["error"] = "database pool not initialized"
		return c.JSON(http.StatusServiceUnavailable, resp)
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 1*time.Second)
	defer cancel()
	if err := h.pool.Ping(ctx); err != nil {
		resp["status"] = "unhealthy"
		resp["error"] = "database unreachable"
		return c.JSON(http.StatusServiceUnavailable, resp)
	}
	resp["status"] = "ok"
	return c.JSON(http.StatusOK, resp)
}
```

Add `"context"` and `"time"` to the import block of `internal/handlers/handlers.go`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/handlers/ -run TestHealth -v`

Expected: PASS for both `TestHealth_NilPool_Returns503` and `TestHealth_ClosedPool_Returns503`.

- [ ] **Step 5: Wire build metadata from main into handlers**

Edit `cmd/app/main.go`. Find where `h := handlers.New(pool)` lives, and add the line directly above it:

```go
handlers.SetBuildInfo(buildSHA, buildTime)
h := handlers.New(pool)
```

- [ ] **Step 6: Verify the full build still compiles and existing tests pass**

Run: `go build ./... && go test ./...`

Expected: both succeed.

- [ ] **Step 7: Commit**

```bash
git add internal/handlers/ cmd/app/main.go
git commit -m "feat: /health pings DB and reports build metadata

Returns 503 when pool is nil or Ping fails; response always carries
build_sha and build_time so deployments can be identified from any
running container. Coolify's container healthcheck consumes this."
```

---

## Task 5: Fail fast on migration errors

The current `main.go` logs a warning when migrations fail and continues startup. For a production-bound app this should abort startup so a broken schema doesn't run.

**Files:**
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Replace the migration call site**

Edit `cmd/app/main.go`. Find:

```go
// Run migrations
if err := db.RunMigrations(databaseURL); err != nil {
    log.Printf("Warning: Could not run migrations: %v", err)
} else {
    log.Println("Migrations completed")
}
```

Replace with:

```go
// Run migrations. Fail fast on error: an HTTP server with a broken
// or stale schema is worse than a restart loop.
if err := db.RunMigrations(databaseURL); err != nil {
    log.Fatalf("migrations failed: %v", err)
}
log.Println("Migrations completed")
```

Also replace the DB connect block immediately below it:

```go
// Connect to database
pool, err := db.Connect(ctx, databaseURL)
if err != nil {
    log.Printf("Warning: Could not connect to database: %v", err)
    log.Println("Continuing without database connection...")
} else {
    defer pool.Close()
    log.Println("Connected to database")
}
```

with:

```go
// Connect to database. Fail fast: same reasoning as migrations.
pool, err := db.Connect(ctx, databaseURL)
if err != nil {
    log.Fatalf("database connect failed: %v", err)
}
defer pool.Close()
log.Println("Connected to database")
```

This means the conditional `if pool != nil` blocks below become always-true. Simplify by removing the `if pool != nil {` outer wrapper around the strategy/ingest/repo setup. Keep the inner `if nasdaqAPIKey != "" {` and `if tiingoAPIKey != "" {` checks since those API keys may legitimately be unset in dev.

Concretely: take everything currently under `if pool != nil { ... }` and dedent it one level. The block ends just before `// Static files`.

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`

Expected: success.

- [ ] **Step 3: Verify the existing handler tests still pass**

Run: `go test ./...`

Expected: PASS (the handler tests use a nil pool and don't go through main).

- [ ] **Step 4: Commit**

```bash
git add cmd/app/main.go
git commit -m "feat: fail fast on migration or DB connect failure

Previously, main logged a warning and continued without a DB; that
hides bad deploys. With Coolify's restart-on-unhealthy policy, exiting
non-zero gives clearer signal and avoids serving requests against a
broken schema."
```

---

## Task 6: Graceful shutdown on SIGINT/SIGTERM

**Files:**
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Replace the server start block**

Edit `cmd/app/main.go`. Find the bottom of `main`:

```go
log.Printf("Starting server on :%s", port)
if err := e.Start(":" + port); err != nil {
    log.Fatalf("Failed to start server: %v", err)
}
```

Replace with:

```go
// Start server in a goroutine so we can listen for shutdown signals.
go func() {
    log.Printf("Starting server on :%s", port)
    if err := e.Start(":" + port); err != nil && err != http.ErrServerClosed {
        log.Fatalf("server error: %v", err)
    }
}()

// Wait for SIGINT or SIGTERM. Coolify sends SIGTERM on redeploy.
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
<-sigCh
log.Println("shutdown signal received, draining...")

shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := e.Shutdown(shutdownCtx); err != nil {
    log.Printf("graceful shutdown error: %v", err)
}
log.Println("server stopped")
```

Add to the import block of `cmd/app/main.go`:

```go
"net/http"
"os/signal"
"syscall"
"time"
```

(`context` and `os` are already imported.)

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`

Expected: success.

- [ ] **Step 3: Smoke-test the shutdown path manually**

In one terminal:
```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app
```
(Make sure `make db-up` has been run.)

In another terminal:
```bash
curl -s localhost:8080/health
kill -TERM $(pgrep -f "go run ./cmd/app" | tail -1)
```

Expected logs from the server include `shutdown signal received, draining...` and `server stopped`, then exit 0. If you see `failed to start server` instead, the signal arrived before the listener bound — retry with a small `sleep 1` between starting the server and sending the signal.

- [ ] **Step 4: Commit**

```bash
git add cmd/app/main.go
git commit -m "feat: graceful shutdown on SIGINT/SIGTERM

Drains in-flight requests with a 10s deadline before exiting. Required
for clean Coolify redeploys (every push to main triggers one)."
```

---

## Task 7: Scaffold the observability package with slog setup

**Files:**
- Create: `internal/observability/logging.go`
- Create: `internal/observability/logging_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/observability/logging_test.go`:

```go
package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"garbage", slog.LevelInfo}, // unknown values default to info
	}
	for _, tc := range cases {
		if got := ParseLevel(tc.in); got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNewLogger_ProductionEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(Config{Env: "production", Level: slog.LevelInfo, Output: &buf})
	logger.Info("hello", "k", "v")

	line := strings.TrimSpace(buf.String())
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("expected JSON output, got %q: %v", line, err)
	}
	if fields["msg"] != "hello" {
		t.Errorf(`msg = %v, want "hello"`, fields["msg"])
	}
	if fields["k"] != "v" {
		t.Errorf(`k = %v, want "v"`, fields["k"])
	}
}

func TestNewLogger_DevelopmentEmitsText(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(Config{Env: "development", Level: slog.LevelInfo, Output: &buf})
	logger.Info("hello", "k", "v")

	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Errorf("expected key=value text format, got %q", out)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/observability/ -v`

Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement logging.go**

Create `internal/observability/logging.go`:

```go
package observability

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config controls how slog is wired at startup.
type Config struct {
	Env    string    // "production" → JSON handler; anything else → text handler
	Level  slog.Level
	Output io.Writer // nil defaults to os.Stdout
}

// ParseLevel maps a string (typically from LOG_LEVEL env var) to a slog level.
// Unknown values default to LevelInfo. Empty string also returns LevelInfo.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger returns a configured *slog.Logger. In production emits JSON;
// otherwise emits human-readable text.
func NewLogger(cfg Config) *slog.Logger {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}
	opts := &slog.HandlerOptions{Level: cfg.Level}
	var handler slog.Handler
	if cfg.Env == "production" {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	return slog.New(handler)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/observability/ -v`

Expected: all three tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/observability/
git commit -m "feat: add observability package with slog config helpers

NewLogger picks JSON vs text handler based on env; ParseLevel maps
LOG_LEVEL string to slog.Level. main wires these in a later task."
```

---

## Task 8: Request-scoped logger and request ID middleware

**Files:**
- Create: `internal/observability/context.go`
- Create: `internal/observability/middleware.go`
- Create: `internal/observability/middleware_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/observability/middleware_test.go`:

```go
package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestRequestLogger_EmitsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(Config{Env: "production", Level: slog.LevelInfo, Output: &buf})

	e := echo.New()
	e.Use(RequestIDMiddleware())
	e.Use(RequestLoggerMiddleware(logger))
	e.GET("/x", func(c echo.Context) error {
		// Inside the handler, the request-scoped logger should be on the context
		// and should already have request_id attached.
		l := LoggerFromContext(c)
		if l == nil {
			t.Fatal("LoggerFromContext returned nil")
		}
		l.Info("inside handler")
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	// Expect at least two JSON lines: the handler's "inside handler" and the
	// access log line emitted by the middleware after the handler returns.
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected >=2 log lines, got %d: %q", len(lines), buf.String())
	}

	var inside, access map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &inside); err != nil {
		t.Fatalf("inside line not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &access); err != nil {
		t.Fatalf("access line not JSON: %v", err)
	}

	rid, ok := inside["request_id"].(string)
	if !ok || rid == "" {
		t.Errorf("inside log missing non-empty request_id: %v", inside["request_id"])
	}
	if access["request_id"] != rid {
		t.Errorf("access log request_id = %v, want %v", access["request_id"], rid)
	}
	if access["method"] != "GET" {
		t.Errorf("access log method = %v, want GET", access["method"])
	}
	if access["path"] != "/x" {
		t.Errorf("access log path = %v, want /x", access["path"])
	}
	if got, ok := access["status"].(float64); !ok || int(got) != 200 {
		t.Errorf("access log status = %v, want 200", access["status"])
	}
}

func TestRequestIDMiddleware_RespectsIncomingHeader(t *testing.T) {
	e := echo.New()
	e.Use(RequestIDMiddleware())
	e.GET("/x", func(c echo.Context) error {
		return c.String(http.StatusOK, RequestIDFromContext(c))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Body.String() != "abc-123" {
		t.Errorf("request id: got %q, want %q", rec.Body.String(), "abc-123")
	}
	if got := rec.Header().Get("X-Request-Id"); got != "abc-123" {
		t.Errorf("response X-Request-Id: got %q, want %q", got, "abc-123")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/observability/ -run TestRequestLogger -v` and `go test ./internal/observability/ -run TestRequestIDMiddleware -v`

Expected: FAIL — `RequestLoggerMiddleware`, `RequestIDMiddleware`, `LoggerFromContext`, `RequestIDFromContext` do not exist.

- [ ] **Step 3: Implement context.go**

Create `internal/observability/context.go`:

```go
package observability

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/labstack/echo/v4"
)

const (
	contextKeyLogger    = "obs.logger"
	contextKeyRequestID = "obs.request_id"
	headerRequestID     = "X-Request-Id"
)

// LoggerFromContext returns the request-scoped logger placed by
// RequestLoggerMiddleware. Returns nil if none was set.
func LoggerFromContext(c echo.Context) *slog.Logger {
	v := c.Get(contextKeyLogger)
	if l, ok := v.(*slog.Logger); ok {
		return l
	}
	return nil
}

// RequestIDFromContext returns the request ID set by RequestIDMiddleware.
// Empty string if none was set.
func RequestIDFromContext(c echo.Context) string {
	v := c.Get(contextKeyRequestID)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
```

- [ ] **Step 4: Implement middleware.go**

Create `internal/observability/middleware.go`:

```go
package observability

import (
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"
)

// RequestIDMiddleware reads the X-Request-Id incoming header (if present)
// or generates a fresh one, stashes it on the echo.Context, and echoes
// it back on the response. Other middlewares and handlers can retrieve
// it via RequestIDFromContext.
func RequestIDMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			rid := c.Request().Header.Get(headerRequestID)
			if rid == "" {
				rid = newRequestID()
			}
			c.Set(contextKeyRequestID, rid)
			c.Response().Header().Set(headerRequestID, rid)
			return next(c)
		}
	}
}

// RequestLoggerMiddleware attaches a request-scoped *slog.Logger
// (already enriched with request_id) to echo.Context and emits an
// access log line after the handler returns. Must run after
// RequestIDMiddleware so the request ID is available.
func RequestLoggerMiddleware(base *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			rid := RequestIDFromContext(c)
			scoped := base.With("request_id", rid)
			c.Set(contextKeyLogger, scoped)

			start := time.Now()
			err := next(c)
			latencyMs := time.Since(start).Milliseconds()

			status := c.Response().Status

			fields := []any{
				"method", c.Request().Method,
				"path", c.Request().URL.Path,
				"status", status,
				"latency_ms", latencyMs,
				"request_id", rid,
			}
			if err != nil {
				fields = append(fields, "error", err.Error())
				base.Error("http_request", fields...)
			} else if status >= 500 {
				base.Error("http_request", fields...)
			} else if status >= 400 {
				base.Warn("http_request", fields...)
			} else {
				base.Info("http_request", fields...)
			}
			return err
		}
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/observability/ -v`

Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/observability/
git commit -m "feat: request ID + structured request logger middleware

RequestIDMiddleware reuses incoming X-Request-Id or generates one;
RequestLoggerMiddleware attaches a *slog.Logger pre-enriched with
request_id to echo.Context and emits an access line per request.
Together they give every log line correlatable to a single request."
```

---

## Task 9: Wire slog and request middleware into main

**Files:**
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Replace logging setup in main**

Edit `cmd/app/main.go`. The strategy: initialize `slog.Default()` very early, swap Echo's existing logger middleware for our structured ones, and replace existing `log.Println` / `log.Printf` calls with `slog` equivalents in this file.

In the import block, add:

```go
"log/slog"

"github.com/mauv0809/crispy-broccoli/internal/observability"
```

Right after `func main() {` and before the `godotenv.Load` call, insert:

```go
env := os.Getenv("ENV")
if env == "" {
    env = "development"
}
logger := observability.NewLogger(observability.Config{
    Env:   env,
    Level: observability.ParseLevel(os.Getenv("LOG_LEVEL")),
})
slog.SetDefault(logger)
```

Then replace the existing Echo middleware setup. Find:

```go
e := echo.New()
e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
    LogStatus:   true,
    LogURI:      true,
    LogError:    true,
    HandleError: true,
    LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
        if v.Error == nil {
            log.Printf("%d %s", v.Status, v.URI)
        } else {
            log.Printf("%d %s - %v", v.Status, v.URI, v.Error)
        }
        return nil
    },
}))
e.Use(middleware.Recover())
```

Replace with:

```go
e := echo.New()
e.HideBanner = true
e.HidePort = true
e.Use(observability.RequestIDMiddleware())
e.Use(observability.RequestLoggerMiddleware(logger))
e.Use(middleware.Recover())
```

The `middleware.Recover` call still uses Echo's default panic recovery. That logs panics through Echo's default logger (stderr). Acceptable for now — moving the recovery middleware to use slog is a separate, optional polish. For this plan, leave Recover as-is.

- [ ] **Step 2: Replace remaining log.Println/log.Printf in main.go with slog**

Still in `cmd/app/main.go`, replace each remaining call:

| Old | New |
|---|---|
| `log.Println("No .env file found, using environment variables")` | `slog.Info("no .env file found, using environment variables")` |
| `log.Fatal("DATABASE_URL environment variable is required")` | `slog.Error("DATABASE_URL environment variable is required"); os.Exit(1)` |
| `log.Fatalf("migrations failed: %v", err)` | `slog.Error("migrations failed", "error", err); os.Exit(1)` |
| `log.Println("Migrations completed")` | `slog.Info("migrations completed")` |
| `log.Fatalf("database connect failed: %v", err)` | `slog.Error("database connect failed", "error", err); os.Exit(1)` |
| `log.Println("Connected to database")` | `slog.Info("database connected")` |
| `log.Println("Nasdaq Data Link client initialized (SEP for equity prices)")` | `slog.Info("nasdaq client initialized")` |
| `log.Println("Warning: NASDAQ_API_KEY not set, Nasdaq data endpoints disabled")` | `slog.Warn("NASDAQ_API_KEY not set; nasdaq endpoints disabled")` |
| `log.Println("Tiingo client initialized (for ETF benchmarks and stock prices)")` | `slog.Info("tiingo client initialized")` |
| `log.Println("Warning: TIINGO_API_KEY not set, ETF benchmark comparison disabled")` | `slog.Warn("TIINGO_API_KEY not set; benchmark comparison disabled")` |
| `log.Println("Strategy engine initialized")` | `slog.Info("strategy engine initialized")` |
| `log.Printf("Warning: failed to seed default strategies: %v", err)` | `slog.Warn("failed to seed default strategies", "error", err)` |
| `log.Println("Strategy endpoints registered")` | `slog.Info("strategy endpoints registered")` |
| `log.Println("Ingestion endpoints registered")` | `slog.Info("ingestion endpoints registered")` |
| `log.Printf("Starting server on :%s", port)` | `slog.Info("starting server", "port", port)` |
| `log.Fatalf("server error: %v", err)` | `slog.Error("server error", "error", err); os.Exit(1)` |
| `log.Println("shutdown signal received, draining...")` | `slog.Info("shutdown signal received; draining")` |
| `log.Printf("graceful shutdown error: %v", err)` | `slog.Error("graceful shutdown error", "error", err)` |
| `log.Println("server stopped")` | `slog.Info("server stopped")` |

Remove `"log"` from the import block once all references are gone.

- [ ] **Step 3: Verify it builds and tests still pass**

Run: `go build ./... && go test ./...`

Expected: success.

- [ ] **Step 4: Smoke-test the new logger**

Run:
```bash
ENV=production LOG_LEVEL=info DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -10
```

Expected: each line is JSON, e.g. `{"time":"...","level":"INFO","msg":"strategy engine initialized"}`. Press Ctrl-C to exit.

Then with `ENV` unset:
```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -10
```

Expected: human-readable text format like `time=... level=INFO msg="strategy engine initialized"`.

- [ ] **Step 5: Commit**

```bash
git add cmd/app/main.go
git commit -m "feat: wire slog default + structured request logging in main

ENV controls JSON vs text handler; LOG_LEVEL controls verbosity.
Replaces Echo's default request logger with one that emits structured
access lines including request_id."
```

---

## Task 10: Migrate log.* calls in other packages to slog

The `internal/handlers`, `internal/ingest`, `internal/strategy`, and `internal/db` packages still use stdlib `log`. Switch them to `slog.Default()` so all output goes through the same handler.

**Files:**
- Modify: any `.go` file in `internal/` that imports `"log"` and calls `log.Println` / `log.Printf` / `log.Fatal*`

- [ ] **Step 1: Find all stdlib log usages**

Run:
```bash
grep -rn '"log"' internal/ | grep -v _test.go
grep -rn 'log\.\(Print\|Fatal\)' internal/ | grep -v _test.go
```

Expected: a list of files. Note them.

- [ ] **Step 2: Replace mechanically**

For each file in the list above:

- Replace `import "log"` with `import "log/slog"`.
- Replace `log.Printf("foo: %v", bar)` with `slog.Info("foo", "value", bar)` (or `slog.Error` / `slog.Warn` based on the message intent — "Warning:" prefix → `Warn`; "Error:" or unrecoverable → `Error`).
- Replace `log.Println("foo")` with `slog.Info("foo")`.
- Replace `log.Fatalf("foo: %v", err)` with `slog.Error("foo", "error", err); os.Exit(1)` (only if `Fatalf` was actually intended — many of these in non-main code should probably bubble errors up; for this pass, keep behavior identical).

For loops/inner code where you need contextual fields, use `slog.With(...)` to build a child logger first.

This is mostly mechanical; if a file's logging is verbose enough that line-by-line edits are awkward, add a brief comment at the top: `// Logging uses slog.Default(); see internal/observability for handler config.`

- [ ] **Step 3: Verify builds and tests pass**

Run: `go build ./... && go test ./...`

Expected: success. Existing tests don't assert log content (other than the observability tests, which are unaffected).

- [ ] **Step 4: Verify no stdlib log imports remain in non-test files**

Run:
```bash
grep -rn '"log"$' internal/ cmd/ | grep -v _test.go || echo "all clear"
```

Expected: `all clear`.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "refactor: replace stdlib log with slog across internal packages

All output now flows through the slog handler configured in main,
giving uniform JSON/text formatting and a single LOG_LEVEL knob."
```

---

## Task 11: Optional Sentry/GlitchTip integration

**Files:**
- Create: `internal/observability/sentry.go`
- Modify: `cmd/app/main.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/getsentry/sentry-go@latest`

Expected: `go.mod` and `go.sum` updated.

- [ ] **Step 2: Implement sentry.go**

Create `internal/observability/sentry.go`:

```go
package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v4"
)

// InitSentry initializes the global Sentry client when dsn is non-empty.
// Returns a cleanup function that should be deferred from main; the
// cleanup is a no-op when Sentry was not initialized.
func InitSentry(dsn, env, release string) (cleanup func(), enabled bool) {
	if dsn == "" {
		return func() {}, false
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      env,
		Release:          release,
		TracesSampleRate: 0,
	})
	if err != nil {
		slog.Error("sentry init failed; continuing without error tracking", "error", err)
		return func() {}, false
	}
	slog.Info("sentry initialized", "environment", env, "release", release)
	return func() {
		sentry.Flush(2 * time.Second)
	}, true
}

// SentryErrorMiddleware reports handler errors to Sentry when it is
// enabled. Safe to add unconditionally — when Sentry is not initialized,
// CaptureException is a no-op.
//
// Must run before any other middleware that swallows errors.
func SentryErrorMiddleware(enabled bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			if err == nil || !enabled {
				return err
			}
			hub := sentry.CurrentHub().Clone()
			hub.Scope().SetTag("request_id", RequestIDFromContext(c))
			hub.Scope().SetTag("path", c.Request().URL.Path)
			hub.Scope().SetTag("method", c.Request().Method)
			hub.CaptureException(err)
			return err
		}
	}
}

// CaptureContextError reports an error from background code that is
// not in an HTTP handler (e.g. a long-running job). No-op when Sentry
// is not initialized.
func CaptureContextError(_ context.Context, err error) {
	if err == nil {
		return
	}
	sentry.CaptureException(err)
}
```

- [ ] **Step 3: Wire it into main**

Edit `cmd/app/main.go`. After the `slog.SetDefault(logger)` line and before `godotenv.Load()` is called (or after, doesn't matter — Sentry doesn't depend on env files but does depend on env vars being readable, which they always are), add:

```go
sentryCleanup, sentryEnabled := observability.InitSentry(
    os.Getenv("SENTRY_DSN"),
    env,
    buildSHA,
)
defer sentryCleanup()
```

Then where the middlewares are wired (after `e.Use(observability.RequestLoggerMiddleware(logger))`), add:

```go
e.Use(observability.SentryErrorMiddleware(sentryEnabled))
```

Order matters: SentryErrorMiddleware must be **after** RequestIDMiddleware so it can tag the event with request_id, and **after** RequestLoggerMiddleware so the access log still records the error. The existing `middleware.Recover()` should remain last among the cross-cutting middlewares.

- [ ] **Step 4: Smoke-test with Sentry disabled**

Run:
```bash
unset SENTRY_DSN
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -5
```

Expected: app starts; no `sentry initialized` log line.

- [ ] **Step 5: Smoke-test with Sentry "enabled" (fake DSN, just confirm no crash)**

Run:
```bash
SENTRY_DSN="https://public@example.invalid/1" \
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  go run ./cmd/app 2>&1 | head -10
```

Expected: app starts; `sentry initialized` line appears. Errors will fail to actually deliver because the host is invalid, but Init succeeds and the app keeps running. Press Ctrl-C.

- [ ] **Step 6: Run all tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/observability/sentry.go cmd/app/main.go go.mod go.sum
git commit -m "feat: optional Sentry/GlitchTip error reporting

InitSentry is a no-op when SENTRY_DSN is unset. The middleware tags
events with request_id, path, and method. Compatible with self-hosted
GlitchTip (wire-compatible with Sentry's protocol)."
```

---

## Task 12: Pin Go tool versions via tools.go

**Files:**
- Create: `tools.go`
- Modify: `Makefile`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Create tools.go**

Create `tools.go` at repo root:

```go
//go:build tools

// Package tools tracks build-time CLI dependencies so their versions are
// locked in go.mod. They are not imported by the application; the build
// tag ensures they are excluded from compiled binaries.
//
// To install or update these tools locally, run `make tools`.
package tools

import (
	_ "github.com/a-h/templ/cmd/templ"
	_ "github.com/swaggo/swag/cmd/swag"
)
```

- [ ] **Step 2: Pin the tools in go.mod**

Run:
```bash
go get github.com/a-h/templ/cmd/templ@v0.3.960
go get github.com/swaggo/swag/cmd/swag@v1.16.6
go mod tidy
```

(The versions above match what's currently in `go.mod`'s require/indirect blocks; adjust if the maintainer wants a newer version. The point is they're pinned.)

Expected: `go.mod` shows direct requires for `github.com/a-h/templ` and `github.com/swaggo/swag` (no longer indirect).

- [ ] **Step 3: Update Makefile tools target to use pinned versions**

Edit `Makefile`. Replace the existing `tools:` target:

```makefile
tools:
	go install github.com/a-h/templ/cmd/templ@latest
	go install github.com/pressly/goose/v3/cmd/goose@latest
	go install github.com/air-verse/air@latest
	go install github.com/swaggo/swag/cmd/swag@latest
```

with:

```makefile
# Install dev tools at the versions pinned in go.mod (templ, swag) plus
# floaters that aren't imported anywhere (goose, air). Bump pinned
# versions by editing tools.go + running `go get`.
tools:
	go install github.com/a-h/templ/cmd/templ
	go install github.com/swaggo/swag/cmd/swag
	go install github.com/pressly/goose/v3/cmd/goose@latest
	go install github.com/air-verse/air@latest
```

(`go install <pkg>` without `@version` uses the version from `go.mod`. `goose` and `air` are dev-only floaters, kept on `@latest` since they're not in tools.go and not pinned.)

- [ ] **Step 4: Verify the tools install successfully**

Run: `make tools`

Expected: succeeds; `templ version` and `swag --version` produce the expected versions.

- [ ] **Step 5: Verify generators still produce identical output**

Run:
```bash
templ generate
swag init -g cmd/app/main.go --parseDependency --parseInternal
git diff
```

Expected: no diff (the committed generated files match what the pinned tools produce).

- [ ] **Step 6: Commit**

```bash
git add tools.go go.mod go.sum Makefile
git commit -m "build: pin templ and swag CLI versions via tools.go

Locks generator versions in go.mod so CI and local devs run identical
binaries; closes the drift mode that would otherwise make
generated-go-fresh CI checks flaky."
```

---

## Task 13: Add the GitHub Actions CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Create the workflow directory and file**

```bash
mkdir -p .github/workflows
```

Create `.github/workflows/ci.yml`:

```yaml
name: CI

on:
  pull_request:
    branches: [main]
  push:
    branches: [main]

env:
  GO_VERSION: "1.24"
  NODE_VERSION: "22"

jobs:
  lint:
    name: lint
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: true
      - name: go vet
        run: go vet ./...

  generated-go-fresh:
    name: generated-go-fresh
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: true
      - name: install pinned generators
        run: |
          go install github.com/a-h/templ/cmd/templ
          go install github.com/swaggo/swag/cmd/swag
      - name: regenerate
        run: |
          templ generate
          swag init -g cmd/app/main.go --parseDependency --parseInternal
      - name: assert no diff
        run: |
          if ! git diff --exit-code; then
            echo "::error::Committed generated Go files are stale. Run 'make tools && templ generate && swag init -g cmd/app/main.go --parseDependency --parseInternal' locally and commit the result."
            exit 1
          fi

  generated-css-fresh:
    name: generated-css-fresh
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: ${{ env.NODE_VERSION }}
          cache: npm
      - run: npm ci
      - run: npm run css:build
      - name: assert no diff
        run: |
          if ! git diff --exit-code -- assets/css/output.css; then
            echo "::error::Committed Tailwind output is stale. Run 'npm ci && npm run css:build' locally and commit assets/css/output.css."
            exit 1
          fi

  test:
    name: test
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:18-alpine
        env:
          POSTGRES_USER: value_user
          POSTGRES_PASSWORD: value_pass
          POSTGRES_DB: value_db
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U value_user -d value_db"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 10
    env:
      DATABASE_URL: postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: true
      - run: go test ./...

  build:
    name: build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: true
      - name: compile
        run: go build -o /tmp/app ./cmd/app
```

- [ ] **Step 2: Lint the YAML locally**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); print('ok')"`

Expected: `ok`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: add GitHub Actions workflow

Five jobs run on PRs and pushes to main: lint (go vet),
generated-go-fresh (templ + swag drift check), generated-css-fresh
(Tailwind drift check), test (with ephemeral Postgres 18 service),
build (compile smoke test). Branch protection should require all five
to pass before merging to main."
```

- [ ] **Step 4: Push the branch and confirm CI runs**

```bash
git push -u origin mauv0809/prod-readiness
```

Then visit the PR (or the branch view in GitHub) and confirm all five jobs run and pass. If any fail, fix and amend. Once green, the workflow is validated.

(Branch protection setup is a manual GitHub UI action — see DEPLOYMENT.md in Task 15.)

---

## Task 14: Update .env.example

**Files:**
- Modify: `.env.example`

- [ ] **Step 1: Replace .env.example with the canonical list**

Overwrite `.env.example`:

```dotenv
# DeepValue environment configuration.
# In local dev, copy this to .env (loaded by godotenv at startup).
# In production, set these in Coolify's "Environment Variables" UI.

# --- Required ---

# Postgres DSN. In Coolify, link the app to its managed Postgres service
# and Coolify will inject this automatically.
DATABASE_URL="postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable"

# Public URL of the app. Used for OAuth redirect URIs (Plan 2) and any
# absolute-URL building. Example: https://deepvalue.example.com
BASE_URL="http://localhost:8080"

# --- Optional but recommended ---

# Server port (default 8080).
PORT=8080

# "production" → JSON logs. Anything else (including unset) → text logs.
ENV=development

# debug | info | warn | error  (default info)
LOG_LEVEL=info

# --- External APIs (required for ingest features) ---

# Nasdaq Data Link / Sharadar SF1 fundamentals + SEP equity prices
NASDAQ_API_KEY="your-nasdaq-api-key"

# Tiingo ETF benchmark prices (SPY, QQQ, ...)
# Free tier: 1000 req/day, 50 req/hour
TIINGO_API_KEY="your-tiingo-api-key"

# --- Optional error tracking ---

# Set to a Sentry or self-hosted GlitchTip DSN to enable error reporting.
# Leave empty to disable (default).
SENTRY_DSN=""
```

- [ ] **Step 2: Commit**

```bash
git add .env.example
git commit -m "docs: refresh .env.example with all production env vars

Documents BASE_URL, ENV, LOG_LEVEL, SENTRY_DSN added in this branch,
plus reorganizes by required vs optional. .env.example doubles as the
canonical list of variables to set in Coolify."
```

---

## Task 15: Add DEPLOYMENT.md and README "Tooling versions" section

**Files:**
- Create: `DEPLOYMENT.md`
- Modify: `README.md`

- [ ] **Step 1: Write DEPLOYMENT.md**

Create `DEPLOYMENT.md` at repo root:

````markdown
# Deployment

DeepValue runs as a single Docker container behind Coolify on a Hetzner VPS,
with a Coolify-managed Postgres on the same host.

## One-time setup (operator)

### 1. Postgres

In Coolify:
1. Project → New Resource → **PostgreSQL**, version 18.
2. Note the auto-generated credentials (username, password, database name).
3. Save.

### 2. App service

In Coolify:
1. Project → New Resource → **Public Repository** (or private with SSH key).
2. URL: `https://github.com/<org>/<repo>`.
3. Branch: `main`.
4. Build pack: **Dockerfile**.
5. Dockerfile path: `Dockerfile`.
6. Port: `8080`.
7. Healthcheck: `GET /health` expecting status 200.
8. **Link this app to the Postgres service** so Coolify injects `DATABASE_URL`
   automatically. (Alternative: copy the DSN into env vars manually.)

### 3. Environment variables

In the Coolify UI for the app service, add the variables from `.env.example`.
See that file for the canonical list and meanings.

Mark these as **secret** (toggle in the UI):
- `NASDAQ_API_KEY`
- `TIINGO_API_KEY`
- `SENTRY_DSN`
- `GOOGLE_CLIENT_SECRET` (Plan 2 — auth)
- `SESSION_KEY` (Plan 2 — auth)

Generate `SESSION_KEY` (Plan 2 dependency, but easy to set up now):
```bash
openssl rand -hex 32
```

### 4. DNS + TLS

1. Point an A record for `deepvalue.<your-domain>` at the VPS IP.
2. In Coolify, set the app's domain. Coolify auto-issues a Let's Encrypt cert.

### 5. GitHub branch protection

In GitHub repo settings → Branches → Add rule for `main`:
- ☑ Require a pull request before merging
- ☑ Require status checks to pass:
  - `lint`
  - `generated-go-fresh`
  - `generated-css-fresh`
  - `test`
  - `build`
- ☑ Require branches to be up to date before merging
- ☑ Require linear history (squash or rebase merges only)

### 6. Coolify auto-deploy

In the app service settings → Source → enable **Auto Deploy** with the webhook
secret Coolify provides. Coolify will redeploy on every push to `main`.

## Runtime

### Logs

In Coolify's UI, the app's "Logs" tab shows stdout. Lines are JSON when
`ENV=production`. Filter by `request_id`, `level`, or any other field.

### Healthcheck

`GET /health` returns:
- `200 {"status":"ok","build_sha":"…","build_time":"…"}` when DB is reachable
- `503 {"status":"unhealthy","error":"…"}` when the DB is down or pool is nil

Coolify's container healthcheck consumes this; an unhealthy container is
restarted automatically.

### Graceful shutdown

The app listens for SIGTERM (Coolify sends this on redeploy) and drains
in-flight requests with a 10s deadline before exiting.

### Error tracking

Errors flow to Sentry/GlitchTip when `SENTRY_DSN` is set. To enable later:
1. Either sign up for Sentry (cloud) or deploy GlitchTip as another Coolify
   service on the same VPS.
2. Copy the DSN into the app's `SENTRY_DSN` env var.
3. Redeploy. No code change required.

## Backups

Not configured at this time (accepted risk). Postgres data lives only on the
VPS volume. To enable later, configure Coolify's built-in S3 backup on the
Postgres service.

## Disaster recovery

If the VPS is lost:
1. Spin up a fresh VPS, install Coolify.
2. Re-create the Postgres and app services per "One-time setup" above.
3. Re-ingest fundamentals via the admin endpoints (slow, consumes API quota).
4. Strategies and portfolio rows are lost unless backups are configured.
````

- [ ] **Step 2: Append "Tooling versions" section to README.md**

Edit `README.md`. Append at the end:

```markdown
## Tooling versions

Code generators (`templ`, `swag`) are pinned via `tools.go` + `go.mod`.
CSS tooling (`tailwindcss`) is pinned via `package.json` + `package-lock.json`.
Both CI and local development must use the pinned versions, otherwise the
`generated-go-fresh` and `generated-css-fresh` CI jobs will fail.

To bump a generator version:

```bash
# Go tool (templ or swag):
go get github.com/a-h/templ/cmd/templ@<new-version>
make tools                      # installs the new version locally
templ generate                  # regenerate
git add tools.go go.mod go.sum internal/views/*_templ.go
git commit -m "chore: bump templ to <new-version>"

# Tailwind:
npm install tailwindcss@<new-version>
npm run css:build
git add package.json package-lock.json assets/css/output.css
git commit -m "chore: bump tailwindcss to <new-version>"
```

To install the locked versions on a fresh checkout:

```bash
make setup        # runs npm install + make tools
```
```

- [ ] **Step 3: Commit**

```bash
git add DEPLOYMENT.md README.md
git commit -m "docs: add DEPLOYMENT.md and tooling-versions guide

DEPLOYMENT.md is the operator runbook for Coolify setup, env vars,
branch protection, and runtime expectations. README gains a section
documenting how to bump pinned generator versions."
```

---

## Final verification

- [ ] **Step 1: Full test suite**

Run: `go test ./... -race`

Expected: PASS.

- [ ] **Step 2: Full local build**

Run: `make build` (this runs `templ generate`, `css:build`, `swag`, then `go build`)

Expected: produces `bin/app` without error; `bin/app -h` (if implemented) or just startup smoke runs.

- [ ] **Step 3: Docker build with realistic args**

Run:
```bash
docker build \
  --build-arg BUILD_SHA=$(git rev-parse --short HEAD) \
  --build-arg BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t deepvalue:plan1 .
```

Expected: builds clean.

- [ ] **Step 4: End-to-end smoke**

```bash
docker run --rm -d --name dv-smoke \
  --network host \
  -e DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable \
  -e ENV=production \
  -e LOG_LEVEL=info \
  deepvalue:plan1
sleep 2
curl -s http://localhost:8080/health
docker logs dv-smoke
docker stop dv-smoke
```

Expected:
- `/health` returns 200 with `status: ok` and a `build_sha` matching the git SHA used at build time
- Logs are JSON
- `docker stop` triggers a clean drain (`shutdown signal received; draining` line in logs) within 10 seconds

- [ ] **Step 5: Push and confirm CI is green**

```bash
git push
```

Visit the branch on GitHub and confirm all five CI jobs pass.

- [ ] **Step 6: Open PR**

```bash
gh pr create --base main --title "Deployment & observability" --body "Implements docs/superpowers/plans/2026-05-05-deployment-and-observability.md. Adds Dockerfile, graceful shutdown, real /health, slog with structured request logging, optional Sentry, CI workflow with tool version pinning, and DEPLOYMENT.md. Plan 2 (auth) builds on this."
```

Expected: PR opens, CI runs and passes, ready for review.
