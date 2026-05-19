// Package scheduler runs the in-process rebalance loop: at each tick it finds
// portfolios whose next_rebalance_due has passed, generates a proposal,
// dispatches the notification email, and handles 3-day reminders + initial-
// notification retry within a configurable window. RunOnce is the testable
// seam; Start launches a goroutine that calls RunOnce on a time.Ticker until
// the context is cancelled or Stop is called.
package scheduler

import (
	"sync"
	"time"
)

// Clock is the tick worker's view of "now". Production uses RealClock;
// tests inject FakeClock so they can advance time deterministically.
type Clock interface {
	Now() time.Time
}

// RealClock returns time.Now(). Goroutine-safe (no shared state).
type RealClock struct{}

// NewRealClock returns the production clock. Cheap; no setup required.
func NewRealClock() RealClock { return RealClock{} }

// Now is monotonic on most platforms. Callers should not assume strict
// monotonicity across leap seconds, NTP adjustments, or VM pauses.
func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a deterministic clock for tests. Advance shifts Now by a
// fixed offset; reads are goroutine-safe so the worker can call Now() from
// its tick goroutine while the test advances from the main goroutine.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock seeds the clock at t. Pass any UTC time the test wants to
// anchor against — typically a fixed date so assertions are stable.
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d. Negative durations move it backward,
// which is sometimes useful when seeding "this proposal was sent N hours ago"
// scenarios.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
