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
