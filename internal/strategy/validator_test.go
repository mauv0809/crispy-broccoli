package strategy

import (
	"testing"
)

func TestValidateRules_ValidMagicFormula(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Filters: []Filter{
			{Field: "market_cap", Operator: ">=", Value: float64(500000000)},
			{Field: "debt_to_equity", Operator: "<=", Value: float64(0.5)},
			{Field: "sector", Operator: "not_in", Value: []any{"Financial Services"}},
		},
		Ranking: []Ranking{
			{Field: "roic", Direction: "desc", Weight: 50},
			{Field: "ev_ebit", Direction: "asc", Weight: 50},
		},
		Dimension: "MRQ",
		Limit:     6,
	}

	err := v.ValidateRules(rules)
	if err != nil {
		t.Errorf("Expected valid rules, got error: %v", err)
	}
}

func TestValidateRules_InvalidDimension(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Filters: []Filter{
			{Field: "market_cap", Operator: ">=", Value: float64(1000000)},
		},
		Dimension: "INVALID",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err == nil {
		t.Error("Expected error for invalid dimension, got nil")
	}
}

func TestValidateRules_InvalidLimit(t *testing.T) {
	v := NewValidator()

	testCases := []struct {
		name  string
		limit int
	}{
		{"zero limit", 0},
		{"negative limit", -1},
		{"too high limit", 101},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rules := Rules{
				Filters: []Filter{
					{Field: "roic", Operator: ">", Value: float64(0)},
				},
				Dimension: "MRQ",
				Limit:     tc.limit,
			}

			err := v.ValidateRules(rules)
			if err == nil {
				t.Errorf("Expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestValidateRules_NoFiltersOrRanking(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Filters:   []Filter{},
		Ranking:   []Ranking{},
		Dimension: "MRQ",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err == nil {
		t.Error("Expected error when no filters or ranking, got nil")
	}
}

func TestValidateRules_InvalidField(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Filters: []Filter{
			{Field: "nonexistent_field", Operator: ">=", Value: float64(100)},
		},
		Dimension: "MRQ",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err == nil {
		t.Error("Expected error for unknown field, got nil")
	}
}

func TestValidateRules_InvalidOperator(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Filters: []Filter{
			{Field: "sector", Operator: ">=", Value: "Technology"}, // >= not valid for text field
		},
		Dimension: "MRQ",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err == nil {
		t.Error("Expected error for invalid operator on text field, got nil")
	}
}

func TestValidateRules_RankingWeightsNot100(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Ranking: []Ranking{
			{Field: "roic", Direction: "desc", Weight: 50},
			{Field: "ev_ebit", Direction: "asc", Weight: 30}, // Only sums to 80
		},
		Dimension: "MRQ",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err == nil {
		t.Error("Expected error when weights don't sum to 100, got nil")
	}
}

func TestValidateRules_InvalidRankingDirection(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Ranking: []Ranking{
			{Field: "roic", Direction: "invalid", Weight: 100},
		},
		Dimension: "MRQ",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err == nil {
		t.Error("Expected error for invalid ranking direction, got nil")
	}
}

func TestValidateRules_NonRankableField(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Ranking: []Ranking{
			{Field: "sector", Direction: "asc", Weight: 100}, // sector is not rankable
		},
		Dimension: "MRQ",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err == nil {
		t.Error("Expected error for non-rankable field in ranking, got nil")
	}
}

func TestValidateRules_BetweenOperator(t *testing.T) {
	v := NewValidator()

	t.Run("valid between", func(t *testing.T) {
		rules := Rules{
			Filters: []Filter{
				{Field: "market_cap", Operator: "between", Value: []any{float64(500000000), float64(10000000000)}},
			},
			Dimension: "MRQ",
			Limit:     10,
		}

		err := v.ValidateRules(rules)
		if err != nil {
			t.Errorf("Expected valid rules, got error: %v", err)
		}
	})

	t.Run("between with wrong value count", func(t *testing.T) {
		rules := Rules{
			Filters: []Filter{
				{Field: "market_cap", Operator: "between", Value: []any{float64(500000000)}},
			},
			Dimension: "MRQ",
			Limit:     10,
		}

		err := v.ValidateRules(rules)
		if err == nil {
			t.Error("Expected error for between with one value, got nil")
		}
	})
}

func TestValidateRules_InOperator(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Filters: []Filter{
			{Field: "sector", Operator: "in", Value: []any{"Technology", "Healthcare"}},
		},
		Dimension: "ARQ",
		Limit:     20,
	}

	err := v.ValidateRules(rules)
	if err != nil {
		t.Errorf("Expected valid rules with 'in' operator, got error: %v", err)
	}
}

func TestValidateRules_IsNullOperator(t *testing.T) {
	v := NewValidator()

	rules := Rules{
		Filters: []Filter{
			{Field: "dividend_yield", Operator: "is_not_null", Value: nil},
		},
		Dimension: "MRQ",
		Limit:     10,
	}

	err := v.ValidateRules(rules)
	if err != nil {
		t.Errorf("Expected valid rules with is_not_null, got error: %v", err)
	}
}

func TestValidateStrategy_EmptyName(t *testing.T) {
	v := NewValidator()

	err := v.ValidateStrategy("", Rules{
		Filters:   []Filter{{Field: "roic", Operator: ">", Value: float64(0)}},
		Dimension: "MRQ",
		Limit:     10,
	})

	if err == nil {
		t.Error("Expected error for empty name, got nil")
	}
}

func TestValidateStrategy_NameTooLong(t *testing.T) {
	v := NewValidator()

	longName := ""
	for i := 0; i < 101; i++ {
		longName += "a"
	}

	err := v.ValidateStrategy(longName, Rules{
		Filters:   []Filter{{Field: "roic", Operator: ">", Value: float64(0)}},
		Dimension: "MRQ",
		Limit:     10,
	})

	if err == nil {
		t.Error("Expected error for name too long, got nil")
	}
}

func TestValidateField(t *testing.T) {
	t.Run("valid field", func(t *testing.T) {
		err := ValidateField("roic")
		if err != nil {
			t.Errorf("Expected no error for valid field, got: %v", err)
		}
	})

	t.Run("invalid field", func(t *testing.T) {
		err := ValidateField("invalid_field")
		if err == nil {
			t.Error("Expected error for invalid field, got nil")
		}
	})
}

func TestValidateRankableField(t *testing.T) {
	t.Run("rankable field", func(t *testing.T) {
		err := ValidateRankableField("roic")
		if err != nil {
			t.Errorf("Expected no error for rankable field, got: %v", err)
		}
	})

	t.Run("non-rankable field", func(t *testing.T) {
		err := ValidateRankableField("sector")
		if err == nil {
			t.Error("Expected error for non-rankable field, got nil")
		}
	})
}
