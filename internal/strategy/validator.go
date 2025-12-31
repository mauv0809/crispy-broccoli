package strategy

import (
	"fmt"
	"strings"
)

// ValidationError holds multiple validation errors
type ValidationError struct {
	Errors []string
}

func (e ValidationError) Error() string {
	return strings.Join(e.Errors, "; ")
}

// IsEmpty returns true if there are no validation errors
func (e ValidationError) IsEmpty() bool {
	return len(e.Errors) == 0
}

// Validator validates strategy rules
type Validator struct{}

// NewValidator creates a new validator
func NewValidator() *Validator {
	return &Validator{}
}

// ValidateRules validates the complete rules structure
func (v *Validator) ValidateRules(rules Rules) error {
	var errs ValidationError

	// Validate dimension (all Sharadar SF1 dimensions)
	validDimensions := map[string]bool{
		"ARQ": true, "ARY": true, // As Reported (Quarterly/Yearly)
		"MRQ": true, "MRY": true, // Most Recent (Quarterly/Yearly)
		"ART": true, "MRT": true, // Trailing Twelve Months
	}
	if !validDimensions[rules.Dimension] {
		errs.Errors = append(errs.Errors, fmt.Sprintf("dimension must be one of ARQ, ARY, MRQ, MRY, ART, MRT; got '%s'", rules.Dimension))
	}

	// Validate limit
	if rules.Limit < 1 || rules.Limit > 100 {
		errs.Errors = append(errs.Errors, fmt.Sprintf("limit must be between 1 and 100, got %d", rules.Limit))
	}

	// At least one filter or ranking required
	if len(rules.Filters) == 0 && len(rules.Ranking) == 0 {
		errs.Errors = append(errs.Errors, "at least one filter or ranking criterion is required")
	}

	// Validate filters
	for i, filter := range rules.Filters {
		if err := v.validateFilter(filter); err != nil {
			errs.Errors = append(errs.Errors, fmt.Sprintf("filter[%d]: %s", i, err))
		}
	}

	// Validate ranking
	if len(rules.Ranking) > 0 {
		if err := v.validateRanking(rules.Ranking); err != nil {
			errs.Errors = append(errs.Errors, err.Error())
		}
	}

	if errs.IsEmpty() {
		return nil
	}
	return errs
}

// validateFilter validates a single filter
func (v *Validator) validateFilter(f Filter) error {
	// Validate field exists
	field, ok := AvailableFields[f.Field]
	if !ok {
		return fmt.Errorf("unknown field '%s'", f.Field)
	}

	// Validate operator is valid for field
	validOperator := false
	for _, op := range field.Operators {
		if op == f.Operator {
			validOperator = true
			break
		}
	}
	if !validOperator {
		return fmt.Errorf("operator '%s' not valid for field '%s'", f.Operator, f.Field)
	}

	// Validate value based on operator
	if err := v.validateFilterValue(f, field); err != nil {
		return err
	}

	return nil
}

// validateFilterValue validates filter value based on operator and field type
func (v *Validator) validateFilterValue(f Filter, field FieldMeta) error {
	switch f.Operator {
	case OpIsNull, OpIsNotNull,
		OpGteMedian, OpGteP25, OpGteP75,
		OpLteMedian, OpLteP25, OpLteP75:
		// Percentile operators compute value dynamically, no value needed
		return nil

	case OpBetween:
		// Value must be array of two numbers
		arr, ok := f.Value.([]any)
		if !ok {
			return fmt.Errorf("'between' operator requires array of two values")
		}
		if len(arr) != 2 {
			return fmt.Errorf("'between' operator requires exactly two values")
		}
		for i, val := range arr {
			if !isNumeric(val) {
				return fmt.Errorf("'between' value[%d] must be numeric", i)
			}
		}
		return nil

	case OpIn, OpNotIn:
		// Value must be array
		arr, ok := f.Value.([]any)
		if !ok {
			// Try string slice
			if _, ok := f.Value.([]string); !ok {
				return fmt.Errorf("'%s' operator requires array value", f.Operator)
			}
		}
		if len(arr) == 0 {
			return fmt.Errorf("'%s' operator requires at least one value", f.Operator)
		}
		return nil

	default:
		// Standard comparison operators
		if f.Value == nil {
			return fmt.Errorf("value is required for operator '%s'", f.Operator)
		}
		if field.Type == "number" && !isNumeric(f.Value) {
			return fmt.Errorf("field '%s' requires numeric value", f.Field)
		}
		return nil
	}
}

// validateRanking validates ranking criteria
func (v *Validator) validateRanking(ranking []Ranking) error {
	if len(ranking) == 0 {
		return nil
	}

	totalWeight := 0
	for i, r := range ranking {
		// Validate field exists and is rankable
		if err := ValidateRankableField(r.Field); err != nil {
			return fmt.Errorf("ranking[%d]: %w", i, err)
		}

		// Validate direction
		if r.Direction != "asc" && r.Direction != "desc" {
			return fmt.Errorf("ranking[%d]: direction must be 'asc' or 'desc'", i)
		}

		// Validate weight
		if r.Weight < 0 || r.Weight > 100 {
			return fmt.Errorf("ranking[%d]: weight must be between 0 and 100", i)
		}

		totalWeight += r.Weight
	}

	// Weights must sum to 100
	if totalWeight != 100 {
		return fmt.Errorf("ranking weights must sum to 100, got %d", totalWeight)
	}

	return nil
}

// ValidateStrategy validates a complete strategy before save
func (v *Validator) ValidateStrategy(name string, rules Rules) error {
	var errs ValidationError

	// Validate name
	name = strings.TrimSpace(name)
	if name == "" {
		errs.Errors = append(errs.Errors, "name is required")
	} else if len(name) > 100 {
		errs.Errors = append(errs.Errors, "name must be 100 characters or less")
	}

	// Validate rules
	if err := v.ValidateRules(rules); err != nil {
		if ve, ok := err.(ValidationError); ok {
			errs.Errors = append(errs.Errors, ve.Errors...)
		} else {
			errs.Errors = append(errs.Errors, err.Error())
		}
	}

	if errs.IsEmpty() {
		return nil
	}
	return errs
}

// isNumeric checks if a value is numeric
func isNumeric(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	case string:
		return false
	default:
		// JSON numbers unmarshal as float64
		_, ok := v.(float64)
		return ok
	}
}
