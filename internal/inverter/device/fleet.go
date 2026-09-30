package device

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// FleetSetItem is one property write a FleetDevice asks its transport to
// make.
type FleetSetItem struct {
	Object   string
	Property string
	Value    float64
}

// FleetGetItem names one property to read.
type FleetGetItem struct {
	Object   string
	Property string
}

// FleetValue is one property value read from the fleet model: a real
// number, or a phasor (gridlabd represents a phase voltage or an apparent
// power this way). Which accessor is correct depends on the property, so
// that choice is left to the caller rather than made once in the
// transport.
type FleetValue struct {
	Re, Im float64
}

// Float64 requires a real quantity: a nonzero imaginary part is an error,
// never silently dropped. Use for a genuinely scalar property (P_Out,
// Q_Out, rated_power, a frequency reading).
func (v FleetValue) Float64() (float64, error) {
	if v.Im != 0 {
		return 0, fmt.Errorf("value %g+%gi is complex, not real", v.Re, v.Im)
	}
	return v.Re, nil
}

// Magnitude is sqrt(Re^2+Im^2): the correct reading for a phasor quantity
// such as a phase voltage, where gridlabd's representation may carry a
// nonzero angle and only the magnitude is wanted.
func (v FleetValue) Magnitude() float64 {
	return math.Hypot(v.Re, v.Im)
}

// FleetTransport is the seam a fleet device backend drives: write device
// properties, advance the shared model to an explicit time, and read
// values back. Defined here, at the consumer, so a later physics source
// can replace the GridLAB-D sidecar (internal/sim/gridlabd) without this
// package depending on it.
type FleetTransport interface {
	// Healthy reports whether the fleet's transport is currently usable. A
	// DOWN fleet (dead sidecar, exhausted restart budget) must make every
	// device on it return an error rather than a reading: no value is
	// fabricated on its behalf.
	Healthy() bool
	// Generation is a count that increases every time this fleet's sidecar
	// process is (re)started, starting at 1 for the first successful
	// start. A device compares it against the generation it last talked
	// to, to notice that a restart reset the model underneath it.
	Generation() uint64
	// Set writes every item and returns the value the model actually
	// applied for each, in the same order.
	Set(ctx context.Context, items []FleetSetItem) ([]float64, error)
	// StepTo advances the shared model to target and returns the time it
	// reached. A target not after the model's current time is a no-op, so
	// more than one device calling StepTo for the same tick is safe.
	StepTo(ctx context.Context, target time.Time) (time.Time, error)
	// Get reads every item and returns each value, in the same order.
	Get(ctx context.Context, items []FleetGetItem) ([]FleetValue, error)
}

// FleetDevice is the DERDevice backend for one managed DER inside a fleet:
// it drives its own inverter object over a transport shared with every
// other device of the same fleet, and reads grid conditions from the
// fleet's shared coupling-point object. Construct one only for a device
// this process actually manages.
//
// Known scope limits:
//   - ApplySetpoint calls StepTo on every device's own call, not once per
//     fleet tick. That is safe (StepTo is a no-op once the model is
//     already at the target), but it means N managed devices step the
//     model N times per tick instead of once. A coordinated fleet loop
//     that steps once per tick belongs to a later aggregator dispatch
//     loop, not this backend.
//   - InverterState's Connected, Energized and Mode are commanded values,
//     not simulator reads: the battery fleet GLM exposes no per-device
//     "grid_connected" property (probed 2026-09-30: status 3, not found),
//     and its four_quadrant_control_mode vocabulary (CONSTANT_PQ) has no
//     correspondence to the IEEE 1547 ControlMode enum GridState reports.
//     generator_status ("ONLINE"/"OFFLINE") could source Energized, but
//     this wire protocol carries no string-valued property today, and
//     inventing a mapping for Connected and Mode with no model source
//     would be exactly the fabricated default data-invariants forbids.
//     This is an open design question, not fixed here.
type FleetDevice struct {
	transport   FleetTransport
	lfdi        string
	inverterObj string
	meterObj    string
	now         func() time.Time

	mu             sync.Mutex
	lastControls   *inverter.ControlOutputs
	lastGeneration uint64
}

// NewFleetDevice constructs a FleetDevice for the managed device identified
// by lfdi, whose inverter object in the fleet's model is inverterObj.
// meterObj is the fleet's shared coupling-point object, read for grid
// voltage and frequency. now supplies the real-time clock StepTo advances
// to (fleets step in real time, not simulated time); pass nil to use
// time.Now.
func NewFleetDevice(transport FleetTransport, lfdi, inverterObj, meterObj string, now func() time.Time) (*FleetDevice, error) {
	if transport == nil {
		return nil, fmt.Errorf("fleet device %s: transport is required", lfdi)
	}
	if inverterObj == "" {
		return nil, fmt.Errorf("fleet device %s: inverter object name is required", lfdi)
	}
	if meterObj == "" {
		return nil, fmt.Errorf("fleet device %s: meter object name is required", lfdi)
	}
	if now == nil {
		now = time.Now
	}
	return &FleetDevice{transport: transport, lfdi: lfdi, inverterObj: inverterObj, meterObj: meterObj, now: now}, nil
}

// LFDI returns the managed device's LFDI, as configured.
func (d *FleetDevice) LFDI() string { return d.lfdi }

// resyncAfterRestart re-applies the last commanded setpoint if the fleet's
// sidecar has restarted since this device last talked to it. A restart
// reloads the GLM from scratch (P_Out back to the template's 0), and
// without this a caller would see healthy readings, or a healthy next
// ApplySetpoint, from a silently reset model as if the last commanded
// setpoint still held. Chosen over reporting the reset as an error: an
// aggregator's managed DER should not silently stop contributing its
// committed power because its physics process happened to restart.
func (d *FleetDevice) resyncAfterRestart(ctx context.Context) error {
	gen := d.transport.Generation()
	d.mu.Lock()
	stale := gen != d.lastGeneration
	controls := d.lastControls
	d.mu.Unlock()
	if !stale || controls == nil {
		d.mu.Lock()
		d.lastGeneration = gen
		d.mu.Unlock()
		return nil
	}
	if _, err := d.transport.Set(ctx, []FleetSetItem{
		{Object: d.inverterObj, Property: "P_Out", Value: controls.ActivePowerW},
		{Object: d.inverterObj, Property: "Q_Out", Value: controls.ReactivePowerVAr},
	}); err != nil {
		return fmt.Errorf("fleet device %s: re-apply setpoint after restart: %w", d.lfdi, err)
	}
	if _, err := d.transport.StepTo(ctx, d.now()); err != nil {
		return fmt.Errorf("fleet device %s: re-apply setpoint after restart: step_to: %w", d.lfdi, err)
	}
	d.mu.Lock()
	d.lastGeneration = gen
	d.mu.Unlock()
	return nil
}

// ReadState reads the device's rated power and the fleet's grid
// conditions. It never commands a setpoint of its own, beyond the
// restart resync above; ApplySetpoint is what steps a new command.
func (d *FleetDevice) ReadState(ctx context.Context) (StateReading, error) {
	if !d.transport.Healthy() {
		return StateReading{}, fmt.Errorf("fleet device %s: sidecar is down", d.lfdi)
	}
	if err := d.resyncAfterRestart(ctx); err != nil {
		return StateReading{}, err
	}
	vals, err := d.transport.Get(ctx, []FleetGetItem{
		{Object: d.inverterObj, Property: "rated_power"},
		{Object: d.meterObj, Property: "measured_voltage_A"},
		{Object: d.meterObj, Property: "nominal_voltage"},
		{Object: d.meterObj, Property: "measured_frequency"},
	})
	if err != nil {
		return StateReading{}, fmt.Errorf("fleet device %s: read state: %w", d.lfdi, err)
	}
	if len(vals) != 4 {
		return StateReading{}, fmt.Errorf("fleet device %s: read state: got %d values, want 4", d.lfdi, len(vals))
	}
	ratedPower, err := vals[0].Float64()
	if err != nil {
		return StateReading{}, fmt.Errorf("fleet device %s: rated_power: %w", d.lfdi, err)
	}
	nominalVoltage, err := vals[2].Float64()
	if err != nil {
		return StateReading{}, fmt.Errorf("fleet device %s: nominal_voltage: %w", d.lfdi, err)
	}
	if nominalVoltage == 0 {
		return StateReading{}, fmt.Errorf("fleet device %s: meter %s has nominal_voltage 0", d.lfdi, d.meterObj)
	}
	freqHz, err := vals[3].Float64()
	if err != nil {
		return StateReading{}, fmt.Errorf("fleet device %s: measured_frequency: %w", d.lfdi, err)
	}
	return StateReading{
		Grid: inverter.GridState{
			VoltsPU: vals[1].Magnitude() / nominalVoltage,
			FreqHz:  freqHz,
			Time:    d.now(),
		},
		MaxPowerW: ratedPower,
	}, nil
}

// ApplySetpoint commands P_Out and Q_Out on the device's inverter object,
// steps the shared model to now, and returns the achieved state as the
// sidecar read it back (data-invariants: the returned state is what the
// protocol reported, never the requested value echoed back unchecked).
// Connected, Energized and Mode are the exception: see the FleetDevice
// doc comment.
func (d *FleetDevice) ApplySetpoint(ctx context.Context, controls inverter.ControlOutputs) (inverter.InverterState, error) {
	if !d.transport.Healthy() {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: sidecar is down", d.lfdi)
	}
	if err := d.resyncAfterRestart(ctx); err != nil {
		return inverter.InverterState{}, err
	}
	if _, err := d.transport.Set(ctx, []FleetSetItem{
		{Object: d.inverterObj, Property: "P_Out", Value: controls.ActivePowerW},
		{Object: d.inverterObj, Property: "Q_Out", Value: controls.ReactivePowerVAr},
	}); err != nil {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: apply setpoint: %w", d.lfdi, err)
	}
	modelTime, err := d.transport.StepTo(ctx, d.now())
	if err != nil {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: step_to: %w", d.lfdi, err)
	}
	got, err := d.transport.Get(ctx, []FleetGetItem{
		{Object: d.inverterObj, Property: "P_Out"},
		{Object: d.inverterObj, Property: "Q_Out"},
	})
	if err != nil {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: read back setpoint: %w", d.lfdi, err)
	}
	if len(got) != 2 {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: read back setpoint: got %d values, want 2", d.lfdi, len(got))
	}
	activeW, err := got[0].Float64()
	if err != nil {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: P_Out: %w", d.lfdi, err)
	}
	reactiveVAr, err := got[1].Float64()
	if err != nil {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: Q_Out: %w", d.lfdi, err)
	}

	cp := controls
	d.mu.Lock()
	d.lastControls = &cp
	d.mu.Unlock()

	return inverter.InverterState{
		ActivePowerW:     activeW,
		ReactivePowerVAr: reactiveVAr,
		Connected:        controls.Connected,
		Energized:        controls.Energized,
		Mode:             controls.Mode,
		Time:             modelTime,
	}, nil
}
