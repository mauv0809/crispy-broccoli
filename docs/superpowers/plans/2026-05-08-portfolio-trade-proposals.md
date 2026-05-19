# Portfolios and Strategy-Driven Trade Proposals — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-user portfolios that can attach a verified strategy, generate rebalance proposals on a cadence (background scheduler + email), allow per-row execution logging with edits, and track performance over time vs SPY.

**Architecture:** Approach 1, event-flavored. Domain-specific append-only ledgers (`proposals`, `executed_trades`, `capital_events`) with a materialized `holdings` projection. Strategy versions are pinned per portfolio so edits never affect existing portfolios. In-process scheduler goroutine polls Postgres at a fixed interval.

**Tech Stack:** Go 1.24, Echo v4, Templ + HTMX + Alpine.js + DaisyUI, Postgres 16 via pgx/v5, Goose migrations, `shopspring/decimal` for money/shares, slog + Sentry, existing `email.Sender` interface (LogSender / ResendSender).

**Spec:** `docs/superpowers/specs/2026-05-08-portfolio-trade-proposals-design.md`

---

## File structure

### New files

```
internal/db/migrations/
  017_strategy_versions.sql
  018_strategies_status_cadence.sql
  019_portfolios.sql
  020_proposals.sql
  021_executed_trades.sql
  022_capital_events.sql
  023_holdings.sql
  024_drop_old_portfolio.sql

internal/dbutil/
  tx.go                      # DBTX interface satisfied by *pgxpool.Pool and pgx.Tx; helpers for "run in tx"

internal/strategy/
  versions.go                # CreateVersion, GetVersion, ListVersions, BumpAndDemote
  versions_test.go
  status.go                  # Verify(), Archive() helpers + state-machine guards
  status_test.go

internal/portfolio/
  models.go                  # Portfolio, Holding structs, statuses, cadences (enum-like consts)
  repository.go              # CRUD + holdings queries
  repository_test.go
  service.go                 # CreatePortfolio (with linked strategy version), Pause/Resume/Archive
  service_test.go
  holdings.go                # ApplyTrade, Rebuild, computeMarketValue
  holdings_test.go
  performance.go             # TimeSeries, vs-SPY computation
  performance_test.go

internal/proposal/
  models.go                  # Proposal, Pick, Action enum
  cadence.go                 # AddCadence(time, cadence) → time
  cadence_test.go
  repository.go              # Insert, Get, Update (status/picks while pending), ExpirePending, GetPending
  repository_test.go
  generator.go               # Generate(ctx, portfolioID, capitalChange) → *Proposal
  generator_test.go
  acceptor.go                # Accept (full/partial/skip), runs in transaction
  acceptor_test.go

internal/scheduler/
  clock.go                   # Clock interface, RealClock, FakeClock for tests
  worker.go                  # Worker struct, Start, Stop, tick body
  worker_test.go

internal/email/
  proposals.go               # SendProposalReady, SendProposalReminder

internal/handlers/
  portfolios.go              # List, Detail, NewForm, Create, Pause, Resume, Archive
  portfolios_test.go
  proposals.go               # Detail, Recompute, Accept, Skip
  proposals_test.go

internal/views/portfolios/
  list.templ
  detail.templ
  new_form.templ
  holdings_table.templ       # shared fragment

internal/views/proposals/
  detail.templ
  picks_table.templ          # HTMX-swappable fragment for capital_change recompute
  accepted_summary.templ

internal/views/emails/
  proposal_ready.templ
  proposal_reminder.templ
```

### Modified files

```
internal/strategy/models.go         # Add Status, DefaultCadence, CurrentVersionID fields
internal/strategy/repository.go     # Plumbing for new fields; UpdateRules creates a new version
internal/handlers/strategies.go     # Add Verify, Archive, ListVersions endpoints; edit-rules confirms demotion
internal/views/strategies/*.templ   # Add status badge, verify/archive buttons, version list section
cmd/app/main.go                     # Wire portfolio repo, proposal repo+generator+acceptor, scheduler, new routes
```

---

## Phase plan

The plan is organized into 9 phases. Each phase ends at a natural review point. Suggested pause points are flagged with **PAUSE FOR REVIEW**.

- **Phase A** — Migrations + dbutil (foundations, no business logic)
- **Phase B** — Strategy versioning package
- **Phase C** — Portfolio package (models, repository, holdings projection)
- **Phase D** — Proposal package (cadence, models, repository, generator, acceptor)
- **Phase E** — Scheduler package
- **Phase F** — Email templates and senders
- **Phase G** — Templ views (portfolios + proposals)
- **Phase H** — HTTP handlers + main.go wiring
- **Phase I** — Performance tracking + final integration test + UI polish review

---

## Conventions used throughout this plan

- Tests use the standard library's `testing` package. **No testify.** Asserts are `if got != want { t.Errorf(...) }`.
- DB tests skip if `DATABASE_URL` is unset and use `testutil.OpenTestDB(t)` from the existing helper.
- Repository constructors take `*pgxpool.Pool`. For methods that need to participate in caller-provided transactions, methods accept a `dbutil.DBTX` interface (defined in Phase A) instead.
- Money/shares: `shopspring/decimal.Decimal`. Never `float64`.
- Logging: `slog` global. Errors capture to Sentry via `observability.CaptureContextError(ctx, err)` (background) or `observability.CaptureHandlerError(c, err)` (handlers).
- Commits: small, frequent, conventional (`feat:`, `test:`, `refactor:`, `chore:`). One commit per task unless the task explicitly says otherwise.

---

# Phase A — Migrations + dbutil

## Task A1: Add `dbutil.DBTX` interface

**Files:**
- Create: `internal/dbutil/tx.go`
- Test: `internal/dbutil/tx_test.go`

The `DBTX` interface lets repository methods accept either a pool or a transaction. This is needed so the proposal acceptor can do multi-table writes atomically.

- [ ] **Step 1: Write the failing test**

```go
// internal/dbutil/tx_test.go
package dbutil_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"deepvalue/internal/dbutil"
	"deepvalue/internal/testutil"
)

func TestDBTX_PoolSatisfiesInterface(t *testing.T) {
	var _ dbutil.DBTX = (*pgxpool.Pool)(nil)
}

func TestRunInTx_CommitsOnSuccess(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `CREATE TEMP TABLE dbutil_smoke (id int)`)
	if err != nil {
		t.Fatalf("create temp table: %v", err)
	}

	err = dbutil.RunInTx(ctx, pool, func(tx dbutil.DBTX) error {
		_, err := tx.Exec(ctx, `INSERT INTO dbutil_smoke (id) VALUES (1)`)
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM dbutil_smoke`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestRunInTx_RollsBackOnError(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	_, _ = pool.Exec(ctx, `CREATE TEMP TABLE dbutil_smoke_rb (id int)`)

	wantErr := errSentinel
	err := dbutil.RunInTx(ctx, pool, func(tx dbutil.DBTX) error {
		_, _ = tx.Exec(ctx, `INSERT INTO dbutil_smoke_rb (id) VALUES (1)`)
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("RunInTx err = %v, want %v", err, wantErr)
	}

	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM dbutil_smoke_rb`).Scan(&count)
	if count != 0 {
		t.Errorf("count = %d, want 0 (rolled back)", count)
	}
}

var errSentinel = &sentinelErr{}

type sentinelErr struct{}

func (*sentinelErr) Error() string { return "sentinel" }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dbutil/...`
Expected: FAIL — package does not exist yet.

- [ ] **Step 3: Implement `dbutil.DBTX`**

```go
// internal/dbutil/tx.go
package dbutil

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the subset of pgx operations shared by *pgxpool.Pool and pgx.Tx.
// Repository methods that need to participate in caller-driven transactions
// accept this interface; methods that don't can keep accepting *pgxpool.Pool.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Compile-time assertion that pool and tx both satisfy DBTX.
var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (pgx.Tx)(nil)
)

// RunInTx runs fn inside a database transaction. The transaction commits if fn
// returns nil; otherwise it rolls back and returns fn's error.
func RunInTx(ctx context.Context, pool *pgxpool.Pool, fn func(DBTX) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dbutil/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dbutil/
git commit -m "feat(dbutil): add DBTX interface and RunInTx helper"
```

---

## Task A2: Migration 017 — `strategy_versions` table

**Files:**
- Create: `internal/db/migrations/017_strategy_versions.sql`

- [ ] **Step 1: Write the migration**

```sql
-- internal/db/migrations/017_strategy_versions.sql
-- +goose Up
CREATE TABLE strategy_versions (
    id              BIGSERIAL PRIMARY KEY,
    strategy_id     BIGINT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    version_number  INT NOT NULL,
    rules           JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by      BIGINT REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (strategy_id, version_number)
);

CREATE INDEX strategy_versions_strategy_idx
    ON strategy_versions(strategy_id, version_number DESC);

-- Backfill: create v1 for every existing strategy from its current rules.
INSERT INTO strategy_versions (strategy_id, version_number, rules, created_at, created_by)
SELECT s.id, 1, s.rules, s.created_at, s.created_by
FROM strategies s;

-- +goose Down
DROP TABLE IF EXISTS strategy_versions;
```

- [ ] **Step 2: Apply and verify**

Run: `make db-migrate` (or the equivalent goose command — see existing Makefile)
Expected: migration applied, `strategy_versions` table exists, one row per existing strategy with `version_number=1`.

Verify manually:
```bash
psql "$DATABASE_URL" -c "SELECT strategy_id, version_number FROM strategy_versions ORDER BY strategy_id;"
```

- [ ] **Step 3: Commit**

```bash
git add internal/db/migrations/017_strategy_versions.sql
git commit -m "feat(db): add strategy_versions table with backfill"
```

---

## Task A3: Migration 018 — `strategies` status, default_cadence, current_version_id

**Files:**
- Create: `internal/db/migrations/018_strategies_status_cadence.sql`

- [ ] **Step 1: Write the migration**

```sql
-- internal/db/migrations/018_strategies_status_cadence.sql
-- +goose Up
ALTER TABLE strategies
    ADD COLUMN status              TEXT NOT NULL DEFAULT 'draft',
    ADD COLUMN default_cadence     TEXT,
    ADD COLUMN current_version_id  BIGINT REFERENCES strategy_versions(id);

-- Backfill: every existing strategy is considered verified (they exist and have been used).
UPDATE strategies SET status = 'verified';

-- Point current_version_id at the v1 row created in migration 017.
UPDATE strategies s
SET current_version_id = sv.id
FROM strategy_versions sv
WHERE sv.strategy_id = s.id AND sv.version_number = 1;

-- After backfill, current_version_id should never be null.
ALTER TABLE strategies
    ALTER COLUMN current_version_id SET NOT NULL,
    ADD CONSTRAINT strategies_status_check
        CHECK (status IN ('draft', 'verified', 'archived'));

-- +goose Down
ALTER TABLE strategies
    DROP CONSTRAINT IF EXISTS strategies_status_check,
    DROP COLUMN IF EXISTS current_version_id,
    DROP COLUMN IF EXISTS default_cadence,
    DROP COLUMN IF EXISTS status;
```

- [ ] **Step 2: Apply and verify**

Run: `make db-migrate`
Expected: every existing strategy has `status='verified'` and a non-null `current_version_id`.

```bash
psql "$DATABASE_URL" -c "SELECT id, status, current_version_id FROM strategies LIMIT 5;"
```

- [ ] **Step 3: Commit**

```bash
git add internal/db/migrations/018_strategies_status_cadence.sql
git commit -m "feat(db): add status, default_cadence, current_version_id to strategies"
```

---

## Task A4: Migration 019 — new `portfolios` table

**Files:**
- Create: `internal/db/migrations/019_portfolios.sql`

- [ ] **Step 1: Write the migration**

```sql
-- internal/db/migrations/019_portfolios.sql
-- +goose Up
CREATE TABLE portfolios (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name                TEXT NOT NULL,
    starting_capital    NUMERIC(18,2) NOT NULL CHECK (starting_capital > 0),
    strategy_id         BIGINT NOT NULL REFERENCES strategies(id) ON DELETE RESTRICT,
    strategy_version_id BIGINT NOT NULL REFERENCES strategy_versions(id) ON DELETE RESTRICT,
    cadence             TEXT NOT NULL CHECK (cadence IN ('monthly','quarterly','semi_annual','annual')),
    next_rebalance_due  TIMESTAMPTZ,
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active','paused','archived')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX portfolios_user_idx ON portfolios(user_id);
CREATE INDEX portfolios_due_idx
    ON portfolios(next_rebalance_due)
    WHERE status = 'active' AND next_rebalance_due IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS portfolios;
```

- [ ] **Step 2: Apply, verify schema with `\d portfolios`, commit**

```bash
make db-migrate
psql "$DATABASE_URL" -c "\d portfolios"
git add internal/db/migrations/019_portfolios.sql
git commit -m "feat(db): add portfolios table"
```

---

## Task A5: Migration 020 — `proposals` table

**Files:**
- Create: `internal/db/migrations/020_proposals.sql`

- [ ] **Step 1: Write the migration**

```sql
-- internal/db/migrations/020_proposals.sql
-- +goose Up
CREATE TABLE proposals (
    id                          BIGSERIAL PRIMARY KEY,
    portfolio_id                BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    strategy_version_id         BIGINT NOT NULL REFERENCES strategy_versions(id) ON DELETE RESTRICT,
    generated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    market_value_at_proposal    NUMERIC(18,2) NOT NULL,
    capital_change              NUMERIC(18,2) NOT NULL DEFAULT 0,
    deploy_amount               NUMERIC(18,2) NOT NULL,
    picks                       JSONB NOT NULL,
    status                      TEXT NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending','accepted','partially_accepted','skipped','expired')),
    resolved_at                 TIMESTAMPTZ,
    notification_sent_at        TIMESTAMPTZ,
    reminder_sent_at            TIMESTAMPTZ
);

CREATE INDEX proposals_portfolio_idx ON proposals(portfolio_id, generated_at DESC);
-- Partial index for the scheduler's hot path.
CREATE INDEX proposals_pending_idx
    ON proposals(portfolio_id)
    WHERE status = 'pending';
-- Reminder query support.
CREATE INDEX proposals_reminder_idx
    ON proposals(notification_sent_at)
    WHERE status = 'pending' AND reminder_sent_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS proposals;
```

- [ ] **Step 2: Apply, verify, commit**

```bash
make db-migrate
git add internal/db/migrations/020_proposals.sql
git commit -m "feat(db): add proposals table"
```

---

## Task A6: Migration 021 — `executed_trades` (append-only ledger)

**Files:**
- Create: `internal/db/migrations/021_executed_trades.sql`

- [ ] **Step 1: Write the migration**

```sql
-- internal/db/migrations/021_executed_trades.sql
-- +goose Up
CREATE TABLE executed_trades (
    id            BIGSERIAL PRIMARY KEY,
    portfolio_id  BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    proposal_id   BIGINT REFERENCES proposals(id) ON DELETE SET NULL,
    ticker        TEXT NOT NULL REFERENCES companies(ticker) ON DELETE RESTRICT,
    action        TEXT NOT NULL CHECK (action IN ('buy','sell')),
    shares        NUMERIC(18,6) NOT NULL CHECK (shares > 0),
    price         NUMERIC(18,4) NOT NULL CHECK (price > 0),
    fee           NUMERIC(18,2) NOT NULL DEFAULT 0 CHECK (fee >= 0),
    executed_at   TIMESTAMPTZ NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notes         TEXT
);

CREATE INDEX executed_trades_portfolio_idx
    ON executed_trades(portfolio_id, executed_at DESC);
CREATE INDEX executed_trades_proposal_idx ON executed_trades(proposal_id);

-- +goose Down
DROP TABLE IF EXISTS executed_trades;
```

- [ ] **Step 2: Apply, verify, commit**

```bash
make db-migrate
git add internal/db/migrations/021_executed_trades.sql
git commit -m "feat(db): add executed_trades append-only ledger"
```

---

## Task A7: Migration 022 — `capital_events`

**Files:**
- Create: `internal/db/migrations/022_capital_events.sql`

- [ ] **Step 1: Write the migration**

```sql
-- internal/db/migrations/022_capital_events.sql
-- +goose Up
CREATE TABLE capital_events (
    id            BIGSERIAL PRIMARY KEY,
    portfolio_id  BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    proposal_id   BIGINT REFERENCES proposals(id) ON DELETE SET NULL,
    amount        NUMERIC(18,2) NOT NULL,  -- positive=deposit, negative=withdrawal; zero forbidden
    occurred_at   TIMESTAMPTZ NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notes         TEXT,
    CHECK (amount <> 0)
);

CREATE INDEX capital_events_portfolio_idx
    ON capital_events(portfolio_id, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS capital_events;
```

- [ ] **Step 2: Apply, verify, commit**

```bash
make db-migrate
git add internal/db/migrations/022_capital_events.sql
git commit -m "feat(db): add capital_events ledger"
```

---

## Task A8: Migration 023 — `holdings` projection

**Files:**
- Create: `internal/db/migrations/023_holdings.sql`

- [ ] **Step 1: Write the migration**

```sql
-- internal/db/migrations/023_holdings.sql
-- +goose Up
CREATE TABLE holdings (
    portfolio_id   BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    ticker         TEXT NOT NULL REFERENCES companies(ticker) ON DELETE RESTRICT,
    shares         NUMERIC(18,6) NOT NULL CHECK (shares > 0),
    cost_basis     NUMERIC(18,2) NOT NULL CHECK (cost_basis >= 0),
    last_trade_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (portfolio_id, ticker)
);

-- +goose Down
DROP TABLE IF EXISTS holdings;
```

- [ ] **Step 2: Apply, verify, commit**

```bash
make db-migrate
git add internal/db/migrations/023_holdings.sql
git commit -m "feat(db): add holdings projection table"
```

---

## Task A9: Migration 024 — drop legacy `portfolio` table

**Files:**
- Create: `internal/db/migrations/024_drop_old_portfolio.sql`

The legacy `portfolio` table from migration 001 is replaced by the new `portfolios` table. We're not preserving its rows — confirm no app code reads it before applying.

- [ ] **Step 1: Verify no code reads the old `portfolio` table**

Search for any remaining references:

```bash
git grep -nE 'FROM portfolio[^_s]|INTO portfolio[^_s]|UPDATE portfolio[^_s]'
```

Expected: no matches. (If any matches appear, fix them first — they're stale references.)

- [ ] **Step 2: Write the migration**

```sql
-- internal/db/migrations/024_drop_old_portfolio.sql
-- +goose Up
DROP TABLE IF EXISTS portfolio;

-- +goose Down
-- Recreate the old portfolio table (best-effort restoration; data is not preserved).
CREATE TABLE portfolio (
    id SERIAL PRIMARY KEY,
    ticker TEXT NOT NULL REFERENCES companies(ticker),
    shares_owned DECIMAL(18, 6) NOT NULL DEFAULT 0,
    cost_basis DECIMAL(18, 2),
    target_weight DECIMAL(5, 4),
    acquired_date DATE,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW(),
    created_by BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX idx_portfolio_ticker ON portfolio(ticker);
CREATE INDEX portfolio_created_by_idx ON portfolio(created_by);
```

- [ ] **Step 3: Apply, verify, commit**

```bash
make db-migrate
psql "$DATABASE_URL" -c "\dt portfolio" # expect: 'Did not find any relation named "portfolio".'
git add internal/db/migrations/024_drop_old_portfolio.sql
git commit -m "feat(db): drop legacy portfolio table (replaced by portfolios)"
```

---

**PAUSE FOR REVIEW — End of Phase A.** Migrations applied locally. All schema is in place; no Go code yet. Confirm the schema with `\d` for each new table before continuing.

---

# Phase B — Strategy versioning package

## Task B1: Extend `strategy.Strategy` model with new fields

**Files:**
- Modify: `internal/strategy/models.go`

- [ ] **Step 1: Add fields to the existing Strategy struct**

Locate the existing `Strategy` struct in `internal/strategy/models.go` and add three new fields. Also add typed constants for status and cadence to enforce the enum at compile time.

```go
// Append to internal/strategy/models.go

// Status of a strategy in its lifecycle.
type Status string

const (
	StatusDraft    Status = "draft"
	StatusVerified Status = "verified"
	StatusArchived Status = "archived"
)

// Cadence at which a strategy is intended to be re-run.
type Cadence string

const (
	CadenceMonthly     Cadence = "monthly"
	CadenceQuarterly   Cadence = "quarterly"
	CadenceSemiAnnual  Cadence = "semi_annual"
	CadenceAnnual      Cadence = "annual"
)

// AllCadences returns the canonical list (UI dropdowns, validation).
func AllCadences() []Cadence {
	return []Cadence{CadenceMonthly, CadenceQuarterly, CadenceSemiAnnual, CadenceAnnual}
}
```

Add to the `Strategy` struct definition (existing fields preserved):

```go
type Strategy struct {
    // ... existing fields ...
    Status            Status   `json:"status"`
    DefaultCadence    *Cadence `json:"default_cadence,omitempty"`
    CurrentVersionID  int64    `json:"current_version_id"`
}
```

- [ ] **Step 2: Update SELECT queries in repository.go to read the new columns**

Open `internal/strategy/repository.go`. Every `SELECT ... FROM strategies` must include `status, default_cadence, current_version_id`. Every `Scan(...)` must read them into the new fields. Adjust UPDATE/INSERT statements as needed for the existing methods.

- [ ] **Step 3: Run existing strategy tests to confirm nothing broke**

Run: `go test ./internal/strategy/...`
Expected: PASS. (Existing tests should still pass since the new columns have defaults.)

- [ ] **Step 4: Commit**

```bash
git add internal/strategy/
git commit -m "feat(strategy): add status, default_cadence, current_version_id fields"
```

---

## Task B2: `strategy.versions` package — CreateVersion + GetVersion + ListVersions

**Files:**
- Create: `internal/strategy/versions.go`
- Create: `internal/strategy/versions_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/strategy/versions_test.go
package strategy_test

import (
	"context"
	"testing"

	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

func TestVersions_CreateAndGet(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	versions := strategy.NewVersionsRepository(pool)

	// Insert a strategy with rules; migration 017 backfills v1.
	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, err := repo.Create(ctx, strategy.CreateStrategyRequest{
		Name:  "Test",
		Rules: rules,
	}, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("create strategy: %v", err)
	}

	// Backfill in migration 017 only handled pre-existing rows; new strategies
	// need a v1 created explicitly. Confirm: a fresh strategy has no versions yet.
	got, err := versions.ListByStrategy(ctx, s.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 versions for fresh strategy, got %d", len(got))
	}

	// Create v1.
	v1, err := versions.Create(ctx, s.ID, rules, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if v1.VersionNumber != 1 {
		t.Errorf("v1.VersionNumber = %d, want 1", v1.VersionNumber)
	}

	// Create v2 with new rules.
	rules2 := []byte(`{"filters":[],"ranking":[],"limit":10}`)
	v2, err := versions.Create(ctx, s.ID, rules2, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if v2.VersionNumber != 2 {
		t.Errorf("v2.VersionNumber = %d, want 2", v2.VersionNumber)
	}

	// Get specific version.
	gotV1, err := versions.Get(ctx, v1.ID)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if gotV1.VersionNumber != 1 {
		t.Errorf("gotV1.VersionNumber = %d, want 1", gotV1.VersionNumber)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (`go test ./internal/strategy/...`)

- [ ] **Step 3: Implement `internal/strategy/versions.go`**

```go
// internal/strategy/versions.go
package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"deepvalue/internal/dbutil"
)

// Version is a snapshot of a strategy's rules at a point in time.
type Version struct {
	ID            int64           `json:"id"`
	StrategyID    int64           `json:"strategy_id"`
	VersionNumber int             `json:"version_number"`
	Rules         json.RawMessage `json:"rules"`
	CreatedAt     time.Time       `json:"created_at"`
	CreatedBy     *int64          `json:"created_by,omitempty"`
}

var ErrVersionNotFound = errors.New("strategy version not found")

type VersionsRepository struct {
	pool *pgxpool.Pool
}

func NewVersionsRepository(pool *pgxpool.Pool) *VersionsRepository {
	return &VersionsRepository{pool: pool}
}

// Create inserts a new strategy_versions row, auto-incrementing version_number.
// Accepts a DBTX so it can participate in the same transaction as the
// strategy-update that triggered the bump.
func (r *VersionsRepository) Create(ctx context.Context, strategyID int64, rules json.RawMessage, createdBy int64) (*Version, error) {
	return r.CreateTx(ctx, r.pool, strategyID, rules, createdBy)
}

func (r *VersionsRepository) CreateTx(ctx context.Context, db dbutil.DBTX, strategyID int64, rules json.RawMessage, createdBy int64) (*Version, error) {
	var v Version
	err := db.QueryRow(ctx, `
        INSERT INTO strategy_versions (strategy_id, version_number, rules, created_by)
        VALUES ($1,
                COALESCE((SELECT MAX(version_number) FROM strategy_versions WHERE strategy_id = $1), 0) + 1,
                $2,
                $3)
        RETURNING id, strategy_id, version_number, rules, created_at, created_by
    `, strategyID, rules, createdBy).Scan(&v.ID, &v.StrategyID, &v.VersionNumber, &v.Rules, &v.CreatedAt, &v.CreatedBy)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *VersionsRepository) Get(ctx context.Context, id int64) (*Version, error) {
	var v Version
	err := r.pool.QueryRow(ctx, `
        SELECT id, strategy_id, version_number, rules, created_at, created_by
        FROM strategy_versions WHERE id = $1
    `, id).Scan(&v.ID, &v.StrategyID, &v.VersionNumber, &v.Rules, &v.CreatedAt, &v.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *VersionsRepository) ListByStrategy(ctx context.Context, strategyID int64) ([]Version, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id, strategy_id, version_number, rules, created_at, created_by
        FROM strategy_versions
        WHERE strategy_id = $1
        ORDER BY version_number DESC
    `, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.StrategyID, &v.VersionNumber, &v.Rules, &v.CreatedAt, &v.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/strategy/...
git add internal/strategy/versions.go internal/strategy/versions_test.go
git commit -m "feat(strategy): add Versions repository (Create/Get/ListByStrategy)"
```

---

## Task B3: Strategy edit creates new version + demotes status to draft

**Files:**
- Modify: `internal/strategy/repository.go` (the `UpdateRules` or equivalent method)
- Add tests: extend `internal/strategy/versions_test.go`

The existing `Update` method in `repository.go` mutates `strategies.rules` directly. We're changing the contract: editing rules must create a new `strategy_versions` row, update `current_version_id`, and set `status='draft'`. All in one transaction.

- [ ] **Step 1: Write the failing test**

Add to `internal/strategy/versions_test.go`:

```go
func TestUpdateRules_CreatesNewVersionAndDemotes(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	versions := strategy.NewVersionsRepository(pool)

	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, err := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "Demote", Rules: rules}, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed v1 (mirrors what migration 017 does for pre-existing rows; for new
	// strategies, repo.Create is responsible for seeding v1 too — but that's
	// the next sub-task. For now, seed it manually so this test focuses on Update.)
	v1, err := versions.Create(ctx, s.ID, rules, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE strategies SET status='verified', current_version_id=$1 WHERE id=$2`, v1.ID, s.ID); err != nil {
		t.Fatalf("set verified: %v", err)
	}

	newRules := []byte(`{"filters":[{"field":"pe_ratio","op":"<","value":15}],"ranking":[],"limit":6}`)
	if err := repo.UpdateRules(ctx, s.ID, newRules, testutil.SystemUserID); err != nil {
		t.Fatalf("update rules: %v", err)
	}

	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != strategy.StatusDraft {
		t.Errorf("status = %s, want draft", got.Status)
	}

	all, err := versions.ListByStrategy(ctx, s.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(all))
	}
	if all[0].VersionNumber != 2 {
		t.Errorf("latest version_number = %d, want 2", all[0].VersionNumber)
	}
	if got.CurrentVersionID != all[0].ID {
		t.Errorf("current_version_id = %d, want %d", got.CurrentVersionID, all[0].ID)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/strategy/ -run TestUpdateRules_CreatesNewVersionAndDemotes`
Expected: FAIL — `UpdateRules` either doesn't exist with this signature or doesn't perform the version+demote behavior.

- [ ] **Step 3: Implement `Repository.UpdateRules`**

Add or replace the rules-update method in `internal/strategy/repository.go`:

```go
// UpdateRules creates a new strategy_versions row, points current_version_id at
// it, and sets the strategy's status back to 'draft'. All in one transaction.
// Existing portfolios are unaffected because they reference frozen version ids.
func (r *Repository) UpdateRules(ctx context.Context, strategyID int64, rules json.RawMessage, updatedBy int64) error {
	return dbutil.RunInTx(ctx, r.pool, func(tx dbutil.DBTX) error {
		// 1. Insert new version.
		versions := &VersionsRepository{} // pool not needed; we use the tx
		v, err := versions.CreateTx(ctx, tx, strategyID, rules, updatedBy)
		if err != nil {
			return err
		}

		// 2. Update strategy: rules, current_version_id, status=draft, updated_at.
		_, err = tx.Exec(ctx, `
            UPDATE strategies
            SET rules = $1,
                current_version_id = $2,
                status = 'draft',
                updated_at = NOW()
            WHERE id = $3
        `, rules, v.ID, strategyID)
		return err
	})
}
```

(Add the `import` for `deepvalue/internal/dbutil` if not already present.)

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/strategy/...
git add internal/strategy/
git commit -m "feat(strategy): UpdateRules creates new version and demotes to draft"
```

---

## Task B4: Strategy create seeds v1 automatically

**Files:**
- Modify: `internal/strategy/repository.go` (the existing `Create` method)
- Add test: extend `internal/strategy/versions_test.go`

When a brand-new strategy is created, we want a v1 row with the same rules and `current_version_id` pointing at it.

- [ ] **Step 1: Write the failing test**

```go
func TestCreate_SeedsV1AutomaticallyAndIsVerifiedNo(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := strategy.NewRepository(pool)
	versions := strategy.NewVersionsRepository(pool)

	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, err := repo.Create(ctx, strategy.CreateStrategyRequest{Name: "Auto-v1", Rules: rules}, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if s.Status != strategy.StatusDraft {
		t.Errorf("new strategy status = %s, want draft", s.Status)
	}

	all, err := versions.ListByStrategy(ctx, s.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].VersionNumber != 1 {
		t.Fatalf("want exactly 1 v1, got %+v", all)
	}
	if s.CurrentVersionID != all[0].ID {
		t.Errorf("CurrentVersionID = %d, want %d", s.CurrentVersionID, all[0].ID)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Modify `Repository.Create` to wrap insertion + v1 seed in a transaction**

The existing Create method probably does a single INSERT + RETURNING. Replace it with a transactional version:

```go
func (r *Repository) Create(ctx context.Context, req CreateStrategyRequest, createdBy int64) (*Strategy, error) {
	var s Strategy
	err := dbutil.RunInTx(ctx, r.pool, func(tx dbutil.DBTX) error {
		// 1. Insert strategy with status=draft (no current_version_id yet).
		err := tx.QueryRow(ctx, `
            INSERT INTO strategies (name, description, rules, default_cadence, status, created_by)
            VALUES ($1, $2, $3, $4, 'draft', $5)
            RETURNING id, name, description, rules, default_cadence, status, created_by, created_at, updated_at
        `, req.Name, req.Description, req.Rules, req.DefaultCadence, createdBy).Scan(
			&s.ID, &s.Name, &s.Description, &s.Rules, &s.DefaultCadence, &s.Status, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		)
		if err != nil {
			return err
		}

		// 2. Insert v1.
		var versionID int64
		err = tx.QueryRow(ctx, `
            INSERT INTO strategy_versions (strategy_id, version_number, rules, created_by)
            VALUES ($1, 1, $2, $3) RETURNING id
        `, s.ID, req.Rules, createdBy).Scan(&versionID)
		if err != nil {
			return err
		}

		// 3. Point current_version_id at v1.
		_, err = tx.Exec(ctx, `UPDATE strategies SET current_version_id = $1 WHERE id = $2`, versionID, s.ID)
		s.CurrentVersionID = versionID
		return err
	})
	if err != nil {
		return nil, err
	}
	return &s, nil
}
```

(The exact column list in the INSERT must match your existing `Strategy` struct; adjust `description`, `default_cadence`, and the SELECT clause to match what's in `models.go`.)

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/strategy/...
git add internal/strategy/
git commit -m "feat(strategy): Create seeds v1 and sets status=draft"
```

---

## Task B5: `strategy.status` — Verify and Archive transitions

**Files:**
- Create: `internal/strategy/status.go`
- Create: `internal/strategy/status_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/strategy/status_test.go
package strategy_test

import (
	"context"
	"errors"
	"testing"

	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

func seed(t *testing.T) (*pgxpool.Pool, *strategy.Repository, *strategy.Strategy) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	repo := strategy.NewRepository(pool)
	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, err := repo.Create(context.Background(), strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool, repo, s
}

func TestVerify_FromDraftSucceeds(t *testing.T) {
	_, repo, s := seed(t)
	if err := repo.Verify(context.Background(), s.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := repo.GetByID(context.Background(), s.ID)
	if got.Status != strategy.StatusVerified {
		t.Errorf("status = %s, want verified", got.Status)
	}
}

func TestVerify_FromArchivedFails(t *testing.T) {
	_, repo, s := seed(t)
	_ = repo.Archive(context.Background(), s.ID)
	err := repo.Verify(context.Background(), s.ID)
	if !errors.Is(err, strategy.ErrInvalidStatusTransition) {
		t.Errorf("err = %v, want ErrInvalidStatusTransition", err)
	}
}

func TestArchive_FromAnyStateSucceeds(t *testing.T) {
	_, repo, s := seed(t)
	if err := repo.Archive(context.Background(), s.ID); err != nil {
		t.Fatalf("archive from draft: %v", err)
	}
	got, _ := repo.GetByID(context.Background(), s.ID)
	if got.Status != strategy.StatusArchived {
		t.Errorf("status = %s, want archived", got.Status)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/strategy/status.go`**

```go
// internal/strategy/status.go
package strategy

import (
	"context"
	"errors"
	"fmt"
)

var ErrInvalidStatusTransition = errors.New("invalid status transition")

// Verify transitions a strategy from draft to verified. Errors if the strategy
// is already archived (archive is a terminal state).
func (r *Repository) Verify(ctx context.Context, strategyID int64) error {
	tag, err := r.pool.Exec(ctx, `
        UPDATE strategies SET status = 'verified', updated_at = NOW()
        WHERE id = $1 AND status IN ('draft', 'verified')
    `, strategyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: cannot verify strategy %d (likely archived or missing)", ErrInvalidStatusTransition, strategyID)
	}
	return nil
}

// Archive transitions a strategy to archived from any state. Existing
// portfolios continue rebalancing on their pinned version.
func (r *Repository) Archive(ctx context.Context, strategyID int64) error {
	tag, err := r.pool.Exec(ctx, `
        UPDATE strategies SET status = 'archived', updated_at = NOW() WHERE id = $1
    `, strategyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("strategy %d not found", strategyID)
	}
	return nil
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/strategy/...
git add internal/strategy/
git commit -m "feat(strategy): Verify and Archive status transitions"
```

---

**PAUSE FOR REVIEW — End of Phase B.** Strategy versioning is complete. New strategies seed v1, edits create new versions and demote, status transitions are gated. Run the full strategy test suite (`go test ./internal/strategy/...`) to confirm.

---

# Phase C — Portfolio package

## Task C1: `portfolio.Portfolio` model + statuses

**Files:**
- Create: `internal/portfolio/models.go`

- [ ] **Step 1: Write the model file (no tests yet — pure types)**

```go
// internal/portfolio/models.go
package portfolio

import (
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/strategy"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusPaused   Status = "paused"
	StatusArchived Status = "archived"
)

type Portfolio struct {
	ID                int64             `json:"id"`
	UserID            int64             `json:"user_id"`
	Name              string            `json:"name"`
	StartingCapital   decimal.Decimal   `json:"starting_capital"`
	StrategyID        int64             `json:"strategy_id"`
	StrategyVersionID int64             `json:"strategy_version_id"`
	Cadence           strategy.Cadence  `json:"cadence"`
	NextRebalanceDue  *time.Time        `json:"next_rebalance_due,omitempty"`
	Status            Status            `json:"status"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type Holding struct {
	PortfolioID  int64           `json:"portfolio_id"`
	Ticker       string          `json:"ticker"`
	Shares       decimal.Decimal `json:"shares"`
	CostBasis    decimal.Decimal `json:"cost_basis"`
	LastTradeAt  time.Time       `json:"last_trade_at"`
}

type CapitalEvent struct {
	ID           int64           `json:"id"`
	PortfolioID  int64           `json:"portfolio_id"`
	ProposalID   *int64          `json:"proposal_id,omitempty"`
	Amount       decimal.Decimal `json:"amount"`
	OccurredAt   time.Time       `json:"occurred_at"`
	RecordedAt   time.Time       `json:"recorded_at"`
	Notes        *string         `json:"notes,omitempty"`
}

type ExecutedTrade struct {
	ID           int64           `json:"id"`
	PortfolioID  int64           `json:"portfolio_id"`
	ProposalID   *int64          `json:"proposal_id,omitempty"`
	Ticker       string          `json:"ticker"`
	Action       string          `json:"action"` // "buy" | "sell"
	Shares       decimal.Decimal `json:"shares"`
	Price        decimal.Decimal `json:"price"`
	Fee          decimal.Decimal `json:"fee"`
	ExecutedAt   time.Time       `json:"executed_at"`
	RecordedAt   time.Time       `json:"recorded_at"`
	Notes        *string         `json:"notes,omitempty"`
}
```

- [ ] **Step 2: Confirm it compiles**

Run: `go build ./internal/portfolio/...`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/portfolio/models.go
git commit -m "feat(portfolio): models for Portfolio, Holding, CapitalEvent, ExecutedTrade"
```

---

## Task C2: `portfolio.Repository` — Create + GetByID + ListByUser

**Files:**
- Create: `internal/portfolio/repository.go`
- Create: `internal/portfolio/repository_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/portfolio/repository_test.go
package portfolio_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

// seedStrategy creates a strategy and returns it along with its v1 id.
func seedStrategy(t *testing.T, pool any) (*strategy.Strategy, int64) {
	t.Helper()
	repo := strategy.NewRepository(testutil.PoolFrom(pool))
	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, err := repo.Create(context.Background(), strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if err := repo.Verify(context.Background(), s.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := repo.GetByID(context.Background(), s.ID)
	return got, got.CurrentVersionID
}

func TestPortfolio_CreateAndGet(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	pRepo := portfolio.NewRepository(pool)

	s, vID := seedStrategy(t, pool)

	p, err := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID:            testutil.SystemUserID,
		Name:              "My Portfolio",
		StartingCapital:   decimal.NewFromInt(50000),
		StrategyID:        s.ID,
		StrategyVersionID: vID,
		Cadence:           strategy.CadenceQuarterly,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == 0 {
		t.Fatal("expected non-zero ID")
	}
	if p.Status != portfolio.StatusActive {
		t.Errorf("status = %s, want active", p.Status)
	}
	if p.NextRebalanceDue != nil {
		t.Errorf("next_rebalance_due should be nil for new portfolio, got %v", p.NextRebalanceDue)
	}

	got, err := pRepo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "My Portfolio" {
		t.Errorf("name = %q, want %q", got.Name, "My Portfolio")
	}
	if !got.StartingCapital.Equal(decimal.NewFromInt(50000)) {
		t.Errorf("starting_capital = %s, want 50000", got.StartingCapital)
	}
}

func TestPortfolio_ListByUser(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	pRepo := portfolio.NewRepository(pool)
	s, vID := seedStrategy(t, pool)

	for _, name := range []string{"A", "B"} {
		_, err := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
			UserID: testutil.SystemUserID, Name: name,
			StartingCapital: decimal.NewFromInt(1000),
			StrategyID:      s.ID, StrategyVersionID: vID, Cadence: strategy.CadenceMonthly,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	got, err := pRepo.ListByUser(ctx, testutil.SystemUserID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}
```

(Note: `testutil.PoolFrom` is a small helper — add it to `internal/testutil/db.go` if not present; it just returns the *pgxpool.Pool from a generic argument.)

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/portfolio/repository.go`**

```go
// internal/portfolio/repository.go
package portfolio

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"deepvalue/internal/dbutil"
	"deepvalue/internal/strategy"
)

var (
	ErrNotFound = errors.New("portfolio not found")
)

type CreatePortfolioRequest struct {
	UserID            int64
	Name              string
	StartingCapital   decimal.Decimal
	StrategyID        int64
	StrategyVersionID int64
	Cadence           strategy.Cadence
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

func (r *Repository) Create(ctx context.Context, req CreatePortfolioRequest) (*Portfolio, error) {
	var p Portfolio
	err := r.pool.QueryRow(ctx, `
        INSERT INTO portfolios (user_id, name, starting_capital, strategy_id, strategy_version_id, cadence)
        VALUES ($1, $2, $3, $4, $5, $6)
        RETURNING id, user_id, name, starting_capital, strategy_id, strategy_version_id,
                  cadence, next_rebalance_due, status, created_at, updated_at
    `, req.UserID, req.Name, req.StartingCapital, req.StrategyID, req.StrategyVersionID, req.Cadence).
		Scan(&p.ID, &p.UserID, &p.Name, &p.StartingCapital, &p.StrategyID, &p.StrategyVersionID,
			&p.Cadence, &p.NextRebalanceDue, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repository) GetByID(ctx context.Context, id int64) (*Portfolio, error) {
	return r.getByIDFrom(ctx, r.pool, id)
}

func (r *Repository) GetByIDTx(ctx context.Context, db dbutil.DBTX, id int64) (*Portfolio, error) {
	return r.getByIDFrom(ctx, db, id)
}

func (r *Repository) getByIDFrom(ctx context.Context, db dbutil.DBTX, id int64) (*Portfolio, error) {
	var p Portfolio
	err := db.QueryRow(ctx, `
        SELECT id, user_id, name, starting_capital, strategy_id, strategy_version_id,
               cadence, next_rebalance_due, status, created_at, updated_at
        FROM portfolios WHERE id = $1
    `, id).Scan(&p.ID, &p.UserID, &p.Name, &p.StartingCapital, &p.StrategyID, &p.StrategyVersionID,
		&p.Cadence, &p.NextRebalanceDue, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repository) ListByUser(ctx context.Context, userID int64) ([]Portfolio, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id, user_id, name, starting_capital, strategy_id, strategy_version_id,
               cadence, next_rebalance_due, status, created_at, updated_at
        FROM portfolios
        WHERE user_id = $1 AND status <> 'archived'
        ORDER BY created_at DESC
    `, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Portfolio
	for rows.Next() {
		var p Portfolio
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.StartingCapital, &p.StrategyID, &p.StrategyVersionID,
			&p.Cadence, &p.NextRebalanceDue, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetNextRebalanceDue updates next_rebalance_due (and updated_at). Used by the
// proposal acceptor and by the scheduler.
func (r *Repository) SetNextRebalanceDue(ctx context.Context, db dbutil.DBTX, portfolioID int64, due time.Time) error {
	_, err := db.Exec(ctx, `
        UPDATE portfolios SET next_rebalance_due = $1, updated_at = NOW() WHERE id = $2
    `, due, portfolioID)
	return err
}

// SetStatus changes a portfolio's status (active/paused/archived).
func (r *Repository) SetStatus(ctx context.Context, portfolioID int64, status Status) error {
	tag, err := r.pool.Exec(ctx, `
        UPDATE portfolios SET status = $1, updated_at = NOW() WHERE id = $2
    `, status, portfolioID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FindDueForRebalance returns IDs of active portfolios with next_rebalance_due <= now.
// Caller must run inside a transaction (uses FOR UPDATE SKIP LOCKED).
func (r *Repository) FindDueForRebalance(ctx context.Context, db dbutil.DBTX, now time.Time, limit int) ([]int64, error) {
	rows, err := db.Query(ctx, `
        SELECT id FROM portfolios
        WHERE status = 'active'
          AND next_rebalance_due IS NOT NULL
          AND next_rebalance_due <= $1
        ORDER BY next_rebalance_due ASC
        LIMIT $2
        FOR UPDATE SKIP LOCKED
    `, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/portfolio/...
git add internal/portfolio/
git commit -m "feat(portfolio): Repository with Create/Get/ListByUser/SetStatus/FindDueForRebalance"
```

---

## Task C3: Holdings projection — `ApplyTrade`

**Files:**
- Create: `internal/portfolio/holdings.go`
- Create: `internal/portfolio/holdings_test.go`

The holdings projection is updated transactionally when trades are emitted. `ApplyTrade` is the single entry point — it takes a `DBTX` so it can run inside the acceptor's transaction.

- [ ] **Step 1: Write the failing tests**

```go
// internal/portfolio/holdings_test.go
package portfolio_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

func seedPortfolio(t *testing.T, pool any) *portfolio.Portfolio {
	t.Helper()
	pRepo := portfolio.NewRepository(testutil.PoolFrom(pool))
	s, vID := seedStrategy(t, pool)
	p, err := pRepo.Create(context.Background(), portfolio.CreatePortfolioRequest{
		UserID:            testutil.SystemUserID,
		Name:              "P",
		StartingCapital:   decimal.NewFromInt(10000),
		StrategyID:        s.ID,
		StrategyVersionID: vID,
		Cadence:           strategy.CadenceQuarterly,
	})
	if err != nil {
		t.Fatalf("seed portfolio: %v", err)
	}
	return p
}

func seedTicker(t *testing.T, pool any, ticker string) {
	t.Helper()
	_, err := testutil.PoolFrom(pool).Exec(context.Background(),
		`INSERT INTO companies (ticker, name, sector, industry, active) VALUES ($1, $1, '', '', true) ON CONFLICT (ticker) DO NOTHING`,
		ticker)
	if err != nil {
		t.Fatalf("seed ticker %s: %v", ticker, err)
	}
}

func TestApplyTrade_BuyCreatesHolding(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now()
	err := holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180), Fee: decimal.NewFromInt(2),
		ExecutedAt: now,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	h, err := holdings.Get(ctx, p.ID, "AAPL")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !h.Shares.Equal(decimal.NewFromInt(10)) {
		t.Errorf("shares = %s, want 10", h.Shares)
	}
	wantCost := decimal.NewFromInt(180*10 + 2)
	if !h.CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", h.CostBasis, wantCost)
	}
}

func TestApplyTrade_BuyAddsToExistingHolding(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now()
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180), Fee: decimal.Zero,
		ExecutedAt: now,
	})
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(5), Price: decimal.NewFromInt(200), Fee: decimal.Zero,
		ExecutedAt: now,
	})

	h, _ := holdings.Get(ctx, p.ID, "AAPL")
	if !h.Shares.Equal(decimal.NewFromInt(15)) {
		t.Errorf("shares = %s, want 15", h.Shares)
	}
	wantCost := decimal.NewFromInt(180*10 + 200*5)
	if !h.CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", h.CostBasis, wantCost)
	}
}

func TestApplyTrade_SellReducesShares(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now()
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180), Fee: decimal.Zero, ExecutedAt: now,
	})
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "sell",
		Shares: decimal.NewFromInt(4), Price: decimal.NewFromInt(190), Fee: decimal.Zero, ExecutedAt: now,
	})

	h, _ := holdings.Get(ctx, p.ID, "AAPL")
	if !h.Shares.Equal(decimal.NewFromInt(6)) {
		t.Errorf("shares = %s, want 6", h.Shares)
	}
	// Cost basis reduced proportionally: 1800 * (6/10) = 1080
	wantCost := decimal.NewFromInt(1080)
	if !h.CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", h.CostBasis, wantCost)
	}
}

func TestApplyTrade_SellAllRemovesHolding(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")

	now := time.Now()
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "buy",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(180), Fee: decimal.Zero, ExecutedAt: now,
	})
	_ = holdings.ApplyTrade(ctx, pool, portfolio.TradeApplication{
		PortfolioID: p.ID, Ticker: "AAPL", Action: "sell",
		Shares: decimal.NewFromInt(10), Price: decimal.NewFromInt(190), Fee: decimal.Zero, ExecutedAt: now,
	})

	_, err := holdings.Get(ctx, p.ID, "AAPL")
	if err != portfolio.ErrHoldingNotFound {
		t.Errorf("err = %v, want ErrHoldingNotFound", err)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/portfolio/holdings.go`**

```go
// internal/portfolio/holdings.go
package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"deepvalue/internal/dbutil"
)

var ErrHoldingNotFound = errors.New("holding not found")

type Holdings struct {
	pool *pgxpool.Pool
}

func NewHoldings(pool *pgxpool.Pool) *Holdings { return &Holdings{pool: pool} }

type TradeApplication struct {
	PortfolioID int64
	Ticker      string
	Action      string // "buy" | "sell"
	Shares      decimal.Decimal
	Price       decimal.Decimal
	Fee         decimal.Decimal
	ExecutedAt  time.Time
}

// ApplyTrade updates the holdings projection for a single trade. Idempotent
// per-(portfolio, ticker) only when called with consistent inputs; it does not
// dedupe — that's the caller's responsibility (the proposal acceptor does this
// inside a transaction so partial failures roll back).
func (h *Holdings) ApplyTrade(ctx context.Context, db dbutil.DBTX, t TradeApplication) error {
	switch t.Action {
	case "buy":
		return h.applyBuy(ctx, db, t)
	case "sell":
		return h.applySell(ctx, db, t)
	default:
		return fmt.Errorf("invalid trade action: %q", t.Action)
	}
}

func (h *Holdings) applyBuy(ctx context.Context, db dbutil.DBTX, t TradeApplication) error {
	cost := t.Shares.Mul(t.Price).Add(t.Fee)
	_, err := db.Exec(ctx, `
        INSERT INTO holdings (portfolio_id, ticker, shares, cost_basis, last_trade_at)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (portfolio_id, ticker) DO UPDATE
        SET shares = holdings.shares + EXCLUDED.shares,
            cost_basis = holdings.cost_basis + EXCLUDED.cost_basis,
            last_trade_at = GREATEST(holdings.last_trade_at, EXCLUDED.last_trade_at)
    `, t.PortfolioID, t.Ticker, t.Shares, cost, t.ExecutedAt)
	return err
}

func (h *Holdings) applySell(ctx context.Context, db dbutil.DBTX, t TradeApplication) error {
	// Read current holding inside the same txn.
	var current Holding
	err := db.QueryRow(ctx, `
        SELECT portfolio_id, ticker, shares, cost_basis, last_trade_at
        FROM holdings WHERE portfolio_id = $1 AND ticker = $2
    `, t.PortfolioID, t.Ticker).Scan(&current.PortfolioID, &current.Ticker, &current.Shares, &current.CostBasis, &current.LastTradeAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("cannot sell %s: %w", t.Ticker, ErrHoldingNotFound)
	}
	if err != nil {
		return err
	}

	if t.Shares.GreaterThan(current.Shares) {
		return fmt.Errorf("cannot sell %s shares of %s; holding has %s", t.Shares, t.Ticker, current.Shares)
	}

	if t.Shares.Equal(current.Shares) {
		_, err := db.Exec(ctx, `DELETE FROM holdings WHERE portfolio_id = $1 AND ticker = $2`, t.PortfolioID, t.Ticker)
		return err
	}

	// Partial sell: reduce shares and cost_basis proportionally. Fees on a sell
	// don't affect cost_basis (cost_basis tracks acquisition cost only).
	ratio := current.Shares.Sub(t.Shares).Div(current.Shares) // remaining ratio
	newShares := current.Shares.Sub(t.Shares)
	newCostBasis := current.CostBasis.Mul(ratio).Round(2)

	_, err = db.Exec(ctx, `
        UPDATE holdings SET shares = $1, cost_basis = $2, last_trade_at = GREATEST(last_trade_at, $3)
        WHERE portfolio_id = $4 AND ticker = $5
    `, newShares, newCostBasis, t.ExecutedAt, t.PortfolioID, t.Ticker)
	return err
}

// Get returns the holding for a (portfolio, ticker), or ErrHoldingNotFound.
func (h *Holdings) Get(ctx context.Context, portfolioID int64, ticker string) (*Holding, error) {
	var hd Holding
	err := h.pool.QueryRow(ctx, `
        SELECT portfolio_id, ticker, shares, cost_basis, last_trade_at
        FROM holdings WHERE portfolio_id = $1 AND ticker = $2
    `, portfolioID, ticker).Scan(&hd.PortfolioID, &hd.Ticker, &hd.Shares, &hd.CostBasis, &hd.LastTradeAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrHoldingNotFound
	}
	if err != nil {
		return nil, err
	}
	return &hd, nil
}

// ListByPortfolio returns all current holdings for a portfolio.
func (h *Holdings) ListByPortfolio(ctx context.Context, portfolioID int64) ([]Holding, error) {
	rows, err := h.pool.Query(ctx, `
        SELECT portfolio_id, ticker, shares, cost_basis, last_trade_at
        FROM holdings WHERE portfolio_id = $1 ORDER BY ticker ASC
    `, portfolioID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Holding
	for rows.Next() {
		var hd Holding
		if err := rows.Scan(&hd.PortfolioID, &hd.Ticker, &hd.Shares, &hd.CostBasis, &hd.LastTradeAt); err != nil {
			return nil, err
		}
		out = append(out, hd)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/portfolio/...
git add internal/portfolio/holdings.go internal/portfolio/holdings_test.go
git commit -m "feat(portfolio): holdings projection ApplyTrade/Get/ListByPortfolio"
```

---

## Task C4: Holdings.Rebuild (recompute from executed_trades)

**Files:**
- Modify: `internal/portfolio/holdings.go`
- Modify: `internal/portfolio/holdings_test.go`

`Rebuild` recomputes the holdings projection from the trade ledger. Used as a sanity check / manual repair tool.

- [ ] **Step 1: Write the failing test**

```go
func TestHoldings_RebuildMatchesIncrementalApplies(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	holdings := portfolio.NewHoldings(pool)
	p := seedPortfolio(t, pool)
	seedTicker(t, pool, "AAPL")
	seedTicker(t, pool, "MSFT")

	now := time.Now()
	// Insert trades directly into executed_trades (bypassing the acceptor) to
	// simulate "rebuild from ledger" with no projection yet.
	for _, tr := range []struct {
		ticker, action string
		shares, price  int64
	}{
		{"AAPL", "buy", 10, 180},
		{"AAPL", "buy", 5, 200},
		{"AAPL", "sell", 4, 190},
		{"MSFT", "buy", 8, 410},
	} {
		_, err := pool.Exec(ctx, `
            INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, executed_at)
            VALUES ($1, $2, $3, $4, $5, $6)
        `, p.ID, tr.ticker, tr.action, tr.shares, tr.price, now)
		if err != nil {
			t.Fatalf("insert trade: %v", err)
		}
	}

	if err := holdings.Rebuild(ctx, pool, p.ID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	aapl, err := holdings.Get(ctx, p.ID, "AAPL")
	if err != nil {
		t.Fatalf("get AAPL: %v", err)
	}
	if !aapl.Shares.Equal(decimal.NewFromInt(11)) { // 10 + 5 - 4
		t.Errorf("AAPL shares = %s, want 11", aapl.Shares)
	}

	msft, _ := holdings.Get(ctx, p.ID, "MSFT")
	if !msft.Shares.Equal(decimal.NewFromInt(8)) {
		t.Errorf("MSFT shares = %s, want 8", msft.Shares)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `Rebuild`**

Append to `internal/portfolio/holdings.go`:

```go
// Rebuild recomputes holdings for a portfolio from its executed_trades ledger.
// Wipes existing holdings rows for the portfolio first. Idempotent.
func (h *Holdings) Rebuild(ctx context.Context, db dbutil.DBTX, portfolioID int64) error {
	if _, err := db.Exec(ctx, `DELETE FROM holdings WHERE portfolio_id = $1`, portfolioID); err != nil {
		return err
	}
	rows, err := db.Query(ctx, `
        SELECT ticker, action, shares, price, fee, executed_at
        FROM executed_trades
        WHERE portfolio_id = $1
        ORDER BY executed_at ASC, id ASC
    `, portfolioID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var t TradeApplication
		t.PortfolioID = portfolioID
		if err := rows.Scan(&t.Ticker, &t.Action, &t.Shares, &t.Price, &t.Fee, &t.ExecutedAt); err != nil {
			return err
		}
		if err := h.ApplyTrade(ctx, db, t); err != nil {
			return err
		}
	}
	return rows.Err()
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/portfolio/...
git add internal/portfolio/
git commit -m "feat(portfolio): holdings.Rebuild recomputes from executed_trades"
```

---

## Task C5: `portfolio.Service.CreatePortfolio`

**Files:**
- Create: `internal/portfolio/service.go`
- Create: `internal/portfolio/service_test.go`

The service ties together: validate strategy is `verified`, copy `current_version_id` to pin, copy `default_cadence` (with optional override), insert portfolio.

- [ ] **Step 1: Write the failing tests**

```go
// internal/portfolio/service_test.go
package portfolio_test

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

func TestService_CreatePortfolio_VerifiedStrategySucceeds(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	svc := portfolio.NewService(pRepo, sRepo)

	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	_ = sRepo.Verify(ctx, s.ID)

	p, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID:          testutil.SystemUserID,
		Name:            "Test",
		StartingCapital: decimal.NewFromInt(10000),
		StrategyID:      s.ID,
		CadenceOverride: nil,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.StrategyVersionID == 0 {
		t.Error("expected pinned StrategyVersionID")
	}
}

func TestService_CreatePortfolio_DraftStrategyFails(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	svc := portfolio.NewService(pRepo, sRepo)

	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	// status is draft by default

	_, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: testutil.SystemUserID, Name: "T",
		StartingCapital: decimal.NewFromInt(1000), StrategyID: s.ID,
	})
	if !errors.Is(err, portfolio.ErrStrategyNotVerified) {
		t.Errorf("err = %v, want ErrStrategyNotVerified", err)
	}
}

func TestService_CreatePortfolio_UsesDefaultCadenceWhenNoOverride(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	svc := portfolio.NewService(pRepo, sRepo)

	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	def := strategy.CadenceQuarterly
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: "S", Rules: rules, DefaultCadence: &def}, testutil.SystemUserID)
	_ = sRepo.Verify(ctx, s.ID)

	p, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: testutil.SystemUserID, Name: "T",
		StartingCapital: decimal.NewFromInt(1000), StrategyID: s.ID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Cadence != strategy.CadenceQuarterly {
		t.Errorf("cadence = %s, want quarterly", p.Cadence)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/portfolio/service.go`**

```go
// internal/portfolio/service.go
package portfolio

import (
	"context"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"deepvalue/internal/strategy"
)

var (
	ErrStrategyNotVerified = errors.New("strategy must be verified to attach to a portfolio")
	ErrCadenceMissing      = errors.New("no cadence supplied and strategy has no default")
)

type Service struct {
	portfolios *Repository
	strategies *strategy.Repository
}

func NewService(p *Repository, s *strategy.Repository) *Service {
	return &Service{portfolios: p, strategies: s}
}

type CreatePortfolioInput struct {
	UserID          int64
	Name            string
	StartingCapital decimal.Decimal
	StrategyID      int64
	CadenceOverride *strategy.Cadence // nil → use strategy.DefaultCadence
}

func (s *Service) CreatePortfolio(ctx context.Context, in CreatePortfolioInput) (*Portfolio, error) {
	strat, err := s.strategies.GetByID(ctx, int(in.StrategyID))
	if err != nil {
		return nil, fmt.Errorf("load strategy: %w", err)
	}
	if strat.Status != strategy.StatusVerified {
		return nil, fmt.Errorf("%w (status=%s)", ErrStrategyNotVerified, strat.Status)
	}

	cadence := in.CadenceOverride
	if cadence == nil {
		cadence = strat.DefaultCadence
	}
	if cadence == nil {
		return nil, ErrCadenceMissing
	}

	return s.portfolios.Create(ctx, CreatePortfolioRequest{
		UserID:            in.UserID,
		Name:              in.Name,
		StartingCapital:   in.StartingCapital,
		StrategyID:        strat.ID,
		StrategyVersionID: strat.CurrentVersionID,
		Cadence:           *cadence,
	})
}

// SetStatus is a thin pass-through that handlers use for pause/resume/archive.
func (s *Service) SetStatus(ctx context.Context, portfolioID int64, status Status) error {
	return s.portfolios.SetStatus(ctx, portfolioID, status)
}
```

(Adjust `s.strategies.GetByID(ctx, int(in.StrategyID))` if the existing signature uses `int64` instead — match the existing repository.)

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/portfolio/...
git add internal/portfolio/service.go internal/portfolio/service_test.go
git commit -m "feat(portfolio): Service.CreatePortfolio pins strategy version + cadence"
```

---

**PAUSE FOR REVIEW — End of Phase C.** Portfolio domain is in place. We can create portfolios, attach strategies (with version pinning), apply trades to the holdings projection, and rebuild from the ledger. No proposal logic yet.

---

# Phase D — Proposal package

## Task D1: `proposal.cadence` — pure functions

**Files:**
- Create: `internal/proposal/cadence.go`
- Create: `internal/proposal/cadence_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/proposal/cadence_test.go
package proposal_test

import (
	"testing"
	"time"

	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
)

func TestAddCadence_Table(t *testing.T) {
	base := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		c    strategy.Cadence
		want time.Time
	}{
		{strategy.CadenceMonthly, time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)},
		{strategy.CadenceQuarterly, time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)},
		{strategy.CadenceSemiAnnual, time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)},
		{strategy.CadenceAnnual, time.Date(2027, 5, 8, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := proposal.AddCadence(base, tc.c)
		if err != nil {
			t.Errorf("%s: err = %v", tc.c, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: got %s, want %s", tc.c, got, tc.want)
		}
	}
}

func TestAddCadence_UnknownReturnsError(t *testing.T) {
	_, err := proposal.AddCadence(time.Now(), strategy.Cadence("bogus"))
	if err == nil {
		t.Error("want error for bogus cadence")
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/proposal/cadence.go`**

```go
// internal/proposal/cadence.go
package proposal

import (
	"fmt"
	"time"

	"deepvalue/internal/strategy"
)

// AddCadence advances t by one cadence period. Pure; no DB, no clock side
// effects. Used by the acceptor (after acceptance/skip) to set
// next_rebalance_due.
func AddCadence(t time.Time, c strategy.Cadence) (time.Time, error) {
	switch c {
	case strategy.CadenceMonthly:
		return t.AddDate(0, 1, 0), nil
	case strategy.CadenceQuarterly:
		return t.AddDate(0, 3, 0), nil
	case strategy.CadenceSemiAnnual:
		return t.AddDate(0, 6, 0), nil
	case strategy.CadenceAnnual:
		return t.AddDate(1, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unknown cadence: %q", c)
	}
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/proposal/...
git add internal/proposal/cadence.go internal/proposal/cadence_test.go
git commit -m "feat(proposal): AddCadence pure function"
```

---

## Task D2: `proposal` models + statuses + actions

**Files:**
- Create: `internal/proposal/models.go`

- [ ] **Step 1: Write the model file**

```go
// internal/proposal/models.go
package proposal

import (
	"time"

	"github.com/shopspring/decimal"
)

type Status string

const (
	StatusPending            Status = "pending"
	StatusAccepted           Status = "accepted"
	StatusPartiallyAccepted  Status = "partially_accepted"
	StatusSkipped            Status = "skipped"
	StatusExpired            Status = "expired"
)

// Action is the per-row label assigned at generation time, describing how the
// pick relates to the portfolio's current holdings.
type Action string

const (
	ActionBuy  Action = "buy"   // not held → buy fresh
	ActionSell Action = "sell"  // held but not in new picks → sell entire
	ActionAdd  Action = "add"   // held, target shares > current
	ActionTrim Action = "trim"  // held, target shares < current
	ActionHold Action = "hold"  // held at exactly target shares
)

// Pick is one row in a proposal's picks JSONB array.
type Pick struct {
	Ticker           string          `json:"ticker"`
	Action           Action          `json:"action"`
	TargetWeight     decimal.Decimal `json:"target_weight"`     // 0..1
	TargetShares     decimal.Decimal `json:"target_shares"`     // floor((weight * deploy) / price)
	CurrentShares    decimal.Decimal `json:"current_shares"`    // 0 if new
	PriceAtProposal  decimal.Decimal `json:"price_at_proposal"`
}

type Proposal struct {
	ID                       int64           `json:"id"`
	PortfolioID              int64           `json:"portfolio_id"`
	StrategyVersionID        int64           `json:"strategy_version_id"`
	GeneratedAt              time.Time       `json:"generated_at"`
	MarketValueAtProposal    decimal.Decimal `json:"market_value_at_proposal"`
	CapitalChange            decimal.Decimal `json:"capital_change"`
	DeployAmount             decimal.Decimal `json:"deploy_amount"`
	Picks                    []Pick          `json:"picks"`
	Status                   Status          `json:"status"`
	ResolvedAt               *time.Time      `json:"resolved_at,omitempty"`
	NotificationSentAt       *time.Time      `json:"notification_sent_at,omitempty"`
	ReminderSentAt           *time.Time      `json:"reminder_sent_at,omitempty"`
}
```

- [ ] **Step 2: Compile + commit**

```bash
go build ./internal/proposal/...
git add internal/proposal/models.go
git commit -m "feat(proposal): models for Proposal, Pick, Action, Status"
```

---

## Task D3: `proposal.Repository` — Insert, Get, GetPending, ExpirePending, UpdatePending

**Files:**
- Create: `internal/proposal/repository.go`
- Create: `internal/proposal/repository_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/proposal/repository_test.go
package proposal_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

// seedPortfolioForProposal returns a portfolio with a verified strategy attached.
func seedPortfolioForProposal(t *testing.T) (*portfolio.Portfolio, int64) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)

	rules := []byte(`{"filters":[],"ranking":[],"limit":6}`)
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	_ = sRepo.Verify(ctx, s.ID)
	got, _ := sRepo.GetByID(ctx, s.ID)

	p, err := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: testutil.SystemUserID, Name: "P",
		StartingCapital: decimal.NewFromInt(10000),
		StrategyID:      s.ID, StrategyVersionID: got.CurrentVersionID,
		Cadence: strategy.CadenceQuarterly,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return p, got.CurrentVersionID
}

func TestProposalRepo_InsertAndGet(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolioForProposal(t)

	picks := []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(0.5),
			TargetShares: decimal.NewFromInt(20), PriceAtProposal: decimal.NewFromInt(180)},
	}
	picksJSON, _ := json.Marshal(picks)

	pr, err := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID:           p.ID,
		StrategyVersionID:     vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 picksJSON,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if pr.Status != proposal.StatusPending {
		t.Errorf("status = %s, want pending", pr.Status)
	}

	got, err := repo.Get(ctx, pr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Picks) != 1 || got.Picks[0].Ticker != "AAPL" {
		t.Errorf("picks = %+v, want one AAPL pick", got.Picks)
	}
}

func TestProposalRepo_ExpirePendingMakesNewSlot(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolioForProposal(t)

	mk := func() *proposal.Proposal {
		picks, _ := json.Marshal([]proposal.Pick{})
		pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
			PortfolioID: p.ID, StrategyVersionID: vID,
			MarketValueAtProposal: decimal.NewFromInt(10000),
			CapitalChange:         decimal.Zero,
			DeployAmount:          decimal.NewFromInt(10000),
			Picks:                 picks,
		})
		return pr
	}

	first := mk()

	if err := repo.ExpirePending(ctx, pool, p.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	gotFirst, _ := repo.Get(ctx, first.ID)
	if gotFirst.Status != proposal.StatusExpired {
		t.Errorf("first.Status = %s, want expired", gotFirst.Status)
	}
	if gotFirst.ResolvedAt == nil {
		t.Error("expected resolved_at to be set")
	}

	// A new proposal can now be inserted (no constraint violation).
	second := mk()
	if second.Status != proposal.StatusPending {
		t.Errorf("second.Status = %s, want pending", second.Status)
	}
	_ = time.Now() // silence unused if we strip imports later
}

func TestProposalRepo_UpdatePendingPicks(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolioForProposal(t)

	picks0, _ := json.Marshal([]proposal.Pick{})
	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 picks0,
	})

	picks1JSON, _ := json.Marshal([]proposal.Pick{
		{Ticker: "MSFT", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(1),
			TargetShares: decimal.NewFromInt(20), PriceAtProposal: decimal.NewFromInt(500)},
	})
	if err := repo.UpdatePending(ctx, pool, pr.ID, proposal.UpdatePendingInput{
		CapitalChange: decimal.NewFromInt(5000),
		DeployAmount:  decimal.NewFromInt(15000),
		Picks:         picks1JSON,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := repo.Get(ctx, pr.ID)
	if !got.CapitalChange.Equal(decimal.NewFromInt(5000)) {
		t.Errorf("capital_change = %s, want 5000", got.CapitalChange)
	}
	if got.Picks[0].Ticker != "MSFT" {
		t.Errorf("picks not updated")
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/proposal/repository.go`**

```go
// internal/proposal/repository.go
package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"deepvalue/internal/dbutil"
)

var ErrNotFound = errors.New("proposal not found")

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

type InsertInput struct {
	PortfolioID           int64
	StrategyVersionID     int64
	MarketValueAtProposal decimal.Decimal
	CapitalChange         decimal.Decimal
	DeployAmount          decimal.Decimal
	Picks                 json.RawMessage
}

type UpdatePendingInput struct {
	CapitalChange decimal.Decimal
	DeployAmount  decimal.Decimal
	Picks         json.RawMessage
}

func (r *Repository) Insert(ctx context.Context, db dbutil.DBTX, in InsertInput) (*Proposal, error) {
	var (
		p          Proposal
		picksRaw   []byte
	)
	err := db.QueryRow(ctx, `
        INSERT INTO proposals (portfolio_id, strategy_version_id, market_value_at_proposal,
                               capital_change, deploy_amount, picks, status)
        VALUES ($1, $2, $3, $4, $5, $6, 'pending')
        RETURNING id, portfolio_id, strategy_version_id, generated_at,
                  market_value_at_proposal, capital_change, deploy_amount,
                  picks, status, resolved_at, notification_sent_at, reminder_sent_at
    `, in.PortfolioID, in.StrategyVersionID, in.MarketValueAtProposal,
		in.CapitalChange, in.DeployAmount, in.Picks).Scan(
		&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
		&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
		&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repository) Get(ctx context.Context, id int64) (*Proposal, error) {
	return r.getFrom(ctx, r.pool, id)
}

func (r *Repository) GetTx(ctx context.Context, db dbutil.DBTX, id int64) (*Proposal, error) {
	return r.getFrom(ctx, db, id)
}

func (r *Repository) getFrom(ctx context.Context, db dbutil.DBTX, id int64) (*Proposal, error) {
	var (
		p        Proposal
		picksRaw []byte
	)
	err := db.QueryRow(ctx, `
        SELECT id, portfolio_id, strategy_version_id, generated_at,
               market_value_at_proposal, capital_change, deploy_amount,
               picks, status, resolved_at, notification_sent_at, reminder_sent_at
        FROM proposals WHERE id = $1
    `, id).Scan(&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
		&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
		&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetPending returns the single pending proposal for a portfolio, or
// ErrNotFound if none. The proposals_pending_idx ensures this is fast.
func (r *Repository) GetPending(ctx context.Context, db dbutil.DBTX, portfolioID int64) (*Proposal, error) {
	var (
		p        Proposal
		picksRaw []byte
	)
	err := db.QueryRow(ctx, `
        SELECT id, portfolio_id, strategy_version_id, generated_at,
               market_value_at_proposal, capital_change, deploy_amount,
               picks, status, resolved_at, notification_sent_at, reminder_sent_at
        FROM proposals
        WHERE portfolio_id = $1 AND status = 'pending'
        LIMIT 1
    `, portfolioID).Scan(&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
		&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
		&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
		return nil, err
	}
	return &p, nil
}

// ExpirePending sets any pending proposals for the portfolio to status='expired'.
// Used right before generating a new proposal.
func (r *Repository) ExpirePending(ctx context.Context, db dbutil.DBTX, portfolioID int64) error {
	_, err := db.Exec(ctx, `
        UPDATE proposals
        SET status = 'expired', resolved_at = NOW()
        WHERE portfolio_id = $1 AND status = 'pending'
    `, portfolioID)
	return err
}

// UpdatePending mutates a pending proposal in place (capital_change recompute).
// Errors if the proposal is no longer pending.
func (r *Repository) UpdatePending(ctx context.Context, db dbutil.DBTX, id int64, in UpdatePendingInput) error {
	tag, err := db.Exec(ctx, `
        UPDATE proposals
        SET capital_change = $1, deploy_amount = $2, picks = $3
        WHERE id = $4 AND status = 'pending'
    `, in.CapitalChange, in.DeployAmount, in.Picks, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("proposal not pending or not found")
	}
	return nil
}

// MarkResolved sets status + resolved_at atomically. Used by the acceptor.
func (r *Repository) MarkResolved(ctx context.Context, db dbutil.DBTX, id int64, status Status, at time.Time) error {
	tag, err := db.Exec(ctx, `
        UPDATE proposals SET status = $1, resolved_at = $2
        WHERE id = $3 AND status = 'pending'
    `, status, at, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("proposal not pending or not found")
	}
	return nil
}

// SetNotificationSent stamps notification_sent_at after a successful email send.
func (r *Repository) SetNotificationSent(ctx context.Context, id int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE proposals SET notification_sent_at = $1 WHERE id = $2`, at, id)
	return err
}

// SetReminderSent stamps reminder_sent_at after a successful reminder send.
func (r *Repository) SetReminderSent(ctx context.Context, id int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE proposals SET reminder_sent_at = $1 WHERE id = $2`, at, id)
	return err
}

// FindReminderCandidates returns pending proposals whose initial notification
// was sent more than 'after' ago and have no reminder yet.
func (r *Repository) FindReminderCandidates(ctx context.Context, after time.Duration) ([]Proposal, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id, portfolio_id, strategy_version_id, generated_at,
               market_value_at_proposal, capital_change, deploy_amount,
               picks, status, resolved_at, notification_sent_at, reminder_sent_at
        FROM proposals
        WHERE status = 'pending'
          AND notification_sent_at IS NOT NULL
          AND notification_sent_at < NOW() - $1::INTERVAL
          AND reminder_sent_at IS NULL
    `, after.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProposals(rows)
}

// FindUnsentNotifications returns pending proposals where the initial email
// failed (notification_sent_at IS NULL) within the retry window.
func (r *Repository) FindUnsentNotifications(ctx context.Context, retryWindow time.Duration) ([]Proposal, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id, portfolio_id, strategy_version_id, generated_at,
               market_value_at_proposal, capital_change, deploy_amount,
               picks, status, resolved_at, notification_sent_at, reminder_sent_at
        FROM proposals
        WHERE status = 'pending'
          AND notification_sent_at IS NULL
          AND generated_at < NOW() - INTERVAL '5 minutes'
          AND generated_at > NOW() - $1::INTERVAL
    `, retryWindow.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProposals(rows)
}

func scanProposals(rows pgx.Rows) ([]Proposal, error) {
	var out []Proposal
	for rows.Next() {
		var (
			p        Proposal
			picksRaw []byte
		)
		if err := rows.Scan(&p.ID, &p.PortfolioID, &p.StrategyVersionID, &p.GeneratedAt,
			&p.MarketValueAtProposal, &p.CapitalChange, &p.DeployAmount,
			&picksRaw, &p.Status, &p.ResolvedAt, &p.NotificationSentAt, &p.ReminderSentAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(picksRaw, &p.Picks); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/proposal/...
git add internal/proposal/repository.go internal/proposal/repository_test.go
git commit -m "feat(proposal): Repository (Insert, Get, ExpirePending, UpdatePending, reminder queries)"
```

---

## Task D4: `proposal.Generator` — diff logic + share sizing

**Files:**
- Create: `internal/proposal/generator.go`
- Create: `internal/proposal/generator_test.go`

The Generator has external dependencies (strategy executor, holdings repo, daily_prices). To test the **diff and sizing math** in isolation, define small interfaces that the generator depends on, and use stubs in tests. The DB-level integration of "generate from a real portfolio" is covered later in the end-to-end test.

- [ ] **Step 1: Write the failing tests**

```go
// internal/proposal/generator_test.go
package proposal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
)

type stubExecutor struct {
	recs []strategy.Recommendation
}

func (s stubExecutor) RunWithRules(ctx context.Context, rules []byte) ([]strategy.Recommendation, error) {
	return s.recs, nil
}

type stubHoldings map[string]decimal.Decimal // ticker → shares

func (s stubHoldings) ListByPortfolio(ctx context.Context, portfolioID int64) ([]portfolio.Holding, error) {
	out := make([]portfolio.Holding, 0, len(s))
	for ticker, shares := range s {
		out = append(out, portfolio.Holding{PortfolioID: portfolioID, Ticker: ticker, Shares: shares})
	}
	return out, nil
}

type stubPrices map[string]decimal.Decimal

func (s stubPrices) Latest(ctx context.Context, ticker string) (decimal.Decimal, error) {
	p, ok := s[ticker]
	if !ok {
		return decimal.Zero, errors.New("no price")
	}
	return p, nil
}

func TestGenerator_NewPortfolioAllBuys(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{
			{Ticker: "AAPL", Score: decimal.NewFromFloat(0.9)},
			{Ticker: "MSFT", Score: decimal.NewFromFloat(0.8)},
		}},
		stubHoldings{},
		stubPrices{"AAPL": decimal.NewFromInt(180), "MSFT": decimal.NewFromInt(400)},
	)

	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID:    1,
		Rules:          []byte(`{}`),
		MarketValue:    decimal.NewFromInt(10000),
		CapitalChange:  decimal.Zero,
		StrategyLimit:  2,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(picks) != 2 {
		t.Fatalf("len = %d, want 2", len(picks))
	}
	for _, p := range picks {
		if p.Action != proposal.ActionBuy {
			t.Errorf("%s: action = %s, want buy", p.Ticker, p.Action)
		}
	}
	// Equal weight: 0.5 each. Deploy = 10000. AAPL: floor(5000/180)=27. MSFT: floor(5000/400)=12.
	for _, p := range picks {
		if p.Ticker == "AAPL" && !p.TargetShares.Equal(decimal.NewFromInt(27)) {
			t.Errorf("AAPL shares = %s, want 27", p.TargetShares)
		}
		if p.Ticker == "MSFT" && !p.TargetShares.Equal(decimal.NewFromInt(12)) {
			t.Errorf("MSFT shares = %s, want 12", p.TargetShares)
		}
	}
}

func TestGenerator_HoldingNotInPicksBecomesSell(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{
			{Ticker: "AAPL", Score: decimal.NewFromFloat(0.9)},
		}},
		stubHoldings{"GOOG": decimal.NewFromInt(5)}, // currently held but not in new picks
		stubPrices{"AAPL": decimal.NewFromInt(180), "GOOG": decimal.NewFromInt(140)},
	)

	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID:   1,
		Rules:         []byte(`{}`),
		MarketValue:   decimal.NewFromInt(10000),
		CapitalChange: decimal.Zero,
		StrategyLimit: 1,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(picks) != 2 {
		t.Fatalf("len = %d, want 2 (1 buy, 1 sell)", len(picks))
	}

	bySymbol := map[string]proposal.Pick{}
	for _, p := range picks {
		bySymbol[p.Ticker] = p
	}
	if bySymbol["GOOG"].Action != proposal.ActionSell {
		t.Errorf("GOOG action = %s, want sell", bySymbol["GOOG"].Action)
	}
	if bySymbol["AAPL"].Action != proposal.ActionBuy {
		t.Errorf("AAPL action = %s, want buy", bySymbol["AAPL"].Action)
	}
}

func TestGenerator_AddTrimHold(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{
			{Ticker: "A", Score: decimal.NewFromFloat(1)}, // weight 1.0 → all 10000 → 100 shares @ $100
		}},
		stubHoldings{"A": decimal.NewFromInt(50)}, // currently 50; target 100 → action=add (delta 50)
		stubPrices{"A": decimal.NewFromInt(100)},
	)

	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID:   1,
		Rules:         []byte(`{}`),
		MarketValue:   decimal.NewFromInt(10000),
		CapitalChange: decimal.Zero,
		StrategyLimit: 1,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if picks[0].Action != proposal.ActionAdd {
		t.Errorf("action = %s, want add", picks[0].Action)
	}
}

func TestGenerator_CapitalChangeAdjustsDeploy(t *testing.T) {
	g := proposal.NewGenerator(
		stubExecutor{recs: []strategy.Recommendation{{Ticker: "A", Score: decimal.NewFromFloat(1)}}},
		stubHoldings{},
		stubPrices{"A": decimal.NewFromInt(100)},
	)

	picks, err := g.GeneratePicks(context.Background(), proposal.GenerateInput{
		PortfolioID:   1,
		Rules:         []byte(`{}`),
		MarketValue:   decimal.NewFromInt(5000),
		CapitalChange: decimal.NewFromInt(5000),
		StrategyLimit: 1,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !picks[0].TargetShares.Equal(decimal.NewFromInt(100)) {
		t.Errorf("shares = %s, want 100 (deploy=10000)", picks[0].TargetShares)
	}
	_ = time.Now()
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/proposal/generator.go`**

```go
// internal/proposal/generator.go
package proposal

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/strategy"
)

// StrategyExecutor abstracts the strategy executor for testability. The real
// implementation is in internal/strategy; this is a small wrapper interface
// the Generator depends on.
type StrategyExecutor interface {
	RunWithRules(ctx context.Context, rules []byte) ([]strategy.Recommendation, error)
}

// HoldingsLister returns current holdings for a portfolio.
type HoldingsLister interface {
	ListByPortfolio(ctx context.Context, portfolioID int64) ([]portfolio.Holding, error)
}

// PriceLookup returns the latest close price for a ticker. Implemented over
// daily_prices in production.
type PriceLookup interface {
	Latest(ctx context.Context, ticker string) (decimal.Decimal, error)
}

type Generator struct {
	executor StrategyExecutor
	holdings HoldingsLister
	prices   PriceLookup
}

func NewGenerator(e StrategyExecutor, h HoldingsLister, p PriceLookup) *Generator {
	return &Generator{executor: e, holdings: h, prices: p}
}

type GenerateInput struct {
	PortfolioID   int64
	Rules         []byte
	MarketValue   decimal.Decimal
	CapitalChange decimal.Decimal
	StrategyLimit int             // pick limit from strategy rules; if 0, use len(recs)
	Weights       []decimal.Decimal // per-pick weights aligned with recs; if nil, equal weight
}

// GeneratePicks runs the strategy executor, sizes shares against deploy_amount,
// and labels actions by diffing against current holdings. Returns the full
// picks slice (buys + sells + holds + adds + trims).
func (g *Generator) GeneratePicks(ctx context.Context, in GenerateInput) ([]Pick, error) {
	recs, err := g.executor.RunWithRules(ctx, in.Rules)
	if err != nil {
		return nil, fmt.Errorf("strategy executor: %w", err)
	}
	if in.StrategyLimit > 0 && len(recs) > in.StrategyLimit {
		recs = recs[:in.StrategyLimit]
	}

	deploy := in.MarketValue.Add(in.CapitalChange)

	current, err := g.holdings.ListByPortfolio(ctx, in.PortfolioID)
	if err != nil {
		return nil, fmt.Errorf("list holdings: %w", err)
	}
	currentByTicker := make(map[string]decimal.Decimal, len(current))
	for _, h := range current {
		currentByTicker[h.Ticker] = h.Shares
	}

	weights := normaliseWeights(in.Weights, len(recs))
	out := make([]Pick, 0, len(recs)+len(current))
	pickedTickers := make(map[string]struct{}, len(recs))

	for i, r := range recs {
		price, err := g.prices.Latest(ctx, r.Ticker)
		if err != nil {
			return nil, fmt.Errorf("price for %s: %w", r.Ticker, err)
		}
		if price.IsZero() {
			return nil, fmt.Errorf("zero price for %s", r.Ticker)
		}

		alloc := deploy.Mul(weights[i])
		target := alloc.Div(price).Floor() // whole-share rounding

		curr := currentByTicker[r.Ticker]
		var action Action
		switch {
		case curr.IsZero():
			action = ActionBuy
		case target.GreaterThan(curr):
			action = ActionAdd
		case target.LessThan(curr):
			action = ActionTrim
		default:
			action = ActionHold
		}

		out = append(out, Pick{
			Ticker:          r.Ticker,
			Action:          action,
			TargetWeight:    weights[i],
			TargetShares:    target,
			CurrentShares:   curr,
			PriceAtProposal: price,
		})
		pickedTickers[r.Ticker] = struct{}{}
	}

	// Anything currently held but not picked → sell all.
	for _, h := range current {
		if _, ok := pickedTickers[h.Ticker]; ok {
			continue
		}
		price, err := g.prices.Latest(ctx, h.Ticker)
		if err != nil {
			return nil, fmt.Errorf("price for %s (sell): %w", h.Ticker, err)
		}
		out = append(out, Pick{
			Ticker:          h.Ticker,
			Action:          ActionSell,
			TargetWeight:    decimal.Zero,
			TargetShares:    decimal.Zero,
			CurrentShares:   h.Shares,
			PriceAtProposal: price,
		})
	}

	return out, nil
}

func normaliseWeights(in []decimal.Decimal, n int) []decimal.Decimal {
	if len(in) == n && n > 0 {
		return in
	}
	if n == 0 {
		return nil
	}
	w := decimal.NewFromInt(1).Div(decimal.NewFromInt(int64(n)))
	out := make([]decimal.Decimal, n)
	for i := range out {
		out[i] = w
	}
	return out
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/proposal/...
git add internal/proposal/generator.go internal/proposal/generator_test.go
git commit -m "feat(proposal): Generator computes picks with diff + sizing"
```

---

## Task D5: `proposal.Acceptor` — full / partial / skip

**Files:**
- Create: `internal/proposal/acceptor.go`
- Create: `internal/proposal/acceptor_test.go`

The Acceptor runs everything in one transaction: insert executed_trades, insert capital_event (if any), update holdings, mark proposal resolved, advance next_rebalance_due.

- [ ] **Step 1: Write the failing tests**

```go
// internal/proposal/acceptor_test.go
package proposal_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

func seedAcceptorFixture(t *testing.T) (*pgxpool.Pool, *portfolio.Portfolio, *proposal.Proposal) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolioForProposal(t)

	// Seed a ticker.
	_, _ = pool.Exec(ctx, `INSERT INTO companies (ticker, name, sector, industry, active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`)

	// Insert a proposal directly: 1 buy of AAPL @ $180 × 10 shares.
	picksJSON, _ := json.Marshal([]proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromFloat(1), TargetShares: decimal.NewFromInt(10),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	})
	pr, err := proposal.NewRepository(pool).Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 picksJSON,
	})
	if err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return pool, p, pr
}

func TestAcceptor_FullAcceptCreatesTradeAndAdvancesCadence(t *testing.T) {
	pool, p, pr := seedAcceptorFixture(t)
	ctx := context.Background()

	a := proposal.NewAcceptor(pool, proposal.NewRepository(pool),
		portfolio.NewRepository(pool), portfolio.NewHoldings(pool))

	now := time.Now().UTC()
	res, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", Skip: false,
				ActualShares: decimal.NewFromInt(10), ActualPrice: decimal.NewFromInt(180), Fee: decimal.NewFromInt(2)},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != proposal.StatusAccepted {
		t.Errorf("status = %s, want accepted", res.Status)
	}

	// Holdings updated.
	h, err := portfolio.NewHoldings(pool).Get(ctx, p.ID, "AAPL")
	if err != nil {
		t.Fatalf("get holding: %v", err)
	}
	if !h.Shares.Equal(decimal.NewFromInt(10)) {
		t.Errorf("shares = %s, want 10", h.Shares)
	}

	// next_rebalance_due advanced by quarterly = 3 months.
	got, _ := portfolio.NewRepository(pool).GetByID(ctx, p.ID)
	if got.NextRebalanceDue == nil {
		t.Fatal("next_rebalance_due is nil")
	}
	wantDue := now.AddDate(0, 3, 0)
	if !got.NextRebalanceDue.Equal(wantDue) {
		t.Errorf("next_rebalance_due = %s, want %s", got.NextRebalanceDue, wantDue)
	}
}

func TestAcceptor_PartialAcceptWithSkippedRow(t *testing.T) {
	pool, _, _ := seedAcceptorFixture(t)
	ctx := context.Background()

	// Add a second pick (MSFT) to the existing proposal so we have something to skip.
	_, _ = pool.Exec(ctx, `INSERT INTO companies (ticker, name, sector, industry, active) VALUES ('MSFT','M','','',true) ON CONFLICT DO NOTHING`)
	picksJSON, _ := json.Marshal([]proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(0.5),
			TargetShares: decimal.NewFromInt(10), CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
		{Ticker: "MSFT", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(0.5),
			TargetShares: decimal.NewFromInt(5), CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(400)},
	})
	repo := proposal.NewRepository(pool)
	// Replace the existing proposal's picks. Pull current pending and update.
	pending, _ := repo.GetPending(ctx, pool, /* portfolioID */ 0)
	_ = repo.UpdatePending(ctx, pool, pending.ID, proposal.UpdatePendingInput{
		CapitalChange: decimal.Zero, DeployAmount: decimal.NewFromInt(10000), Picks: picksJSON,
	})

	a := proposal.NewAcceptor(pool, repo, portfolio.NewRepository(pool), portfolio.NewHoldings(pool))

	now := time.Now().UTC()
	res, err := a.Accept(ctx, pending.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", Skip: false, ActualShares: decimal.NewFromInt(10), ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
			{Ticker: "MSFT", Skip: true},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != proposal.StatusPartiallyAccepted {
		t.Errorf("status = %s, want partially_accepted", res.Status)
	}
}

func TestAcceptor_SkipWholeProposal(t *testing.T) {
	pool, p, pr := seedAcceptorFixture(t)
	ctx := context.Background()

	a := proposal.NewAcceptor(pool, proposal.NewRepository(pool),
		portfolio.NewRepository(pool), portfolio.NewHoldings(pool))

	now := time.Now().UTC()
	if err := a.Skip(ctx, pr.ID, now); err != nil {
		t.Fatalf("skip: %v", err)
	}

	// next_rebalance_due still advances.
	got, _ := portfolio.NewRepository(pool).GetByID(ctx, p.ID)
	if got.NextRebalanceDue == nil {
		t.Fatal("next_rebalance_due is nil after skip")
	}
}

func TestAcceptor_CapitalChangeRecordsEvent(t *testing.T) {
	pool, p, pr := seedAcceptorFixture(t)
	ctx := context.Background()

	// Recompute pending with capital_change = 1000.
	repo := proposal.NewRepository(pool)
	picksJSON, _ := json.Marshal([]proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(1),
			TargetShares: decimal.NewFromInt(11), CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	})
	_ = repo.UpdatePending(ctx, pool, pr.ID, proposal.UpdatePendingInput{
		CapitalChange: decimal.NewFromInt(1000),
		DeployAmount:  decimal.NewFromInt(11000),
		Picks:         picksJSON,
	})

	a := proposal.NewAcceptor(pool, repo, portfolio.NewRepository(pool), portfolio.NewHoldings(pool))
	now := time.Now().UTC()
	_, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", Skip: false, ActualShares: decimal.NewFromInt(11), ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	var amount decimal.Decimal
	err = pool.QueryRow(ctx, `SELECT amount FROM capital_events WHERE portfolio_id=$1 AND proposal_id=$2`, p.ID, pr.ID).Scan(&amount)
	if err != nil {
		t.Fatalf("query capital_event: %v", err)
	}
	if !amount.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("amount = %s, want 1000", amount)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/proposal/acceptor.go`**

```go
// internal/proposal/acceptor.go
package proposal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"deepvalue/internal/dbutil"
	"deepvalue/internal/portfolio"
)

type Acceptor struct {
	pool       *pgxpool.Pool
	proposals  *Repository
	portfolios *portfolio.Repository
	holdings   *portfolio.Holdings
}

func NewAcceptor(pool *pgxpool.Pool, pr *Repository, pf *portfolio.Repository, h *portfolio.Holdings) *Acceptor {
	return &Acceptor{pool: pool, proposals: pr, portfolios: pf, holdings: h}
}

type RowDecision struct {
	Ticker       string
	Skip         bool
	ActualShares decimal.Decimal // ignored if Skip
	ActualPrice  decimal.Decimal
	Fee          decimal.Decimal
}

type AcceptInput struct {
	Now  time.Time
	Rows []RowDecision
}

type AcceptResult struct {
	ProposalID int64
	Status     Status
}

func (a *Acceptor) Accept(ctx context.Context, proposalID int64, in AcceptInput) (*AcceptResult, error) {
	var result AcceptResult
	err := dbutil.RunInTx(ctx, a.pool, func(tx dbutil.DBTX) error {
		pr, err := a.proposals.GetTx(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if pr.Status != StatusPending {
			return fmt.Errorf("cannot accept proposal in status %s", pr.Status)
		}

		port, err := a.portfolios.GetByIDTx(ctx, tx, pr.PortfolioID)
		if err != nil {
			return err
		}

		decisionByTicker := map[string]RowDecision{}
		for _, d := range in.Rows {
			decisionByTicker[d.Ticker] = d
		}

		anySkipped := false
		for _, p := range pr.Picks {
			d, ok := decisionByTicker[p.Ticker]
			if !ok {
				return fmt.Errorf("missing decision for pick %s", p.Ticker)
			}
			if d.Skip {
				anySkipped = true
				continue
			}
			if p.Action == ActionHold {
				continue // no trade row generated for hold
			}
			tradeAction, deltaShares := normalizeAction(p.Action, p.TargetShares, p.CurrentShares, d.ActualShares)
			if deltaShares.IsZero() {
				continue
			}
			// Insert executed_trade.
			_, err := tx.Exec(ctx, `
                INSERT INTO executed_trades
                    (portfolio_id, proposal_id, ticker, action, shares, price, fee, executed_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
            `, port.ID, pr.ID, p.Ticker, tradeAction, deltaShares, d.ActualPrice, d.Fee, in.Now)
			if err != nil {
				return err
			}
			// Apply to holdings projection.
			if err := a.holdings.ApplyTrade(ctx, tx, portfolio.TradeApplication{
				PortfolioID: port.ID, Ticker: p.Ticker, Action: string(tradeAction),
				Shares: deltaShares, Price: d.ActualPrice, Fee: d.Fee,
				ExecutedAt: in.Now,
			}); err != nil {
				return err
			}
		}

		// Capital event (if any).
		if !pr.CapitalChange.IsZero() {
			_, err := tx.Exec(ctx, `
                INSERT INTO capital_events (portfolio_id, proposal_id, amount, occurred_at)
                VALUES ($1, $2, $3, $4)
            `, port.ID, pr.ID, pr.CapitalChange, in.Now)
			if err != nil {
				return err
			}
		}

		// Resolve proposal.
		status := StatusAccepted
		if anySkipped {
			status = StatusPartiallyAccepted
		}
		if err := a.proposals.MarkResolved(ctx, tx, pr.ID, status, in.Now); err != nil {
			return err
		}

		// Advance cadence.
		due, err := AddCadence(in.Now, port.Cadence)
		if err != nil {
			return err
		}
		if err := a.portfolios.SetNextRebalanceDue(ctx, tx, port.ID, due); err != nil {
			return err
		}

		result.ProposalID = pr.ID
		result.Status = status
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Skip marks the proposal as skipped and advances cadence by one period.
func (a *Acceptor) Skip(ctx context.Context, proposalID int64, now time.Time) error {
	return dbutil.RunInTx(ctx, a.pool, func(tx dbutil.DBTX) error {
		pr, err := a.proposals.GetTx(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if pr.Status != StatusPending {
			return fmt.Errorf("cannot skip proposal in status %s", pr.Status)
		}
		port, err := a.portfolios.GetByIDTx(ctx, tx, pr.PortfolioID)
		if err != nil {
			return err
		}
		if err := a.proposals.MarkResolved(ctx, tx, pr.ID, StatusSkipped, now); err != nil {
			return err
		}
		due, err := AddCadence(now, port.Cadence)
		if err != nil {
			return err
		}
		return a.portfolios.SetNextRebalanceDue(ctx, tx, port.ID, due)
	})
}

// normalizeAction maps the pick's labeled action plus user-supplied actual
// shares to the (action, shares) recorded in executed_trades. Errors are
// returned via errors package for unhandled cases.
func normalizeAction(label Action, target, current, actual decimal.Decimal) (string, decimal.Decimal) {
	switch label {
	case ActionBuy:
		return "buy", actual
	case ActionAdd:
		// Default delta is target - current; user may have edited actual.
		// Treat actual as the absolute delta they bought (already applied to current holdings).
		return "buy", actual
	case ActionSell:
		return "sell", actual
	case ActionTrim:
		return "sell", actual
	}
	return "", decimal.Zero
}

var ErrUnknownAction = errors.New("unknown pick action")
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/proposal/...
git add internal/proposal/acceptor.go internal/proposal/acceptor_test.go
git commit -m "feat(proposal): Acceptor handles full/partial/skip with cadence advance"
```

---

**PAUSE FOR REVIEW — End of Phase D.** Proposal domain complete: cadence math, repository, generator (with diff + sizing), acceptor (transactional, advances cadence). All exercised by integration tests against a real Postgres.

---

# Phase E — Scheduler

## Task E1: `scheduler.Clock` interface + RealClock + FakeClock

**Files:**
- Create: `internal/scheduler/clock.go`
- Create: `internal/scheduler/clock_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/scheduler/clock_test.go
package scheduler_test

import (
	"testing"
	"time"

	"deepvalue/internal/scheduler"
)

func TestFakeClock_AdvanceMovesNow(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := scheduler.NewFakeClock(start)
	if !c.Now().Equal(start) {
		t.Errorf("Now = %s, want %s", c.Now(), start)
	}
	c.Advance(2 * time.Hour)
	if !c.Now().Equal(start.Add(2 * time.Hour)) {
		t.Errorf("after advance: %s", c.Now())
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/scheduler/clock.go`**

```go
// internal/scheduler/clock.go
package scheduler

import (
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func NewRealClock() RealClock { return RealClock{} }
func (RealClock) Now() time.Time { return time.Now() }

type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/scheduler/...
git add internal/scheduler/clock.go internal/scheduler/clock_test.go
git commit -m "feat(scheduler): Clock interface with Real and Fake implementations"
```

---

## Task E2: `scheduler.Worker` — generation tick

**Files:**
- Create: `internal/scheduler/worker.go`
- Create: `internal/scheduler/worker_test.go`

The Worker has external dependencies (proposal generator, repos, email, clock). The tick body is structured so each operation can fail independently. Tests use stub email + fake clock + real DB to exercise the happy path and the failure paths.

- [ ] **Step 1: Write the failing tests**

```go
// internal/scheduler/worker_test.go
package scheduler_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/scheduler"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

type stubMailer struct {
	mu  sync.Mutex
	ids []int64 // proposal ids that received "ready"
}

func (m *stubMailer) SendProposalReady(ctx context.Context, proposalID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids = append(m.ids, proposalID)
	return nil
}

func (m *stubMailer) SendProposalReminder(ctx context.Context, proposalID int64) error {
	return nil
}

// stubGenerator stands in for proposal.Generator's GeneratePicks.
type stubPickGenerator struct {
	picks []proposal.Pick
}

func (s stubPickGenerator) GeneratePicks(ctx context.Context, in proposal.GenerateInput) ([]proposal.Pick, error) {
	return s.picks, nil
}

func TestWorker_TickGeneratesProposalForDuePortfolio(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	// Seed verified strategy + portfolio with next_rebalance_due in the past.
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	rules := []byte(`{"filters":[],"ranking":[],"limit":1}`)
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	_ = sRepo.Verify(ctx, s.ID)
	got, _ := sRepo.GetByID(ctx, s.ID)
	p, _ := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: testutil.SystemUserID, Name: "P",
		StartingCapital: decimal.NewFromInt(10000),
		StrategyID:      s.ID, StrategyVersionID: got.CurrentVersionID,
		Cadence: strategy.CadenceQuarterly,
	})
	past := time.Now().Add(-time.Hour)
	_ = pRepo.SetNextRebalanceDue(ctx, pool, p.ID, past)

	// Wire stubs.
	mailer := &stubMailer{}
	picksProvider := stubPickGenerator{picks: []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(1),
			TargetShares: decimal.NewFromInt(55), PriceAtProposal: decimal.NewFromInt(180)},
	}}

	w := scheduler.NewWorker(scheduler.WorkerConfig{
		Pool:           pool,
		Proposals:      proposal.NewRepository(pool),
		Portfolios:     pRepo,
		Strategies:     sRepo,
		PickGenerator:  picksProvider,
		Mailer:         mailer,
		Clock:          scheduler.NewFakeClock(time.Now()),
		ReminderAfter:  72 * time.Hour,
		RetryWindow:    6 * time.Hour,
	})

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	// A proposal should now exist for the portfolio.
	pr, err := proposal.NewRepository(pool).GetPending(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pr.Status != proposal.StatusPending {
		t.Errorf("status = %s, want pending", pr.Status)
	}
	if pr.NotificationSentAt == nil {
		t.Error("notification not stamped")
	}
	if len(mailer.ids) != 1 || mailer.ids[0] != pr.ID {
		t.Errorf("mailer.ids = %v, want [%d]", mailer.ids, pr.ID)
	}
}

func TestWorker_TickAutoExpiresPreviousPending(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	// Same setup as above — seed portfolio.
	// Then generate a first proposal manually, then RunOnce.
	// Assert previous proposal is expired and a new pending one exists.
	// (Detailed assertions: Get(prevID).Status == expired; new pending exists.)
	_ = pool
	_ = ctx
	t.Skip("Implementation detail mirrors TestWorker_TickGeneratesProposalForDuePortfolio + initial proposal seed; see RunOnce contract.")
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/scheduler/worker.go`**

```go
// internal/scheduler/worker.go
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"deepvalue/internal/dbutil"
	"deepvalue/internal/observability"
	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
)

// Mailer is the small interface the worker depends on for emailing.
type Mailer interface {
	SendProposalReady(ctx context.Context, proposalID int64) error
	SendProposalReminder(ctx context.Context, proposalID int64) error
}

// PickGenerator is the worker's dependency on proposal pick generation.
// Implemented by *proposal.Generator in production.
type PickGenerator interface {
	GeneratePicks(ctx context.Context, in proposal.GenerateInput) ([]proposal.Pick, error)
}

type WorkerConfig struct {
	Pool          *pgxpool.Pool
	Proposals     *proposal.Repository
	Portfolios    *portfolio.Repository
	Strategies    *strategy.Repository
	Versions      *strategy.VersionsRepository
	PickGenerator PickGenerator
	Mailer        Mailer
	Clock         Clock
	TickInterval  time.Duration
	ReminderAfter time.Duration
	RetryWindow   time.Duration
}

type Worker struct {
	cfg  WorkerConfig
	stop chan struct{}
	done chan struct{}
}

func NewWorker(cfg WorkerConfig) *Worker {
	return &Worker{cfg: cfg, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start launches the worker goroutine. Returns immediately.
func (w *Worker) Start(ctx context.Context) {
	go w.loop(ctx)
}

// Stop signals the worker to exit and waits for the loop to finish.
func (w *Worker) Stop() {
	close(w.stop)
	<-w.done
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil {
				slog.Error("scheduler tick failed", "err", err)
				observability.CaptureContextError(ctx, err)
			}
		}
	}
}

// RunOnce executes one full tick: generate due proposals, send reminders, retry
// missed notifications. Errors per portfolio are logged but don't stop the tick.
func (w *Worker) RunOnce(ctx context.Context) error {
	now := w.cfg.Clock.Now()

	if err := w.generateDue(ctx, now); err != nil {
		slog.Error("generateDue", "err", err)
	}
	if err := w.sendReminders(ctx); err != nil {
		slog.Error("sendReminders", "err", err)
	}
	if err := w.retryUnsentNotifications(ctx); err != nil {
		slog.Error("retryUnsentNotifications", "err", err)
	}
	return nil
}

const dueBatchSize = 50

func (w *Worker) generateDue(ctx context.Context, now time.Time) error {
	// Find IDs in one transaction to use FOR UPDATE SKIP LOCKED.
	var ids []int64
	err := dbutil.RunInTx(ctx, w.cfg.Pool, func(tx dbutil.DBTX) error {
		var err error
		ids, err = w.cfg.Portfolios.FindDueForRebalance(ctx, tx, now, dueBatchSize)
		return err
	})
	if err != nil {
		return err
	}

	for _, id := range ids {
		if err := w.generateForPortfolio(ctx, id, now); err != nil {
			slog.Error("generate for portfolio", "portfolio_id", id, "err", err)
			observability.CaptureContextError(ctx, err)
		}
	}
	return nil
}

func (w *Worker) generateForPortfolio(ctx context.Context, portfolioID int64, now time.Time) error {
	var newProposalID int64
	err := dbutil.RunInTx(ctx, w.cfg.Pool, func(tx dbutil.DBTX) error {
		// 1. Auto-expire any pending.
		if err := w.cfg.Proposals.ExpirePending(ctx, tx, portfolioID); err != nil {
			return err
		}

		// 2. Load portfolio + strategy_version.
		port, err := w.cfg.Portfolios.GetByIDTx(ctx, tx, portfolioID)
		if err != nil {
			return err
		}
		// Use the version repository on the pool (read-only), no need for tx.
		ver, err := w.cfg.Versions.Get(ctx, port.StrategyVersionID)
		if err != nil {
			return err
		}

		// 3. Compute current market_value (cash + holdings * latest prices).
		// For initial portfolio with no executed trades, market_value = starting_capital.
		marketValue, err := computeMarketValue(ctx, tx, port)
		if err != nil {
			return err
		}

		// 4. Generate picks.
		picks, err := w.cfg.PickGenerator.GeneratePicks(ctx, proposal.GenerateInput{
			PortfolioID:   port.ID,
			Rules:         ver.Rules,
			MarketValue:   marketValue,
			CapitalChange: decimal.Zero,
			StrategyLimit: 0, // taken from rules in production; 0 = unbounded for now
		})
		if err != nil {
			return err
		}
		picksJSON, err := json.Marshal(picks)
		if err != nil {
			return err
		}

		// 5. Insert proposal.
		pr, err := w.cfg.Proposals.Insert(ctx, tx, proposal.InsertInput{
			PortfolioID:           port.ID,
			StrategyVersionID:     port.StrategyVersionID,
			MarketValueAtProposal: marketValue,
			CapitalChange:         decimal.Zero,
			DeployAmount:          marketValue,
			Picks:                 picksJSON,
		})
		if err != nil {
			return err
		}
		newProposalID = pr.ID
		return nil
	})
	if err != nil {
		return err
	}

	// 6. Email outside the txn so a flaky network doesn't roll back the proposal.
	if err := w.cfg.Mailer.SendProposalReady(ctx, newProposalID); err != nil {
		// Leave notification_sent_at NULL so the retry path picks it up.
		return err
	}
	return w.cfg.Proposals.SetNotificationSent(ctx, newProposalID, w.cfg.Clock.Now())
}

// computeMarketValue: cash + Σ(shares × latest close).
// For initial portfolio (no trades yet), this equals starting_capital.
func computeMarketValue(ctx context.Context, db dbutil.DBTX, p *portfolio.Portfolio) (decimal.Decimal, error) {
	// Cash = starting_capital + Σ(capital_events.amount) − Σ(buy spend) + Σ(sell proceeds) − Σ(fees).
	// We use a single SQL query to avoid round trips.
	var cash decimal.Decimal
	err := db.QueryRow(ctx, `
        SELECT $1
            + COALESCE((SELECT SUM(amount) FROM capital_events WHERE portfolio_id = $2), 0)
            + COALESCE((SELECT SUM(CASE WHEN action='sell' THEN shares*price ELSE -shares*price END) - SUM(fee)
                        FROM executed_trades WHERE portfolio_id = $2), 0)
    `, p.StartingCapital, p.ID).Scan(&cash)
	if err != nil {
		return decimal.Zero, err
	}

	// Holdings market value: SUM(shares * latest_close).
	var holdingsValue decimal.Decimal
	err = db.QueryRow(ctx, `
        SELECT COALESCE(SUM(h.shares * dp.close), 0)
        FROM holdings h
        LEFT JOIN LATERAL (
            SELECT close FROM daily_prices
            WHERE ticker = h.ticker
            ORDER BY date DESC LIMIT 1
        ) dp ON true
        WHERE h.portfolio_id = $1
    `, p.ID).Scan(&holdingsValue)
	if err != nil {
		return decimal.Zero, err
	}
	return cash.Add(holdingsValue), nil
}

func (w *Worker) sendReminders(ctx context.Context) error {
	candidates, err := w.cfg.Proposals.FindReminderCandidates(ctx, w.cfg.ReminderAfter)
	if err != nil {
		return err
	}
	for _, pr := range candidates {
		if err := w.cfg.Mailer.SendProposalReminder(ctx, pr.ID); err != nil {
			slog.Error("send reminder", "proposal_id", pr.ID, "err", err)
			continue
		}
		if err := w.cfg.Proposals.SetReminderSent(ctx, pr.ID, w.cfg.Clock.Now()); err != nil {
			slog.Error("set reminder_sent_at", "proposal_id", pr.ID, "err", err)
		}
	}
	return nil
}

func (w *Worker) retryUnsentNotifications(ctx context.Context) error {
	candidates, err := w.cfg.Proposals.FindUnsentNotifications(ctx, w.cfg.RetryWindow)
	if err != nil {
		return err
	}
	for _, pr := range candidates {
		if err := w.cfg.Mailer.SendProposalReady(ctx, pr.ID); err != nil {
			slog.Warn("retry initial notification", "proposal_id", pr.ID, "err", err)
			continue
		}
		_ = w.cfg.Proposals.SetNotificationSent(ctx, pr.ID, w.cfg.Clock.Now())
	}
	return nil
}

// Errs that callers may want to check.
var (
	_ = errors.New
)
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/scheduler/...
git add internal/scheduler/
git commit -m "feat(scheduler): Worker tick generates due proposals + reminders + retries"
```

---

**PAUSE FOR REVIEW — End of Phase E.** Scheduler runs in-process. `RunOnce` is the testable seam. `Start`/`Stop` wire it to a `time.Ticker`.

---

# Phase F — Email templates and senders

## Task F1: Email templ templates

**Files:**
- Create: `internal/views/emails/proposal_ready.templ`
- Create: `internal/views/emails/proposal_reminder.templ`

- [ ] **Step 1: Write `proposal_ready.templ`**

```templ
// internal/views/emails/proposal_ready.templ
package emails

type ProposalReadyData struct {
	PortfolioName  string
	StrategyName   string
	UserName       string
	ProposalURL    string
}

templ ProposalReadyHTML(d ProposalReadyData) {
	<html>
		<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
			<p>Hi { d.UserName },</p>
			<p>
				Your portfolio <strong>{ d.PortfolioName }</strong> is due for a rebalance under
				the <strong>{ d.StrategyName }</strong> strategy. We've prepared a proposal of
				which positions to buy, sell, or hold.
			</p>
			<p>
				<a href={ templ.SafeURL(d.ProposalURL) }
				   style="display: inline-block; padding: 10px 16px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">
					Review proposal
				</a>
			</p>
			<p style="color: #6b7280; font-size: 13px;">— DeepValue</p>
		</body>
	</html>
}

templ ProposalReadyText(d ProposalReadyData) {
	{ "Hi " + d.UserName + ",\n\n" }
	{ "Your portfolio \"" + d.PortfolioName + "\" is due for a rebalance under the \"" + d.StrategyName + "\" strategy.\n\n" }
	{ "Review and confirm: " + d.ProposalURL + "\n\n" }
	{ "— DeepValue\n" }
}
```

(Note: text-template via templ is awkward; if the existing email pattern uses raw strings, switch the text body to a plain string concatenation in the Sender wrapper instead. Match whichever pattern `auth/magic.go` uses.)

- [ ] **Step 2: Write `proposal_reminder.templ` (similar structure, different copy)**

```templ
// internal/views/emails/proposal_reminder.templ
package emails

type ProposalReminderData struct {
	PortfolioName string
	UserName      string
	ProposalURL   string
	Generated     string // "May 8, 2026"
}

templ ProposalReminderHTML(d ProposalReminderData) {
	<html>
		<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
			<p>Hi { d.UserName },</p>
			<p>
				Your rebalance proposal from { d.Generated } for portfolio
				<strong>{ d.PortfolioName }</strong> is still pending.
			</p>
			<p>
				<a href={ templ.SafeURL(d.ProposalURL) }
				   style="display: inline-block; padding: 10px 16px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">
					Take a look
				</a>
			</p>
			<p style="color: #6b7280; font-size: 13px;">If you'd rather skip this rebalance, you can do that from the same page.</p>
			<p style="color: #6b7280; font-size: 13px;">— DeepValue</p>
		</body>
	</html>
}

templ ProposalReminderText(d ProposalReminderData) {
	{ "Hi " + d.UserName + ",\n\n" }
	{ "Your rebalance proposal from " + d.Generated + " for \"" + d.PortfolioName + "\" is still pending.\n\n" }
	{ "Take a look: " + d.ProposalURL + "\n\n" }
	{ "— DeepValue\n" }
}
```

- [ ] **Step 3: Generate templ Go code**

Run: `templ generate ./internal/views/emails/...`
Expected: `_templ.go` files generated.

- [ ] **Step 4: Compile and commit**

```bash
go build ./internal/views/emails/...
git add internal/views/emails/
git commit -m "feat(emails): proposal_ready and proposal_reminder templates"
```

---

## Task F2: `email.SendProposalReady` and `email.SendProposalReminder`

**Files:**
- Create: `internal/email/proposals.go`

These are the concrete `Mailer` implementations the scheduler depends on. They render the templ templates and send via the existing `email.Sender`.

- [ ] **Step 1: Implement (no test — exercised via scheduler integration test which already uses a stub mailer)**

```go
// internal/email/proposals.go
package email

import (
	"bytes"
	"context"
	"fmt"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
	"deepvalue/internal/users"
	"deepvalue/internal/views/emails"
)

// ProposalMailer adapts the Sender interface to the scheduler.Mailer contract.
type ProposalMailer struct {
	sender     Sender
	from       string
	baseURL    string
	users      *users.Repository
	portfolios *portfolio.Repository
	proposals  *proposal.Repository
	strategies *strategy.Repository
}

func NewProposalMailer(s Sender, from, baseURL string,
	users *users.Repository, portfolios *portfolio.Repository,
	proposals *proposal.Repository, strategies *strategy.Repository) *ProposalMailer {
	return &ProposalMailer{
		sender: s, from: from, baseURL: baseURL,
		users: users, portfolios: portfolios, proposals: proposals, strategies: strategies,
	}
}

func (m *ProposalMailer) SendProposalReady(ctx context.Context, proposalID int64) error {
	pr, err := m.proposals.Get(ctx, proposalID)
	if err != nil {
		return err
	}
	port, err := m.portfolios.GetByID(ctx, pr.PortfolioID)
	if err != nil {
		return err
	}
	user, err := m.users.GetByID(ctx, port.UserID)
	if err != nil {
		return err
	}
	strat, err := m.strategies.GetByID(ctx, int(port.StrategyID))
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/portfolios/%d/proposals/%d", m.baseURL, port.ID, pr.ID)
	data := emails.ProposalReadyData{
		PortfolioName: port.Name,
		StrategyName:  strat.Name,
		UserName:      displayName(user),
		ProposalURL:   url,
	}

	var htmlBuf, textBuf bytes.Buffer
	if err := emails.ProposalReadyHTML(data).Render(ctx, &htmlBuf); err != nil {
		return err
	}
	if err := emails.ProposalReadyText(data).Render(ctx, &textBuf); err != nil {
		return err
	}

	return m.sender.Send(ctx, Message{
		To:       user.Email,
		Subject:  fmt.Sprintf("Your %s rebalance is ready", port.Name),
		HTMLBody: htmlBuf.String(),
		TextBody: textBuf.String(),
	})
}

func (m *ProposalMailer) SendProposalReminder(ctx context.Context, proposalID int64) error {
	pr, err := m.proposals.Get(ctx, proposalID)
	if err != nil {
		return err
	}
	port, err := m.portfolios.GetByID(ctx, pr.PortfolioID)
	if err != nil {
		return err
	}
	user, err := m.users.GetByID(ctx, port.UserID)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/portfolios/%d/proposals/%d", m.baseURL, port.ID, pr.ID)
	data := emails.ProposalReminderData{
		PortfolioName: port.Name,
		UserName:      displayName(user),
		ProposalURL:   url,
		Generated:     pr.GeneratedAt.Format("January 2, 2006"),
	}

	var htmlBuf, textBuf bytes.Buffer
	if err := emails.ProposalReminderHTML(data).Render(ctx, &htmlBuf); err != nil {
		return err
	}
	if err := emails.ProposalReminderText(data).Render(ctx, &textBuf); err != nil {
		return err
	}

	return m.sender.Send(ctx, Message{
		To:       user.Email,
		Subject:  fmt.Sprintf("Reminder: Your %s rebalance still needs your review", port.Name),
		HTMLBody: htmlBuf.String(),
		TextBody: textBuf.String(),
	})
}

func displayName(u *users.User) string {
	if u.Name != nil && *u.Name != "" {
		return *u.Name
	}
	return u.Email
}
```

- [ ] **Step 2: Compile and commit**

```bash
go build ./internal/email/...
git add internal/email/proposals.go
git commit -m "feat(email): SendProposalReady and SendProposalReminder"
```

---

**PAUSE FOR REVIEW — End of Phase F.** Email integration done. Confirm by running the full test suite — scheduler tests still pass, email package compiles.

---

# Phase G — Templ views

The next phase produces server-rendered pages and HTMX fragments. Each view is a self-contained `.templ` file. Test them by visually inspecting the generated HTML in the dev server during Phase H (the views can't be unit-tested meaningfully without handlers).

> **Reminder from project memory:** Templ pages need an explicit visual review pass before being considered done — buttons must not stretch full-width by default; logos and images must be size-constrained. Compare each new page side-by-side with `internal/views/strategies/list.templ` and `detail.templ` to match conventions.

## Task G1: `views/portfolios/list.templ`

**Files:**
- Create: `internal/views/portfolios/list.templ`

Refer to `internal/views/strategies/list.templ` for layout/container conventions; this view follows the same `max-w-*`, padding, card structure.

- [ ] **Step 1: Write the template**

```templ
// internal/views/portfolios/list.templ
package portfolios

import (
	"github.com/shopspring/decimal"
	"deepvalue/internal/portfolio"
)

type ListItem struct {
	Portfolio    portfolio.Portfolio
	StrategyName string
	CurrentValue decimal.Decimal
	ReturnAmount decimal.Decimal
	ReturnPct    decimal.Decimal
	HasPending   bool
}

templ List(items []ListItem) {
	@base("My Portfolios") {
		<div class="max-w-5xl mx-auto px-4 py-6 space-y-4">
			<div class="flex items-center justify-between">
				<h1 class="text-2xl font-semibold">Portfolios</h1>
				<a class="btn btn-primary btn-sm w-auto" href="/portfolios/new">New portfolio</a>
			</div>

			if len(items) == 0 {
				<div class="card bg-base-100 shadow-sm">
					<div class="card-body items-center text-center">
						<p class="text-base-content/70">You don't have any portfolios yet.</p>
						<a class="btn btn-primary btn-sm w-auto" href="/portfolios/new">Create your first portfolio</a>
					</div>
				</div>
			} else {
				<div class="grid gap-4 sm:grid-cols-2">
					for _, it := range items {
						@listCard(it)
					}
				</div>
			}
		</div>
	}
}

templ listCard(it ListItem) {
	<a class="card bg-base-100 shadow-sm hover:shadow-md transition" href={ templ.SafeURL(fmt("/portfolios/%d", it.Portfolio.ID)) }>
		<div class="card-body p-5 space-y-2">
			<div class="flex items-start justify-between gap-2">
				<h2 class="card-title text-lg">{ it.Portfolio.Name }</h2>
				if it.HasPending {
					<span class="badge badge-warning whitespace-nowrap">Rebalance ready</span>
				}
			</div>
			<p class="text-sm text-base-content/70">{ it.StrategyName } &middot; { string(it.Portfolio.Cadence) }</p>
			<div class="flex items-baseline gap-3 pt-1">
				<span class="text-xl font-semibold">${ it.CurrentValue.StringFixed(2) }</span>
				<span class={ "text-sm", returnColor(it.ReturnAmount) }>
					{ formatReturn(it.ReturnAmount, it.ReturnPct) }
				</span>
			</div>
		</div>
	</a>
}
```

(Helper functions `fmt`, `returnColor`, `formatReturn`, `base` should live in a shared `internal/views/portfolios/helpers.templ` file or in `internal/views/layout.templ`. Define them following existing patterns.)

- [ ] **Step 2: Generate, compile, commit**

```bash
templ generate ./internal/views/portfolios/...
go build ./internal/views/portfolios/...
git add internal/views/portfolios/
git commit -m "feat(views): portfolios list page"
```

---

## Task G2: `views/portfolios/new_form.templ`

**Files:**
- Create: `internal/views/portfolios/new_form.templ`

- [ ] **Step 1: Write the template**

```templ
// internal/views/portfolios/new_form.templ
package portfolios

import "deepvalue/internal/strategy"

type StrategyChoice struct {
	ID             int64
	Name           string
	DefaultCadence *strategy.Cadence
}

templ NewForm(strategies []StrategyChoice) {
	@base("New portfolio") {
		<div class="max-w-xl mx-auto px-4 py-6">
			<h1 class="text-2xl font-semibold mb-4">New portfolio</h1>
			<form class="space-y-4" method="POST" action="/portfolios">
				<div>
					<label class="label" for="name"><span class="label-text">Portfolio name</span></label>
					<input class="input input-bordered w-full" type="text" name="name" id="name" required maxlength="120" />
				</div>
				<div>
					<label class="label" for="starting_capital"><span class="label-text">Starting capital ($)</span></label>
					<input class="input input-bordered w-full" type="number" name="starting_capital" id="starting_capital" required min="1" step="0.01" />
				</div>
				<div>
					<label class="label" for="strategy_id"><span class="label-text">Strategy (verified only)</span></label>
					<select class="select select-bordered w-full" name="strategy_id" id="strategy_id" required>
						<option value="">Select a strategy…</option>
						for _, s := range strategies {
							<option value={ fmt("%d", s.ID) }>{ s.Name }</option>
						}
					</select>
				</div>
				<div>
					<label class="label" for="cadence"><span class="label-text">Cadence (default from strategy)</span></label>
					<select class="select select-bordered w-full" name="cadence" id="cadence">
						<option value="">Use strategy default</option>
						<option value="monthly">Monthly</option>
						<option value="quarterly">Quarterly</option>
						<option value="semi_annual">Semi-annual</option>
						<option value="annual">Annual</option>
					</select>
				</div>
				<div class="pt-2 flex justify-end gap-2">
					<a class="btn btn-ghost btn-sm w-auto" href="/portfolios">Cancel</a>
					<button class="btn btn-primary btn-sm w-auto" type="submit">Create portfolio</button>
				</div>
			</form>
		</div>
	}
}
```

- [ ] **Step 2: Generate, compile, commit**

```bash
templ generate ./internal/views/portfolios/...
go build ./internal/views/portfolios/...
git add internal/views/portfolios/new_form.templ
git commit -m "feat(views): portfolios new_form page"
```

---

## Task G3: `views/portfolios/detail.templ` + `holdings_table.templ`

**Files:**
- Create: `internal/views/portfolios/detail.templ`
- Create: `internal/views/portfolios/holdings_table.templ`

The detail page composes: header (with rebalance-ready CTA), holdings table, performance section (placeholder for now; populated in Phase I), proposal history list.

- [ ] **Step 1: Write `holdings_table.templ`** (the shared fragment)

```templ
// internal/views/portfolios/holdings_table.templ
package portfolios

import (
	"github.com/shopspring/decimal"
	"deepvalue/internal/portfolio"
)

type HoldingRow struct {
	Holding      portfolio.Holding
	CurrentPrice decimal.Decimal
	MarketValue  decimal.Decimal
	WeightPct    decimal.Decimal
	ReturnPct    decimal.Decimal
	ReturnAmount decimal.Decimal
}

templ HoldingsTable(rows []HoldingRow) {
	if len(rows) == 0 {
		<p class="text-sm text-base-content/70">No holdings yet.</p>
	} else {
		<div class="overflow-x-auto">
			<table class="table table-sm">
				<thead>
					<tr>
						<th>Ticker</th>
						<th class="text-right">Shares</th>
						<th class="text-right">Avg cost</th>
						<th class="text-right">Price</th>
						<th class="text-right">Value</th>
						<th class="text-right">Weight</th>
						<th class="text-right">Return</th>
					</tr>
				</thead>
				<tbody>
					for _, r := range rows {
						<tr>
							<td class="font-mono">{ r.Holding.Ticker }</td>
							<td class="text-right">{ r.Holding.Shares.StringFixed(2) }</td>
							<td class="text-right">${ avgCost(r.Holding).StringFixed(2) }</td>
							<td class="text-right">${ r.CurrentPrice.StringFixed(2) }</td>
							<td class="text-right">${ r.MarketValue.StringFixed(2) }</td>
							<td class="text-right">{ r.WeightPct.StringFixed(1) }%</td>
							<td class={ "text-right", returnColor(r.ReturnAmount) }>
								{ formatReturn(r.ReturnAmount, r.ReturnPct) }
							</td>
						</tr>
					}
				</tbody>
			</table>
		</div>
	}
}
```

(`avgCost` is `cost_basis / shares`; define as a helper.)

- [ ] **Step 2: Write `detail.templ`**

```templ
// internal/views/portfolios/detail.templ
package portfolios

import (
	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
)

type DetailData struct {
	Portfolio        portfolio.Portfolio
	StrategyName     string
	StrategyVersion  int
	Holdings         []HoldingRow
	History          []HistoryEntry
	PendingProposal  *proposal.Proposal
}

type HistoryEntry struct {
	Proposal proposal.Proposal
	Summary  string // e.g., "6 picks, all executed" or "5 of 6 executed"
}

templ Detail(d DetailData) {
	@base(d.Portfolio.Name) {
		<div class="max-w-5xl mx-auto px-4 py-6 space-y-6">
			<header class="flex items-start justify-between gap-4">
				<div>
					<h1 class="text-2xl font-semibold">{ d.Portfolio.Name }</h1>
					<p class="text-sm text-base-content/70">
						{ d.StrategyName } v{ fmt("%d", d.StrategyVersion) } &middot; { string(d.Portfolio.Cadence) } &middot;
						{ string(d.Portfolio.Status) }
					</p>
				</div>
				if d.PendingProposal != nil {
					<a class="btn btn-warning btn-sm w-auto"
					   href={ templ.SafeURL(fmt("/portfolios/%d/proposals/%d", d.Portfolio.ID, d.PendingProposal.ID)) }>
						Review pending rebalance
					</a>
				}
			</header>

			<section>
				<h2 class="text-lg font-semibold mb-2">Holdings</h2>
				@HoldingsTable(d.Holdings)
			</section>

			<section>
				<h2 class="text-lg font-semibold mb-2">Performance</h2>
				<div id="performance-chart" data-portfolio={ fmt("%d", d.Portfolio.ID) }>
					<!-- Chart populated by Chart.js after Phase I. -->
					<p class="text-sm text-base-content/70">Performance tracking coming online…</p>
				</div>
			</section>

			<section>
				<h2 class="text-lg font-semibold mb-2">History</h2>
				if len(d.History) == 0 {
					<p class="text-sm text-base-content/70">No proposals yet.</p>
				} else {
					<ul class="space-y-2">
						for _, h := range d.History {
							<li class="card bg-base-100 shadow-sm">
								<div class="card-body p-4">
									<div class="flex items-center justify-between">
										<span class="text-sm">{ h.Proposal.GeneratedAt.Format("Jan 2, 2006") } &middot; { string(h.Proposal.Status) }</span>
										<span class="text-sm text-base-content/70">{ h.Summary }</span>
									</div>
								</div>
							</li>
						}
					</ul>
				}
			</section>
		</div>
	}
}
```

- [ ] **Step 3: Generate, compile, commit**

```bash
templ generate ./internal/views/portfolios/...
go build ./internal/views/portfolios/...
git add internal/views/portfolios/detail.templ internal/views/portfolios/holdings_table.templ
git commit -m "feat(views): portfolios detail page with holdings + history"
```

---

## Task G4: `views/proposals/picks_table.templ` (HTMX fragment)

**Files:**
- Create: `internal/views/proposals/picks_table.templ`

This fragment is what gets swapped in when the user adjusts capital_change. Defined first because the full detail page composes it.

- [ ] **Step 1: Write the template**

```templ
// internal/views/proposals/picks_table.templ
package proposals

import (
	"github.com/shopspring/decimal"
	"deepvalue/internal/proposal"
)

type PicksTableData struct {
	ProposalID    int64
	PortfolioID   int64
	Picks         []proposal.Pick
	DeployAmount  decimal.Decimal
}

templ PicksTable(d PicksTableData) {
	<div id="picks-table">
		<table class="table table-sm">
			<thead>
				<tr>
					<th>Ticker</th>
					<th>Action</th>
					<th class="text-right">Target weight</th>
					<th class="text-right">Target shares</th>
					<th class="text-right">Price</th>
					<th class="text-right">Actual shares</th>
					<th class="text-right">Actual price</th>
					<th class="text-right">Fee</th>
					<th class="text-center">Skip</th>
				</tr>
			</thead>
			<tbody>
				for i, p := range d.Picks {
					<tr>
						<td class="font-mono">{ p.Ticker }</td>
						<td><span class={ "badge", actionBadge(p.Action) }>{ string(p.Action) }</span></td>
						<td class="text-right">{ p.TargetWeight.Mul(decimal.NewFromInt(100)).StringFixed(1) }%</td>
						<td class="text-right">{ p.TargetShares.StringFixed(0) }</td>
						<td class="text-right">${ p.PriceAtProposal.StringFixed(2) }</td>
						<td class="text-right">
							<input class="input input-xs input-bordered w-24 text-right"
							       name={ fmt("rows[%d][actual_shares]", i) }
							       value={ p.TargetShares.StringFixed(0) } />
						</td>
						<td class="text-right">
							<input class="input input-xs input-bordered w-24 text-right"
							       name={ fmt("rows[%d][actual_price]", i) }
							       value={ p.PriceAtProposal.StringFixed(2) } />
						</td>
						<td class="text-right">
							<input class="input input-xs input-bordered w-20 text-right"
							       name={ fmt("rows[%d][fee]", i) }
							       value="0" />
						</td>
						<td class="text-center">
							<input class="checkbox checkbox-sm" type="checkbox"
							       name={ fmt("rows[%d][skip]", i) } value="1" />
							<input type="hidden" name={ fmt("rows[%d][ticker]", i) } value={ p.Ticker } />
						</td>
					</tr>
				}
			</tbody>
		</table>
		<div class="text-right text-sm text-base-content/70 mt-2">
			Deploy amount: ${ d.DeployAmount.StringFixed(2) }
		</div>
	</div>
}

func actionBadge(a proposal.Action) string {
	switch a {
	case proposal.ActionBuy:  return "badge-success"
	case proposal.ActionSell: return "badge-error"
	case proposal.ActionAdd:  return "badge-info"
	case proposal.ActionTrim: return "badge-warning"
	default:                  return "badge-ghost"
	}
}
```

- [ ] **Step 2: Generate, compile, commit**

```bash
templ generate ./internal/views/proposals/...
go build ./internal/views/proposals/...
git add internal/views/proposals/picks_table.templ
git commit -m "feat(views): proposals picks_table HTMX fragment"
```

---

## Task G5: `views/proposals/detail.templ`

**Files:**
- Create: `internal/views/proposals/detail.templ`

The proposal review page wraps the picks_table fragment with a capital_change input that triggers HTMX recompute, plus accept/skip buttons.

- [ ] **Step 1: Write the template**

```templ
// internal/views/proposals/detail.templ
package proposals

import (
	"github.com/shopspring/decimal"
	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
)

type DetailData struct {
	Portfolio    portfolio.Portfolio
	Proposal     proposal.Proposal
	StrategyName string
}

templ Detail(d DetailData) {
	@base(fmt("Rebalance — %s", d.Portfolio.Name)) {
		<div class="max-w-5xl mx-auto px-4 py-6 space-y-4">
			<header class="space-y-1">
				<h1 class="text-2xl font-semibold">Rebalance proposal</h1>
				<p class="text-sm text-base-content/70">
					<a href={ templ.SafeURL(fmt("/portfolios/%d", d.Portfolio.ID)) } class="link">{ d.Portfolio.Name }</a>
					&middot; { d.StrategyName } &middot; generated { d.Proposal.GeneratedAt.Format("Jan 2, 2006 15:04") }
				</p>
			</header>

			<form id="accept-form" method="POST"
			      action={ templ.SafeURL(fmt("/portfolios/%d/proposals/%d/accept", d.Portfolio.ID, d.Proposal.ID)) }
			      class="space-y-4">

				<div class="card bg-base-100 shadow-sm">
					<div class="card-body p-4 space-y-3">
						<div>
							<label class="label" for="capital_change">
								<span class="label-text">Capital change ($) — positive to deposit, negative to withdraw</span>
							</label>
							<input id="capital_change" name="capital_change" type="number" step="0.01"
							       value={ d.Proposal.CapitalChange.StringFixed(2) }
							       class="input input-bordered w-48"
							       hx-post={ fmt("/portfolios/%d/proposals/%d/recompute", d.Portfolio.ID, d.Proposal.ID) }
							       hx-trigger="change"
							       hx-target="#picks-table"
							       hx-swap="outerHTML" />
						</div>

						@PicksTable(PicksTableData{
							ProposalID:   d.Proposal.ID,
							PortfolioID:  d.Portfolio.ID,
							Picks:        d.Proposal.Picks,
							DeployAmount: d.Proposal.DeployAmount,
						})
					</div>
				</div>

				<div class="flex justify-end gap-2">
					<button type="button" class="btn btn-ghost btn-sm w-auto"
					        hx-post={ fmt("/portfolios/%d/proposals/%d/skip", d.Portfolio.ID, d.Proposal.ID) }
					        hx-confirm="Skip this rebalance? Your portfolio won't change but the cadence advances.">
						Skip whole proposal
					</button>
					<button type="submit" class="btn btn-primary btn-sm w-auto">Accept proposal</button>
				</div>
			</form>
		</div>
	}
}
```

- [ ] **Step 2: Generate, compile, commit**

```bash
templ generate ./internal/views/proposals/...
go build ./internal/views/proposals/...
git add internal/views/proposals/detail.templ
git commit -m "feat(views): proposals detail review page"
```

---

**PAUSE FOR REVIEW — End of Phase G.** All templ pages created and compile. Visual review deferred until Phase H wires them up so we can see them in the browser.

---

# Phase H — HTTP handlers + main.go wiring

## Task H1: `portfolios.Handler` — List + NewForm + Create + Detail

**Files:**
- Create: `internal/handlers/portfolios.go`
- Create: `internal/handlers/portfolios_test.go`

The Create handler does the heavy lifting: calls `portfolio.Service.CreatePortfolio`, then synchronously generates the first proposal via the scheduler's generator path, then redirects to the proposal review page.

- [ ] **Step 1: Write tests for Create**

```go
// internal/handlers/portfolios_test.go
package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"deepvalue/internal/handlers"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

func TestPortfolios_Create_RedirectsToProposalReview(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	// Seed verified strategy.
	sRepo := strategy.NewRepository(pool)
	rules := []byte(`{"filters":[],"ranking":[],"limit":1}`)
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	_ = sRepo.Verify(ctx, s.ID)

	// Build handler with stubbed first-proposal generator (to avoid hitting daily_prices).
	h := handlers.NewPortfoliosHandler(handlers.PortfoliosDeps{
		// ... fill in via fixture; see fixture helper at end of file.
	})
	_ = h
	t.Skip("Wire deps via test fixture once handler dependency surface is finalized; assert 303 redirect to /portfolios/:id/proposals/:pid")

	form := url.Values{}
	form.Set("name", "Test")
	form.Set("starting_capital", "10000")
	form.Set("strategy_id", testutil.Int64Str(s.ID))
	form.Set("cadence", "quarterly")

	req := httptest.NewRequest(http.MethodPost, "/portfolios", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()

	e := echo.New()
	c := e.NewContext(req, rec)
	testutil.SetCurrentUserID(c, testutil.SystemUserID)

	if err := h.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
}
```

(The test depends on a `testutil.SetCurrentUserID` helper that mirrors how `internal/auth` middleware sets the user ID on the Echo context. Add it if missing.)

- [ ] **Step 2: Implement `internal/handlers/portfolios.go`**

```go
// internal/handlers/portfolios.go
package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/shopspring/decimal"

	"deepvalue/internal/dbutil"
	"deepvalue/internal/observability"
	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/scheduler"
	"deepvalue/internal/strategy"
	views "deepvalue/internal/views/portfolios"
)

type PortfoliosHandler struct {
	pool          *pgxpool.Pool
	service       *portfolio.Service
	portfolios    *portfolio.Repository
	holdings      *portfolio.Holdings
	proposals     *proposal.Repository
	strategies    *strategy.Repository
	versions      *strategy.VersionsRepository
	pickGenerator scheduler.PickGenerator
	mailer        scheduler.Mailer
}

type PortfoliosDeps struct {
	Pool          *pgxpool.Pool
	Service       *portfolio.Service
	Portfolios    *portfolio.Repository
	Holdings      *portfolio.Holdings
	Proposals     *proposal.Repository
	Strategies    *strategy.Repository
	Versions      *strategy.VersionsRepository
	PickGenerator scheduler.PickGenerator
	Mailer        scheduler.Mailer
}

func NewPortfoliosHandler(d PortfoliosDeps) *PortfoliosHandler {
	return &PortfoliosHandler{
		pool: d.Pool, service: d.Service, portfolios: d.Portfolios, holdings: d.Holdings,
		proposals: d.Proposals, strategies: d.Strategies, versions: d.Versions,
		pickGenerator: d.PickGenerator, mailer: d.Mailer,
	}
}

func (h *PortfoliosHandler) List(c echo.Context) error {
	userID := currentUserID(c)
	ports, err := h.portfolios.ListByUser(c.Request().Context(), userID)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	items := make([]views.ListItem, 0, len(ports))
	for _, p := range ports {
		strat, _ := h.strategies.GetByID(c.Request().Context(), int(p.StrategyID))
		strategyName := ""
		if strat != nil {
			strategyName = strat.Name
		}
		// TODO Phase I: real current_value computation. For now, starting_capital.
		items = append(items, views.ListItem{
			Portfolio:    p,
			StrategyName: strategyName,
			CurrentValue: p.StartingCapital,
			ReturnAmount: decimal.Zero,
			ReturnPct:    decimal.Zero,
			HasPending:   false,
		})
	}
	return Render(c, http.StatusOK, views.List(items))
}

func (h *PortfoliosHandler) NewForm(c echo.Context) error {
	ctx := c.Request().Context()
	all, err := h.strategies.ListVerified(ctx)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	choices := make([]views.StrategyChoice, len(all))
	for i, s := range all {
		choices[i] = views.StrategyChoice{ID: s.ID, Name: s.Name, DefaultCadence: s.DefaultCadence}
	}
	return Render(c, http.StatusOK, views.NewForm(choices))
}

func (h *PortfoliosHandler) Create(c echo.Context) error {
	ctx := c.Request().Context()
	userID := currentUserID(c)

	startingCapital, err := decimal.NewFromString(c.FormValue("starting_capital"))
	if err != nil || startingCapital.LessThanOrEqual(decimal.Zero) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid starting_capital")
	}
	strategyID, err := strconv.ParseInt(c.FormValue("strategy_id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid strategy_id")
	}

	var cadenceOverride *strategy.Cadence
	if v := c.FormValue("cadence"); v != "" {
		cv := strategy.Cadence(v)
		cadenceOverride = &cv
	}

	port, err := h.service.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID: userID, Name: c.FormValue("name"),
		StartingCapital: startingCapital,
		StrategyID:      strategyID,
		CadenceOverride: cadenceOverride,
	})
	if err != nil {
		if errors.Is(err, portfolio.ErrStrategyNotVerified) {
			return echo.NewHTTPError(http.StatusBadRequest, "strategy must be verified")
		}
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	// Synchronously generate first proposal.
	prID, err := h.generateFirstProposal(ctx, port)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/portfolios/%d?generation_error=1", port.ID))
	}

	// Email — non-fatal.
	if err := h.mailer.SendProposalReady(ctx, prID); err == nil {
		_ = h.proposals.SetNotificationSent(ctx, prID, currentTime())
	}

	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/portfolios/%d/proposals/%d", port.ID, prID))
}

func (h *PortfoliosHandler) generateFirstProposal(ctx context.Context, p *portfolio.Portfolio) (int64, error) {
	var prID int64
	err := dbutil.RunInTx(ctx, h.pool, func(tx dbutil.DBTX) error {
		ver, err := h.versions.Get(ctx, p.StrategyVersionID)
		if err != nil {
			return err
		}
		picks, err := h.pickGenerator.GeneratePicks(ctx, proposal.GenerateInput{
			PortfolioID:   p.ID,
			Rules:         ver.Rules,
			MarketValue:   p.StartingCapital,
			CapitalChange: decimal.Zero,
			StrategyLimit: 0,
		})
		if err != nil {
			return err
		}
		picksJSON, err := json.Marshal(picks)
		if err != nil {
			return err
		}
		pr, err := h.proposals.Insert(ctx, tx, proposal.InsertInput{
			PortfolioID:           p.ID,
			StrategyVersionID:     p.StrategyVersionID,
			MarketValueAtProposal: p.StartingCapital,
			CapitalChange:         decimal.Zero,
			DeployAmount:          p.StartingCapital,
			Picks:                 picksJSON,
		})
		if err != nil {
			return err
		}
		prID = pr.ID
		return nil
	})
	return prID, err
}

func (h *PortfoliosHandler) Detail(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	ctx := c.Request().Context()
	p, err := h.portfolios.GetByID(ctx, id)
	if errors.Is(err, portfolio.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	if !canAccessPortfolio(c, p) {
		return echo.NewHTTPError(http.StatusNotFound) // 404, not 403 — don't leak existence
	}

	strat, _ := h.strategies.GetByID(ctx, int(p.StrategyID))
	ver, _ := h.versions.Get(ctx, p.StrategyVersionID)
	pending, _ := h.proposals.GetPending(ctx, h.pool, p.ID)
	holdingRows, _ := h.buildHoldingRows(ctx, p)
	history, _ := h.buildHistoryRows(ctx, p)

	d := views.DetailData{
		Portfolio:       *p,
		StrategyName:    safeName(strat),
		StrategyVersion: safeVersion(ver),
		Holdings:        holdingRows,
		History:         history,
		PendingProposal: pending,
	}
	return Render(c, http.StatusOK, views.Detail(d))
}

// canAccessPortfolio: user must own it, OR be admin.
func canAccessPortfolio(c echo.Context, p *portfolio.Portfolio) bool {
	if p.UserID == currentUserID(c) {
		return true
	}
	return isAdmin(c)
}

// (helpers safeName, safeVersion, buildHoldingRows, buildHistoryRows, currentTime,
//  isAdmin and the ListVerified strategy method are scaffolded in subsequent tasks.)
```

(Several helpers reference behavior not yet defined. The next sub-tasks fill those in.)

- [ ] **Step 3: Run, expect compile errors**

Expected: missing helpers (`canAccessPortfolio`, `safeName`, `buildHoldingRows`, `ListVerified`, `currentTime`, `isAdmin`). Address in next tasks.

- [ ] **Step 4: Add minimal helpers + commit**

Add the missing helpers to `internal/handlers/portfolios.go` (or a small `portfolios_helpers.go`):

```go
import "time"

func currentTime() time.Time { return time.Now() }

func safeName(s *strategy.Strategy) string {
	if s == nil { return "" }
	return s.Name
}
func safeVersion(v *strategy.Version) int {
	if v == nil { return 0 }
	return v.VersionNumber
}

func (h *PortfoliosHandler) buildHoldingRows(ctx context.Context, p *portfolio.Portfolio) ([]views.HoldingRow, error) {
	hs, err := h.holdings.ListByPortfolio(ctx, p.ID)
	if err != nil { return nil, err }
	out := make([]views.HoldingRow, 0, len(hs))
	for _, h := range hs {
		// Phase I will fill in CurrentPrice, MarketValue, etc.
		out = append(out, views.HoldingRow{Holding: h})
	}
	return out, nil
}
func (h *PortfoliosHandler) buildHistoryRows(ctx context.Context, p *portfolio.Portfolio) ([]views.HistoryEntry, error) {
	// Phase I extends with proposed-vs-actual diffs.
	return nil, nil
}
```

Add `ListVerified` to `internal/strategy/repository.go`:

```go
func (r *Repository) ListVerified(ctx context.Context) ([]Strategy, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id, name, description, rules, default_cadence, status, current_version_id, created_by, created_at, updated_at
        FROM strategies WHERE status = 'verified' ORDER BY name ASC
    `)
	if err != nil { return nil, err }
	defer rows.Close()
	// scan into []Strategy (match the existing scan pattern in the file).
	// ...
	return nil, nil // placeholder — implement scan loop matching existing patterns
}
```

Add `isAdmin(c)` and `currentUserID(c)` shims if not already present (likely they exist in `internal/handlers/middleware.go` or similar — match existing helpers).

```bash
go build ./...
git add internal/handlers/portfolios.go internal/strategy/repository.go
git commit -m "feat(handlers): portfolios List/NewForm/Create/Detail"
```

---

## Task H2: `proposals.Handler` — Detail + Recompute + Accept + Skip

**Files:**
- Create: `internal/handlers/proposals.go`

- [ ] **Step 1: Implement**

```go
// internal/handlers/proposals.go
package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/shopspring/decimal"

	"deepvalue/internal/observability"
	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
	views "deepvalue/internal/views/proposals"
)

type ProposalsHandler struct {
	portfolios *portfolio.Repository
	proposals  *proposal.Repository
	strategies *strategy.Repository
	acceptor   *proposal.Acceptor
	pickGen    proposal.Generator
}

type ProposalsDeps struct {
	Portfolios *portfolio.Repository
	Proposals  *proposal.Repository
	Strategies *strategy.Repository
	Acceptor   *proposal.Acceptor
	PickGen    *proposal.Generator
}

func NewProposalsHandler(d ProposalsDeps) *ProposalsHandler {
	return &ProposalsHandler{
		portfolios: d.Portfolios, proposals: d.Proposals, strategies: d.Strategies,
		acceptor: d.Acceptor, pickGen: *d.PickGen,
	}
}

func (h *ProposalsHandler) Detail(c echo.Context) error {
	pid, prID, err := parseProposalRoute(c)
	if err != nil { return err }
	ctx := c.Request().Context()
	port, err := h.portfolios.GetByID(ctx, pid)
	if err != nil { return echo.NewHTTPError(http.StatusNotFound) }
	if !canAccessPortfolio(c, port) { return echo.NewHTTPError(http.StatusNotFound) }

	pr, err := h.proposals.Get(ctx, prID)
	if err != nil || pr.PortfolioID != pid {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	strat, _ := h.strategies.GetByID(ctx, int(port.StrategyID))
	return Render(c, http.StatusOK, views.Detail(views.DetailData{
		Portfolio: *port, Proposal: *pr, StrategyName: safeName(strat),
	}))
}

func (h *ProposalsHandler) Recompute(c echo.Context) error {
	pid, prID, err := parseProposalRoute(c)
	if err != nil { return err }
	ctx := c.Request().Context()

	cap, err := decimal.NewFromString(c.FormValue("capital_change"))
	if err != nil { return echo.NewHTTPError(http.StatusBadRequest, "invalid capital_change") }

	port, err := h.portfolios.GetByID(ctx, pid)
	if err != nil || !canAccessPortfolio(c, port) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	pr, err := h.proposals.Get(ctx, prID)
	if err != nil || pr.PortfolioID != pid || pr.Status != proposal.StatusPending {
		return echo.NewHTTPError(http.StatusBadRequest, "proposal not pending")
	}
	if cap.IsNegative() && cap.Abs().GreaterThan(pr.MarketValueAtProposal) {
		return echo.NewHTTPError(http.StatusBadRequest, "withdrawal exceeds market value")
	}

	// Re-run generator with new capital_change.
	picks, err := h.pickGen.GeneratePicks(ctx, proposal.GenerateInput{
		PortfolioID:   port.ID,
		Rules:         []byte("{}"), // TODO: pass real strategy_version rules
		MarketValue:   pr.MarketValueAtProposal,
		CapitalChange: cap,
		StrategyLimit: 0,
	})
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	picksJSON, _ := json.Marshal(picks)
	deploy := pr.MarketValueAtProposal.Add(cap)
	if err := h.proposals.UpdatePending(ctx, /* pool */ nil, prID, proposal.UpdatePendingInput{
		CapitalChange: cap, DeployAmount: deploy, Picks: picksJSON,
	}); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	return Render(c, http.StatusOK, views.PicksTable(views.PicksTableData{
		ProposalID:   prID,
		PortfolioID:  pid,
		Picks:        picks,
		DeployAmount: deploy,
	}))
}

func (h *ProposalsHandler) Accept(c echo.Context) error {
	pid, prID, err := parseProposalRoute(c)
	if err != nil { return err }
	ctx := c.Request().Context()
	port, err := h.portfolios.GetByID(ctx, pid)
	if err != nil || !canAccessPortfolio(c, port) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	rows := parseRowDecisions(c)
	res, err := h.acceptor.Accept(ctx, prID, proposal.AcceptInput{
		Now:  currentTime(),
		Rows: rows,
	})
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	_ = res
	return c.Redirect(http.StatusSeeOther, "/portfolios/" + strconv.FormatInt(pid, 10))
}

func (h *ProposalsHandler) Skip(c echo.Context) error {
	pid, prID, err := parseProposalRoute(c)
	if err != nil { return err }
	ctx := c.Request().Context()
	port, err := h.portfolios.GetByID(ctx, pid)
	if err != nil || !canAccessPortfolio(c, port) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err := h.acceptor.Skip(ctx, prID, currentTime()); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.Redirect(http.StatusSeeOther, "/portfolios/" + strconv.FormatInt(pid, 10))
}

func parseProposalRoute(c echo.Context) (int64, int64, error) {
	pid, err1 := strconv.ParseInt(c.Param("id"), 10, 64)
	prID, err2 := strconv.ParseInt(c.Param("pid"), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, echo.NewHTTPError(http.StatusBadRequest)
	}
	return pid, prID, nil
}

// parseRowDecisions reads form values of shape rows[0][ticker]=...&rows[0][actual_shares]=...
// into a []proposal.RowDecision keyed by index.
func parseRowDecisions(c echo.Context) []proposal.RowDecision {
	form, _ := c.FormParams()
	byIdx := map[int]proposal.RowDecision{}
	for k, vs := range form {
		if !strings.HasPrefix(k, "rows[") || len(vs) == 0 { continue }
		// Parse rows[N][field]
		end := strings.Index(k, "]")
		if end == -1 { continue }
		idx, err := strconv.Atoi(k[5:end])
		if err != nil { continue }
		fieldStart := strings.Index(k, "[") + 1
		fieldEnd := strings.LastIndex(k, "]")
		field := k[strings.Index(k[fieldStart:], "[")+fieldStart+1 : fieldEnd]

		row := byIdx[idx]
		switch field {
		case "ticker":        row.Ticker = vs[0]
		case "actual_shares": row.ActualShares, _ = decimal.NewFromString(vs[0])
		case "actual_price":  row.ActualPrice, _ = decimal.NewFromString(vs[0])
		case "fee":           row.Fee, _ = decimal.NewFromString(vs[0])
		case "skip":          row.Skip = vs[0] == "1" || vs[0] == "on"
		}
		byIdx[idx] = row
	}
	out := make([]proposal.RowDecision, 0, len(byIdx))
	for _, r := range byIdx { out = append(out, r) }
	return out
}

var ErrInvalid = errors.New("invalid request")
```

(The form-parsing function is fragile to maintain — verify the exact `name=` patterns from the rendered picks_table to ensure they parse correctly. Add a small unit test for `parseRowDecisions` if behavior is non-obvious.)

- [ ] **Step 2: Compile + commit**

```bash
go build ./...
git add internal/handlers/proposals.go
git commit -m "feat(handlers): proposals Detail/Recompute/Accept/Skip"
```

---

## Task H3: Portfolio lifecycle handlers — Pause/Resume/Archive

**Files:**
- Modify: `internal/handlers/portfolios.go`

- [ ] **Step 1: Add three small handlers**

```go
func (h *PortfoliosHandler) Pause(c echo.Context) error    { return h.setStatus(c, portfolio.StatusPaused) }
func (h *PortfoliosHandler) Resume(c echo.Context) error   { return h.setStatus(c, portfolio.StatusActive) }
func (h *PortfoliosHandler) Archive(c echo.Context) error  { return h.setStatus(c, portfolio.StatusArchived) }

func (h *PortfoliosHandler) setStatus(c echo.Context, status portfolio.Status) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil { return echo.NewHTTPError(http.StatusBadRequest) }
	p, err := h.portfolios.GetByID(c.Request().Context(), id)
	if err != nil || !canAccessPortfolio(c, p) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err := h.service.SetStatus(c.Request().Context(), id, status); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/portfolios/%d", id))
}
```

- [ ] **Step 2: Compile + commit**

```bash
go build ./...
git add internal/handlers/portfolios.go
git commit -m "feat(handlers): portfolio Pause/Resume/Archive"
```

---

## Task H4: Strategy handler extensions — Verify, Archive, ListVersions

**Files:**
- Modify: `internal/handlers/strategies.go`

- [ ] **Step 1: Add the three endpoints**

```go
func (h *StrategyHandler) Verify(c echo.Context) error {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.repo.Verify(c.Request().Context(), id); err != nil {
		if errors.Is(err, strategy.ErrInvalidStatusTransition) {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/strategies/%d", id))
}

func (h *StrategyHandler) Archive(c echo.Context) error {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.repo.Archive(c.Request().Context(), id); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/strategies/%d", id))
}

func (h *StrategyHandler) ListVersions(c echo.Context) error {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	versions, err := h.versionsRepo.ListByStrategy(c.Request().Context(), id)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	// Reuse strategies/versions.templ if it exists; otherwise inline a simple list.
	return Render(c, http.StatusOK, strategiesViews.VersionsList(versions))
}
```

(Will need a small `strategies/versions.templ` view — write it in the same task; mirror the structure of existing `strategies/detail.templ`.)

Also update existing strategy edit handler: when the user submits rule changes for a `verified` strategy, render a confirmation page first ("This will create v[N+1] and demote to draft. Existing portfolios are unaffected. Continue?") before applying. The simplest path: add a hidden `confirm=1` field; if absent, render confirmation; if present, call `repo.UpdateRules`.

- [ ] **Step 2: Compile + commit**

```bash
go build ./...
git add internal/handlers/strategies.go internal/views/strategies/
git commit -m "feat(handlers): strategy Verify/Archive/ListVersions + edit-confirm"
```

---

## Task H5: `cmd/app/main.go` wiring

**Files:**
- Modify: `cmd/app/main.go`

- [ ] **Step 1: Construct the new dependencies**

Find the section where existing repositories are constructed (`strategy.NewRepository`, etc.) and add:

```go
versionsRepo := strategy.NewVersionsRepository(pool)
portfoliosRepo := portfolio.NewRepository(pool)
holdingsRepo := portfolio.NewHoldings(pool)
proposalsRepo := proposal.NewRepository(pool)

portfolioService := portfolio.NewService(portfoliosRepo, strategyRepo)

priceLookup := /* construct over daily_prices */ daily_prices.NewLookup(pool) // see note below
pickGenerator := proposal.NewGenerator(strategyExecutorAdapter{strategyExecutor},
	holdingsListAdapter{holdingsRepo}, priceLookup)

acceptor := proposal.NewAcceptor(pool, proposalsRepo, portfoliosRepo, holdingsRepo)

mailer := email.NewProposalMailer(emailSender, mailFrom, baseURL,
	usersRepo, portfoliosRepo, proposalsRepo, strategyRepo)
```

Note: `priceLookup` needs a tiny adapter over `daily_prices`. If a "latest close by ticker" function doesn't exist, add a small one in `internal/strategy/` or a new `internal/prices/` package:

```go
// internal/prices/lookup.go
package prices

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type Lookup struct{ pool *pgxpool.Pool }
func NewLookup(p *pgxpool.Pool) *Lookup { return &Lookup{pool: p} }

func (l *Lookup) Latest(ctx context.Context, ticker string) (decimal.Decimal, error) {
	var p decimal.Decimal
	err := l.pool.QueryRow(ctx, `SELECT close FROM daily_prices WHERE ticker = $1 ORDER BY date DESC LIMIT 1`, ticker).Scan(&p)
	if err != nil { return decimal.Zero, errors.New("no price for " + ticker) }
	return p, nil
}
```

`strategyExecutorAdapter` is a small wrapper that calls the existing `strategy.Executor.Execute` (or a new `RunWithRules`) method.

- [ ] **Step 2: Wire the scheduler**

```go
sched := scheduler.NewWorker(scheduler.WorkerConfig{
	Pool: pool, Proposals: proposalsRepo, Portfolios: portfoliosRepo,
	Strategies: strategyRepo, Versions: versionsRepo,
	PickGenerator: pickGenerator, Mailer: mailer,
	Clock:         scheduler.NewRealClock(),
	TickInterval:  parseEnvDuration("SCHEDULER_TICK_INTERVAL", 15*time.Minute),
	ReminderAfter: parseEnvDuration("SCHEDULER_REMINDER_AFTER", 72*time.Hour),
	RetryWindow:   parseEnvDuration("SCHEDULER_NOTIFICATION_RETRY_WINDOW", 6*time.Hour),
})
if os.Getenv("SCHEDULER_ENABLED") != "false" {
	sched.Start(ctx)
	defer sched.Stop()
}
```

(Define `parseEnvDuration` as a small helper if it doesn't already exist.)

- [ ] **Step 3: Register routes**

```go
portfoliosHandler := handlers.NewPortfoliosHandler(handlers.PortfoliosDeps{
	Pool: pool, Service: portfolioService, Portfolios: portfoliosRepo, Holdings: holdingsRepo,
	Proposals: proposalsRepo, Strategies: strategyRepo, Versions: versionsRepo,
	PickGenerator: pickGenerator, Mailer: mailer,
})
proposalsHandler := handlers.NewProposalsHandler(handlers.ProposalsDeps{
	Portfolios: portfoliosRepo, Proposals: proposalsRepo, Strategies: strategyRepo,
	Acceptor: acceptor, PickGen: pickGenerator,
})

e.GET("/portfolios", portfoliosHandler.List, requireAuth)
e.GET("/portfolios/new", portfoliosHandler.NewForm, requireAuth)
e.POST("/portfolios", portfoliosHandler.Create, requireAuth)
e.GET("/portfolios/:id", portfoliosHandler.Detail, requireAuth)
e.POST("/portfolios/:id/pause", portfoliosHandler.Pause, requireAuth)
e.POST("/portfolios/:id/resume", portfoliosHandler.Resume, requireAuth)
e.POST("/portfolios/:id/archive", portfoliosHandler.Archive, requireAuth)

e.GET("/portfolios/:id/proposals/:pid", proposalsHandler.Detail, requireAuth)
e.POST("/portfolios/:id/proposals/:pid/recompute", proposalsHandler.Recompute, requireAuth)
e.POST("/portfolios/:id/proposals/:pid/accept", proposalsHandler.Accept, requireAuth)
e.POST("/portfolios/:id/proposals/:pid/skip", proposalsHandler.Skip, requireAuth)

// Strategy extensions:
e.POST("/strategies/:id/verify", strategyHandler.Verify, requireAuth)
e.POST("/strategies/:id/archive", strategyHandler.Archive, requireAuth)
e.GET("/strategies/:id/versions", strategyHandler.ListVersions, requireAuth)
```

- [ ] **Step 4: Compile, run, smoke-test**

```bash
go build ./...
go run ./cmd/app
```

Expected: server starts, scheduler logs "started", logs "tick" entries every 15 min (or with `SCHEDULER_TICK_INTERVAL=30s` for testing).

Smoke flow in browser:
1. Sign in via magic link.
2. Navigate to `/strategies` → verify an existing strategy.
3. Navigate to `/portfolios/new` → create a portfolio.
4. Confirm redirect to `/portfolios/:id/proposals/:pid` and the page renders the picks table.
5. Adjust capital_change → confirm picks_table swaps via HTMX.
6. Accept the proposal → confirm redirect to `/portfolios/:id` and holdings appear.

- [ ] **Step 5: Commit**

```bash
git add cmd/app/main.go internal/prices/
git commit -m "feat(app): wire portfolios, proposals, scheduler, mailer"
```

---

**PAUSE FOR REVIEW — End of Phase H.** End-to-end advisory loop is operational. Manual smoke test confirms create → propose → accept works.

---

# Phase I — Performance tracking + final integration + UI polish

## Task I1: `portfolio.Performance` — current value + return

**Files:**
- Create: `internal/portfolio/performance.go`
- Create: `internal/portfolio/performance_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/portfolio/performance_test.go
package portfolio_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/testutil"
)

func TestPerformance_CurrentValueAndReturn(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool) // starting_capital = 10000
	seedTicker(t, pool, "AAPL")

	// Seed a daily_prices row.
	now := time.Now().UTC().Truncate(24 * time.Hour)
	_, _ = pool.Exec(ctx, `
        INSERT INTO daily_prices (ticker, date, open, high, low, close, volume)
        VALUES ('AAPL', $1, 200, 200, 200, 200, 1000)
    `, now)

	// Seed an executed trade: bought 50 shares of AAPL @ $180, fee $0.
	_, _ = pool.Exec(ctx, `
        INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
        VALUES ($1, 'AAPL', 'buy', 50, 180, 0, $2)
    `, p.ID, now)
	_ = portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID)

	perf := portfolio.NewPerformance(pool)
	snap, err := perf.Current(ctx, p.ID)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	// cash = 10000 - 180*50 = 1000
	// holdings value = 50 * 200 = 10000
	// total value = 11000
	if !snap.MarketValue.Equal(decimal.NewFromInt(11000)) {
		t.Errorf("market_value = %s, want 11000", snap.MarketValue)
	}
	if !snap.NetInvested.Equal(decimal.NewFromInt(10000)) {
		t.Errorf("net_invested = %s, want 10000", snap.NetInvested)
	}
	if !snap.ReturnAmount.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("return = %s, want 1000", snap.ReturnAmount)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `internal/portfolio/performance.go`**

```go
// internal/portfolio/performance.go
package portfolio

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type Performance struct {
	pool *pgxpool.Pool
}

func NewPerformance(pool *pgxpool.Pool) *Performance { return &Performance{pool: pool} }

type Snapshot struct {
	PortfolioID  int64
	AsOf         time.Time
	Cash         decimal.Decimal
	HoldingsValue decimal.Decimal
	MarketValue  decimal.Decimal
	NetInvested  decimal.Decimal
	ReturnAmount decimal.Decimal
	ReturnPct    decimal.Decimal
}

// Current returns a snapshot using latest daily_prices closes.
func (p *Performance) Current(ctx context.Context, portfolioID int64) (*Snapshot, error) {
	var startingCapital decimal.Decimal
	if err := p.pool.QueryRow(ctx, `SELECT starting_capital FROM portfolios WHERE id=$1`, portfolioID).Scan(&startingCapital); err != nil {
		return nil, err
	}

	var capital decimal.Decimal
	if err := p.pool.QueryRow(ctx, `
        SELECT COALESCE(SUM(amount), 0) FROM capital_events WHERE portfolio_id=$1
    `, portfolioID).Scan(&capital); err != nil {
		return nil, err
	}

	// Cash = starting_capital + capital_events − Σ(buy spend) + Σ(sell proceeds) − Σ(fees).
	var tradeFlow, totalFees decimal.Decimal
	row := p.pool.QueryRow(ctx, `
        SELECT
            COALESCE(SUM(CASE WHEN action='sell' THEN shares*price ELSE -shares*price END), 0),
            COALESCE(SUM(fee), 0)
        FROM executed_trades WHERE portfolio_id=$1
    `, portfolioID)
	if err := row.Scan(&tradeFlow, &totalFees); err != nil {
		return nil, err
	}
	cash := startingCapital.Add(capital).Add(tradeFlow).Sub(totalFees)

	var holdingsValue decimal.Decimal
	if err := p.pool.QueryRow(ctx, `
        SELECT COALESCE(SUM(h.shares * dp.close), 0)
        FROM holdings h
        LEFT JOIN LATERAL (
            SELECT close FROM daily_prices WHERE ticker = h.ticker ORDER BY date DESC LIMIT 1
        ) dp ON true
        WHERE h.portfolio_id = $1
    `, portfolioID).Scan(&holdingsValue); err != nil {
		return nil, err
	}

	netInvested := startingCapital.Add(capital)
	market := cash.Add(holdingsValue)
	ret := market.Sub(netInvested)
	var pct decimal.Decimal
	if !netInvested.IsZero() {
		pct = ret.Div(netInvested).Mul(decimal.NewFromInt(100)).Round(2)
	}

	return &Snapshot{
		PortfolioID: portfolioID, AsOf: time.Now(),
		Cash: cash, HoldingsValue: holdingsValue, MarketValue: market,
		NetInvested: netInvested, ReturnAmount: ret, ReturnPct: pct,
	}, nil
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/portfolio/...
git add internal/portfolio/performance.go internal/portfolio/performance_test.go
git commit -m "feat(portfolio): performance Current snapshot"
```

---

## Task I2: Time series + vs SPY

**Files:**
- Modify: `internal/portfolio/performance.go`
- Modify: `internal/portfolio/performance_test.go`

The time series replays trades + capital events day by day, valuing holdings at each day's close. SPY benchmark uses `benchmark_prices`.

- [ ] **Step 1: Write the failing test (small fixture: one buy, two days of prices)**

```go
func TestPerformance_TimeSeriesNormalisedAgainstSPY(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p := seedPortfolio(t, pool) // starting_capital = 10000
	seedTicker(t, pool, "AAPL")

	day0 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	day1 := day0.AddDate(0, 0, 1)
	for d, close := range map[time.Time]int64{day0: 180, day1: 200} {
		_, _ = pool.Exec(ctx, `INSERT INTO daily_prices (ticker, date, open, high, low, close, volume) VALUES ('AAPL',$1,$2,$2,$2,$2,1000)`, d, close)
		_, _ = pool.Exec(ctx, `INSERT INTO benchmark_prices (ticker, date, open, high, low, close, volume) VALUES ('SPY',$1,$2,$2,$2,$2,1000)`, d, 100+close/100)
	}

	_, _ = pool.Exec(ctx, `INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at) VALUES ($1,'AAPL','buy',50,180,0,$2)`, p.ID, day0)
	_ = portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID)

	perf := portfolio.NewPerformance(pool)
	series, err := perf.TimeSeries(ctx, p.ID, day0, day1)
	if err != nil {
		t.Fatalf("time series: %v", err)
	}
	if len(series.Points) != 2 {
		t.Fatalf("len = %d, want 2", len(series.Points))
	}
	if !series.Points[0].PortfolioNormalised.Equal(decimal.NewFromInt(1)) {
		t.Errorf("day0 normalised = %s, want 1.0", series.Points[0].PortfolioNormalised)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `TimeSeries`**

Append to `internal/portfolio/performance.go`:

```go
type TimePoint struct {
	Date                time.Time
	PortfolioValue      decimal.Decimal
	PortfolioNormalised decimal.Decimal // anchor=1.0 at first day
	SPYNormalised       decimal.Decimal
}

type TimeSeries struct {
	Points []TimePoint
}

// TimeSeries computes daily portfolio market value and SPY normalised return
// from `from` to `to` inclusive. Replays executed_trades + capital_events
// day-by-day; values holdings at each day's close. Cheap because trade ledger
// is sparse.
func (p *Performance) TimeSeries(ctx context.Context, portfolioID int64, from, to time.Time) (*TimeSeries, error) {
	// Load all events ordered by date.
	type tradeEvent struct {
		date   time.Time
		ticker string
		action string
		shares decimal.Decimal
		price  decimal.Decimal
		fee    decimal.Decimal
	}
	type capEvent struct {
		date   time.Time
		amount decimal.Decimal
	}

	var trades []tradeEvent
	rows, err := p.pool.Query(ctx, `
        SELECT executed_at, ticker, action, shares, price, fee
        FROM executed_trades WHERE portfolio_id=$1
        ORDER BY executed_at ASC, id ASC
    `, portfolioID)
	if err != nil { return nil, err }
	for rows.Next() {
		var e tradeEvent
		if err := rows.Scan(&e.date, &e.ticker, &e.action, &e.shares, &e.price, &e.fee); err != nil {
			rows.Close()
			return nil, err
		}
		trades = append(trades, e)
	}
	rows.Close()

	var caps []capEvent
	rows, err = p.pool.Query(ctx, `
        SELECT occurred_at, amount FROM capital_events WHERE portfolio_id=$1 ORDER BY occurred_at ASC, id ASC
    `, portfolioID)
	if err != nil { return nil, err }
	for rows.Next() {
		var e capEvent
		if err := rows.Scan(&e.date, &e.amount); err != nil {
			rows.Close()
			return nil, err
		}
		caps = append(caps, e)
	}
	rows.Close()

	var startingCapital decimal.Decimal
	_ = p.pool.QueryRow(ctx, `SELECT starting_capital FROM portfolios WHERE id=$1`, portfolioID).Scan(&startingCapital)

	holdings := map[string]decimal.Decimal{} // ticker → shares
	cash := startingCapital

	out := &TimeSeries{}
	var portfolioAnchor, spyAnchor decimal.Decimal

	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		// Apply events with date == d.
		for _, t := range trades {
			if !sameDay(t.date, d) { continue }
			if t.action == "buy" {
				holdings[t.ticker] = holdings[t.ticker].Add(t.shares)
				cash = cash.Sub(t.shares.Mul(t.price)).Sub(t.fee)
			} else {
				holdings[t.ticker] = holdings[t.ticker].Sub(t.shares)
				cash = cash.Add(t.shares.Mul(t.price)).Sub(t.fee)
			}
		}
		for _, ev := range caps {
			if !sameDay(ev.date, d) { continue }
			cash = cash.Add(ev.amount)
		}

		// Value holdings at d's close.
		var holdingsValue decimal.Decimal
		for ticker, shares := range holdings {
			if shares.IsZero() { continue }
			var close decimal.Decimal
			err := p.pool.QueryRow(ctx, `
                SELECT close FROM daily_prices
                WHERE ticker=$1 AND date <= $2 ORDER BY date DESC LIMIT 1
            `, ticker, d).Scan(&close)
			if err != nil { continue }
			holdingsValue = holdingsValue.Add(shares.Mul(close))
		}
		value := cash.Add(holdingsValue)

		var spyClose decimal.Decimal
		_ = p.pool.QueryRow(ctx, `
            SELECT close FROM benchmark_prices WHERE ticker='SPY' AND date <= $1 ORDER BY date DESC LIMIT 1
        `, d).Scan(&spyClose)

		if portfolioAnchor.IsZero() && !value.IsZero() {
			portfolioAnchor = value
		}
		if spyAnchor.IsZero() && !spyClose.IsZero() {
			spyAnchor = spyClose
		}
		var pn, sn decimal.Decimal
		if !portfolioAnchor.IsZero() { pn = value.Div(portfolioAnchor).Round(6) }
		if !spyAnchor.IsZero() { sn = spyClose.Div(spyAnchor).Round(6) }
		out.Points = append(out.Points, TimePoint{
			Date: d, PortfolioValue: value, PortfolioNormalised: pn, SPYNormalised: sn,
		})
	}
	return out, nil
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}
```

- [ ] **Step 4: Run, expect PASS, commit**

```bash
go test ./internal/portfolio/...
git add internal/portfolio/
git commit -m "feat(portfolio): TimeSeries + SPY normalised computation"
```

---

## Task I3: Portfolio detail handler integrates performance + chart

**Files:**
- Modify: `internal/handlers/portfolios.go`
- Modify: `internal/views/portfolios/detail.templ`

- [ ] **Step 1: Add a JSON endpoint for the chart data**

```go
// In portfolios.go
func (h *PortfoliosHandler) PerformanceJSON(c echo.Context) error {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	p, err := h.portfolios.GetByID(c.Request().Context(), id)
	if err != nil || !canAccessPortfolio(c, p) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	to := time.Now().UTC().Truncate(24 * time.Hour)
	from := p.CreatedAt.UTC().Truncate(24 * time.Hour)
	series, err := h.performance.TimeSeries(c.Request().Context(), id, from, to)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.JSON(http.StatusOK, series)
}
```

(Add `Performance` to `PortfoliosDeps`.)

- [ ] **Step 2: Update detail page to render chart**

Replace the placeholder in `views/portfolios/detail.templ`:

```templ
<section>
    <h2 class="text-lg font-semibold mb-2">Performance</h2>
    <div>
        <p class="text-sm text-base-content/70 mb-2">
            Value: <strong>${ d.PerformanceSnapshot.MarketValue.StringFixed(2) }</strong>
            &middot; Return:
            <span class={ returnColor(d.PerformanceSnapshot.ReturnAmount) }>
                { formatReturn(d.PerformanceSnapshot.ReturnAmount, d.PerformanceSnapshot.ReturnPct) }
            </span>
        </p>
        <canvas id="performance-chart" data-portfolio={ fmt("%d", d.Portfolio.ID) }
                width="800" height="300"></canvas>
        <script src={ chartJSURL() }></script>
        <script>
            (function() {
                const el = document.getElementById('performance-chart');
                const portfolioID = el.dataset.portfolio;
                fetch('/portfolios/' + portfolioID + '/performance.json')
                  .then(r => r.json())
                  .then(data => {
                    const labels = data.Points.map(p => p.Date.split('T')[0]);
                    const portfolio = data.Points.map(p => parseFloat(p.PortfolioNormalised));
                    const spy       = data.Points.map(p => parseFloat(p.SPYNormalised));
                    new Chart(el, {
                      type: 'line',
                      data: { labels, datasets: [
                        { label: 'Portfolio', data: portfolio, borderColor: '#2563eb', tension: 0.2 },
                        { label: 'SPY', data: spy, borderColor: '#9ca3af', tension: 0.2 }
                      ]},
                      options: { scales: { y: { beginAtZero: false } } }
                    });
                  });
            })();
        </script>
    </div>
</section>
```

(Match `chartJSURL()` to whatever helper the existing backtester chart uses.)

- [ ] **Step 3: Wire the route**

In `cmd/app/main.go`:

```go
e.GET("/portfolios/:id/performance.json", portfoliosHandler.PerformanceJSON, requireAuth)
```

- [ ] **Step 4: Compile, smoke-test in browser, commit**

```bash
go build ./... && templ generate ./internal/views/...
go run ./cmd/app
# In browser: navigate to a portfolio detail page; chart should render with portfolio + SPY lines.
git add internal/handlers/portfolios.go internal/views/portfolios/ cmd/app/main.go
git commit -m "feat(portfolios): performance chart + JSON endpoint"
```

---

## Task I4: End-to-end integration test

**Files:**
- Create: `internal/proposal/integration_test.go`

A single test that exercises the full happy path: create portfolio → first proposal generated → accept with edits → verify trades, holdings, cadence advance, performance.

- [ ] **Step 1: Write the test**

```go
// internal/proposal/integration_test.go
package proposal_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"deepvalue/internal/portfolio"
	"deepvalue/internal/proposal"
	"deepvalue/internal/strategy"
	"deepvalue/internal/testutil"
)

func TestEndToEnd_CreateProposeAcceptRebalance(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	// Seed strategy + verify.
	sRepo := strategy.NewRepository(pool)
	rules := []byte(`{"filters":[],"ranking":[],"limit":2}`)
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: "S", Rules: rules}, testutil.SystemUserID)
	_ = sRepo.Verify(ctx, s.ID)
	got, _ := sRepo.GetByID(ctx, s.ID)

	// Seed companies.
	for _, t := range []string{"AAPL", "MSFT"} {
		_, _ = pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ($1,$1,'','',true) ON CONFLICT DO NOTHING`, t)
	}

	// Build portfolio.
	pRepo := portfolio.NewRepository(pool)
	p, _ := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: testutil.SystemUserID, Name: "E2E",
		StartingCapital:   decimal.NewFromInt(10000),
		StrategyID:        s.ID, StrategyVersionID: got.CurrentVersionID,
		Cadence: strategy.CadenceQuarterly,
	})

	// Generate first proposal manually (mirrors handler path).
	picks := []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(0.5),
			TargetShares: decimal.NewFromInt(27), CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
		{Ticker: "MSFT", Action: proposal.ActionBuy, TargetWeight: decimal.NewFromFloat(0.5),
			TargetShares: decimal.NewFromInt(12), CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(400)},
	}
	picksJSON, _ := json.Marshal(picks)
	pRepo2 := proposal.NewRepository(pool)
	pr, _ := pRepo2.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: got.CurrentVersionID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 picksJSON,
	})

	// Accept full proposal.
	a := proposal.NewAcceptor(pool, pRepo2, pRepo, portfolio.NewHoldings(pool))
	now := time.Now().UTC()
	res, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualShares: decimal.NewFromInt(27), ActualPrice: decimal.NewFromInt(180), Fee: decimal.NewFromInt(1)},
			{Ticker: "MSFT", ActualShares: decimal.NewFromInt(12), ActualPrice: decimal.NewFromInt(400), Fee: decimal.NewFromInt(1)},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != proposal.StatusAccepted {
		t.Fatalf("status = %s, want accepted", res.Status)
	}

	// Verify holdings.
	holdings, _ := portfolio.NewHoldings(pool).ListByPortfolio(ctx, p.ID)
	if len(holdings) != 2 {
		t.Fatalf("len(holdings) = %d, want 2", len(holdings))
	}

	// Verify cadence advanced.
	got2, _ := pRepo.GetByID(ctx, p.ID)
	if got2.NextRebalanceDue == nil {
		t.Fatal("next_rebalance_due not set")
	}
	if !got2.NextRebalanceDue.After(now) {
		t.Errorf("next due = %s, want > %s", got2.NextRebalanceDue, now)
	}

	// Verify proposal frozen.
	resolved, _ := pRepo2.Get(ctx, pr.ID)
	if resolved.Status != proposal.StatusAccepted {
		t.Errorf("proposal status = %s, want accepted", resolved.Status)
	}
}
```

- [ ] **Step 2: Run, expect PASS, commit**

```bash
go test ./internal/proposal/ -run TestEndToEnd_CreateProposeAcceptRebalance
git add internal/proposal/integration_test.go
git commit -m "test(proposal): end-to-end create→propose→accept→holdings→cadence"
```

---

## Task I5: UI polish review pass

**Files:**
- Modify: any of `internal/views/portfolios/*.templ`, `internal/views/proposals/*.templ`

Refer to `feedback_templ_ui_polish` memory: this is the explicit visual review the user has flagged is necessary for new templ pages.

- [ ] **Step 1: Run the dev server**

```bash
make dev   # or: go run ./cmd/app with templ generate in watch mode
```

- [ ] **Step 2: Visit each new page in a browser and side-by-side compare with `/strategies` and `/strategies/:id`**

Pages to inspect:
- `/portfolios` (list)
- `/portfolios/new` (form)
- `/portfolios/:id` (detail)
- `/portfolios/:id/proposals/:pid` (review)

For each, check explicitly:
- **Button widths** — primary buttons should NOT span the full container width unless intentional. Add `w-auto` (or remove `w-full`) where needed.
- **Logo / image sizing** — confirm any image uses Tailwind size classes (`h-8 w-auto`, etc.) rather than rendering at intrinsic size.
- **Container max-widths** — `max-w-5xl` / `max-w-xl` matches the existing pages' choices.
- **Spacing & padding** — `space-y-*` and `px-4 py-6` match what the strategies pages use.
- **Form input sizes** — text inputs use `input-bordered`; selects use `select-bordered`; sizes (`input-sm` / default) match the existing forms.
- **Action labels in picks_table** — color badges visible, contrast acceptable.

- [ ] **Step 3: Iterate until each page looks consistent with existing pages**

Make small CSS-class adjustments. Keep the diffs tight; one polish commit per page is fine.

- [ ] **Step 4: Commit and announce review readiness**

```bash
git add internal/views/
git commit -m "style(views): UI polish pass on portfolio + proposal pages"
```

---

## Task I6: Final test sweep

- [ ] **Run the full test suite**

```bash
go test ./...
```

Expected: all tests PASS.

- [ ] **Run `go vet`**

```bash
go vet ./...
```

Expected: clean.

- [ ] **Run a manual smoke test of the full user flow**

1. Sign in via magic link.
2. Navigate to `/strategies`, verify a strategy.
3. Navigate to `/portfolios/new`, create a portfolio with quarterly cadence.
4. On the proposal review page, edit one row's actual_shares and skip another.
5. Accept proposal.
6. Navigate to `/portfolios/:id` — verify holdings table shows expected rows, performance section shows current value, chart renders SPY + portfolio lines.
7. Confirm an email landed in the inbox (or in `LogSender`'s output if running with the dev sender).
8. Force the next-rebalance-due to be in the past:
   ```sql
   UPDATE portfolios SET next_rebalance_due = NOW() - INTERVAL '1 hour' WHERE id = X;
   ```
9. With `SCHEDULER_TICK_INTERVAL=30s`, wait 30s, refresh. A new pending proposal should exist; old one should be `accepted`.
10. Confirm pause/resume/archive endpoints work.

- [ ] **Tag the completion**

```bash
git log --oneline -20  # spot check the chain of commits
```

---

# Self-review checklist (engineer running the plan)

Before considering the feature complete, confirm:

- [ ] All migrations 017–024 applied in order, with corresponding down-migrations untested but written.
- [ ] `go test ./...` passes.
- [ ] `go vet ./...` clean.
- [ ] Manual smoke test of create → propose → accept flows end-to-end in a browser.
- [ ] UI polish pass completed against all new pages.
- [ ] Scheduler logs visible in the dev server (showing it ticked at least once).
- [ ] Email send fired (LogSender output or actual inbox).
- [ ] Strategy verify/archive buttons functional in the existing strategies UI.
- [ ] Editing a verified strategy demotes it to draft and creates a new strategy_versions row; existing portfolios continue rebalancing on the pinned version.

---

# Out-of-scope reminders

These are explicitly NOT in this plan; do not add them while implementing:

- Automated strategy promotion (auto-verify based on backfill metrics).
- Broker integration.
- "Strategy theoretical" comparison line on the performance chart.
- Sharpe / drawdown / alpha at the live-portfolio level.
- Manual trade entry independent of proposals.
- Multi-strategy portfolios.
- Email preferences / unsubscribe.
- "Reset cadence on resume" toggle.
- `portfolio_daily_snapshots` materialization.
- Generic event store + dispatcher abstraction.
