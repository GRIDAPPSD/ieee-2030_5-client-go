// Package inverter : IEEE 2030.5 Table 34 LogEvent code constants (PEN 0
// general category), consumed by the alarm detectors.
//
// CSIP V1.2 BASIC-027 requires the DER Client to POST a LogEvent on each
// alarm-class state transition (LVRT trip, HVRT trip, freq-watt curtailment,
// inverter offline, manufacturer-specific faults). Each LogEvent carries a
// logEventCode whose meaning is namespaced by logEventPEN: when PEN == 0
// the codes are interpreted per IEEE 2030.5-2018 §9.5 Table 34 ("general"
// category). The mapping below is
// the same set the emitter unit tests treat as the
// reference (code 1 → LVRT, code 5 → Volt/Var curtailment, etc).
//
// Future Pike: if the production deployment registers a PEN with IANA and
// wishes to redefine these codes, the alarm detector accepts the code map
// as a struct field : swap the constants below for a vendor-specific map
// at construction. Do NOT redefine the constants here; downstream tests
// pin to these numeric values.
//
// IEEE 2030.5-2018 §9.5, Table 34 reference.

package inverter

// LogEvent codes per IEEE 2030.5-2018 §9.5 Table 34 (PEN == 0 general
// category). The numeric values are stable across deployments and
// interoperate with any server that follows the spec literally.
const (
	// LogEventCodeVoltageLow corresponds to LE_VOLT_LO : Low Voltage
	// Ride-Through trip. Emitted when the inverter trips because grid
	// voltage stayed below the IEEE 1547 Table 2 curve for longer than
	// the curve permits. See internal/inverter/ridethrough.go
	// CheckVoltageTrip with voltsPU < 1.0.
	LogEventCodeVoltageLow uint8 = 1

	// LogEventCodeVoltageHigh corresponds to LE_VOLT_HI : High Voltage
	// Ride-Through trip. Emitted when grid voltage stayed above the
	// IEEE 1547 Table 2 over-voltage curve for longer than permitted.
	LogEventCodeVoltageHigh uint8 = 2

	// LogEventCodeFrequencyLow corresponds to LE_FREQ_LO : Low Frequency
	// Ride-Through trip (IEEE 1547 Table 3 under-frequency).
	LogEventCodeFrequencyLow uint8 = 3

	// LogEventCodeFrequencyHigh corresponds to LE_FREQ_HI : High
	// Frequency Ride-Through trip (IEEE 1547 Table 3 over-frequency).
	LogEventCodeFrequencyHigh uint8 = 4

	// LogEventCodeReactiveLimit corresponds to LE_REACTIVE_LIMIT :
	// Volt/Var curtailment activated. Emitted when the Volt/Var curve
	// drives reactive power outside the deadband (the inverter is
	// actively absorbing or injecting vars to support voltage).
	LogEventCodeReactiveLimit uint8 = 5

	// LogEventCodeActiveLimit corresponds to LE_ACTIVE_LIMIT :
	// Freq/Watt curtailment activated. Emitted when frequency droop
	// reduces active power below the pre-disturbance setpoint.
	LogEventCodeActiveLimit uint8 = 6

	// LogEventCodeGenDisable corresponds to LE_GEN_DISABLE : inverter
	// offline (de-energized via OpModConnect=false / OpModEnergize=false
	// from the server, OR a protective trip cleared the contactors).
	LogEventCodeGenDisable uint8 = 7
)
