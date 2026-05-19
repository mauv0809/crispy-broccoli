# Portfolios and Strategy-Driven Trade Proposals — Design

**Date:** 2026-05-08
**Status:** Draft for review
**Owner:** mauv0809

## Goal

Extend DeepValue beyond strategy evaluation into a forward-looking platform: let a user create a portfolio, attach a verified strategy with a chosen rebalance cadence, receive proposals at each cadence, log what they actually executed, and track the portfolio's performance over time. Advisory only — no broker integration.

Out of scope for v1: broker connectivity, automated strategy promotion based on backfill metrics, multiple-strategy portfolios, "strategy theoretical" comparison line, per-holding return attribution, push notifications beyond email, "force generate now" admin actions, daily snapshot materialization for performance.

## Decisions made during brainstorming

| Topic | Decision |
|---|---|
| Mode | Pure advisory; user logs what they actually did |
| Portfolio lifecycle | Starts empty with `starting_capital`; first proposal generated synchronously on portfolio creation |
| Per-user portfolios | One user → many portfolios |
| Position sizing | Strategy weights when defined, else equal weight |
| Capital flows | `deploy_amount = current_market_value + capital_change`; `capital_change` may be negative (withdrawal); never partial deployment |
| Strategy composition rule | Acceptance always fully replaces portfolio with proposed picks; "add capital to keep extra stocks" is intentionally unsupported |
| Cadence ownership | Strategy carries a `default_cadence` (the one it was backtested with); portfolio can override at attach time |
| Strategy status | `draft` → `verified` → `archived`; promotion is manual via a "verify" button in v1. Auto-promotion based on backfill metrics is explicitly future work. |
| Strategy versioning | Every edit creates a new `strategy_versions` row; `strategies.status` drops to `draft` on edit |
| Portfolio version pinning | `portfolios.strategy_version_id` is pinned at attach time; existing portfolios are never affected by strategy edits |
| Status gating | `status` controls **attachment and upgrade**, never blocks rebalancing of already-attached portfolios |
| Execution logging | Per-row editable (actual shares, actual price, fee) with per-row skip; whole-proposal skip also supported |
| Architecture style | Approach 1 (domain-specific tables), event-flavored — append-only ledgers + materialized projection |
| Storage | Postgres; KV store project considered but not a fit for the event log |
| `proposal.picks` | JSONB on the proposal row (read/written as a unit); not a child table |
| Scheduler | In-process goroutine, 15-minute tick, `FOR UPDATE SKIP LOCKED` |
| First proposal UX | Generated synchronously on portfolio create (vs. waiting for next tick) |
| Email cadence | Initial send on proposal generation; one reminder after 3 days; then stop until next cadence |
| Auto-expiry | When generating a new proposal for a portfolio, any existing `pending` proposal is set to `expired` first |
| Skip semantics | Skipping a whole proposal still advances `next_rebalance_due` by one cadence period |
| Performance metrics | v1: current value, net invested, total return $/%, time-weighted return, vs SPY chart; computed on-demand |

## Architecture

### Module layout

```
internal/portfolio/    Portfolio CRUD + holdings projection updater
internal/proposal/     Proposal generator, acceptor, cadence helpers
internal/scheduler/    Background goroutine: tick → find due → generate → email
internal/strategy/     Extended with version management + status helpers

internal/handlers/     New: portfolios.go, proposals.go; extended: strategies.go
internal/views/        New: portfolios/, proposals/, emails/ templ files
internal/email/        New: SendProposalReady(), SendProposalReminder()
internal/db/migrations 017–024: versioning, status/cadence, portfolios, proposals,
                       trades, capital events, holdings, drop legacy table
```

### Dependency direction

```
handlers ──► proposal ──► strategy
         ──► portfolio ──► (holdings projection)
scheduler ──► proposal (generator)
          ──► email
proposal/acceptor ──► portfolio/holdings (transactional)
```

### Boundaries

- **portfolio** — owns portfolios table + holdings projection; receives "apply this trade" calls from the proposal acceptor; knows nothing about strategies or proposals.
- **proposal** — orchestrates "given a portfolio + pinned strategy version, produce picks; given accepted picks, emit trades." Depends on portfolio (apply trades) and strategy (executor).
- **scheduler** — polls due portfolios, calls `proposal.Generate()`, dispatches email; replaceable.
- **strategy** — existing concerns plus version management.

## Data model

### New / changed tables

```sql
-- Strategy versioning (NEW)
strategy_versions
  id              bigserial PK
  strategy_id     bigint    NOT NULL FK strategies(id) ON DELETE CASCADE
  version_number  int       NOT NULL
  rules           jsonb     NOT NULL
  created_at      timestamptz NOT NULL DEFAULT now()
  created_by      bigint    FK users(id)
  UNIQUE (strategy_id, version_number)

-- Strategies (EXTENDED)
strategies
  + status               text NOT NULL DEFAULT 'draft'   -- draft | verified | archived
  + default_cadence      text                            -- monthly | quarterly | semi_annual | annual
  + current_version_id   bigint FK strategy_versions(id)

-- Portfolios (REPLACES legacy 'portfolio' table; old table dropped in 024)
portfolios
  id                  bigserial PK
  user_id             bigint NOT NULL FK users(id)
  name                text NOT NULL
  starting_capital    numeric(18,2) NOT NULL
  strategy_id         bigint NOT NULL FK strategies(id)
  strategy_version_id bigint NOT NULL FK strategy_versions(id)   -- pinned at attach time
  cadence             text NOT NULL                     -- copied from strategy.default_cadence; overridable
  next_rebalance_due  timestamptz                       -- nullable until first proposal resolved
  status              text NOT NULL DEFAULT 'active'    -- active | paused | archived
  created_at          timestamptz NOT NULL DEFAULT now()
  updated_at          timestamptz NOT NULL DEFAULT now()

-- Proposals (append-only past 'pending'; mutable only while pending)
proposals
  id                          bigserial PK
  portfolio_id                bigint NOT NULL FK portfolios(id)
  strategy_version_id         bigint NOT NULL FK strategy_versions(id)
  generated_at                timestamptz NOT NULL DEFAULT now()
  market_value_at_proposal    numeric(18,2) NOT NULL
  capital_change              numeric(18,2) NOT NULL DEFAULT 0
  deploy_amount               numeric(18,2) NOT NULL  -- = market_value + capital_change
  picks                       jsonb         NOT NULL  -- [{ticker, action, target_weight, target_shares, price_at_proposal}]
  status                      text          NOT NULL DEFAULT 'pending'
                                  -- pending | accepted | partially_accepted | skipped | expired
  resolved_at                 timestamptz
  notification_sent_at        timestamptz
  reminder_sent_at            timestamptz

-- Executed trades (append-only ledger)
executed_trades
  id            bigserial PK
  portfolio_id  bigint        NOT NULL FK portfolios(id)
  proposal_id   bigint        FK proposals(id)        -- nullable for future manual trade entry
  ticker        text          NOT NULL FK companies(ticker)
  action        text          NOT NULL                -- buy | sell
  shares        numeric(18,6) NOT NULL
  price         numeric(18,4) NOT NULL
  fee           numeric(18,2) NOT NULL DEFAULT 0
  executed_at   timestamptz   NOT NULL                -- user-supplied; defaults to now() on accept
  recorded_at   timestamptz   NOT NULL DEFAULT now()
  notes         text

-- Capital events (append-only ledger)
capital_events
  id            bigserial PK
  portfolio_id  bigint        NOT NULL FK portfolios(id)
  proposal_id   bigint        FK proposals(id)        -- nullable
  amount        numeric(18,2) NOT NULL                -- +deposit, −withdrawal
  occurred_at   timestamptz   NOT NULL
  recorded_at   timestamptz   NOT NULL DEFAULT now()
  notes         text

-- Holdings (projection; recomputable from executed_trades)
holdings
  portfolio_id   bigint        NOT NULL FK portfolios(id)
  ticker         text          NOT NULL FK companies(ticker)
  shares         numeric(18,6) NOT NULL
  cost_basis     numeric(18,2) NOT NULL
  last_trade_at  timestamptz   NOT NULL
  PRIMARY KEY (portfolio_id, ticker)
```

### Indexes

```sql
CREATE INDEX ON portfolios(user_id);
CREATE INDEX ON portfolios(next_rebalance_due) WHERE status = 'active';
CREATE INDEX ON proposals(portfolio_id, generated_at DESC);
CREATE INDEX ON executed_trades(portfolio_id, executed_at DESC);
CREATE INDEX ON capital_events(portfolio_id, occurred_at DESC);
CREATE INDEX ON strategy_versions(strategy_id, version_number DESC);
```

### Append-only invariants

- `executed_trades` and `capital_events` are insert-only; never updated or deleted.
- `proposals` becomes immutable once `status` leaves `pending`. While `pending`, only `picks`, `capital_change`, `deploy_amount`, `notification_sent_at`, and `reminder_sent_at` may change.
- `holdings` is a projection updated transactionally with `executed_trades` inserts; recomputable via `holdings.Rebuild(portfolio_id)` for sanity checks.

## Strategy versioning rules

1. Editing a strategy's rules creates a new `strategy_versions` row, increments `version_number`, updates `strategies.current_version_id`, and sets `strategies.status = 'draft'`.
2. `portfolios.strategy_version_id` is pinned at attachment and never auto-updates. Existing portfolios continue rebalancing on their pinned version regardless of edits.
3. `strategies.status` controls **attachment and upgrade only**:

   | Status | New portfolios may attach | Existing portfolios may upgrade | Existing portfolios keep rebalancing |
   |---|---|---|---|
   | `draft` | ❌ | ❌ | ✅ |
   | `verified` | ✅ (uses `current_version_id`) | ✅ | ✅ |
   | `archived` | ❌ | ❌ | ✅ |

4. "Upgrade portfolio to vN" is a future explicit user action; not in v1 scope.
5. Scheduler does not consult `strategies.status` — it operates on `portfolios.strategy_version_id`.
6. Strategies are **archive-only, never hard-deleted.** Deleting a strategy that has any `strategy_versions` referenced by a portfolio would leave dangling references; instead the UI exposes only "archive." A future cleanup action could permit deletion when no portfolios reference any of the strategy's versions, but it's not in v1 scope.

## Proposal lifecycle

### State machine

```
                       (no state)
                            │
        scheduler tick │ portfolio create
                            ▼
                       ┌─────────┐
              ┌────────│ pending │────────┐
              │        └─────────┘        │
   accept (full or          │             │
   partial)        skip whole       no action; next
                  proposal          generation cycle
              │              │             │
              ▼              ▼             ▼
   ┌─────────────────┐  ┌─────────┐  ┌─────────┐
   │ accepted /      │  │ skipped │  │ expired │
   │ partially_      │  └─────────┘  └─────────┘
   │ accepted        │
   └─────────────────┘
```

### Generation

1. Load portfolio, its `strategy_version_id`, current holdings, current market value.
2. Run strategy executor against current data → ranked picks.
3. `deploy_amount = market_value + capital_change` (initial portfolio: `starting_capital + capital_change`, default 0).
4. For each pick: `target_weight` from strategy weights or equal; `target_shares = floor(weight × deploy_amount / current_price)`; `price_at_proposal = current_price`.
5. Diff against current holdings to label each row's `action`:
   - `buy` — not held, in new picks
   - `sell` — held, not in new picks
   - `add` — held, in picks, target > current
   - `trim` — held, in picks, target < current
   - `hold` — held, in picks, target = current
6. Insert `proposals` row with `status='pending'`, `picks` JSONB, `strategy_version_id`.
7. Send notification email; set `notification_sent_at`.

### Capital adjustment (while pending)

User changes the capital_change input on the review page. HTMX POSTs to `/portfolios/:id/proposals/:pid/recompute`. Server recomputes `deploy_amount`, regenerates `picks` (wholesale array replacement), updates the row, returns rendered table fragment. Original `generated_at` and `strategy_version_id` are not touched. Validation: `capital_change ≥ −market_value_at_proposal` (no overdraft).

### Acceptance

Pick actions normalize to `executed_trades.action` at acceptance time:

| pick.action | Generates `executed_trades` row? | `action` | Default `shares` (editable by user) |
|---|---|---|---|
| `buy`  | yes | `buy`  | `target_shares` |
| `add`  | yes | `buy`  | `target_shares − current_shares` |
| `trim` | yes | `sell` | `current_shares − target_shares` |
| `sell` | yes | `sell` | `current_shares` |
| `hold` | no  | —      | — |

Single transaction:
1. For each accepted row that produces a trade, insert `executed_trades` using the normalized action and user-supplied actual shares/price/fee (defaults per the table above; `actual_price` defaults to `price_at_proposal`; `fee` defaults to 0).
2. If `capital_change ≠ 0`, insert a `capital_events` row.
3. Update `holdings`: add shares + cost_basis on `buy`, subtract on `sell`, delete row at zero shares.
4. Set `proposals.status = 'accepted'` if every non-hold row was accepted (skipped rows count against this), else `'partially_accepted'`. Set `resolved_at = now()`.
5. Set `portfolios.next_rebalance_due = cadence.AddCadence(now(), portfolio.cadence)`.

**Per-row skip semantics:** A skipped row produces no `executed_trades` row. The corresponding holding (if any) is left unchanged in the projection — i.e., the user is choosing not to perform that buy/sell/add/trim, and the portfolio drifts off-strategy by exactly that amount until the next rebalance.

### Skip whole proposal

`proposals.status = 'skipped'`, `resolved_at = now()`. No trades. `next_rebalance_due` advances by one cadence period (no bunching of missed cycles).

### Auto-expire

When the scheduler is about to generate a new proposal for a portfolio, any existing `pending` proposal is updated to `status='expired', resolved_at=now()` first. Enforces "at most one pending proposal per portfolio."

## Background scheduler

Single goroutine, `time.Ticker` at 15 minutes, started by `main` and stopped via `context.Cancel`.

### Tick body

```sql
-- 1. Find due portfolios
SELECT id FROM portfolios
WHERE status = 'active'
  AND next_rebalance_due IS NOT NULL
  AND next_rebalance_due <= now()
ORDER BY next_rebalance_due ASC
FOR UPDATE SKIP LOCKED;
```

For each row, in its own transaction: auto-expire any pending proposal, call `proposal.Generate(..., capitalChange=0)`, commit. Outside the transaction, send notification email; set `notification_sent_at` on success.

```sql
-- 2. Send 3-day reminders
SELECT id, portfolio_id FROM proposals
WHERE status = 'pending'
  AND notification_sent_at IS NOT NULL
  AND notification_sent_at < now() - INTERVAL '3 days'
  AND reminder_sent_at IS NULL;
```

For each: send reminder, set `reminder_sent_at`. No further nags after that.

```sql
-- 3. Retry initial notification for proposals where email send failed
SELECT id FROM proposals
WHERE status = 'pending'
  AND notification_sent_at IS NULL
  AND generated_at < now() - INTERVAL '5 minutes'
  AND generated_at > now() - INTERVAL '6 hours';
```

Each tick re-attempts pending sends within the 6-hour window; after 6 hours we stop trying (the proposal still exists; the user can find it in-app). No persistent retry counter — the time bound provides an implicit cap.

### First proposal on portfolio creation

Synchronous: `handlers/portfolios.go::Create` creates the portfolio, then calls `proposal.Generate(...)` inline before redirecting the user to the proposal review page. Email fires too. If generation fails, portfolio still exists; UI shows an error with a "retry generation" button.

### Configuration

```
SCHEDULER_TICK_INTERVAL=15m
SCHEDULER_REMINDER_AFTER=72h
SCHEDULER_NOTIFICATION_RETRY_WINDOW=6h
SCHEDULER_ENABLED=true        # disabled in tests
```

### Testability

`scheduler.Worker` takes a `Clock` and `Ticker` interface, real in prod, fakes in tests. Tests advance fake time and assert side effects deterministically.

## Email

Two templates in `internal/views/emails/`: `proposal_ready.templ` and `proposal_reminder.templ`. Plain HTML + text. Subject lines name the portfolio. Body is short with a single CTA link; full proposal is rendered in the app, behind auth. Sent synchronously from the scheduler via `email.SendProposalReady` / `SendProposalReminder`.

No "rebalance accepted" confirmation email in v1. No email preferences pane in v1.

## Frontend

### Routes

```
GET  /portfolios                                    List user's portfolios
GET  /portfolios/new                                Form
POST /portfolios                                    Create + generate first proposal + redirect

GET  /portfolios/:id                                Detail (holdings, performance, history)
POST /portfolios/:id/pause
POST /portfolios/:id/resume
POST /portfolios/:id/archive

GET  /portfolios/:id/proposals/:pid                 Review page
POST /portfolios/:id/proposals/:pid/recompute       HTMX: capital_change changed
POST /portfolios/:id/proposals/:pid/accept          Accept (per-row edits + skips)
POST /portfolios/:id/proposals/:pid/skip            Skip whole proposal

GET  /strategies/:id/versions                       Version history (extension)
POST /strategies/:id/verify                         draft → verified
POST /strategies/:id/archive                        → archived
```

All under `RequireAuth`. Row-level checks in handlers: a user only sees their own portfolios; admins see any portfolio with a "viewing as admin" banner. Non-owner non-admin requests return 404 (not 403).

### Pages

- **Portfolios list** — card per portfolio: name, strategy + cadence, current value, return, "rebalance ready" badge if pending.
- **Portfolio detail** — header (strategy + version + cadence + next rebalance), holdings table, performance section (chart + metrics), proposal history (collapsed list, expandable to show proposed vs actual diffs).
- **Proposal review page** — capital adjustment input (Alpine + HTMX recompute), picks table with per-row editable inputs (actual shares, price, fee) and skip checkbox, totals footer, accept / skip whole proposal actions.
- **Portfolio create form** — name, starting capital, strategy picker (verified only), cadence picker (default from strategy).
- **Strategy list/detail extensions** — status column, verify/archive buttons, version history section. Editing a verified strategy shows a confirmation: "This will create v[N+1] and demote to draft. Existing portfolios are unaffected."
- **Dashboard widget** — top 3 portfolios by value with rebalance-ready badges.

### UI polish pass

All new templ pages introduced in this design (`portfolios/`, `proposals/`, `emails/`, the strategy verify/archive button additions) need an explicit visual review against existing pages. Known recurring issues to check for:

- Buttons stretching to full container width when they shouldn't (DaisyUI defaults to `w-full` in some flex contexts; constrain explicitly).
- Logos / images sized via raw asset dimensions rather than constrained classes.
- Container widths and spacing matching existing pages (use the same `max-w-*` and padding conventions as `strategies/` views).

A separate workspace is fixing these issues on the login page; reconcile against whatever conventions land there before considering this design's UI work complete.

## Performance tracking (v1)

### Metrics

- Current value = cash + Σ(shares × latest close).
- Net invested = Σ(capital_events.amount).
- Total return $ = current_value − net_invested.
- Total return % = total_return / net_invested.
- Time-weighted return — chained period returns between capital flow events.
- vs SPY since inception — both normalized to 1.0 at portfolio start.

### Computation

On-demand via Echo middleware caching (5-minute TTL). Algorithm:

```
For each date D from portfolio start to today:
  holdings_at_D = trade ledger replayed up to D
  cash_at_D    = capital_events summed up to D minus trade dollar flows
  value_at_D   = cash_at_D + Σ(holdings_at_D × daily_prices[ticker, D])
```

Materialization into `portfolio_daily_snapshots` is deferred until histories or portfolio counts grow large enough to make on-demand computation slow.

### Chart

Reuse the backtester's Chart.js setup. Two normalized lines: portfolio and SPY. Granularity selector (daily/weekly/monthly).

### Strategy adherence (lightweight)

Each historical proposal in the portfolio detail page shows a small proposed-vs-actual diff (e.g., "1 row skipped" or "all 6 executed at avg +0.4% vs proposed price"). No rolled-up adherence score in v1.

## Edge cases

| Case | Handling | Location |
|---|---|---|
| Strategy returns 0 picks | Empty proposal generated; UI shows "no picks this cycle"; skip-only | `proposal.Generator`, `proposals/detail.templ` |
| All current holdings re-selected | Action labels become hold/add/trim only | `proposal.Generator` |
| Withdrawal exceeds market value | Block at recompute endpoint with inline error | `handlers/proposals.go::Recompute` |
| Capital adjustment causes 0 target shares for a row | Render with 0-share warning; user can skip or add capital | `proposals/detail.templ` |
| Strategy executor errors during scheduler tick | Log + Sentry; `next_rebalance_due` unchanged → retried next tick | `scheduler.Worker` |
| Email send fails after proposal commit | NULL `notification_sent_at` retried up to 3× within 6h | `scheduler.Worker` |
| Concurrent acceptance attempts on same proposal | Second transaction fails on `status='pending'` check inside the txn | `proposal.Acceptor` |
| Ticker delisted between proposal and acceptance | Acceptor blocks that row with inline error; user must skip | `proposal.Acceptor` |
| User views another user's portfolio | 404 (do not leak existence) | `handlers/portfolios.go` |
| Admin views any user's portfolio | Allowed if `is_admin`, with "viewing as admin" banner | `handlers/portfolios.go` |
| Long-paused portfolio resumed | Rebalances immediately on resume (acceptable v1; "reset cadence on resume" toggle is future) | `handlers/portfolios.go::Resume` |

## Error handling philosophy

- DB errors bubble as 500 + Sentry. No retries inside handlers.
- Validation errors return 400 with inline form messages.
- Strategy executor errors during scheduler are logged + counted, not surfaced to user (Sentry investigation path).
- No silent failures.

## Observability

- Sentry breadcrumbs on every scheduler tick: portfolios processed, proposals generated, emails sent, errors per category.
- Structured logs include `portfolio_id`, `proposal_id`, `user_id`.
- Counters: `proposals_generated_total`, `proposals_accepted_total`, `proposals_skipped_total`, `proposals_expired_total`, `scheduler_errors_total`.

## Testing

### Unit (no DB)

- `cadence.AddCadence` — table-driven.
- `proposal.Generate` — fake strategy executor + fake holdings repo. Asserts pick math, action labels, version snapshot reference.
- `proposal.Acceptor` — fake repos. Asserts trade emission, holdings update, status transition, `next_rebalance_due` advance.
- `portfolio/holdings.ApplyTrade` — math correctness, fee handling, zero-share row deletion.

### Integration (real Postgres)

- End-to-end: create portfolio → first proposal → accept with edits → ledger + holdings + cadence advance.
- Skip whole proposal → cadence advances.
- Capital adjustment → picks recomputed, deploy_amount reflects change.
- Strategy edit while portfolio active → new version row, status drops to draft, pinned-version rebalance still works.
- Scheduler tick with fake clock → due portfolios get proposals + emails; non-due untouched.
- Auto-expire on regeneration → previous pending becomes expired, new one created.

Match the style of existing `strategy.Executor` and `strategy.Backtester` tests.

## Future work (explicitly deferred)

- Automated strategy promotion based on backfill success metrics.
- Broker integration (Alpaca, IBKR, etc.).
- "Strategy theoretical" comparison line on performance charts.
- Per-holding return attribution.
- Sharpe / max drawdown / alpha at the live-portfolio level.
- "Force generate now" admin/user action.
- Manual trade entry independent of proposals.
- Multi-strategy portfolios.
- Email preferences and unsubscribe.
- "Reset cadence on resume" toggle.
- `portfolio_daily_snapshots` materialization for performance.
- Generic event store + dispatcher (revisit when 3+ features want event sourcing OR a user activity feed is added).
