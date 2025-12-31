package strategy

import "fmt"

// FieldMeta describes an available field for filtering/ranking
type FieldMeta struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Table       string   `json:"table"`     // financial_metrics or daily_prices
	Column      string   `json:"column"`    // actual DB column name if different
	Type        string   `json:"type"`      // number, text, boolean
	Operators   []string `json:"operators"` // Valid operators for this field
	Description string   `json:"description"`
	Rankable    bool     `json:"rankable"` // Can this field be used in ranking?
}

// Supported operators
const (
	OpGreaterEqual = ">="
	OpLessEqual    = "<="
	OpGreater      = ">"
	OpLess         = "<"
	OpEqual        = "="
	OpNotEqual     = "!="
	OpIn           = "in"
	OpNotIn        = "not_in"
	OpBetween      = "between"
	OpIsNull       = "is_null"
	OpIsNotNull    = "is_not_null"
	OpGteMedian    = ">=median" // Greater than or equal to median of the field
	OpGteP25       = ">=p25"    // Greater than or equal to 25th percentile
	OpGteP75       = ">=p75"    // Greater than or equal to 75th percentile
	OpLteMedian    = "<=median" // Less than or equal to median
	OpLteP25       = "<=p25"    // Less than or equal to 25th percentile
	OpLteP75       = "<=p75"    // Less than or equal to 75th percentile
)

// Operator groups
var (
	NumericOperators = []string{OpGreaterEqual, OpLessEqual, OpGreater, OpLess, OpEqual, OpNotEqual, OpBetween, OpIsNull, OpIsNotNull, OpGteMedian, OpGteP25, OpGteP75, OpLteMedian, OpLteP25, OpLteP75}
	TextOperators    = []string{OpEqual, OpNotEqual, OpIn, OpNotIn, OpIsNull, OpIsNotNull}
)

// AvailableFields contains all fields that can be used in strategies
var AvailableFields = map[string]FieldMeta{
	// From financial_metrics table
	"market_cap": {
		Name:        "market_cap",
		Label:       "Market Cap",
		Table:       "financial_metrics",
		Column:      "market_cap",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Company market capitalization in USD",
		Rankable:    true,
	},
	"roic": {
		Name:        "roic",
		Label:       "ROIC",
		Table:       "financial_metrics",
		Column:      "roic",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Return on Invested Capital",
		Rankable:    true,
	},
	"ev_ebit": {
		Name:        "ev_ebit",
		Label:       "EV/EBIT",
		Table:       "financial_metrics",
		Column:      "ev_ebit",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Enterprise Value to EBIT (Acquirer's Multiple)",
		Rankable:    true,
	},
	"pe_ratio": {
		Name:        "pe_ratio",
		Label:       "P/E Ratio",
		Table:       "financial_metrics",
		Column:      "pe_ratio",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Price to Earnings ratio",
		Rankable:    true,
	},
	"pb_ratio": {
		Name:        "pb_ratio",
		Label:       "P/B Ratio",
		Table:       "financial_metrics",
		Column:      "pb_ratio",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Price to Book ratio",
		Rankable:    true,
	},
	"debt_to_equity": {
		Name:        "debt_to_equity",
		Label:       "Debt/Equity",
		Table:       "financial_metrics",
		Column:      "debt_to_equity",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Debt to Equity ratio (leverage)",
		Rankable:    true,
	},
	"revenue": {
		Name:        "revenue",
		Label:       "Revenue",
		Table:       "financial_metrics",
		Column:      "revenue",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Total revenue in USD",
		Rankable:    true,
	},
	"net_income": {
		Name:        "net_income",
		Label:       "Net Income",
		Table:       "financial_metrics",
		Column:      "net_income",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Net income in USD",
		Rankable:    true,
	},
	"fcf": {
		Name:        "fcf",
		Label:       "Free Cash Flow",
		Table:       "financial_metrics",
		Column:      "fcf",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Free Cash Flow in USD",
		Rankable:    true,
	},
	"ebitda": {
		Name:        "ebitda",
		Label:       "EBITDA",
		Table:       "financial_metrics",
		Column:      "ebitda",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Earnings Before Interest, Taxes, Depreciation, and Amortization",
		Rankable:    true,
	},
	"enterprise_value": {
		Name:        "enterprise_value",
		Label:       "Enterprise Value",
		Table:       "financial_metrics",
		Column:      "enterprise_value",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Enterprise Value in USD",
		Rankable:    true,
	},
	"assets": {
		Name:        "assets",
		Label:       "Total Assets",
		Table:       "financial_metrics",
		Column:      "assets",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Total assets in USD",
		Rankable:    true,
	},
	"gross_profit": {
		Name:        "gross_profit",
		Label:       "Gross Profit",
		Table:       "financial_metrics",
		Column:      "gross_profit",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Gross profit in USD",
		Rankable:    true,
	},
	"roe": {
		Name:        "roe",
		Label:       "ROE",
		Table:       "financial_metrics",
		Column:      "roe",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Return on Equity (Net Income / Equity)",
		Rankable:    true,
	},
	"roa": {
		Name:        "roa",
		Label:       "ROA",
		Table:       "financial_metrics",
		Column:      "roa",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Return on Assets (Net Income / Assets)",
		Rankable:    true,
	},
	"gp_a": {
		Name:        "gp_a",
		Label:       "GP/A",
		Table:       "financial_metrics",
		Column:      "gp_a",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Gross Profit to Assets ratio",
		Rankable:    true,
	},
	"accruals": {
		Name:        "accruals",
		Label:       "Accruals",
		Table:       "financial_metrics",
		Column:      "accruals",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Earnings Quality (FCF / Net Income)",
		Rankable:    true,
	},

	// From companies table
	"sector": {
		Name:        "sector",
		Label:       "Sector",
		Table:       "companies",
		Column:      "sector",
		Type:        "text",
		Operators:   TextOperators,
		Description: "Company sector classification",
		Rankable:    false,
	},
	"industry": {
		Name:        "industry",
		Label:       "Industry",
		Table:       "companies",
		Column:      "industry",
		Type:        "text",
		Operators:   TextOperators,
		Description: "Company industry classification",
		Rankable:    false,
	},

	// From daily_prices table (latest)
	"price": {
		Name:        "price",
		Label:       "Stock Price",
		Table:       "daily_prices",
		Column:      "close_price",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Current stock price",
		Rankable:    true,
	},
	"dividend_yield": {
		Name:        "dividend_yield",
		Label:       "Dividend Yield",
		Table:       "daily_prices",
		Column:      "dividend_yield",
		Type:        "number",
		Operators:   NumericOperators,
		Description: "Annual dividend yield percentage",
		Rankable:    true,
	},
}

// GetAvailableFields returns all available fields as a slice
func GetAvailableFields() []FieldMeta {
	fields := make([]FieldMeta, 0, len(AvailableFields))
	for _, f := range AvailableFields {
		fields = append(fields, f)
	}
	return fields
}

// GetRankableFields returns only fields that can be used in ranking
func GetRankableFields() []FieldMeta {
	fields := make([]FieldMeta, 0)
	for _, f := range AvailableFields {
		if f.Rankable {
			fields = append(fields, f)
		}
	}
	return fields
}

// ValidateField checks if a field name is valid
func ValidateField(name string) error {
	if _, ok := AvailableFields[name]; !ok {
		return fmt.Errorf("unknown field: %s", name)
	}
	return nil
}

// ValidateOperator checks if an operator is valid for a field
func ValidateOperator(fieldName, operator string) error {
	field, ok := AvailableFields[fieldName]
	if !ok {
		return fmt.Errorf("unknown field: %s", fieldName)
	}

	for _, op := range field.Operators {
		if op == operator {
			return nil
		}
	}
	return fmt.Errorf("operator %s is not valid for field %s", operator, fieldName)
}

// ValidateRankableField checks if a field can be used in ranking
func ValidateRankableField(name string) error {
	field, ok := AvailableFields[name]
	if !ok {
		return fmt.Errorf("unknown field: %s", name)
	}
	if !field.Rankable {
		return fmt.Errorf("field %s cannot be used in ranking", name)
	}
	return nil
}
