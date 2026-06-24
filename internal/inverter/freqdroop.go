package inverter

import "math"

// DefaultDroopPct is the IEEE 1547 default frequency droop for Category B (5%).
const DefaultDroopPct = 5.0

// FreqDroop calculates the active power adjustment for frequency droop response.
// Per IEEE 1547 section 6.5.2.7:
//
//	deltaP = -(deltaF / (droopPct/100 * nominalHz)) * ratedW
//
// Returns the adjusted active power in watts, clamped to [0, preDisturbancePW].
// droopPct is a percentage (e.g., 5.0 = 5%).
func FreqDroop(freqHz, nominalHz, droopPct, preDisturbancePW, ratedW float64) float64 {
	if droopPct <= 0 {
		return preDisturbancePW
	}

	deltaF := freqHz - nominalHz
	deadband := 0.036 * nominalHz // ~2.16 Hz deadband per typical settings

	// Within deadband : no adjustment
	if math.Abs(deltaF) < 0.017*nominalHz {
		return preDisturbancePW
	}

	droopFraction := droopPct / 100.0
	deltaP := -(deltaF / (droopFraction * nominalHz)) * ratedW

	adjustedP := preDisturbancePW + deltaP

	// Clamp to valid range
	if adjustedP < 0 {
		adjustedP = 0
	}
	if adjustedP > ratedW {
		adjustedP = ratedW
	}

	_ = deadband // suppress unused
	return adjustedP
}
