package ingest

import (
	"time"

	"github.com/shopspring/decimal"
)

// Response is the raw API response from Nasdaq Data Link Tables API.
// The data is column-oriented: columns define the schema, data contains rows as arrays.
type Response struct {
	Datatable struct {
		Data    [][]interface{} `json:"data"`
		Columns []Column        `json:"columns"`
	} `json:"datatable"`
	Meta struct {
		NextCursorID *string `json:"next_cursor_id"`
	} `json:"meta"`
}

// Column describes a column in the response.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TickerRow represents a row from SHARADAR/TICKERS table.
type TickerRow struct {
	Ticker       string
	Name         string
	Exchange     string
	Sector       string
	Industry     string
	ScaleRevenue string
	IsDelisted   bool
	LastUpdated  *time.Time
}

// SF1Row represents a row from SHARADAR/SF1 table (fundamentals).
type SF1Row struct {
	Ticker       string
	Dimension    string
	CalendarDate time.Time
	DateKey      time.Time
	LastUpdated  *time.Time

	// Key fundamentals we need for screening
	Revenue         *decimal.Decimal
	NetIncome       *decimal.Decimal
	EBITDA          *decimal.Decimal
	FCF             *decimal.Decimal
	ROIC            *decimal.Decimal
	PE              *decimal.Decimal
	EVEBIT          *decimal.Decimal
	PB              *decimal.Decimal
	DE              *decimal.Decimal // Debt to Equity
	MarketCap       *decimal.Decimal
	EV              *decimal.Decimal
	Price           *decimal.Decimal
	ReportPeriod    *time.Time
}

// DailyRow represents a row from SHARADAR/DAILY table (daily prices).
type DailyRow struct {
	Ticker          string
	Date            time.Time
	Open            *decimal.Decimal
	High            *decimal.Decimal
	Low             *decimal.Decimal
	Close           *decimal.Decimal
	Volume          *int64
	Dividends       *decimal.Decimal
	CloseUnadj      *decimal.Decimal
	MarketCap       *decimal.Decimal
	EV              *decimal.Decimal
	PE              *decimal.Decimal
	PB              *decimal.Decimal
	LastUpdated     *time.Time
}

// SP500Row represents a row from SHARADAR/SP500 table.
type SP500Row struct {
	Date      time.Time
	Action    string // "current", "added", "removed"
	Ticker    string
	Name      string
	Conticker string
	Conname   string
}