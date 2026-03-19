package inverter

import "time"

// RideThroughState tracks the ride-through state machine.
type RideThroughState int

const (
	RTNormal          RideThroughState = iota
	RTRidingThrough                   // within ride-through region
	RTTripped                         // protective trip
	RTWaitReconnect                   // waiting for reconnect delay
)

// RideThroughResult is the output of the ride-through evaluator.
type RideThroughResult struct {
	State        RideThroughState
	ShouldTrip   bool
	CanReconnect bool
}

// IEEE 1547 Table 4: Enter service criteria (Category B)
const (
	EnterServiceVMin float64       = 0.917  // minimum voltage p.u.
	EnterServiceVMax float64       = 1.05   // maximum voltage p.u.
	EnterServiceFMin float64       = 59.5   // minimum frequency Hz
	EnterServiceFMax float64       = 60.1   // maximum frequency Hz
	EnterServiceDelay time.Duration = 300 * time.Second // intentional delay
	ReconnectDelay   time.Duration = 300 * time.Second // Category B
)

// CheckEnterService returns true if grid conditions allow entering service
// per IEEE 1547 Table 4.
func CheckEnterService(grid GridState) bool {
	return grid.VoltsPU >= EnterServiceVMin &&
		grid.VoltsPU <= EnterServiceVMax &&
		grid.FreqHz >= EnterServiceFMin &&
		grid.FreqHz <= EnterServiceFMax
}

// CheckVoltageTrip evaluates IEEE 1547 voltage trip requirements.
// Returns true if the inverter must trip at the given voltage and duration.
// Based on Category III (most stringent) ride-through per 1547 Table 2.
func CheckVoltageTrip(voltsPU float64, duration time.Duration) bool {
	switch {
	case voltsPU < 0.50:
		return duration >= 160*time.Millisecond // must trip in 0.16s
	case voltsPU < 0.70:
		return duration >= 10*time.Second
	case voltsPU < 0.88:
		return duration >= 20*time.Second
	case voltsPU > 1.20:
		return duration >= 160*time.Millisecond
	case voltsPU > 1.10:
		return duration >= 12*time.Second
	default:
		return false // normal operating range
	}
}

// CheckFrequencyTrip evaluates IEEE 1547 frequency trip requirements.
// Based on 1547 Table 3.
func CheckFrequencyTrip(freqHz float64, duration time.Duration) bool {
	switch {
	case freqHz < 57.0:
		return duration >= 160*time.Millisecond
	case freqHz < 59.0:
		return duration >= 300*time.Second
	case freqHz > 62.0:
		return duration >= 160*time.Millisecond
	case freqHz > 60.5:
		return duration >= 300*time.Second
	default:
		return false // normal range
	}
}

// EvaluateRideThrough checks if the inverter should trip, ride through,
// or reconnect based on current grid conditions and time in abnormal state.
func EvaluateRideThrough(grid GridState, abnormalDuration time.Duration, currentState RideThroughState) RideThroughResult {
	isNormal := grid.VoltsPU >= 0.88 && grid.VoltsPU <= 1.10 &&
		grid.FreqHz >= 59.0 && grid.FreqHz <= 60.5

	switch currentState {
	case RTNormal:
		if isNormal {
			return RideThroughResult{State: RTNormal}
		}
		// Enter ride-through
		if CheckVoltageTrip(grid.VoltsPU, abnormalDuration) ||
			CheckFrequencyTrip(grid.FreqHz, abnormalDuration) {
			return RideThroughResult{State: RTTripped, ShouldTrip: true}
		}
		return RideThroughResult{State: RTRidingThrough}

	case RTRidingThrough:
		if isNormal {
			return RideThroughResult{State: RTNormal}
		}
		if CheckVoltageTrip(grid.VoltsPU, abnormalDuration) ||
			CheckFrequencyTrip(grid.FreqHz, abnormalDuration) {
			return RideThroughResult{State: RTTripped, ShouldTrip: true}
		}
		return RideThroughResult{State: RTRidingThrough}

	case RTTripped:
		if isNormal && abnormalDuration >= ReconnectDelay {
			return RideThroughResult{State: RTWaitReconnect, CanReconnect: true}
		}
		return RideThroughResult{State: RTTripped}

	case RTWaitReconnect:
		if CheckEnterService(grid) {
			return RideThroughResult{State: RTNormal, CanReconnect: true}
		}
		return RideThroughResult{State: RTWaitReconnect}
	}

	return RideThroughResult{State: RTNormal}
}
