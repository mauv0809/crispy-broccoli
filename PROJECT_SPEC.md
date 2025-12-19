# 📘 Project Specification: DeepValue

## 1. Project Overview

- **Name:** DeepValue
- **Domain:** deepvalue.utiger.dk
- **Goal:** A personal wealth management tool to automate a "Value Investing" strategy.
- **Core Function:** Maintains a portfolio of exactly 6 high-conviction stocks, rebalanced quarterly.
- **Strategy:** "Magic Formula" style — buying high-quality companies (High ROIC) at a discount (Low EV/EBIT).
- **Architecture:** Monolithic Go application with server-side rendering (HTMX). No complex SPA frameworks.

## 2. Tech Stack & Tools

| Component | Choice | Reason |
|-----------|--------|--------|
| Language | Go (Golang) 1.23+ | Performance, strict typing for money, single binary deployment. |
| Database | PostgreSQL 16 | Relational data integrity for financial time-series. |
| Data Source | Nasdaq Data Link | Specifically Sharadar Core US Fundamentals (SF1) via Tables API. |
| Web Framework | Echo | Lightweight routing. |
| Templating | Templ | Type-safe HTML generation (compiles .templ to .go). |
| Frontend | HTMX | Interactive UI without writing JavaScript. |
| CSS | TailwindCSS | Utility-first styling (via CDN for simplicity). |
| Migrations | Goose | Simple SQL migration tool. |
| DB Driver | pgx/v5 | High-performance Postgres driver for Go. |

## 3. Directory Structure

```
/cmd/app/           # Main entry point (main.go)
/internal
    /db             # Database connection (pgxpool)
    /ingest         # Sharadar JSON parsing & ingestion logic
    /models         # Go Structs reflecting DB tables
    /analysis       # Screening logic (Strategy Pattern)
    /handlers       # HTTP Handlers (Dashboard, Rebalance)
    /views          # Templ components (.templ files)
/sql/migrations     # Goose migration files (.sql)
docker-compose.yml  # Local infrastructure
go.mod
```

## 4. Infrastructure (Docker)

**File:** `docker-compose.yml`

```yaml
services:
  db:
    image: postgres:16-alpine
    ports:
      - "5432:5432"
    environment:
      - POSTGRES_USER=value_user
      - POSTGRES_PASSWORD=value_pass
      - POSTGRES_DB=value_db
    volumes:
      - ./pgdata:/var/lib/postgresql/data
```

## 5. Database Schema

**File:** `sql/migrations/001_initial_schema.sql`

We use a "Flat Metric" approach optimized for the Sharadar SF1 dataset.

```sql
-- +goose Up

-- 1. COMPANIES: Master list
CREATE TABLE companies (
    ticker TEXT PRIMARY KEY,
    name TEXT,
    sector TEXT,
    industry TEXT,
    active BOOLEAN DEFAULT TRUE
);

-- 2. METRICS: The "Sharadar" Data
CREATE TABLE financial_metrics (
    id SERIAL PRIMARY KEY,
    ticker TEXT REFERENCES companies(ticker),
    date_key DATE NOT NULL,          -- API field: 'datekey'
    report_period DATE NOT NULL,     -- API field: 'reportperiod'

    -- Fundamentals
    revenue BIGINT,
    net_income BIGINT,               -- 'netinc'
    ebitda BIGINT,
    fcf BIGINT,                      -- 'fcf' (Free Cash Flow)

    -- Valuation Ratios (Pre-calculated by Sharadar)
    roic DOUBLE PRECISION,           -- Return on Invested Capital
    pe_ratio DOUBLE PRECISION,       -- 'pe'
    ev_ebit DOUBLE PRECISION,        -- 'evebit' (The Acquirer's Multiple)
    pb_ratio DOUBLE PRECISION,       -- 'pb'
    debt_to_equity DOUBLE PRECISION, -- 'de'

    -- Market Data
    market_cap BIGINT,               -- 'marketcap'
    enterprise_value BIGINT,         -- 'ev'
    price DOUBLE PRECISION,

    last_updated DATE DEFAULT CURRENT_DATE,
    UNIQUE(ticker, date_key)
);

-- 3. PORTFOLIO: Current Holdings
CREATE TABLE portfolio (
    ticker TEXT REFERENCES companies(ticker),
    shares_owned INT NOT NULL DEFAULT 0,
    cost_basis DECIMAL(12, 2),
    target_weight DECIMAL(5, 2) DEFAULT 0.16, -- 1/6th of portfolio
    acquired_date DATE
);

-- +goose Down
DROP TABLE portfolio;
DROP TABLE financial_metrics;
DROP TABLE companies;
```

## 6. Data Ingestion (Sharadar Parser)

**Source:** Nasdaq Data Link Tables API.
**Endpoint:** `https://data.nasdaq.com/api/v3/datatables/SHARADAR/SF1.json`

### Parsing Logic (`internal/ingest/parser.go`)

The API returns data in column-oriented arrays. We must map them dynamically.

```go
type SharadarResponse struct {
    Datatable struct {
        Data    [][]interface{} `json:"data"`
        Columns []struct {
            Name string `json:"name"`
            Type string `json:"type"`
        } `json:"columns"`
    } `json:"datatable"`
}

// Logic:
// 1. Unmarshal JSON.
// 2. Build map[string]int of Column Names -> Indices.
// 3. Iterate Data rows.
// 4. Extract "roic" using the map index.
// 5. Insert into DB.
```

## 7. Analysis Engine (Strategy Pattern)

**File:** `internal/analysis/strategy.go`

We use interfaces to allow swapping strategies (e.g., Magic Formula vs. Dividend Yield) without code changes.

```go
// The Contract
type Strategy interface {
    Name() string
    RunScreen(ctx context.Context, db *pgxpool.Pool) ([]models.Recommendation, error)
}

// Implementation 1: MagicFormula
// Path: internal/analysis/magic_formula.go
// Logic:
// 1. Filter: MarketCap > 500M, Debt/Equity < 0.5.
// 2. Rank: ROIC (Descending) AND EV/EBIT (Ascending).
// 3. Score: Sum of ranks.
// 4. Select: Top 6, max 2 per sector.
```

## 8. Backtesting Engine

**Purpose:** Validate strategy effectiveness against historical data before committing real capital.

### Core Functionality

```go
type BacktestResult struct {
    StrategyName    string
    StartDate       time.Time
    EndDate         time.Time
    InitialCapital  decimal.Decimal
    FinalValue      decimal.Decimal
    TotalReturn     float64         // Percentage
    CAGR            float64         // Compound Annual Growth Rate
    MaxDrawdown     float64         // Worst peak-to-trough decline
    SharpeRatio     float64         // Risk-adjusted return
    Trades          []Trade         // All rebalancing actions
}

type Backtest interface {
    Run(ctx context.Context, strategy Strategy, startDate, endDate time.Time) (*BacktestResult, error)
}
```

### Logic
1. Start at `startDate` with initial capital
2. Run strategy to get initial portfolio allocation
3. Step forward quarterly (rebalance dates)
4. At each rebalance: run strategy, calculate new weights, simulate trades
5. Track portfolio value using historical prices
6. Calculate performance metrics at end

## 9. Benchmark Comparison

**Purpose:** Compare strategy returns against S&P 500 to answer "does this beat the market?"

### Metrics Comparison Table

| Metric | Strategy | S&P 500 | Delta |
|--------|----------|---------|-------|
| Total Return | +87.3% | +62.1% | +25.2% |
| CAGR | 14.2% | 10.8% | +3.4% |
| Max Drawdown | -18.4% | -23.6% | +5.2% |
| Sharpe Ratio | 1.24 | 0.89 | +0.35 |

### Data Requirements
- Need historical S&P 500 prices (can use SPY ETF as proxy)
- Store in `benchmark_prices` table or fetch on-demand

## 10. Percentage-Based Rebalancing

**Purpose:** Show precise allocation adjustments, not just buy/sell signals.

### Rebalancing Output

```
Ticker   Current%   Target%    Action      Amount
─────────────────────────────────────────────────
AAPL     18.2%      16.67%     Trim        -$1,530
MSFT     12.1%      16.67%     Add         +$4,570
GOOGL    14.8%      16.67%     Add         +$1,870
NVDA      0.0%      16.67%     Buy         +$16,670
JNJ      22.4%      16.67%     Trim        -$5,730
XOM      15.3%      16.67%     Add         +$1,370
META     17.2%       0.00%     Sell        -$17,200
─────────────────────────────────────────────────
Total Portfolio Value: $100,000
```

### Rebalancing Logic
1. Calculate current weight: `(shares × current_price) / total_portfolio_value`
2. Compare to target weight (equal weight = 16.67% for 6 stocks)
3. Calculate dollar amount to adjust: `(target% - current%) × total_value`
4. Generate actionable trade list

## 11. UI/UX Specification

### Dashboard (`views/dashboard.templ`)

#### Portfolio Summary
- Simple HTML Table
- **Columns:** Ticker, Shares, Avg Price, Current Price, Weight%, Unr. P/L

#### Action Panel
- **Dropdown:** Select Strategy (default: "Magic Formula")
- **Button:** `[ Run Analysis ]`
  - `hx-post="/analyze"`
  - `hx-target="#results-area"`
  - `hx-indicator="#loading"`

#### Results Area (Partial)
- Displays "Proposed Portfolio" with percentage allocations
- Rebalancing table with current vs target weights
- Dollar amounts for each trade

#### Backtest Panel
- Date range selector (start/end)
- **Button:** `[ Run Backtest ]`
- Results: performance chart, metrics table, benchmark comparison

## 12. Implementation Phases

### Phase 1: Skeleton (Core Infrastructure)
- [ ] Create `docker-compose.yml` and start Postgres
- [ ] Run Goose migrations for initial schema
- [ ] Setup Go module, Echo router, Templ views
- [ ] Create basic models and DB connection (pgxpool)
- [ ] Wire up `main.go` with basic health endpoint

### Phase 2: Data Layer
- [ ] Implement Sharadar API client and parser
- [ ] Build data ingestion pipeline
- [ ] Add benchmark price data (SPY)

### Phase 3: Strategy Engine
- [ ] Implement `Strategy` interface
- [ ] Build first strategy: Magic Formula
- [ ] Add strategy registry for swappable strategies

### Phase 4: Backtesting
- [ ] Implement `Backtest` interface
- [ ] Build backtesting engine with quarterly rebalancing
- [ ] Calculate performance metrics (CAGR, Sharpe, MaxDD)
- [ ] Add S&P 500 benchmark comparison

### Phase 5: Portfolio & Rebalancing
- [ ] Implement percentage-based rebalancing logic
- [ ] Generate trade recommendations with dollar amounts
- [ ] Build portfolio tracking

### Phase 6: UI
- [ ] Dashboard with portfolio summary
- [ ] Strategy selection and analysis panel
- [ ] Backtest results with charts
- [ ] Rebalancing table view

## 13. Deployment

### Production Stack
- **Hosting:** VPS (e.g., Hetzner, DigitalOcean)
- **Reverse Proxy:** Caddy (automatic HTTPS, simple config)
- **Containers:** Docker Compose for app + Postgres
- **Future:** Terraform for VPS provisioning and infrastructure-as-code

### Docker Compose (Production)

```yaml
services:
  app:
    build: .
    restart: unless-stopped
    environment:
      - DATABASE_URL=postgres://value_user:value_pass@db:5432/value_db
    depends_on:
      - db

  db:
    image: postgres:16-alpine
    restart: unless-stopped
    volumes:
      - pgdata:/var/lib/postgresql/data
    environment:
      - POSTGRES_USER=value_user
      - POSTGRES_PASSWORD=value_pass
      - POSTGRES_DB=value_db

  caddy:
    image: caddy:2-alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - caddy_data:/data

volumes:
  pgdata:
  caddy_data:
```

### Caddyfile

```
yourdomain.com {
    reverse_proxy app:8080
}
```

## 14. Tailwind CSS Style

Using Catppuccin theme: https://github.com/catppuccin/tailwindcss