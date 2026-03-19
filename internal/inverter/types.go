package inverter

import "time"

// InverterRating defines the simulated 10kW PV inverter nameplate.
// IEEE 1547 Category B, residential split-phase.
var Rating = struct {
	RatedW    float64 // nameplate active power (watts)
	RatedVA   float64 // nameplate apparent power (VA)
	RatedVAr  float64 // reactive power capability (44% per 1547 Table 7)
	NominalV  float64 // nominal voltage (volts)
	NominalHz float64 // nominal frequency (Hz)
}{
	RatedW:    10000,
	RatedVA:   11100,
	RatedVAr:  4400,
	NominalV:  240,
	NominalHz: 60,
}

// GridState represents the current grid conditions at the point of coupling.
type GridState struct {
	VoltsPU float64   // per-unit voltage (1.0 = nominal)
	FreqHz  float64   // frequency in Hz
	Time    time.Time // simulation time
}

// InverterState represents the current inverter output and status.
type InverterState struct {
	ActivePowerW    float64     // output active power (W), positive = delivering
	ReactivePowerVAr float64   // output reactive power (VAr), positive = over-excited
	PowerFactor     float64     // power factor (0-1)
	VoltsPU         float64     // measured voltage per-unit
	FreqHz          float64     // measured frequency Hz
	Connected       bool        // galvanic connection to grid
	Energized       bool        // delivering/absorbing power
	Mode            ControlMode // active control mode
	Time            time.Time   // state timestamp
}

// ControlMode identifies the active IEEE 1547 control mode.
type ControlMode int

const (
	ModeConstantPF  ControlMode = iota // 5.3.2 constant power factor
	ModeVoltVar                        // 5.3.3 voltage-reactive power
	ModeWattVar                        // 5.3.4 active power-reactive power
	ModeConstantQ                      // 5.3.5 constant reactive power
	ModeVoltWatt                       // 5.4.2 voltage-active power
	ModeFreqDroop                      // 6.5.2.7 frequency droop
	ModeDisconnected                   // ceased to energize
	ModeTripped                        // protective trip
	ModeEnterService                   // waiting to enter service
)

// String returns the mode name.
func (m ControlMode) String() string {
	names := [...]string{
		"ConstantPF", "VoltVar", "WattVar", "ConstantQ",
		"VoltWatt", "FreqDroop", "Disconnected", "Tripped", "EnterService",
	}
	if int(m) < len(names) {
		return names[m]
	}
	return "Unknown"
}

// ControlOutputs is the result of applying control modes to the inverter.
type ControlOutputs struct {
	ActivePowerW     float64 // limited active power
	ReactivePowerVAr float64 // commanded reactive power
	Connected        bool    // should be connected
	Energized        bool    // should be energized
	Mode             ControlMode
}

// SimConfig holds the inverter client configuration.
type SimConfig struct {
	ServerURL      string
	CertFile       string
	KeyFile        string
	CAFile         string
	Scenario       string
	TimeScale      float64       // simulation speed multiplier
	TickInterval   time.Duration // simulation tick
	ReportInterval time.Duration // status/metering report interval
}

// CurvePoint is a single point on a piecewise linear control curve.
type CurvePoint struct {
	X float64 // independent variable (voltage p.u. or power fraction)
	Y float64 // dependent variable (reactive power fraction or power fraction)
}
