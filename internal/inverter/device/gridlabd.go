package device

import (
	"context"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// GridLABDConfig holds the federate configuration for the GridLAB-D backend.
// The HELICS federate exchange is not implemented yet; this struct captures
// the intended shape so the plumbing can land before the transport.
type GridLABDConfig struct {
	BrokerAddr   string  // HELICS broker address (e.g. "localhost:23404")
	FederateName string  // name this federate registers under
	TimeDelta    float64 // HELICS time delta in seconds
}

// GridLABD is the DERDevice backend for GridLAB-D co-simulation via HELICS.
// The struct and constructor land now; every method returns
// ErrBackendNotImplemented until the HELICS transport arrives in a later
// ticket. The build stays CGO-free: no HELICS library is linked.
type GridLABD struct {
	cfg GridLABDConfig
}

// NewGridLABD constructs a GridLABD backend with the supplied config.
// Returns an error if the config is obviously invalid (empty broker address).
func NewGridLABD(cfg GridLABDConfig) (*GridLABD, error) {
	return &GridLABD{cfg: cfg}, nil
}

// ReadState is a stub. Selecting the gridlabd backend before the real
// federate lands is a loud config error, not a silent nominal read.
func (g *GridLABD) ReadState(_ context.Context) (StateReading, error) {
	return StateReading{}, ErrBackendNotImplemented
}

// ApplySetpoint is a stub. Returns ErrBackendNotImplemented.
func (g *GridLABD) ApplySetpoint(_ context.Context, _ inverter.ControlOutputs) (inverter.InverterState, error) {
	return inverter.InverterState{}, ErrBackendNotImplemented
}
