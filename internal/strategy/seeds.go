package strategy

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultStrategies returns the built-in strategies to seed
func DefaultStrategies() []struct {
	Name        string
	Description string
	Rules       Rules
} {
	return []struct {
		Name        string
		Description string
		Rules       Rules
	}{
		{
			// Matches the Python backtest strategy exactly:
			// - ROA >= 12% (Python uses netinc/assets which is ROA, not ROE)
			// - GP/A >= median (high operating quality)
			// - EV/EBIT between 0 and 25
			// - Ranking: 60% EV/EBIT (asc) + 40% ROA (desc)
			// - Universe: S&P 500 members only (point-in-time)
			// - Weights: [25%, 25%, 15%, 15%, 10%, 10%]
			Name:        "Quality Value (Python Backtest)",
			Description: "Matches Python backtest: ROA>=12%, GP/A>=median, EV/EBIT 0-25. S&P 500 universe. Ranks 60% valuation + 40% quality.",
			Rules: Rules{
				Filters: []Filter{
					// Quality filter: high return on assets (netinc / assets)
					{Field: "roa", Operator: ">=", Value: 0.12},
					// Quality filter: gross profit / assets above median
					{Field: "gp_a", Operator: ">=median", Value: nil},
					// Valuation filter: positive and reasonable EV/EBIT
					{Field: "ev_ebit", Operator: ">", Value: 0.0},
					{Field: "ev_ebit", Operator: "<=", Value: 25.0},
				},
				Ranking: []Ranking{
					// 60% weight on valuation (lower EV/EBIT = cheaper = better)
					{Field: "ev_ebit", Direction: "asc", Weight: 60},
					// 40% weight on quality (higher ROA = better)
					{Field: "roa", Direction: "desc", Weight: 40},
				},
				Dimension: "ARY",   // Annual Reported Yearly (matches Python's ART)
				Limit:     6,
				Universe:  "sp500", // Restrict to S&P 500 members
				Weights:   []float64{0.25, 0.25, 0.15, 0.15, 0.10, 0.10},
			},
		},
	}
}

// SeedDefaultStrategies inserts default strategies if they don't exist
func SeedDefaultStrategies(ctx context.Context, pool *pgxpool.Pool) error {
	repo := NewRepository(pool)

	// Check if we already have default strategies
	existing, err := repo.GetDefaultStrategies(ctx)
	if err != nil {
		return err
	}

	existingNames := make(map[string]bool)
	for _, s := range existing {
		existingNames[s.Name] = true
	}

	defaults := DefaultStrategies()
	seeded := 0

	for _, def := range defaults {
		if existingNames[def.Name] {
			continue
		}

		_, err := repo.CreateDefaultStrategy(ctx, def.Name, def.Description, def.Rules)
		if err != nil {
			log.Printf("Warning: failed to seed strategy %q: %v", def.Name, err)
			continue
		}
		seeded++
		log.Printf("Seeded default strategy: %s", def.Name)
	}

	if seeded > 0 {
		log.Printf("Seeded %d default strategies", seeded)
	}

	return nil
}
