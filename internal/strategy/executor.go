package strategy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Executor builds and executes SQL queries from strategy rules
type Executor struct {
	pool *pgxpool.Pool
}

// NewExecutor creates a new strategy executor
func NewExecutor(pool *pgxpool.Pool) *Executor {
	return &Executor{pool: pool}
}

// Execute runs a strategy and returns recommendations (uses latest data)
func (e *Executor) Execute(ctx context.Context, strategy *Strategy) (*RunResult, error) {
	return e.ExecuteAsOf(ctx, strategy, time.Time{}, 0)
}

// ExecuteAsOf runs a strategy using point-in-time data.
// If asOfDate is zero, uses latest data. Otherwise uses data available as of (asOfDate - lagDays).
func (e *Executor) ExecuteAsOf(ctx context.Context, strategy *Strategy, asOfDate time.Time, lagDays int) (*RunResult, error) {
	startTime := time.Now()

	// Calculate cutoff date for fundamentals
	var cutoffDate *time.Time
	if !asOfDate.IsZero() {
		cutoff := asOfDate.AddDate(0, 0, -lagDays)
		cutoffDate = &cutoff
	}

	// For universe filtering, we need the as-of date (or current date for live runs)
	universeDate := asOfDate
	if universeDate.IsZero() {
		universeDate = time.Now()
	}

	// Build the query
	query, args, err := e.buildQuery(strategy.Rules, cutoffDate, universeDate)
	if err != nil {
		return nil, fmt.Errorf("building query: %w", err)
	}

	// Debug: log the query
	fmt.Printf("=== Strategy Query ===\n%s\n=== Args: %v ===\n", query, args)

	// Execute query
	rows, err := e.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("executing query: %w", err)
	}
	defer rows.Close()

	// Get column descriptions for dynamic field mapping
	fieldDescs := rows.FieldDescriptions()
	colNames := make([]string, len(fieldDescs))
	for i, fd := range fieldDescs {
		colNames[i] = fd.Name
	}

	// Collect results
	var recommendations []Recommendation
	rank := 0
	for rows.Next() {
		rank++
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("reading row values: %w", err)
		}

		rec := e.rowToRecommendation(rank, colNames, values, strategy.Rules)
		recommendations = append(recommendations, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	// Fallback logic: if fewer than limit stocks passed filters, fill from best-ranked
	// This matches the Python backtest behavior
	if len(recommendations) < strategy.Rules.Limit && len(strategy.Rules.Filters) > 0 {
		needed := strategy.Rules.Limit - len(recommendations)

		// Get tickers already selected
		excludeTickers := make([]string, len(recommendations))
		for i, rec := range recommendations {
			excludeTickers[i] = rec.Ticker
		}

		// Run fallback query (ranking only, no filters, excluding already selected)
		fallbackRecs, err := e.getFallbackRecommendations(ctx, strategy, cutoffDate, universeDate, excludeTickers, needed)
		if err != nil {
			// Log but don't fail - just return what we have
			fmt.Printf("Fallback query failed: %v\n", err)
		} else if len(fallbackRecs) > 0 {
			// Append fallback recommendations with adjusted ranks
			startRank := len(recommendations) + 1
			for i, rec := range fallbackRecs {
				rec.Rank = startRank + i
				rec.IsFallback = true // Mark as fallback recommendation
				recommendations = append(recommendations, rec)
			}
		}
	}

	// Get total screened count
	stocksScreened, err := e.countStocksScreenedAsOf(ctx, strategy.Rules.Dimension, cutoffDate)
	if err != nil {
		stocksScreened = 0 // Non-fatal, continue
	}

	executionTimeMs := int(time.Since(startTime).Milliseconds())

	runAt := startTime
	if !asOfDate.IsZero() {
		runAt = asOfDate // For backtests, use the as-of date
	}

	return &RunResult{
		StrategyID:      strategy.ID,
		StrategyName:    strategy.Name,
		RunAt:           runAt,
		ExecutionTimeMs: executionTimeMs,
		StocksScreened:  stocksScreened,
		StocksMatched:   len(recommendations),
		Recommendations: recommendations,
	}, nil
}

// buildQuery constructs a parameterized SQL query from rules.
// If cutoffDate is non-nil, restricts to data available as of that date (for backtesting).
// universeDate is used for point-in-time S&P 500 membership filtering.
func (e *Executor) buildQuery(rules Rules, cutoffDate *time.Time, universeDate time.Time) (string, []any, error) {
	args := []any{}
	argIndex := 1

	// Determine which tables we need
	needsDailyPrices := e.needsTable(rules, "daily_prices")
	needsSP500 := rules.Universe == "sp500"

	// Build SELECT clause - include all fields used in ranking plus standard fields
	selectFields := []string{
		"fm.ticker",
		"c.name AS company_name",
		"c.sector",
	}

	// Add fields from ranking for metrics
	for _, r := range rules.Ranking {
		field := AvailableFields[r.Field]
		selectFields = append(selectFields, e.fieldToSQL(field)+" AS "+r.Field)
	}

	// Build base CTE
	var sb strings.Builder
	sb.WriteString("WITH base AS (\n")
	sb.WriteString("    SELECT\n        ")
	sb.WriteString(strings.Join(selectFields, ",\n        "))
	sb.WriteString("\n    FROM financial_metrics fm\n")
	sb.WriteString("    JOIN companies c ON fm.ticker = c.ticker\n")

	// Join with S&P 500 membership if universe restricted
	if needsSP500 {
		sb.WriteString(fmt.Sprintf(`    JOIN (
        SELECT DISTINCT ticker FROM sp500_membership
        WHERE action = 'added' AND date <= $%d
        AND ticker NOT IN (
            SELECT ticker FROM sp500_membership
            WHERE action = 'removed' AND date <= $%d
        )
    ) sp500 ON fm.ticker = sp500.ticker
`, argIndex, argIndex+1))
		args = append(args, universeDate, universeDate)
		argIndex += 2
	}

	if needsDailyPrices {
		sb.WriteString("    LEFT JOIN LATERAL (\n")
		sb.WriteString("        SELECT * FROM daily_prices dp\n")
		sb.WriteString("        WHERE dp.ticker = fm.ticker\n")
		if cutoffDate != nil {
			sb.WriteString(fmt.Sprintf("          AND dp.date <= $%d\n", argIndex))
			args = append(args, *cutoffDate)
			argIndex++
		}
		sb.WriteString("        ORDER BY dp.date DESC LIMIT 1\n")
		sb.WriteString("    ) dp ON true\n")
	}

	// Add WHERE clause for dimension and latest data
	sb.WriteString(fmt.Sprintf("    WHERE fm.dimension = $%d\n", argIndex))
	args = append(args, rules.Dimension)
	argIndex++

	// Get latest financial data for each ticker (with optional cutoff for backtesting)
	sb.WriteString("      AND fm.date_key = (\n")
	sb.WriteString("          SELECT MAX(date_key) FROM financial_metrics fm2\n")
	sb.WriteString(fmt.Sprintf("          WHERE fm2.ticker = fm.ticker AND fm2.dimension = $%d\n", argIndex))
	args = append(args, rules.Dimension)
	argIndex++
	if cutoffDate != nil {
		sb.WriteString(fmt.Sprintf("            AND fm2.date_key <= $%d\n", argIndex))
		args = append(args, *cutoffDate)
		argIndex++
	}
	sb.WriteString("      )\n")

	// Only active companies
	sb.WriteString("      AND c.active = true\n")

	// Add filters
	for _, f := range rules.Filters {
		whereClause, filterArgs, newIndex := e.filterToSQL(f, argIndex, rules.Dimension)
		if whereClause != "" {
			sb.WriteString("      AND " + whereClause + "\n")
			args = append(args, filterArgs...)
			argIndex = newIndex
		}
	}

	sb.WriteString(")")

	// Build ranking CTE if we have ranking criteria
	if len(rules.Ranking) > 0 {
		sb.WriteString(",\nranked AS (\n")
		sb.WriteString("    SELECT base.*")

		// Add percentile rank for each ranking field
		for i, r := range rules.Ranking {
			// Higher is better for desc, lower is better for asc
			orderDir := "DESC"
			if r.Direction == "asc" {
				orderDir = "ASC"
			}
			sb.WriteString(fmt.Sprintf(",\n        PERCENT_RANK() OVER (ORDER BY %s %s NULLS LAST) * %d AS rank_score_%d",
				r.Field, orderDir, r.Weight, i))
		}

		sb.WriteString("\n    FROM base\n")

		// Filter out NULLs for ranking fields
		sb.WriteString("    WHERE ")
		nullChecks := make([]string, len(rules.Ranking))
		for i, r := range rules.Ranking {
			nullChecks[i] = r.Field + " IS NOT NULL"
		}
		sb.WriteString(strings.Join(nullChecks, " AND "))
		sb.WriteString("\n)")

		// Final SELECT with combined score
		sb.WriteString("\nSELECT ranked.*,\n    (")
		scoreTerms := make([]string, len(rules.Ranking))
		for i := range rules.Ranking {
			scoreTerms[i] = fmt.Sprintf("rank_score_%d", i)
		}
		sb.WriteString(strings.Join(scoreTerms, " + "))
		sb.WriteString(") AS combined_score\n")
		sb.WriteString("FROM ranked\n")
		sb.WriteString("ORDER BY combined_score ASC\n") // Lower score = better rank
	} else {
		// No ranking - just return filtered results
		sb.WriteString("\nSELECT *, 0 AS combined_score FROM base\n")
	}

	sb.WriteString(fmt.Sprintf("LIMIT $%d", argIndex))
	args = append(args, rules.Limit)

	return sb.String(), args, nil
}

// filterToSQL converts a filter to SQL WHERE clause
// dimension is used to scope percentile calculations to the same data dimension
func (e *Executor) filterToSQL(f Filter, argIndex int, dimension string) (string, []any, int) {
	field := AvailableFields[f.Field]
	fieldSQL := e.fieldToSQL(field)
	args := []any{}

	switch f.Operator {
	case OpIsNull:
		return fmt.Sprintf("%s IS NULL", fieldSQL), args, argIndex
	case OpIsNotNull:
		return fmt.Sprintf("%s IS NOT NULL", fieldSQL), args, argIndex

	case OpBetween:
		arr := f.Value.([]any)
		args = append(args, arr[0], arr[1])
		return fmt.Sprintf("%s BETWEEN $%d AND $%d", fieldSQL, argIndex, argIndex+1), args, argIndex + 2

	case OpIn:
		arr := toAnySlice(f.Value)
		placeholders := make([]string, len(arr))
		for i, v := range arr {
			placeholders[i] = fmt.Sprintf("$%d", argIndex+i)
			args = append(args, v)
		}
		return fmt.Sprintf("%s IN (%s)", fieldSQL, strings.Join(placeholders, ", ")), args, argIndex + len(arr)

	case OpNotIn:
		arr := toAnySlice(f.Value)
		placeholders := make([]string, len(arr))
		for i, v := range arr {
			placeholders[i] = fmt.Sprintf("$%d", argIndex+i)
			args = append(args, v)
		}
		return fmt.Sprintf("%s NOT IN (%s)", fieldSQL, strings.Join(placeholders, ", ")), args, argIndex + len(arr)

	case OpGreaterEqual:
		args = append(args, f.Value)
		return fmt.Sprintf("%s >= $%d", fieldSQL, argIndex), args, argIndex + 1

	case OpLessEqual:
		args = append(args, f.Value)
		return fmt.Sprintf("%s <= $%d", fieldSQL, argIndex), args, argIndex + 1

	case OpGreater:
		args = append(args, f.Value)
		return fmt.Sprintf("%s > $%d", fieldSQL, argIndex), args, argIndex + 1

	case OpLess:
		args = append(args, f.Value)
		return fmt.Sprintf("%s < $%d", fieldSQL, argIndex), args, argIndex + 1

	case OpEqual:
		args = append(args, f.Value)
		return fmt.Sprintf("%s = $%d", fieldSQL, argIndex), args, argIndex + 1

	case OpNotEqual:
		args = append(args, f.Value)
		return fmt.Sprintf("%s != $%d", fieldSQL, argIndex), args, argIndex + 1

	case OpGteMedian:
		// >= median uses a subquery to compute the 50th percentile within the same dimension
		args = append(args, dimension)
		return fmt.Sprintf("%s >= (SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY %s) FROM financial_metrics WHERE %s IS NOT NULL AND dimension = $%d)", fieldSQL, field.Column, field.Column, argIndex), args, argIndex + 1

	case OpGteP25:
		args = append(args, dimension)
		return fmt.Sprintf("%s >= (SELECT PERCENTILE_CONT(0.25) WITHIN GROUP (ORDER BY %s) FROM financial_metrics WHERE %s IS NOT NULL AND dimension = $%d)", fieldSQL, field.Column, field.Column, argIndex), args, argIndex + 1

	case OpGteP75:
		args = append(args, dimension)
		return fmt.Sprintf("%s >= (SELECT PERCENTILE_CONT(0.75) WITHIN GROUP (ORDER BY %s) FROM financial_metrics WHERE %s IS NOT NULL AND dimension = $%d)", fieldSQL, field.Column, field.Column, argIndex), args, argIndex + 1

	case OpLteMedian:
		args = append(args, dimension)
		return fmt.Sprintf("%s <= (SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY %s) FROM financial_metrics WHERE %s IS NOT NULL AND dimension = $%d)", fieldSQL, field.Column, field.Column, argIndex), args, argIndex + 1

	case OpLteP25:
		args = append(args, dimension)
		return fmt.Sprintf("%s <= (SELECT PERCENTILE_CONT(0.25) WITHIN GROUP (ORDER BY %s) FROM financial_metrics WHERE %s IS NOT NULL AND dimension = $%d)", fieldSQL, field.Column, field.Column, argIndex), args, argIndex + 1

	case OpLteP75:
		args = append(args, dimension)
		return fmt.Sprintf("%s <= (SELECT PERCENTILE_CONT(0.75) WITHIN GROUP (ORDER BY %s) FROM financial_metrics WHERE %s IS NOT NULL AND dimension = $%d)", fieldSQL, field.Column, field.Column, argIndex), args, argIndex + 1
	}

	return "", nil, argIndex
}

// fieldToSQL returns the SQL expression for a field
func (e *Executor) fieldToSQL(field FieldMeta) string {
	switch field.Table {
	case "financial_metrics":
		return "fm." + field.Column
	case "companies":
		return "c." + field.Column
	case "daily_prices":
		return "dp." + field.Column
	default:
		return field.Column
	}
}

// needsTable checks if any filter or ranking uses a specific table
func (e *Executor) needsTable(rules Rules, table string) bool {
	for _, f := range rules.Filters {
		if field, ok := AvailableFields[f.Field]; ok && field.Table == table {
			return true
		}
	}
	for _, r := range rules.Ranking {
		if field, ok := AvailableFields[r.Field]; ok && field.Table == table {
			return true
		}
	}
	return false
}

// rowToRecommendation converts a database row to a Recommendation
func (e *Executor) rowToRecommendation(rank int, colNames []string, values []any, rules Rules) Recommendation {
	rec := Recommendation{
		Rank:    rank,
		Metrics: make(map[string]any),
	}

	for i, colName := range colNames {
		val := values[i]
		switch colName {
		case "ticker":
			if v, ok := val.(string); ok {
				rec.Ticker = v
			}
		case "company_name":
			if v, ok := val.(string); ok {
				rec.CompanyName = v
			}
		case "sector":
			if v, ok := val.(string); ok {
				rec.Sector = v
			}
		case "combined_score":
			if v, ok := toFloat64(val); ok {
				// Convert score to 0-100 range (inverted since lower was better)
				rec.Score = (1 - v/100) * 100
			}
		default:
			// Check if it's a ranking field metric (not a rank_score_N)
			if !strings.HasPrefix(colName, "rank_score_") {
				rec.Metrics[colName] = val
			}
		}
	}

	return rec
}

// countStocksScreenedAsOf returns total stocks with data for dimension, optionally filtered by cutoff date
func (e *Executor) countStocksScreenedAsOf(ctx context.Context, dimension string, cutoffDate *time.Time) (int, error) {
	var count int
	var err error

	if cutoffDate != nil {
		err = e.pool.QueryRow(ctx, `
			SELECT COUNT(DISTINCT fm.ticker)
			FROM financial_metrics fm
			JOIN companies c ON fm.ticker = c.ticker
			WHERE fm.dimension = $1 AND c.active = true AND fm.date_key <= $2
		`, dimension, *cutoffDate).Scan(&count)
	} else {
		err = e.pool.QueryRow(ctx, `
			SELECT COUNT(DISTINCT fm.ticker)
			FROM financial_metrics fm
			JOIN companies c ON fm.ticker = c.ticker
			WHERE fm.dimension = $1 AND c.active = true
		`, dimension).Scan(&count)
	}
	return count, err
}

// toAnySlice converts interface{} to []any
func toAnySlice(v any) []any {
	switch arr := v.(type) {
	case []any:
		return arr
	case []string:
		result := make([]any, len(arr))
		for i, s := range arr {
			result[i] = s
		}
		return result
	case []int:
		result := make([]any, len(arr))
		for i, n := range arr {
			result[i] = n
		}
		return result
	case []float64:
		result := make([]any, len(arr))
		for i, n := range arr {
			result[i] = n
		}
		return result
	default:
		return nil
	}
}

// toFloat64 converts various numeric types to float64
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}

// getFallbackRecommendations fetches additional stocks using ranking only (no filters)
// to fill slots when not enough stocks pass the main filters.
// This matches the Python backtest behavior of filling from best-ranked non-survivors.
func (e *Executor) getFallbackRecommendations(ctx context.Context, strategy *Strategy, cutoffDate *time.Time, universeDate time.Time, excludeTickers []string, limit int) ([]Recommendation, error) {
	if len(strategy.Rules.Ranking) == 0 || limit <= 0 {
		return nil, nil
	}

	// Build a simplified query with ranking only, excluding already selected tickers
	fallbackRules := Rules{
		Filters:   []Filter{}, // No filters for fallback
		Ranking:   strategy.Rules.Ranking,
		Dimension: strategy.Rules.Dimension,
		Limit:     limit,
		Universe:  strategy.Rules.Universe,
	}

	query, args, err := e.buildQuery(fallbackRules, cutoffDate, universeDate)
	if err != nil {
		return nil, fmt.Errorf("building fallback query: %w", err)
	}

	// Add exclusion of already selected tickers
	if len(excludeTickers) > 0 {
		// We need to inject the exclusion into the query
		// Find WHERE clause and add NOT IN condition
		excludePlaceholders := make([]string, len(excludeTickers))
		for i := range excludeTickers {
			excludePlaceholders[i] = fmt.Sprintf("$%d", len(args)+i+1)
		}
		excludeClause := fmt.Sprintf(" AND fm.ticker NOT IN (%s)", strings.Join(excludePlaceholders, ", "))

		// Insert before the closing paren of the base CTE
		query = strings.Replace(query, "\n)", excludeClause+"\n)", 1)

		for _, t := range excludeTickers {
			args = append(args, t)
		}
	}

	rows, err := e.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("executing fallback query: %w", err)
	}
	defer rows.Close()

	fieldDescs := rows.FieldDescriptions()
	colNames := make([]string, len(fieldDescs))
	for i, fd := range fieldDescs {
		colNames[i] = fd.Name
	}

	var recommendations []Recommendation
	rank := 0
	for rows.Next() {
		rank++
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("reading fallback row: %w", err)
		}

		rec := e.rowToRecommendation(rank, colNames, values, strategy.Rules)
		recommendations = append(recommendations, rec)
	}

	return recommendations, rows.Err()
}
