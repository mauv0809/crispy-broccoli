package strategy

import (
	"encoding/json"
	"time"
)

// Strategy represents a stock screening strategy stored in the database
type Strategy struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Rules       Rules     `json:"rules"`
	IsDefault   bool      `json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Rules defines the composable rules structure for a strategy
type Rules struct {
	Filters   []Filter  `json:"filters"`
	Ranking   []Ranking `json:"ranking"`
	Dimension string    `json:"dimension"` // MRQ, MRY, ARQ, ARY
	Limit     int       `json:"limit"`
	Weights   []float64 `json:"weights,omitempty"`  // Optional custom portfolio weights (must sum to 1.0)
	Universe  string    `json:"universe,omitempty"` // Optional: "sp500" to restrict to S&P 500 members, empty for all stocks
}

// Filter defines a single filter condition
type Filter struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

// Ranking defines a ranking criterion with weighted score
type Ranking struct {
	Field     string `json:"field"`
	Direction string `json:"direction"` // asc or desc
	Weight    int    `json:"weight"`    // percentage, must sum to 100
}

// Recommendation represents a stock recommendation from strategy execution
type Recommendation struct {
	Rank        int            `json:"rank"`
	Ticker      string         `json:"ticker"`
	CompanyName string         `json:"company_name"`
	Sector      string         `json:"sector"`
	Score       float64        `json:"score"`
	Metrics     map[string]any `json:"metrics"`     // All requested fields
	IsFallback  bool           `json:"is_fallback"` // True if this stock didn't pass all filters but was added to fill quota
}

// StrategyRun represents a single execution of a strategy
type StrategyRun struct {
	ID              int64            `json:"id"`
	StrategyID      int64            `json:"strategy_id"`
	RunAt           time.Time        `json:"run_at"`
	Results         []Recommendation `json:"results"`
	ExecutionTimeMs int              `json:"execution_time_ms"`
	StocksScreened  int              `json:"stocks_screened"`
	StocksMatched   int              `json:"stocks_matched"`
}

// RunResult is the response returned after executing a strategy
type RunResult struct {
	StrategyID      int64            `json:"strategy_id"`
	StrategyName    string           `json:"strategy_name"`
	RunAt           time.Time        `json:"run_at"`
	ExecutionTimeMs int              `json:"execution_time_ms"`
	StocksScreened  int              `json:"stocks_screened"`
	StocksMatched   int              `json:"stocks_matched"`
	Recommendations []Recommendation `json:"recommendations"`
}

// Scan implements sql.Scanner for Rules (JSONB)
func (r *Rules) Scan(src any) error {
	if src == nil {
		*r = Rules{}
		return nil
	}
	switch v := src.(type) {
	case []byte:
		return json.Unmarshal(v, r)
	case string:
		return json.Unmarshal([]byte(v), r)
	}
	return nil
}

// CreateStrategyRequest is the request body for creating a strategy
type CreateStrategyRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Rules       Rules  `json:"rules"`
}

// UpdateStrategyRequest is the request body for updating a strategy
type UpdateStrategyRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Rules       Rules  `json:"rules"`
}
