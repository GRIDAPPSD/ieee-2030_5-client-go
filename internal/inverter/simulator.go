package inverter

import (
	"math"
	"time"
)

// Irradiance returns the solar irradiance in W/m² at the given time.
// Uses a sinusoidal model: 0 at night (6pm-6am), peak 1000 at noon.
func Irradiance(t time.Time) float64 {
	hour := float64(t.Hour()) + float64(t.Minute())/60.0
	if hour < 6 || hour > 18 {
		return 0
	}
	return 1000 * math.Sin(math.Pi*(hour-6)/12)
}

// MaxPowerW returns the maximum available active power from the PV array
// given the current irradiance. Linear relationship to rated power.
func MaxPowerW(irradiance float64) float64 {
	if irradiance <= 0 {
		return 0
	}
	return (irradiance / 1000.0) * Rating.RatedW
}

// EnforceVALimit adjusts P and Q so that apparent power S = sqrt(P²+Q²)
// does not exceed the inverter's VA rating. If S > rating, Q is reduced first,
// then P if necessary.
func EnforceVALimit(pW, qVAr float64) (float64, float64) {
	s := math.Sqrt(pW*pW + qVAr*qVAr)
	if s <= Rating.RatedVA {
		return pW, qVAr
	}

	// Scale Q down first, keeping P
	maxQ := math.Sqrt(Rating.RatedVA*Rating.RatedVA - pW*pW)
	if maxQ >= 0 {
		if qVAr > 0 {
			return pW, math.Min(qVAr, maxQ)
		}
		return pW, math.Max(qVAr, -maxQ)
	}

	// P alone exceeds VA rating
	return Rating.RatedVA, 0
}

// ComputePowerFactor calculates the power factor from P and Q.
// Returns 1.0 if Q is zero or P is zero.
func ComputePowerFactor(pW, qVAr float64) float64 {
	s := math.Sqrt(pW*pW + qVAr*qVAr)
	if s == 0 {
		return 1.0
	}
	return math.Abs(pW) / s
}

// ComputeOutput builds the final InverterState from control outputs and grid state.
func ComputeOutput(controls ControlOutputs, grid GridState) InverterState {
	p := controls.ActivePowerW
	q := controls.ReactivePowerVAr

	if !controls.Connected || !controls.Energized {
		p = 0
		q = 0
	}

	p, q = EnforceVALimit(p, q)

	return InverterState{
		ActivePowerW:     p,
		ReactivePowerVAr: q,
		PowerFactor:      ComputePowerFactor(p, q),
		VoltsPU:          grid.VoltsPU,
		FreqHz:           grid.FreqHz,
		Connected:        controls.Connected,
		Energized:        controls.Energized,
		Mode:             controls.Mode,
		Time:             grid.Time,
	}
}
