# DeepValue

A personal value investing portfolio manager that automates stock screening, backtesting, and rebalancing.

## Overview

DeepValue helps you:
- Screen stocks using value investing strategies (Magic Formula, etc.)
- Backtest strategies against historical data
- Compare performance vs S&P 500
- Generate percentage-based rebalancing instructions

**Strategy:** Buy high-quality companies (High ROIC) at a discount (Low EV/EBIT).

## Tech Stack

| Component | Choice |
|-----------|--------|
| Language | Go 1.23+ |
| Database | PostgreSQL 18 |
| Web Framework | Echo |
| Templates | Templ |
| Frontend | HTMX |
| Styling | Tailwind 4 + Catppuccin |
| Migrations | Goose (embedded) |
| Data Source | Nasdaq Data Link (Sharadar SF1) |

## Quick Start

```bash
# First time setup
make setup          # Install npm deps + Go tools

# Start services
make db-up          # Start Postgres + pgweb

# Run the app
make dev            # Start with hot reload
```

**URLs:**
- App: http://localhost:8080
- pgweb (DB UI): http://localhost:8081

## Environment Variables

Copy `.env.example` to `.env` and configure:

```bash
DATABASE_URL=postgres://value_user:value_pass@localhost:5432/value_db?sslmode=disable
PORT=8080
NASDAQ_API_KEY=your-api-key-here
```

## Project Structure

```
cmd/app/                    # Entry point
internal/
    db/                     # Database connection + migrations
        migrations/         # SQL migration files
    models/                 # Go structs
    handlers/               # HTTP handlers
    views/                  # Templ components
assets/
    css/                    # Tailwind input/output
docker-compose.yml          # Postgres + pgweb
Makefile                    # Build commands
```

## Make Commands

```bash
make build          # Build binary
make run            # Run app
make dev            # Run with hot reload (Air)
make db-up          # Start Postgres
make db-down        # Stop Postgres
make migrate-create # Create new migration
make css-build      # Build Tailwind CSS
make css-watch      # Watch CSS changes
make templ-generate # Generate templ files
make setup          # First-time setup
```

## Theme

DeepValue uses Catppuccin with 4 flavors:
- **Latte** (light)
- **Frappe** (medium dark)
- **Macchiato** (darker)
- **Mocha** (darkest, default)

Toggle themes via the dropdown in the nav bar.