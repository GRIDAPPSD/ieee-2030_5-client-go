// Package inverter : concrete LogEventRateLimiter, satisfying
// the seam defined in log_event_emitter.go.
//
// CSIP V1.2 BASIC-027 lists trip-flap mitigation as a deployment concern:
// a marginal voltage signal flapping in and out of the trip region must
// not flood the server with LogEvents. The primary mitigation lives in
// the alarm detector (edge-triggered: emit only on transitions, not while
// the condition holds). This limiter is defense-in-depth : if the
// detector's edge logic ever regresses, the limiter caps emissions to a
// configurable rate per logEventCode so the server is never flooded
// regardless.
//
// Design: per-code last-emit timestamp keyed by logEventCode, single
// mutex (the lock is held for nanoseconds : no contention concern for
// the simulator's ~1Hz tick). The window is configurable so tests can
// pin a short window without sleeping a minute.
//
// Concurrency: Allow is safe for concurrent use. The clock function is
// invoked under the mutex : callers that want a deterministic clock
// (tests) pass a synthetic time source.

package inverter

import (
	"sync"
	"time"
)

// DefaultLogEventWindow is the per-code rate-limit window CSIP V1.2
// BASIC-027 deployments target by default: at most one LogEvent per
// logEventCode per minute. The detector's edge-triggered transition
// logic is the primary anti-spam; this is the secondary cap.
const DefaultLogEventWindow = time.Minute

// PerCodeLogEventLimiter implements LogEventRateLimiter (declared in
// log_event_emitter.go) using a per-code last-emit timestamp.
//
// Allow returns true when (now - lastEmit[code]) >= window, false
// otherwise. On allow the limiter records now as the new lastEmit, so
// the next Allow for the same code within the window denies. Codes are
// tracked independently : a deny on code 1 does not affect code 5.
//
// The zero value is NOT ready; construct via NewPerCodeLogEventLimiter.
//
// Pike rule: clock injection. A non-nil now function is required so
// tests can drive synthetic time without sleeping. NewPerCodeLogEventLimiter
// supplies time.Now when called with nil.
type PerCodeLogEventLimiter struct {
	window   time.Duration
	now      func() time.Time
	mu       sync.Mutex
	lastEmit map[uint8]time.Time
}

// NewPerCodeLogEventLimiter constructs a limiter with the supplied
// per-code window and clock. A zero or negative window is replaced by
// DefaultLogEventWindow (1 minute) so a caller that passes 0 still gets
// safe defense-in-depth behavior. A nil now function defaults to
// time.Now.
//
// main.go wires this with DefaultLogEventWindow + time.Now;
// unit tests pin a short window + synthetic clock to exercise the
// allow/deny edges without sleeping.
func NewPerCodeLogEventLimiter(window time.Duration, now func() time.Time) *PerCodeLogEventLimiter {
	if window <= 0 {
		window = DefaultLogEventWindow
	}
	if now == nil {
		now = time.Now
	}
	return &PerCodeLogEventLimiter{
		window:   window,
		now:      now,
		lastEmit: make(map[uint8]time.Time),
	}
}

// Allow satisfies LogEventRateLimiter (log_event_emitter.go). Returns
// true if the supplied logEventCode has not been emitted within the
// configured window and records the new emit timestamp. Returns false
// otherwise; the caller (PostLogEvent) returns ErrRateLimited and skips
// the HTTP traffic.
//
// First call for any code always returns true (no prior emit recorded).
func (l *PerCodeLogEventLimiter) Allow(code uint8) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	last, seen := l.lastEmit[code]
	if seen && now.Sub(last) < l.window {
		return false
	}
	l.lastEmit[code] = now
	return true
}

// Window returns the configured rate-limit window. Exposed so tests can
// assert the constructor's default-substitution behavior and main.go
// can log the value.
func (l *PerCodeLogEventLimiter) Window() time.Duration { return l.window }
