# Value Investing Portfolio Manager (Go-Lith)

## 📌 Project Overview
This application is a monolithic "Value Investing" tool designed to manage a high-conviction portfolio of exactly 6 stocks. It ingests financial data, runs a fundamental analysis screener (Magic Formula style), and generates quarterly rebalancing instructions.

**Primary Goal:** Automate the selection of high-quality companies (High ROIC) selling at a discount (Low EV/EBIT).

---

## 🛠 Tech Stack
* **Language:** Go (Golang) 1.23+
* **Database:** PostgreSQL 16
* **Data Source:** Nasdaq Data Link (Sharadar Core US Fundamentals / SF1)
* **Web Server:** `Chi` or `net/http`
* **Frontend:** `HTMX` (Interactivity) + `Templ` (Type-safe HTML)
* **Styling:** TailwindCSS (via CDN)
* **Migrations:** `goose`
* **Driver:** `pgx/v5`

---

## 📂 Project Structure
Maintain the following standard Go project layout:

```text
/cmd/app/           # Main entry point
/internal
    /db             # Database connection (pgxpool)
    /ingest         # Sharadar API JSON parsing & ingestion
    /models         # Go structs matching DB tables
    /analysis       # Screening & Ranking logic
    /handlers       # HTTP Handlers (Dashboard, Actions)
    /views          # Templ (.templ) UI components
/sql/migrations     # Goose SQL migration files
docker-compose.yml  # Local Dev Infrastructure
