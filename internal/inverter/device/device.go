// Package device defines the DERDevice seam between the SEP2 control domain
// and a source of physical state. Three backends implement DERDevice:
// Synthetic (the scenario harness), GridLABD (HELICS co-simulation stub), and
// RealDevice (SunSpec/Modbus hardware stub). Backend selection happens once at
// construction via New; the tick loop never branches on backend identity.
package device

import (
	"context"
	"errors"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// ErrBackendNotImplemented is returned by stub backends that are not yet
// wired to their underlying transport (GridLABD, RealDevice).
var ErrBackendNotImplemented = errors.New("backend not implemented")

// DERDevice is the seam between the SEP2 control domain and a source of
// physical state. ReadState returns the device's current measured state;
// ApplySetpoint pushes a commanded control output to the device and reports
// back the achieved state.
//
// Three backends implement it: Synthetic (scenario harness, no I/O),
// GridLABD (HELICS co-simulation, stub), RealDevice (SunSpec/Modbus, stub).
// The interface is defined here in its own package so the dependency
// direction is unambiguous: backends import inverter domain types; the
// inverter domain never imports backends.
type DERDevice interface {
	// ReadState returns the device's current physical state plus the
	// maximum active power available this tick. For the synthetic backend
	// this is the scenario-walked grid and the irradiance-derived ceiling;
	// for hardware it is a live register read.
	ReadState(ctx context.Context) (StateReading, error)

	// ApplySetpoint commands a control output to the device and returns the
	// achieved state after the device responds. For the synthetic backend
	// this is the ComputeOutput math; for hardware it is a register write
	// followed by a read-back.
	ApplySetpoint(ctx context.Context, controls inverter.ControlOutputs) (inverter.InverterState, error)
}

// StateReading is the result of ReadState: the grid conditions the
// controller evaluates against plus the available active-power ceiling for
// this tick.
type StateReading struct {
	Grid      inverter.GridState // VoltsPU, FreqHz, Time
	MaxPowerW float64            // available active power ceiling (W)
	// Irradiance carries the raw W/m² value for the HMI irradiance display.
	// Populated by the Synthetic backend; zero for other backends.
	Irradiance float64
}
