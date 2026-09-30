package device

import (
	"context"
	"fmt"
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
	// Set writes every item and returns the value the model actually
	// applied for each, in the same order.
	Set(ctx context.Context, items []FleetSetItem) ([]float64, error)
	// StepTo advances the shared model to target and returns the time it
	// reached. A target not after the model's current time is a no-op, so
	// more than one device calling StepTo for the same tick is safe.
	StepTo(ctx context.Context, target time.Time) (time.Time, error)
	// Get reads every item and returns each value, in the same order.
	Get(ctx context.Context, items []FleetGetItem) ([]float64, error)
}

// FleetDevice is the DERDevice backend for one managed DER inside a fleet:
// it drives its own inverter object over a transport shared with every
// other device of the same fleet. Constructed only in the aggregator role
// (ADR-009 decision 2's construction-time gate).
//
// Known scope limit: ApplySetpoint calls StepTo on every device's own
// call, not once per fleet tick. That is safe (StepTo is a no-op once the
// model is already at the target), but it means N managed devices step
// the model N times per tick instead of once. The coordinated fleet loop
// that steps once per tick belongs to the aggregator's fleet loop
// (ADR-009's later "C4b" work), not this backend.
type FleetDevice struct {
	transport   FleetTransport
	lfdi        string
	inverterObj string
	now         func() time.Time
}

// NewFleetDevice constructs a FleetDevice for the managed device identified
// by lfdi, whose inverter object in the fleet's model is inverterObj. now
// supplies the real-time clock StepTo advances to (fleets run in real
// time, ADR-009); pass nil to use time.Now.
func NewFleetDevice(transport FleetTransport, lfdi, inverterObj string, now func() time.Time) (*FleetDevice, error) {
	if transport == nil {
		return nil, fmt.Errorf("fleet device %s: transport is required", lfdi)
	}
	if inverterObj == "" {
		return nil, fmt.Errorf("fleet device %s: inverter object name is required", lfdi)
	}
	if now == nil {
		now = time.Now
	}
	return &FleetDevice{transport: transport, lfdi: lfdi, inverterObj: inverterObj, now: now}, nil
}

// LFDI returns the managed device's LFDI, as configured.
func (d *FleetDevice) LFDI() string { return d.lfdi }

// ReadState reads the device's current output and rated power ceiling. It
// never advances the model; ApplySetpoint is what steps time.
func (d *FleetDevice) ReadState(ctx context.Context) (StateReading, error) {
	if !d.transport.Healthy() {
		return StateReading{}, fmt.Errorf("fleet device %s: sidecar is down", d.lfdi)
	}
	vals, err := d.transport.Get(ctx, []FleetGetItem{
		{Object: d.inverterObj, Property: "P_Out"},
		{Object: d.inverterObj, Property: "Q_Out"},
		{Object: d.inverterObj, Property: "rated_power"},
	})
	if err != nil {
		return StateReading{}, fmt.Errorf("fleet device %s: read state: %w", d.lfdi, err)
	}
	return StateReading{
		Grid:      inverter.GridState{VoltsPU: 1.0, FreqHz: inverter.Rating.NominalHz, Time: d.now()},
		MaxPowerW: vals[2],
	}, nil
}

// ApplySetpoint commands P_Out and Q_Out on the device's inverter object,
// steps the shared model to now, and returns the achieved state as the
// sidecar read it back (data-invariants: the returned state is what the
// protocol reported, never the requested value echoed back unchecked).
func (d *FleetDevice) ApplySetpoint(ctx context.Context, controls inverter.ControlOutputs) (inverter.InverterState, error) {
	if !d.transport.Healthy() {
		return inverter.InverterState{}, fmt.Errorf("fleet device %s: sidecar is down", d.lfdi)
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
	return inverter.InverterState{
		ActivePowerW:     got[0],
		ReactivePowerVAr: got[1],
		Connected:        controls.Connected,
		Energized:        controls.Energized,
		Mode:             controls.Mode,
		Time:             modelTime,
	}, nil
}
