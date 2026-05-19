package proposal

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// strategyExecutorAdapter adapts the real strategy.Executor to the
// StrategyExecutor interface the proposal Generator depends on. The adapter
// unmarshals the rules JSON (which the generator passes through opaquely from
// strategy_versions.rules) into a typed strategy.Rules, calls Execute, and
// returns the recommendations.
//
// We unmarshal into a synthetic *strategy.Strategy with only Rules set —
// strategy.Executor doesn't read the other fields when the rules-driven SQL
// path is exercised.
type strategyExecutorAdapter struct {
	executor *strategy.Executor
}

// NewStrategyExecutorAdapter wraps a real executor as the small surface the
// generator needs.
func NewStrategyExecutorAdapter(e *strategy.Executor) StrategyExecutor {
	return strategyExecutorAdapter{executor: e}
}

func (a strategyExecutorAdapter) RunWithRules(ctx context.Context, rulesJSON []byte) ([]strategy.Recommendation, error) {
	var rules strategy.Rules
	if err := json.Unmarshal(rulesJSON, &rules); err != nil {
		return nil, fmt.Errorf("unmarshaling rules: %w", err)
	}
	s := &strategy.Strategy{Rules: rules}
	res, err := a.executor.Execute(ctx, s)
	if err != nil {
		return nil, err
	}
	return res.Recommendations, nil
}
