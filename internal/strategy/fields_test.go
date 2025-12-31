package strategy

import (
	"testing"
)

func TestGetAvailableFields(t *testing.T) {
	fields := GetAvailableFields()

	if len(fields) == 0 {
		t.Error("Expected some available fields, got 0")
	}

	// Verify we have some expected fields
	fieldNames := make(map[string]bool)
	for _, f := range fields {
		fieldNames[f.Name] = true
	}

	expectedFields := []string{"roic", "ev_ebit", "market_cap", "sector", "pe_ratio"}
	for _, name := range expectedFields {
		if !fieldNames[name] {
			t.Errorf("Expected field %s to be available", name)
		}
	}
}

func TestGetRankableFields(t *testing.T) {
	fields := GetRankableFields()

	if len(fields) == 0 {
		t.Error("Expected some rankable fields, got 0")
	}

	// Verify all returned fields are rankable
	for _, f := range fields {
		if !f.Rankable {
			t.Errorf("Field %s in rankable fields is not rankable", f.Name)
		}
	}

	// Verify sector is NOT in rankable fields
	for _, f := range fields {
		if f.Name == "sector" {
			t.Error("sector should not be in rankable fields")
		}
	}
}

func TestValidateOperator(t *testing.T) {
	testCases := []struct {
		name        string
		fieldName   string
		operator    string
		expectError bool
	}{
		{"valid numeric operator", "roic", ">=", false},
		{"valid numeric operator less", "market_cap", "<", false},
		{"valid text operator", "sector", "=", false},
		{"valid text in operator", "sector", "in", false},
		{"invalid operator for text", "sector", ">=", true},
		{"invalid operator for numeric", "roic", "in", true},
		{"unknown field", "unknown", "=", true},
		{"between for numeric", "market_cap", "between", false},
		{"is_null for any", "dividend_yield", "is_null", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOperator(tc.fieldName, tc.operator)
			if tc.expectError && err == nil {
				t.Errorf("Expected error for %s, got nil", tc.name)
			}
			if !tc.expectError && err != nil {
				t.Errorf("Expected no error for %s, got: %v", tc.name, err)
			}
		})
	}
}

func TestFieldMeta_Tables(t *testing.T) {
	// Check that fields have correct table assignments
	testCases := []struct {
		fieldName string
		table     string
	}{
		{"roic", "financial_metrics"},
		{"market_cap", "financial_metrics"},
		{"sector", "companies"},
		{"industry", "companies"},
		{"dividend_yield", "daily_prices"},
		{"price", "daily_prices"},
	}

	for _, tc := range testCases {
		t.Run(tc.fieldName, func(t *testing.T) {
			field, ok := AvailableFields[tc.fieldName]
			if !ok {
				t.Fatalf("Field %s not found", tc.fieldName)
			}
			if field.Table != tc.table {
				t.Errorf("Expected table %s for field %s, got %s", tc.table, tc.fieldName, field.Table)
			}
		})
	}
}

func TestFieldMeta_Types(t *testing.T) {
	numericFields := []string{"roic", "ev_ebit", "market_cap", "pe_ratio", "debt_to_equity"}
	textFields := []string{"sector", "industry"}

	for _, name := range numericFields {
		field, ok := AvailableFields[name]
		if !ok {
			t.Fatalf("Field %s not found", name)
		}
		if field.Type != "number" {
			t.Errorf("Expected field %s to be number type, got %s", name, field.Type)
		}
	}

	for _, name := range textFields {
		field, ok := AvailableFields[name]
		if !ok {
			t.Fatalf("Field %s not found", name)
		}
		if field.Type != "text" {
			t.Errorf("Expected field %s to be text type, got %s", name, field.Type)
		}
	}
}
