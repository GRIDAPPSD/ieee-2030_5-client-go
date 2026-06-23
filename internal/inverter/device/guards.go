package device

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// Nameplate carries the DER capability limits used by the range-clamp guard.
// Sourced from inverter.Rating for now; a later ticket wires it from the
// device's read-once DERCapability.
type Nameplate struct {
	RatedW   float64 // maximum active power (W)
	RatedVAr float64 // maximum reactive power capability (VAr)
}

// ErrRateLimitExceeded is returned by ApplySetpoint when the write-rate
// guard rejects a call that exceeds the configured setpoint frequency.
var ErrRateLimitExceeded = errors.New("setpoint write rate limit exceeded")

// ErrMalformedControl is returned when guard 5 rejects a control value that
// is NaN, infinite, or otherwise malformed. The inner backend is never called.
var ErrMalformedControl = errors.New("malformed or out-of-range control value rejected")

// tokenBucket is a minimal write-rate limiter. It allows up to `burst` calls
// per `window` measured in wall time.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   int
	burst    int
	window   time.Duration
	lastFill time.Time
}

func newTokenBucket(burst int, window time.Duration) *tokenBucket {
	return &tokenBucket{
		tokens:   burst,
		burst:    burst,
		window:   window,
		lastFill: time.Now(),
	}
}

// allow returns true and consumes one token if one is available. Tokens
// refill to `burst` after each full `window` has elapsed.
func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	if now.Sub(tb.lastFill) >= tb.window {
		tb.tokens = tb.burst
		tb.lastFill = now
	}
	if tb.tokens <= 0 {
		return false
	}
	tb.tokens--
	return true
}

// guardedDevice wraps a DERDevice with the five safety guards.
// It is created by WithSafetyGuards and is the only entity that should
// ever wrap a RealDevice. Synthetic and GridLABD paths bypass it entirely.
//
// Concurrency: guardedDevice is NOT safe for concurrent callers. The mu
// mutex protects only the lastGood cache; it does NOT protect the inner
// blocking call in ApplySetpoint. The future hardware ticket (IEEESIM-007)
// must enforce single-caller discipline or add a call-level lock if the
// tick loop and a reporting goroutine need concurrent access.
type guardedDevice struct {
	inner     DERDevice
	nameplate Nameplate
	limiter   *tokenBucket
	mu        sync.Mutex
	lastGood  inverter.InverterState
	hasGood   bool // true once at least one successful ApplySetpoint has been recorded
}

// WithSafetyGuards wraps inner with all five safety guards and returns a
// DERDevice. It should be applied ONLY to the real-device backend.
//
//   - Guard 1: range-clamp against nameplate before write.
//   - Guard 2: type/units/scale validation (stub body; call site is real).
//   - Guard 3: write rate limiting via token bucket.
//   - Guard 4: fail-safe on comms loss (hold last-known-good or safe default).
//   - Guard 5: reject, do not coerce, malformed/NaN/out-of-range controls.
func WithSafetyGuards(inner DERDevice, np Nameplate) DERDevice {
	return &guardedDevice{
		inner:     inner,
		nameplate: np,
		limiter:   newTokenBucket(defaultWriteBurst, defaultWriteWindow),
	}
}

// defaultWriteBurst and defaultWriteWindow define the rate-limit policy:
// at most defaultWriteBurst setpoints per defaultWriteWindow wall-clock time.
const (
	defaultWriteBurst = 10
	defaultWriteWindow = time.Second
)

// ReadState passes through to inner unguarded. Reads are always safe.
func (g *guardedDevice) ReadState(ctx context.Context) (StateReading, error) {
	return g.inner.ReadState(ctx)
}

// ApplySetpoint runs all five guards in order before delegating to inner.
func (g *guardedDevice) ApplySetpoint(ctx context.Context, controls inverter.ControlOutputs) (inverter.InverterState, error) {
	// Guard 5 (reject): refuse malformed / out-of-range values outright.
	// NaN or infinite inputs are never clamped; they indicate a caller bug.
	if err := rejectMalformed(controls); err != nil {
		return inverter.InverterState{}, err
	}

	// Guard 2 (stub): type/units/scale validation against the register map.
	// The register map lives in go-sunspec (out of scope for this card);
	// the body returns nil today so the call site and structure land now.
	if err := validateUnitsAndScale(controls); err != nil {
		return inverter.InverterState{}, err
	}

	// Guard 1 (clamp): clamp to nameplate before write.
	controls = clampToNameplate(controls, g.nameplate)

	// Guard 3 (rate limit): reject, do not block.
	if !g.limiter.allow() {
		return inverter.InverterState{}, ErrRateLimitExceeded
	}

	// Delegate to inner.
	state, err := g.inner.ApplySetpoint(ctx, controls)

	// Guard 4 (fail-safe): on error, return last-known-good or safe default.
	if err != nil {
		return g.failSafe(), fmt.Errorf("device write failed (fail-safe active): %w", err)
	}

	// Cache the last successful state.
	g.mu.Lock()
	g.lastGood = state
	g.hasGood = true
	g.mu.Unlock()

	return state, nil
}

// clampToNameplate clamps ActivePowerW to [0, nameplate.RatedW] and
// ReactivePowerVAr to [-nameplate.RatedVAr, +nameplate.RatedVAr]. This is
// guard 1: clamp values at the rail before writing to the device.
func clampToNameplate(c inverter.ControlOutputs, np Nameplate) inverter.ControlOutputs {
	if c.ActivePowerW > np.RatedW {
		c.ActivePowerW = np.RatedW
	}
	if c.ActivePowerW < 0 {
		c.ActivePowerW = 0
	}
	if c.ReactivePowerVAr > np.RatedVAr {
		c.ReactivePowerVAr = np.RatedVAr
	}
	if c.ReactivePowerVAr < -np.RatedVAr {
		c.ReactivePowerVAr = -np.RatedVAr
	}
	return c
}

// rejectMalformed is guard 5: reject NaN or infinite ActivePowerW and
// ReactivePowerVAr outright. The inner backend is never called for a
// malformed control. Note: negative ActivePowerW is NOT rejected here; it
// is clamped to 0 by guard 1 (clampToNameplate), because a negative P
// request is on-the-rail, not malformed.
func rejectMalformed(c inverter.ControlOutputs) error {
	if math.IsNaN(c.ActivePowerW) || math.IsInf(c.ActivePowerW, 0) {
		return fmt.Errorf("%w: ActivePowerW=%v", ErrMalformedControl, c.ActivePowerW)
	}
	if math.IsNaN(c.ReactivePowerVAr) || math.IsInf(c.ReactivePowerVAr, 0) {
		return fmt.Errorf("%w: ReactivePowerVAr=%v", ErrMalformedControl, c.ReactivePowerVAr)
	}
	return nil
}

// validateUnitsAndScale is guard 2 (stub). The register map lives in
// go-sunspec (out of scope for IEEESIM-003). The body returns nil today;
// the call site and structure land now so the validation wires in with
// go-sunspec without touching the guard chain.
//
// IEEESIM-007 (GitLab issue #7) wires the real implementation. The expected
// contract: validate that controls.ActivePowerW and controls.ReactivePowerVAr
// match the scale factor and units defined in the SunSpec register map for
// this device model. Reject (not coerce) on mismatch.
func validateUnitsAndScale(_ inverter.ControlOutputs) error {
	return nil
}

// failSafe returns the last-known-good InverterState if one exists, or a
// safe default (zero power, disconnected). This is guard 4.
func (g *guardedDevice) failSafe() inverter.InverterState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hasGood {
		return g.lastGood
	}
	return inverter.InverterState{
		Connected: false,
		Energized: false,
		Mode:      inverter.ModeDisconnected,
	}
}
