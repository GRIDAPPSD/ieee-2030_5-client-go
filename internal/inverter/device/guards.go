package device

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// Nameplate carries the DER capability limits used by guard 1 (range-clamp)
// and guard 2 (scale-bounds). Sourced from inverter.Rating for now; a later
// ticket wires it from the device's read-once DERCapability.
type Nameplate struct {
	RatedW   float64 // maximum active power (W)
	RatedVAr float64 // maximum reactive power capability (VAr)
}

// GuardConfig carries optional configuration for the configurable safety
// guards. The zero value is valid: guard 4 (staleness) is disabled, and
// guard 2 scale bounds are derived from the nameplate.
type GuardConfig struct {
	// MaxStateAge is the staleness bound for guard 4. ApplySetpoint is
	// rejected when the most recent successful ReadState call is older than
	// this value. Zero disables guard 4, which is appropriate for simulator
	// backends that do not pre-call ReadState before every tick.
	MaxStateAge time.Duration
}

// ErrRateLimitExceeded is returned by ApplySetpoint when guard 3 rejects a
// call that exceeds the configured setpoint frequency.
var ErrRateLimitExceeded = errors.New("setpoint write rate limit exceeded")

// ErrMalformedControl is returned when guard 5 rejects a control value that
// is NaN, infinite, or otherwise malformed. The inner backend is never called.
var ErrMalformedControl = errors.New("malformed or out-of-range control value rejected")

// ErrScaleMismatch is returned by guard 2 when ActivePowerW or
// ReactivePowerVAr exceeds the plausible SI-unit scale bound for this device.
// Values this far above the nameplate strongly indicate a mis-applied unit
// conversion upstream (e.g., milli-watts submitted as watts). The inner
// backend is never called.
var ErrScaleMismatch = errors.New("setpoint rejected: scale or units mismatch")

// ErrStaleState is returned by guard 4 when the most recent successful
// ReadState call is older than the configured MaxStateAge, or when no
// ReadState has been performed since construction. The inner backend is never
// called.
var ErrStaleState = errors.New("setpoint rejected: device state is stale")

// defaultScaleMultiplier is the factor by which nameplate values are
// multiplied to produce the guard 2 scale bounds. Values more than
// defaultScaleMultiplier times the nameplate are rejected as likely
// mis-scaled (e.g., a 10 kW inverter with values arriving in milli-watts
// would have RatedW * 100 = 1 MW: anything above that is suspicious).
const defaultScaleMultiplier = 100.0

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

// guardedDevice wraps a DERDevice with five pre-write safety guards and a
// post-write fail-safe recovery. It is created by WithSafetyGuards and is
// the only entity that should ever wrap a RealDevice. Synthetic and GridLABD
// paths bypass it entirely.
//
// Concurrency: guardedDevice is NOT safe for concurrent callers. The mu
// mutex protects only the cached lastGood state and the lastReadTime stamp.
// It does NOT protect the inner blocking call in ApplySetpoint. The real-
// device tick loop must enforce single-caller discipline.
type guardedDevice struct {
	inner        DERDevice
	nameplate    Nameplate
	scaleMaxW    float64       // guard 2: max plausible |ActivePowerW| (W)
	scaleMaxVAr  float64       // guard 2: max plausible |ReactivePowerVAr| (VAr)
	maxStateAge  time.Duration // guard 4: zero disables the staleness check
	limiter      *tokenBucket
	mu           sync.Mutex
	lastGood     inverter.InverterState
	hasGood      bool      // true once at least one successful ApplySetpoint has been recorded
	lastReadTime time.Time // guard 4: time of most recent successful ReadState
}

// WithSafetyGuards wraps inner with all five pre-write safety guards and a
// post-write fail-safe, and returns a DERDevice. It should be applied ONLY
// to the real-device backend.
//
//   - Guard 1: range-clamp against nameplate before write.
//   - Guard 2: scale-bounds validation: reject values whose magnitude exceeds
//     defaultScaleMultiplier times the nameplate; this catches mis-applied
//     unit conversions (e.g., milli-watts submitted as watts). Fails closed
//     when the nameplate has zero or negative RatedW or RatedVAr.
//   - Guard 3: write rate limiting via token bucket; reject, do not block.
//   - Guard 4: staleness bound: reject when the most recent successful
//     ReadState call is older than GuardConfig.MaxStateAge, or when
//     ReadState has never been called. Disabled when MaxStateAge is zero.
//   - Guard 5: reject (do not coerce) NaN or infinite control values.
//   - Fail-safe (post-write): on inner backend error, return the cached
//     last-known-good state or the safe zero-power disconnected default.
func WithSafetyGuards(inner DERDevice, np Nameplate, gc GuardConfig) DERDevice {
	return &guardedDevice{
		inner:       inner,
		nameplate:   np,
		scaleMaxW:   np.RatedW * defaultScaleMultiplier,
		scaleMaxVAr: np.RatedVAr * defaultScaleMultiplier,
		maxStateAge: gc.MaxStateAge,
		limiter:     newTokenBucket(defaultWriteBurst, defaultWriteWindow),
	}
}

// defaultWriteBurst and defaultWriteWindow define the rate-limit policy:
// at most defaultWriteBurst setpoints per defaultWriteWindow wall-clock time.
const (
	defaultWriteBurst  = 10
	defaultWriteWindow = time.Second
)

// ReadState passes through to inner and records the timestamp on success.
// The timestamp is used by guard 4 (staleness bound) in ApplySetpoint.
//
// Residual risk: lastReadTime is device-global, not bound to the specific
// ReadState call that feeds control computation. If any goroutine other than
// the tick loop calls ReadState (for example, a telemetry reporter), it
// advances the staleness clock and can mask stale state on the control write
// path. The single-caller invariant (only the tick loop calls both ReadState
// and ApplySetpoint on this guardedDevice) is structural and must be upheld
// by callers. It is not enforced by this type.
func (g *guardedDevice) ReadState(ctx context.Context) (StateReading, error) {
	reading, err := g.inner.ReadState(ctx)
	if err == nil {
		g.mu.Lock()
		g.lastReadTime = time.Now()
		g.mu.Unlock()
	}
	return reading, err
}

// ApplySetpoint runs all five guards in order before delegating to inner,
// and applies the fail-safe recovery if the inner write fails.
//
// Ordering rationale: cheapest no-mutation rejects run first (guard 5
// NaN/inf, guard 2 scale bounds), then guard 1 clamp (mutates controls),
// then guard 4 staleness (reads shared state, no resource cost), then guard 3
// rate limiter (consumes a budget token). Staleness runs before the rate
// limiter so a stale setpoint is rejected without burning rate-limit budget.
func (g *guardedDevice) ApplySetpoint(ctx context.Context, controls inverter.ControlOutputs) (inverter.InverterState, error) {
	// Guard 5 (reject): refuse malformed / NaN / infinite values outright.
	// NaN or infinite inputs are never clamped; they indicate a caller bug.
	// Package function: reads only the controls argument, no receiver state.
	if err := rejectMalformed(controls); err != nil {
		return inverter.InverterState{}, err
	}

	// Guard 2 (scale): reject values whose magnitude implies a mis-applied
	// unit conversion upstream. Fails closed when scale bounds are unset.
	// Receiver method: reads g.scaleMaxW and g.scaleMaxVAr from receiver state.
	if err := g.validateUnitsAndScale(controls); err != nil {
		return inverter.InverterState{}, err
	}

	// Guard 1 (clamp): clamp to nameplate before write.
	// Package function: reads only controls and nameplate, no shared state.
	controls = clampToNameplate(controls, g.nameplate)

	// Guard 4 (staleness): reject if device state has not been refreshed
	// recently enough to trust the setpoint. Runs before guard 3 (rate limit)
	// so a stale setpoint does not consume a rate-limit token.
	// Receiver method: reads g.lastReadTime and g.maxStateAge from receiver state.
	if err := g.checkStaleness(); err != nil {
		return inverter.InverterState{}, err
	}

	// Guard 3 (rate limit): reject, do not block. Placed after guard 4 so
	// stale-setpoint rejections do not consume budget.
	if !g.limiter.allow() {
		return inverter.InverterState{}, ErrRateLimitExceeded
	}

	// INVARIANT: only the tick loop reaches this line. guardedDevice is not
	// safe for concurrent callers; the tick loop enforces single-caller
	// discipline. See the ReadState comment on the device-global freshness
	// residual risk.
	state, err := g.inner.ApplySetpoint(ctx, controls)

	// Fail-safe recovery: on write error, return the last-known-good state
	// or the safe zero-power disconnected default.
	if err != nil {
		return g.failSafe(), fmt.Errorf("device write failed (fail-safe active): %w", err)
	}

	// Cache the last successful state for the fail-safe path.
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

// validateUnitsAndScale is guard 2: validates that controls.ActivePowerW
// and controls.ReactivePowerVAr are within the plausible SI-unit range for
// this device. Values whose magnitude exceeds scaleMaxW or scaleMaxVAr are
// more than defaultScaleMultiplier times the nameplate and strongly suggest
// a mis-applied unit conversion upstream (e.g., a caller that submitted
// milli-watts as watts). The inner backend is never called on mismatch.
//
// The guard fails closed when the scale bounds are indeterminate (zero or
// negative nameplate): rather than silently passing an unchecked value to a
// real hardware register, the guard refuses the write and requires the
// nameplate to be configured.
//
// The full SunSpec register-map scale-factor validation (from go-sunspec) is
// out of scope here; this implementation uses the nameplate as a
// proxy for the expected SI-unit range.
func (g *guardedDevice) validateUnitsAndScale(c inverter.ControlOutputs) error {
	if g.scaleMaxW <= 0 || g.scaleMaxVAr <= 0 {
		// Nameplate not set: scale bounds are indeterminate. Fail closed
		// rather than pass an unchecked value to the hardware register.
		return fmt.Errorf("%w: scale bounds indeterminate (nameplate not configured)", ErrScaleMismatch)
	}
	if c.ActivePowerW > g.scaleMaxW || c.ActivePowerW < -g.scaleMaxW {
		return fmt.Errorf("%w: ActivePowerW=%.6g exceeds scale bound %.6g W",
			ErrScaleMismatch, c.ActivePowerW, g.scaleMaxW)
	}
	if c.ReactivePowerVAr > g.scaleMaxVAr || c.ReactivePowerVAr < -g.scaleMaxVAr {
		return fmt.Errorf("%w: ReactivePowerVAr=%.6g exceeds scale bound %.6g VAr",
			ErrScaleMismatch, c.ReactivePowerVAr, g.scaleMaxVAr)
	}
	return nil
}

// checkStaleness is guard 4: refuses ApplySetpoint when the most recent
// successful ReadState call is older than g.maxStateAge, or when ReadState
// has not been called at all since construction. This ensures setpoints are
// computed from current device state, not from state that may have drifted.
//
// When maxStateAge is zero, guard 4 is disabled entirely. Zero is the correct
// setting for simulator backends that do not pre-call ReadState before every
// tick, and for unit tests that exercise only the other guards.
//
// The guard fails closed: an unread device is treated as "infinitely stale"
// and the write is refused until ReadState succeeds at least once.
func (g *guardedDevice) checkStaleness() error {
	if g.maxStateAge <= 0 {
		return nil // guard 4 disabled
	}
	g.mu.Lock()
	t := g.lastReadTime
	g.mu.Unlock()
	if t.IsZero() {
		return fmt.Errorf("%w: ReadState has not been called since construction", ErrStaleState)
	}
	if age := time.Since(t); age > g.maxStateAge {
		return fmt.Errorf("%w: last ReadState was %.2fs ago (max %.2fs)",
			ErrStaleState, age.Seconds(), g.maxStateAge.Seconds())
	}
	return nil
}

// failSafe returns the last-known-good InverterState if one exists, or the
// safe zero-power disconnected default. This is the post-write fail-safe
// recovery path, invoked when the inner backend returns an error.
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
