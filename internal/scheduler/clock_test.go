package scheduler_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mauv0809/crispy-broccoli/internal/scheduler"
)

func TestRealClock_NowAdvancesMonotonically(t *testing.T) {
	c := scheduler.NewRealClock()
	a := c.Now()
	time.Sleep(time.Millisecond) // small but real
	b := c.Now()
	if !b.After(a) {
		t.Errorf("RealClock.Now should advance: a=%s b=%s", a, b)
	}
}

func TestFakeClock_AdvanceMovesNow(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := scheduler.NewFakeClock(start)
	if !c.Now().Equal(start) {
		t.Errorf("Now = %s, want %s", c.Now(), start)
	}
	c.Advance(2 * time.Hour)
	if !c.Now().Equal(start.Add(2 * time.Hour)) {
		t.Errorf("after Advance: Now = %s, want %s", c.Now(), start.Add(2*time.Hour))
	}
}

func TestFakeClock_AdvanceConcurrent(t *testing.T) {
	// Worker tick reads Now() while a test goroutine advances. Verify FakeClock
	// is goroutine-safe (the implementation uses a mutex).
	c := scheduler.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = c.Now() }()
		go func() { defer wg.Done(); c.Advance(time.Second) }()
	}
	wg.Wait()
	want := time.Date(2026, 1, 1, 0, 1, 40, 0, time.UTC) // +100s
	if !c.Now().Equal(want) {
		t.Errorf("after 100×1s advances: Now = %s, want %s", c.Now(), want)
	}
}
