// Package proposal owns the proposal lifecycle: cadence math, generation
// (diffing strategy picks against current holdings), the proposals storage
// layer, and the transactional acceptor that emits executed trades and
// advances the portfolio's rebalance schedule.
package proposal

import (
	"fmt"
	"time"

	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// AddCadence advances t by one cadence period. Pure; no DB, no clock side
// effects. The portfolio acceptor calls this after acceptance/skip to set
// next_rebalance_due. Timezone is preserved.
func AddCadence(t time.Time, c strategy.Cadence) (time.Time, error) {
	switch c {
	case strategy.CadenceMonthly:
		return t.AddDate(0, 1, 0), nil
	case strategy.CadenceQuarterly:
		return t.AddDate(0, 3, 0), nil
	case strategy.CadenceSemiAnnual:
		return t.AddDate(0, 6, 0), nil
	case strategy.CadenceAnnual:
		return t.AddDate(1, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unknown cadence: %q", c)
	}
}
