package device

import (
	"context"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// RealDeviceConfig holds connection parameters for the SunSpec/Modbus
// hardware backend. No go-sunspec dependency yet; this struct captures the
// intended shape for the future plumbing.
type RealDeviceConfig struct {
	Host   string // Modbus TCP host
	Port   int    // Modbus TCP port (typically 502)
	UnitID uint8  // Modbus unit identifier
}

// RealDevice is the DERDevice backend for a physical SunSpec/Modbus inverter.
// Pure-Go (no CGO surface). Every method returns ErrBackendNotImplemented
// until go-sunspec is wired in a later ticket. The safety-guard decorator
// wraps this backend when it is selected; the guards are exercised against
// a fake DERDevice in unit tests even though this backend body is a stub.
type RealDevice struct {
	cfg RealDeviceConfig
}

// NewRealDevice constructs a RealDevice backend.
func NewRealDevice(cfg RealDeviceConfig) (*RealDevice, error) {
	return &RealDevice{cfg: cfg}, nil
}

// ReadState is a stub. Returns ErrBackendNotImplemented.
func (r *RealDevice) ReadState(_ context.Context) (StateReading, error) {
	return StateReading{}, ErrBackendNotImplemented
}

// ApplySetpoint is a stub. Returns ErrBackendNotImplemented.
func (r *RealDevice) ApplySetpoint(_ context.Context, _ inverter.ControlOutputs) (inverter.InverterState, error) {
	return inverter.InverterState{}, ErrBackendNotImplemented
}
