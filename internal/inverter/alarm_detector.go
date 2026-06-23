// Package inverter — IEEE-054 edge-triggered alarm transition detector.
//
// CSIP V1.2 BASIC-027 (Alarms, pp 139-140) requires the DER Client to POST
// a LogEvent on each alarm-class state TRANSITION. Continuous emission
// while a condition holds would flood the server; emission only on the
// edge is the primary anti-spam mechanism. The rate-limiter
// (PerCodeLogEventLimiter, log_event_ratelimiter.go) is defense-in-depth.
//
// AlarmDetector tracks five alarm classes (mapped to Table 34 codes):
//
//	LogEventCodeVoltageLow      (1) — LVRT trip:    voltsPU < 0.88 and trip curve exceeded
//	LogEventCodeVoltageHigh     (2) — HVRT trip:    voltsPU > 1.10 and trip curve exceeded
//	LogEventCodeFrequencyLow    (3) — under-freq:   freqHz  < 59.0 and trip curve exceeded
//	LogEventCodeFrequencyHigh   (4) — over-freq:    freqHz  > 60.5 and trip curve exceeded
//	LogEventCodeReactiveLimit   (5) — VV curtail:   |Q| above curtailment threshold
//	LogEventCodeActiveLimit     (6) — FW curtail:   freq-droop reduced active power below preDisturbance
//	LogEventCodeGenDisable      (7) — inverter offline: !Connected || !Energized
//
// Wiring: cmd/inverterclient/main.go constructs one AlarmDetector at
// startup, wires the LogEvent emitter (PostLogEvent) + rate-limiter +
// EndDevice.LogEventListLink.Href, and calls Evaluate on every
// simulation tick AFTER ComputeOutput. Evaluate is a synchronous
// pass that fires the emitter callback for each transitioning class.
//
// Concurrency: not safe for concurrent Evaluate. The simulation loop is
// single-goroutine; if a future caller multiplexes Evaluate from
// multiple goroutines, wrap with a mutex at that callsite. Documented
// here so the contract is explicit.

package inverter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// LogEventEmitter is the seam consumed by AlarmDetector for outbound
// LogEvent POSTs. Defined at the consumer (this file) per the Pike rule
// "interfaces at the consumer." (*SEP2Client).PostLogEvent satisfies it.
//
// The detector adapts to the emitter's graceful-bypass contract:
// ErrLogEventLinkAbsent and ErrRateLimited are silent drops (logged at
// debug-ish level only); ErrMethodNotAllowed is logged warn-level and
// the alarm class is suppressed for the remainder of the run (BASIC-027
// graceful-degradation — no retry storm).
type LogEventEmitter interface {
	PostLogEvent(ctx context.Context, logEventListHref string, evt sep2.LogEvent) (string, error)
}

// AlarmInputs is the per-tick read-only view the detector consumes.
// Time is the simulator clock (cfg.TimeScale-scaled); it is the value
// stamped into the alarm-onset record but not into the LogEvent itself
// (the emitter pulls server-synced time from c.Now()).
type AlarmInputs struct {
	Grid             GridState
	Connected        bool
	Energized        bool
	ActivePowerW     float64 // post-curtailment active power
	ReactivePowerVAr float64 // post-VV reactive power
	PreDisturbancePW float64 // active power setpoint before any curtailment
	RatedVAr         float64 // for the |Q|/rated threshold check
	AbnormalDuration time.Duration
	Time             time.Time
}

// AlarmState is the per-class on/off bit the detector remembers
// between ticks. Edge = on→off or off→on; emission happens on the
// off→on transition (alarm "set"). off→off and on→on are no-ops.
//
// Off-edge (on→off, the "clear" event) is intentionally NOT emitted —
// BASIC-027 only requires emit on alarm onset. Adding clear-edge
// emissions is a follow-up if the operator requests it; the state
// machine already tracks the bits so adding the emit site is a
// one-liner.
type AlarmState struct {
	VoltageLow    bool
	VoltageHigh   bool
	FrequencyLow  bool
	FrequencyHigh bool
	ReactiveLimit bool
	ActiveLimit   bool
	GenDisable    bool
}

// AlarmDetector is the edge-triggered alarm tracker. Construct via
// NewAlarmDetector; the zero value is NOT ready (no emitter, no href).
//
// suppressed tracks codes that returned ErrMethodNotAllowed from the
// emitter — once a code is suppressed, future transitions for that
// code are silently dropped (BASIC-027 graceful-degradation: the
// server has signaled it does not implement the LogEvent function
// set, so further POSTs would be wasted traffic).
type AlarmDetector struct {
	emitter          LogEventEmitter
	logEventListHref string
	prev             AlarmState
	suppressed       map[uint8]bool
	// reactiveLimitThreshold is the |Q| / RatedVAr ratio above which
	// LE_REACTIVE_LIMIT fires. Default 0.10 (10% of rated VAr) — small
	// enough that the VV curve drives it past the deadband, large
	// enough to ignore numerical noise inside the deadband. Operator
	// can tune via NewAlarmDetector option in the future.
	reactiveLimitThreshold float64
	// activeLimitMargin is the fractional reduction in active power
	// (preDisturbance - actual) / preDisturbance that triggers
	// LE_ACTIVE_LIMIT. Default 0.05 (5% droop). Prevents false alarms
	// from sub-1% jitter when freqHz hovers near the deadband.
	activeLimitMargin float64
}

// NewAlarmDetector constructs a detector with the supplied emitter and
// LogEventList href. A nil emitter or empty href degrades to a no-op
// (Evaluate becomes free; the simulator runs without complaint, mirror-
// ing the BASIC-027 "OPTIONAL function set" graceful-bypass contract).
//
// Tuning knobs are private — future expansion can convert to functional
// options without breaking the call site in main.go.
func NewAlarmDetector(emitter LogEventEmitter, logEventListHref string) *AlarmDetector {
	return &AlarmDetector{
		emitter:                emitter,
		logEventListHref:       logEventListHref,
		suppressed:             make(map[uint8]bool),
		reactiveLimitThreshold: 0.10,
		activeLimitMargin:      0.05,
	}
}

// enabled returns true when the detector has the emitter + href needed
// to actually POST. Tests construct the detector with nil/empty to
// exercise the no-op fast path.
func (d *AlarmDetector) enabled() bool {
	return d.emitter != nil && d.logEventListHref != ""
}

// Evaluate computes the current alarm classification, diffs against
// the previous state, and emits a LogEvent for each off→on transition.
// Returns the new AlarmState so the caller can log or assert it
// (tests do; main.go ignores the return).
//
// Edge-triggered: an alarm that stays on across consecutive Evaluate
// calls fires exactly once (on the off→on tick). Operator must see a
// resolution-then-recurrence (on→off→on) to see another emission.
// The rate-limiter is defense-in-depth against a detector regression
// that breaks edge semantics.
//
// ctx is propagated to PostLogEvent — a cancelled context aborts the
// in-flight HTTP traffic but does not roll back the state-mutation
// (the alarm flipped on; cancelling the report doesn't unflip it).
func (d *AlarmDetector) Evaluate(ctx context.Context, in AlarmInputs) AlarmState {
	next := d.classify(in)

	// Edge fan-out: walk the seven classes in code order so the log
	// trail is deterministic across runs.
	d.fireIfRising(ctx, d.prev.VoltageLow, next.VoltageLow, LogEventCodeVoltageLow, in)
	d.fireIfRising(ctx, d.prev.VoltageHigh, next.VoltageHigh, LogEventCodeVoltageHigh, in)
	d.fireIfRising(ctx, d.prev.FrequencyLow, next.FrequencyLow, LogEventCodeFrequencyLow, in)
	d.fireIfRising(ctx, d.prev.FrequencyHigh, next.FrequencyHigh, LogEventCodeFrequencyHigh, in)
	d.fireIfRising(ctx, d.prev.ReactiveLimit, next.ReactiveLimit, LogEventCodeReactiveLimit, in)
	d.fireIfRising(ctx, d.prev.ActiveLimit, next.ActiveLimit, LogEventCodeActiveLimit, in)
	d.fireIfRising(ctx, d.prev.GenDisable, next.GenDisable, LogEventCodeGenDisable, in)

	d.prev = next
	return next
}

// classify maps the raw inputs to the seven boolean alarm classes.
// Each class consults the existing pure-function detectors in
// ridethrough.go / voltvar.go / freqdroop.go so the classification
// logic stays one place. The function is exported via the test file
// so unit tests can drive the classification table without
// constructing a full emitter.
func (d *AlarmDetector) classify(in AlarmInputs) AlarmState {
	st := AlarmState{
		GenDisable: !in.Connected || !in.Energized,
	}

	if CheckVoltageTrip(in.Grid.VoltsPU, in.AbnormalDuration) {
		if in.Grid.VoltsPU < 1.0 {
			st.VoltageLow = true
		} else {
			st.VoltageHigh = true
		}
	}

	if CheckFrequencyTrip(in.Grid.FreqHz, in.AbnormalDuration) {
		if in.Grid.FreqHz < 60.0 {
			st.FrequencyLow = true
		} else {
			st.FrequencyHigh = true
		}
	}

	// Reactive curtailment: |Q| above threshold * RatedVAr. The VV
	// curve drives Q outside the deadband only when voltage is
	// outside [V2, V3] (0.98..1.02 p.u. by IEEE 1547 defaults); the
	// threshold check both gates that AND ignores numerical noise.
	if in.RatedVAr > 0 {
		ratio := in.ReactivePowerVAr / in.RatedVAr
		if ratio < 0 {
			ratio = -ratio
		}
		if ratio >= d.reactiveLimitThreshold {
			st.ReactiveLimit = true
		}
	}

	// Active power curtailment: actual P is meaningfully below the
	// pre-disturbance setpoint by more than activeLimitMargin. The
	// deadband on FreqDroop already prevents <1% jitter from
	// reducing P; this margin is a second filter against false
	// alarms from VA-limit clamping in ComputeOutput.
	if in.PreDisturbancePW > 0 {
		drop := (in.PreDisturbancePW - in.ActivePowerW) / in.PreDisturbancePW
		if drop >= d.activeLimitMargin {
			st.ActiveLimit = true
		}
	}

	return st
}

// fireIfRising emits a LogEvent when prev == false && next == true.
// Codes suppressed by a prior 405 are silently skipped.
func (d *AlarmDetector) fireIfRising(
	ctx context.Context,
	prev, next bool,
	code uint8,
	in AlarmInputs,
) {
	if !next || prev {
		return // not a rising edge
	}
	if d.suppressed[code] {
		return // server returned 405 previously — graceful bypass
	}
	if !d.enabled() {
		return // no emitter / no href — no-op
	}

	evt := sep2.LogEvent{
		FunctionSet:  sep2.FunctionSetDER,
		LogEventCode: code,
		ProfileID:    2, // DER profile per CSIP V1.2
		Details:      logEventDetailFor(code, in),
		// CreatedDateTime and LogEventPEN deliberately left zero —
		// PostLogEvent fills both via c.Now() + c.PEN().
	}

	if _, err := d.emitter.PostLogEvent(ctx, d.logEventListHref, evt); err != nil {
		switch {
		case errors.Is(err, ErrLogEventLinkAbsent):
			// Should not happen here (enabled() guards against it),
			// but graceful-bypass in case the emitter changes shape.
			log.Printf("alarm-detector code=%d: LogEventList absent (drop): %v", code, err)
		case errors.Is(err, ErrRateLimited):
			log.Printf("alarm-detector code=%d: rate-limited (drop): %v", code, err)
		case errors.Is(err, ErrMethodNotAllowed):
			log.Printf("alarm-detector code=%d: server replied 405; suppressing further emits for this code", code)
			d.suppressed[code] = true
		default:
			log.Printf("alarm-detector code=%d: emit failed (continue): %v", code, err)
		}
		return
	}
	log.Printf("alarm-detector: emitted code=%d V=%.3fpu F=%.2fHz", code, in.Grid.VoltsPU, in.Grid.FreqHz)
}

// logEventDetailFor builds a short, fixed-form human-readable detail
// string. The XSD limits Details to 32 characters in the 2018 schema,
// 96 in 2023 — both are honored. Operators read this in their server
// UI; keep it terse.
func logEventDetailFor(code uint8, in AlarmInputs) string {
	switch code {
	case LogEventCodeVoltageLow, LogEventCodeVoltageHigh:
		return shortDetailF(in.Grid.VoltsPU, "pu")
	case LogEventCodeFrequencyLow, LogEventCodeFrequencyHigh:
		return shortDetailF(in.Grid.FreqHz, "Hz")
	case LogEventCodeReactiveLimit:
		return shortDetailF(in.ReactivePowerVAr, "VAr")
	case LogEventCodeActiveLimit:
		return shortDetailF(in.ActivePowerW, "W")
	case LogEventCodeGenDisable:
		return "offline"
	default:
		return ""
	}
}

// shortDetailF formats a float with two decimals and a unit suffix,
// e.g. "0.75pu". The 2018 XSD caps Details at 32 chars; "%.2f<unit>"
// is bounded at ~14 worst-case so there's no truncation risk.
func shortDetailF(v float64, unit string) string {
	return fmt.Sprintf("%.2f%s", v, unit)
}
