package ingest

import (
	"fmt"
	"log"
	"time"

	"github.com/shopspring/decimal"
)

// buildColumnIndex creates a map from column name to array index.
func buildColumnIndex(columns []Column) map[string]int {
	idx := make(map[string]int, len(columns))
	for i, col := range columns {
		idx[col.Name] = i
	}
	return idx
}

// getString safely extracts a string from row data.
func getString(row []interface{}, idx map[string]int, col string) string {
	i, ok := idx[col]
	if !ok || i >= len(row) || row[i] == nil {
		return ""
	}
	if s, ok := row[i].(string); ok {
		return s
	}
	return fmt.Sprintf("%v", row[i])
}

// getBool safely extracts a boolean from row data.
func getBool(row []interface{}, idx map[string]int, col string) bool {
	i, ok := idx[col]
	if !ok || i >= len(row) || row[i] == nil {
		return false
	}
	switch v := row[i].(type) {
	case bool:
		return v
	case string:
		return v == "Y" || v == "true" || v == "1"
	case float64:
		return v != 0
	}
	return false
}

// getDecimal safely extracts a decimal from row data.
func getDecimal(row []interface{}, idx map[string]int, col string) *decimal.Decimal {
	i, ok := idx[col]
	if !ok || i >= len(row) || row[i] == nil {
		return nil
	}
	switch v := row[i].(type) {
	case float64:
		d := decimal.NewFromFloat(v)
		return &d
	case string:
		d, err := decimal.NewFromString(v)
		if err != nil {
			return nil
		}
		return &d
	}
	return nil
}

// getInt64 safely extracts an int64 from row data.
func getInt64(row []interface{}, idx map[string]int, col string) *int64 {
	i, ok := idx[col]
	if !ok || i >= len(row) || row[i] == nil {
		return nil
	}
	switch v := row[i].(type) {
	case float64:
		n := int64(v)
		return &n
	case int64:
		return &v
	case int:
		n := int64(v)
		return &n
	}
	return nil
}

// getTime safely extracts a time.Time from row data (expects YYYY-MM-DD format).
func getTime(row []interface{}, idx map[string]int, col string) *time.Time {
	i, ok := idx[col]
	if !ok || i >= len(row) || row[i] == nil {
		return nil
	}
	if s, ok := row[i].(string); ok && s != "" {
		// Try multiple formats
		formats := []string{
			"2006-01-02",
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, format := range formats {
			if t, err := time.Parse(format, s); err == nil {
				return &t
			}
		}
	}
	return nil
}

// ParseTickers parses a SHARADAR/TICKERS response into typed rows.
func ParseTickers(resp *Response) ([]TickerRow, error) {
	idx := buildColumnIndex(resp.Datatable.Columns)
	rows := make([]TickerRow, 0, len(resp.Datatable.Data))

	for _, row := range resp.Datatable.Data {
		tr := TickerRow{
			Ticker:       getString(row, idx, "ticker"),
			Name:         getString(row, idx, "name"),
			Exchange:     getString(row, idx, "exchange"),
			Sector:       getString(row, idx, "sector"),
			Industry:     getString(row, idx, "industry"),
			ScaleRevenue: getString(row, idx, "scalerevenue"),
			IsDelisted:   getBool(row, idx, "isdelisted"),
			LastUpdated:  getTime(row, idx, "lastupdated"),
		}
		if tr.Ticker != "" {
			rows = append(rows, tr)
		}
	}

	return rows, nil
}

// ParseSF1 parses a SHARADAR/SF1 response into typed rows.
func ParseSF1(resp *Response) ([]SF1Row, error) {
	idx := buildColumnIndex(resp.Datatable.Columns)
	rows := make([]SF1Row, 0, len(resp.Datatable.Data))

	for _, row := range resp.Datatable.Data {
		dateKey := getTime(row, idx, "datekey")
		if dateKey == nil {
			continue // Skip rows without a datekey
		}

		calendarDate := getTime(row, idx, "calendardate")
		if calendarDate == nil {
			calendarDate = dateKey
		}

		sr := SF1Row{
			Ticker:       getString(row, idx, "ticker"),
			Dimension:    getString(row, idx, "dimension"),
			CalendarDate: *calendarDate,
			DateKey:      *dateKey,
			LastUpdated:  getTime(row, idx, "lastupdated"),

			Revenue:      getDecimal(row, idx, "revenue"),
			NetIncome:    getDecimal(row, idx, "netinc"),
			EBITDA:       getDecimal(row, idx, "ebitda"),
			FCF:          getDecimal(row, idx, "fcf"),
			ROIC:         getDecimal(row, idx, "roic"),
			PE:           getDecimal(row, idx, "pe"),
			EVEBIT:       getDecimal(row, idx, "evebit"),
			PB:           getDecimal(row, idx, "pb"),
			DE:           getDecimal(row, idx, "de"),
			MarketCap:    getDecimal(row, idx, "marketcap"),
			EV:           getDecimal(row, idx, "ev"),
			Price:        getDecimal(row, idx, "price"),
			ReportPeriod: getTime(row, idx, "reportperiod"),
		}
		if sr.Ticker != "" {
			rows = append(rows, sr)
		}
	}

	return rows, nil
}

// ParseDaily parses a SHARADAR/DAILY response into typed rows.
func ParseDaily(resp *Response) ([]DailyRow, error) {
	idx := buildColumnIndex(resp.Datatable.Columns)
	rows := make([]DailyRow, 0, len(resp.Datatable.Data))

	// Debug: log column names and first row
	colNames := make([]string, 0, len(resp.Datatable.Columns))
	for _, col := range resp.Datatable.Columns {
		colNames = append(colNames, col.Name)
	}
	log.Printf("DAILY columns: %v", colNames)
	if len(resp.Datatable.Data) > 0 {
		log.Printf("DAILY first row sample: %v", resp.Datatable.Data[0])
	}

	for i, row := range resp.Datatable.Data {
		date := getTime(row, idx, "date")
		if date == nil {
			continue
		}

		dr := DailyRow{
			Ticker:      getString(row, idx, "ticker"),
			Date:        *date,
			Open:        getDecimal(row, idx, "open"),
			High:        getDecimal(row, idx, "high"),
			Low:         getDecimal(row, idx, "low"),
			Close:       getDecimal(row, idx, "close"),
			Volume:      getInt64(row, idx, "volume"),
			Dividends:   getDecimal(row, idx, "dividends"),
			CloseUnadj:  getDecimal(row, idx, "closeunadj"),
			MarketCap:   getDecimal(row, idx, "marketcap"),
			EV:          getDecimal(row, idx, "ev"),
			PE:          getDecimal(row, idx, "pe"),
			PB:          getDecimal(row, idx, "pb"),
			LastUpdated: getTime(row, idx, "lastupdated"),
		}

		// Debug: log first parsed row
		if i == 0 {
			log.Printf("DAILY first parsed row: Ticker=%s Date=%s Open=%v High=%v Low=%v Close=%v Volume=%v MarketCap=%v",
				dr.Ticker, dr.Date.Format("2006-01-02"), dr.Open, dr.High, dr.Low, dr.Close, dr.Volume, dr.MarketCap)
		}

		if dr.Ticker != "" {
			rows = append(rows, dr)
		}
	}

	return rows, nil
}

// ParseSP500 parses a SHARADAR/SP500 response into typed rows.
func ParseSP500(resp *Response) ([]SP500Row, error) {
	idx := buildColumnIndex(resp.Datatable.Columns)
	rows := make([]SP500Row, 0, len(resp.Datatable.Data))

	for _, row := range resp.Datatable.Data {
		date := getTime(row, idx, "date")

		sr := SP500Row{
			Action:    getString(row, idx, "action"),
			Ticker:    getString(row, idx, "ticker"),
			Name:      getString(row, idx, "name"),
			Conticker: getString(row, idx, "conticker"),
			Conname:   getString(row, idx, "conname"),
		}
		if date != nil {
			sr.Date = *date
		}
		if sr.Ticker != "" {
			rows = append(rows, sr)
		}
	}

	return rows, nil
}