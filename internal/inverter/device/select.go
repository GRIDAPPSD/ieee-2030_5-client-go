package device

import (
	"fmt"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// defaultMaxStateAge is the staleness bound used when the caller does not
// supply a DeviceMaxStateAge in SimConfig. Five seconds is generous enough
// to cover a tick loop that reads state immediately before writing, while
// tight enough to catch a loop that has stalled or is replaying old data.
const defaultMaxStateAge = 5 * time.Second

// New constructs the DERDevice for the configured backend. Selection happens
// once at startup; the tick loop never branches on backend identity.
//
// Supported values for cfg.Backend:
//   - "" or "synthetic": the scenario harness (default, no I/O).
//   - "gridlabd": GridLAB-D co-simulation via HELICS (stub until HELICS lands).
//   - "realdevice": SunSpec/Modbus hardware with safety guards applied.
//
// An unknown backend is a loud startup error (fail-closed discipline).
func New(cfg inverter.SimConfig, sc inverter.Scenario) (DERDevice, error) {
	switch cfg.Backend {
	case "", "synthetic":
		return NewSynthetic(sc, cfg.TickInterval, cfg.TimeScale), nil
	case "gridlabd":
		return NewGridLABD(gridLABDConfigFrom(cfg))
	case "realdevice":
		base, err := NewRealDevice(realDeviceConfigFrom(cfg))
		if err != nil {
			return nil, err
		}
		maxAge := cfg.DeviceMaxStateAge
		if maxAge <= 0 {
			maxAge = defaultMaxStateAge
		}
		return WithSafetyGuards(base, nameplateFromRating(), GuardConfig{MaxStateAge: maxAge}), nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want synthetic|gridlabd|realdevice)", cfg.Backend)
	}
}

// nameplateFromRating builds a Nameplate from the package-level inverter.Rating.
// A later ticket wires this from the device's read-once DERCapability.
func nameplateFromRating() Nameplate {
	return Nameplate{
		RatedW:   inverter.Rating.RatedW,
		RatedVAr: inverter.Rating.RatedVAr,
	}
}

// gridLABDConfigFrom extracts GridLABD config from SimConfig.
// The fields are stubs; the real HELICS config arrives with the federate ticket.
func gridLABDConfigFrom(_ inverter.SimConfig) GridLABDConfig {
	return GridLABDConfig{
		BrokerAddr:   "localhost:23404",
		FederateName: "inverterclient",
		TimeDelta:    1.0,
	}
}

// realDeviceConfigFrom extracts RealDevice config from SimConfig.
// The fields are stubs; the real Modbus config arrives with the go-sunspec ticket.
func realDeviceConfigFrom(_ inverter.SimConfig) RealDeviceConfig {
	return RealDeviceConfig{
		Host:   "localhost",
		Port:   502,
		UnitID: 1,
	}
}
