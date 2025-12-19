package models

import (
	"time"

	"github.com/shopspring/decimal"
)

type Setting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Company struct {
	Ticker    string    `json:"ticker"`
	Name      string    `json:"name"`
	Sector    string    `json:"sector"`
	Industry  string    `json:"industry"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type FinancialMetric struct {
	ID              int             `json:"id"`
	Ticker          string          `json:"ticker"`
	Dimension       string          `json:"dimension"` // ARQ, MRQ, ARY, MRY, ART, MRT
	DateKey         time.Time       `json:"date_key"`
	ReportPeriod    time.Time       `json:"report_period"`
	Revenue         decimal.Decimal `json:"revenue"`
	NetIncome       decimal.Decimal `json:"net_income"`
	EBITDA          decimal.Decimal `json:"ebitda"`
	FCF             decimal.Decimal `json:"fcf"`
	ROIC            decimal.Decimal `json:"roic"`
	PERatio         decimal.Decimal `json:"pe_ratio"`
	EVEBIT          decimal.Decimal `json:"ev_ebit"`
	PBRatio         decimal.Decimal `json:"pb_ratio"`
	DebtToEquity    decimal.Decimal `json:"debt_to_equity"`
	MarketCap       decimal.Decimal `json:"market_cap"`
	EnterpriseValue decimal.Decimal `json:"enterprise_value"`
	Price           decimal.Decimal `json:"price"`
	LastUpdated     *time.Time      `json:"last_updated"` // From Sharadar API
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type PortfolioHolding struct {
	ID           int             `json:"id"`
	Ticker       string          `json:"ticker"`
	SharesOwned  decimal.Decimal `json:"shares_owned"`
	CostBasis    decimal.Decimal `json:"cost_basis"`
	TargetWeight decimal.Decimal `json:"target_weight"`
	AcquiredDate time.Time       `json:"acquired_date"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type DailyPrice struct {
	ID              int             `json:"id"`
	Ticker          string          `json:"ticker"`
	Date            time.Time       `json:"date"`
	Open            decimal.Decimal `json:"open"`
	High            decimal.Decimal `json:"high"`
	Low             decimal.Decimal `json:"low"`
	Close           decimal.Decimal `json:"close"`
	Volume          int64           `json:"volume"`
	Dividends       decimal.Decimal `json:"dividends"`
	CloseUnadj      decimal.Decimal `json:"close_unadj"`
	MarketCap       decimal.Decimal `json:"market_cap"`
	EnterpriseValue decimal.Decimal `json:"enterprise_value"`
	PERatio         decimal.Decimal `json:"pe_ratio"`
	PBRatio         decimal.Decimal `json:"pb_ratio"`
	LastUpdated     *time.Time      `json:"last_updated"` // From Sharadar API
	CreatedAt       time.Time       `json:"created_at"`
}
