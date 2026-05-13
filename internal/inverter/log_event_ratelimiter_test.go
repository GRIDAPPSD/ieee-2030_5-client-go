// IEEE-054 PerCodeLogEventLimiter unit tests.
//
// Covers the contract documented on PerCodeLogEventLimiter:
//  1. First Allow per code → true (no prior emit).
//  2. Second Allow within window for the same code → false (denied).
//  3. Per-code isolation: deny on code A does not affect code B.
//  4. Time-based reset: advancing the clock past `window` re-allows.
//  5. Constructor default substitution: zero or negative window →
//     DefaultLogEventWindow; nil now func → time.Now (smoke).
//  6. Window() accessor returns the configured (or substituted) value.

package inverter

import (
	"sync"
	"testing"
	"time"
)

// fakeClock returns a controllable time.Time. Tests advance it with
// step(); the limiter sees the new time on the next Allow call.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) step(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func TestPerCodeLogEventLimiter_FirstAllowPerCodeIsTrue(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	rl := NewPerCodeLogEventLimiter(time.Minute, clk.Now)

	if !rl.Allow(LogEventCodeVoltageLow) {
		t.Error("first Allow(code=1) = false, want true")
	}
}

func TestPerCodeLogEventLimiter_SecondAllowWithinWindowDenies(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	rl := NewPerCodeLogEventLimiter(time.Minute, clk.Now)

	if !rl.Allow(LogEventCodeVoltageLow) {
		t.Fatal("first Allow = false")
	}
	// Within window.
	clk.step(30 * time.Second)
	if rl.Allow(LogEventCodeVoltageLow) {
		t.Error("second Allow within window = true, want false")
	}
	// Edge case: exactly at the boundary (now == last + window) — allow.
	clk.step(30 * time.Second) // total 60s since first emit
	if !rl.Allow(LogEventCodeVoltageLow) {
		t.Error("Allow at exact window boundary = false, want true")
	}
}

func TestPerCodeLogEventLimiter_PerCodeIsolation(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	rl := NewPerCodeLogEventLimiter(time.Minute, clk.Now)

	if !rl.Allow(LogEventCodeVoltageLow) {
		t.Fatal("first Allow(1) = false")
	}
	// Deny path on code 1: second emit within window denied.
	clk.step(10 * time.Second)
	if rl.Allow(LogEventCodeVoltageLow) {
		t.Error("Allow(1) within window = true, want false")
	}
	// Code 5 is unaffected — first emit allowed.
	if !rl.Allow(LogEventCodeReactiveLimit) {
		t.Error("Allow(5) after Allow(1) deny = false, want true (per-code isolation)")
	}
	// Code 5 is now in its own window — second emit denied.
	clk.step(10 * time.Second)
	if rl.Allow(LogEventCodeReactiveLimit) {
		t.Error("Allow(5) within its own window = true, want false")
	}
}

func TestPerCodeLogEventLimiter_TimeBasedReset(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	rl := NewPerCodeLogEventLimiter(time.Minute, clk.Now)

	if !rl.Allow(LogEventCodeVoltageLow) {
		t.Fatal("first Allow = false")
	}
	// Step past window — limiter must re-allow.
	clk.step(2 * time.Minute)
	if !rl.Allow(LogEventCodeVoltageLow) {
		t.Error("Allow after window expired = false, want true")
	}
	// Re-emit recorded → immediate retry inside new window denied.
	clk.step(1 * time.Second)
	if rl.Allow(LogEventCodeVoltageLow) {
		t.Error("Allow right after time-reset emit = true, want false")
	}
}

func TestNewPerCodeLogEventLimiter_ZeroOrNegativeWindowDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   time.Duration
	}{
		{"zero", 0},
		{"negative", -1 * time.Second},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rl := NewPerCodeLogEventLimiter(tc.in, time.Now)
			if rl.Window() != DefaultLogEventWindow {
				t.Errorf("Window() = %v, want DefaultLogEventWindow (%v)",
					rl.Window(), DefaultLogEventWindow)
			}
		})
	}
}

func TestNewPerCodeLogEventLimiter_NilNowDefaultsToTimeNow(t *testing.T) {
	t.Parallel()
	rl := NewPerCodeLogEventLimiter(time.Minute, nil)
	// Smoke: first call succeeds. We can't pin time.Now without a clock
	// shim, but the alternative (panicking on nil) would have crashed
	// here.
	if !rl.Allow(LogEventCodeVoltageLow) {
		t.Error("first Allow with nil-now-defaulted limiter = false")
	}
}

func TestPerCodeLogEventLimiter_Window_ReturnsConfigured(t *testing.T) {
	t.Parallel()
	rl := NewPerCodeLogEventLimiter(45*time.Second, time.Now)
	if got, want := rl.Window(), 45*time.Second; got != want {
		t.Errorf("Window() = %v, want %v", got, want)
	}
}

func TestPerCodeLogEventLimiter_ConcurrentAllowIsSafe(t *testing.T) {
	t.Parallel()
	rl := NewPerCodeLogEventLimiter(time.Millisecond, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = rl.Allow(uint8(seed % 7))
			}
		}(i)
	}
	wg.Wait()
}
