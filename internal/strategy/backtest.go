package strategy

import "time"

// BacktestConfig defines the parameters for running a backtest
type BacktestConfig struct {
	StrategyID     int       `json:"strategy_id"`
	StartDate      time.Time `json:"start_date"`
	EndDate        time.Time `json:"end_date"`
	RebalanceFreq  string    `json:"rebalance_freq"` // "monthly", "quarterly", "semi-annual", "annual"
	LagDays        int       `json:"lag_days"`       // Days before rebalance to use for fundamentals (default 60)
	InitialCapital float64   `json:"initial_capital"`
}

// BacktestResult contains the complete results of a backtest
type BacktestResult struct {
	Config              BacktestConfig   `json:"config"`
	Periods             []BacktestPeriod `json:"periods"`
	TotalReturn         float64          `json:"total_return"`         // Total percentage return
	AnnualizedReturn    float64          `json:"annualized_return"`    // CAGR
	BenchmarkReturn     float64          `json:"benchmark_return"`     // SPY total return
	BenchmarkAnnualized float64          `json:"benchmark_annualized"` // SPY CAGR
	Alpha               float64          `json:"alpha"`                // Excess return vs benchmark
	MaxDrawdown         float64          `json:"max_drawdown"`         // Maximum peak-to-trough decline
	SharpeRatio         float64          `json:"sharpe_ratio"`         // Risk-adjusted return (if we add volatility calc)
	PortfolioCurve      []CurvePoint     `json:"portfolio_curve"`      // For charting
	BenchmarkCurve      []CurvePoint     `json:"benchmark_curve"`      // For charting
	ExecutionTimeMs     int64            `json:"execution_time_ms"`
}

// BacktestPeriod represents one rebalancing period
type BacktestPeriod struct {
	StartDate     time.Time `json:"start_date"`
	EndDate       time.Time `json:"end_date"`
	Holdings      []Holding `json:"holdings"`
	PeriodReturn  float64   `json:"period_return"`  // Return for this period
	CumulativeVal float64   `json:"cumulative_val"` // Portfolio value at end of period
}

// Holding represents a stock position during a period
type Holding struct {
	Ticker     string  `json:"ticker"`
	Rank       int     `json:"rank"`
	Weight     float64 `json:"weight"`      // Portfolio weight (0-1)
	EntryPrice float64 `json:"entry_price"` // Price at period start
	ExitPrice  float64 `json:"exit_price"`  // Price at period end
	Return     float64 `json:"return"`      // Individual stock return
	IsFallback bool    `json:"is_fallback"` // True if added via fallback (didn't pass filters)
}

// CurvePoint represents a single point on a performance curve
type CurvePoint struct {
	Date  time.Time `json:"date"`
	Value float64   `json:"value"` // Normalized value (starts at 1.0)
}

// PricePoint represents a single price data point
type PricePoint struct {
	Date  time.Time
	Close float64
}

// RankWeights maps portfolio size to weight distribution
// Higher ranked stocks get more weight
var RankWeights = map[int][]float64{
	1:  {1.0},
	2:  {0.5, 0.5},
	3:  {0.4, 0.35, 0.25},
	4:  {0.3, 0.27, 0.23, 0.2},
	5:  {0.27, 0.23, 0.2, 0.17, 0.13},
	6:  {0.25, 0.25, 0.15, 0.15, 0.1, 0.1},
	7:  {0.20, 0.18, 0.15, 0.14, 0.12, 0.11, 0.10},
	8:  {0.18, 0.16, 0.14, 0.13, 0.11, 0.10, 0.09, 0.09},
	9:  {0.16, 0.14, 0.13, 0.12, 0.11, 0.10, 0.09, 0.08, 0.07},
	10: {0.15, 0.13, 0.12, 0.11, 0.10, 0.09, 0.08, 0.08, 0.07, 0.07},
}

// GetWeights returns the weight distribution for a given portfolio size
// Falls back to equal weights if size not in map
func GetWeights(size int) []float64 {
	if weights, ok := RankWeights[size]; ok {
		return weights
	}
	// Equal weights fallback
	weights := make([]float64, size)
	w := 1.0 / float64(size)
	for i := range weights {
		weights[i] = w
	}
	return weights
}

// GetWeightsWithCustom returns custom weights if provided and valid,
// otherwise falls back to default rank weights
func GetWeightsWithCustom(size int, customWeights []float64) []float64 {
	// Use custom weights if provided and match the size
	if len(customWeights) > 0 {
		if len(customWeights) == size {
			return customWeights
		}
		// If custom weights don't match size, scale them
		if len(customWeights) > size {
			// Truncate and renormalize
			weights := customWeights[:size]
			sum := 0.0
			for _, w := range weights {
				sum += w
			}
			if sum > 0 {
				for i := range weights {
					weights[i] = weights[i] / sum
				}
			}
			return weights
		}
		// If fewer custom weights than positions, use equal weights for extras
		weights := make([]float64, size)
		customSum := 0.0
		for i, w := range customWeights {
			weights[i] = w
			customSum += w
		}
		// Distribute remaining weight equally among extra positions
		remaining := 1.0 - customSum
		if remaining > 0 && size > len(customWeights) {
			extraWeight := remaining / float64(size-len(customWeights))
			for i := len(customWeights); i < size; i++ {
				weights[i] = extraWeight
			}
		}
		return weights
	}
	// Fall back to default weights
	return GetWeights(size)
}

// DefaultBacktestConfig returns sensible defaults
func DefaultBacktestConfig() BacktestConfig {
	return BacktestConfig{
		RebalanceFreq:  "semi-annual",
		LagDays:        60,
		InitialCapital: 10000,
	}
}
