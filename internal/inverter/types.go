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
	ActivePowerW     float64     // output active power (W), positive = delivering
	ReactivePowerVAr float64     // output reactive power (VAr), positive = over-excited
	PowerFactor      float64     // power factor (0-1)
	VoltsPU          float64     // measured voltage per-unit
	FreqHz           float64     // measured frequency Hz
	Connected        bool        // galvanic connection to grid
	Energized        bool        // delivering/absorbing power
	Mode             ControlMode // active control mode
	Time             time.Time   // state timestamp
}

// ControlMode identifies the active IEEE 1547 control mode.
type ControlMode int

const (
	ModeConstantPF   ControlMode = iota // 5.3.2 constant power factor
	ModeVoltVar                         // 5.3.3 voltage-reactive power
	ModeWattVar                         // 5.3.4 active power-reactive power
	ModeConstantQ                       // 5.3.5 constant reactive power
	ModeVoltWatt                        // 5.4.2 voltage-active power
	ModeFreqDroop                       // 6.5.2.7 frequency droop
	ModeDisconnected                    // ceased to energize
	ModeTripped                         // protective trip
	ModeEnterService                    // waiting to enter service
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

	// CSIPStrict, when true, drops the GCM cipher-suite fallback from the
	// TLS handshake so connections to non-CCM-8 peers fail loudly instead
	// of silently degrading. Default false retains GCM for interop with
	// the in-tree test/dev server (see internal/tls/ccmserver.go).
	CSIPStrict bool

	// CSIP, when true, selects CSIP-mode EndDevice discovery: the inverter
	// GETs the server's EndDeviceList and finds its own EndDevice by LFDI
	// match instead of POSTing to /edev to self-register. CSIP devices are
	// pre-allowlisted out-of-band; the device's job is to discover its
	// pre-provisioned EndDevice, not to create one. If our LFDI is not in
	// the list, the inverter idles and re-polls the list. Default false
	// retains the IEEE 2030.5 self-registration path. See IEEE-029.
	CSIP bool

	// ExpectedPIN is the out-of-band-provisioned PIN that the device should
	// see echoed by the server's Registration resource (CSIP V1.2 BASIC-001
	// step 5 / IEEE 2030.5 §10). IEEE-032 plumbs the value and logs it
	// alongside the server-presented PIN; mismatch enforcement and the
	// "0 = skip" sentinel land in IEEE-033. IEEE-034 corrects the mismatch
	// branch to fatal-on-nonzero (rg.PIN==0 stays idle; nonzero mismatch is
	// a wrong-device/server-pair condition and must fail loud).
	ExpectedPIN uint

	// AllowUnregistered, when true, bypasses the strict missing-RegistrationLink
	// check in --csip mode and lets Phase 3+ proceed without a server-published
	// Registration resource. Default false: in CSIP mode, a nil RegistrationLink
	// triggers an idle-loop that re-fetches the EndDevice on dcap.PollRate until
	// the server publishes the link (CSIP V1.2 BASIC-001 step 5 commissioning
	// gate). Non-CSIP mode (--csip=false) ignores this flag — the legacy
	// IEEE 2030.5 Register() POST path skips Phase 2b entirely.
	// See IEEE-034.
	AllowUnregistered bool

	// LogEventPEN is the IANA-registered Private Enterprise Number stamped
	// into every LogEvent the inverter POSTs (IEEE 2030.5 §9.5 logEventPEN).
	// Production deployments must register their own PEN with IANA and
	// configure it here (--pen flag / SEP2_PEN env). Zero (default) means
	// "no manufacturer namespace" — acceptable for test / interop, but
	// downstream operators reading log archives cannot disambiguate codes
	// across vendors without a real PEN. See IEEE-053.
	LogEventPEN uint32
}

// CurvePoint is a single point on a piecewise linear control curve.
type CurvePoint struct {
	X float64 // independent variable (voltage p.u. or power fraction)
	Y float64 // dependent variable (reactive power fraction or power fraction)
}
