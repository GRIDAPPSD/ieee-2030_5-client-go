package inverter

import (
	"math"

	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// ApplyControls processes a DERControlBase and grid state to determine
// the inverter's output. Implements IEEE 1547 priority ordering per section 4.7:
//   a) connect/disconnect (highest)
//   b) trip/ride-through
//   c) volt-watt / freq-droop
//   d) active power limit
//   e) volt-var / watt-var / constant-PF / constant-Q
func ApplyControls(base *sep2.DERControlBase, grid GridState, maxPW float64) ControlOutputs {
	out := ControlOutputs{
		ActivePowerW: maxPW,
		Connected:    true,
		Energized:    true,
		Mode:         ModeConstantPF,
	}

	if base == nil {
		// No controls — default to PF=1.0
		return out
	}

	// Priority a: connect/disconnect
	if base.OpModConnect != nil && !*base.OpModConnect {
		return ControlOutputs{
			Connected: false,
			Energized: false,
			Mode:      ModeDisconnected,
		}
	}
	if base.OpModEnergize != nil && !*base.OpModEnergize {
		return ControlOutputs{
			Connected: true,
			Energized: false,
			Mode:      ModeDisconnected,
		}
	}

	// Priority c: volt-watt (limits active power based on voltage)
	if base.OpModVoltVar != nil {
		// Volt-watt uses the default curve
		curve := VoltWattDefaultCurve()
		pFraction := EvaluateCurve(curve, grid.VoltsPU)
		voltWattLimit := pFraction * Rating.RatedW
		if voltWattLimit < out.ActivePowerW {
			out.ActivePowerW = voltWattLimit
			out.Mode = ModeVoltWatt
		}
	}

	// Priority c: frequency droop
	if base.OpModFreqDroop != nil {
		droopPct := float64(*base.OpModFreqDroop) / 100.0 // stored as hundredths
		if droopPct <= 0 {
			droopPct = DefaultDroopPct
		}
		adjustedP := FreqDroop(grid.FreqHz, Rating.NominalHz, droopPct, out.ActivePowerW, Rating.RatedW)
		if adjustedP < out.ActivePowerW {
			out.ActivePowerW = adjustedP
			out.Mode = ModeFreqDroop
		}
	}

	// Priority d: active power limit
	if base.OpModMaxLimW != nil {
		limitW := float64(base.OpModMaxLimW.Value)
		if limitW < out.ActivePowerW {
			out.ActivePowerW = limitW
		}
	}
	if base.OpModFixedW != nil {
		out.ActivePowerW = float64(base.OpModFixedW.Value)
	}
	if base.OpModTargetW != nil {
		out.ActivePowerW = float64(base.OpModTargetW.Value)
	}

	// Priority e: reactive power modes (mutually exclusive)
	switch {
	case base.OpModVoltVar != nil:
		// Volt-var: reactive power as function of voltage
		curve := VoltVarDefaultCurve()
		qFraction := EvaluateCurve(curve, grid.VoltsPU)
		out.ReactivePowerVAr = qFraction * Rating.RatedVAr
		out.Mode = ModeVoltVar

	case base.OpModFixedVar != nil:
		out.ReactivePowerVAr = float64(base.OpModFixedVar.Value)
		out.Mode = ModeConstantQ

	case base.OpModFixedPFAbsorbW != nil:
		// Fixed PF for power absorption (under-excited)
		pf := float64(base.OpModFixedPFAbsorbW.Displacement) / 1000.0
		if pf > 0 && pf <= 1.0 && out.ActivePowerW > 0 {
			// Q = P * tan(acos(PF)), negative for absorption
			tanPhi := tanFromPF(pf)
			out.ReactivePowerVAr = -out.ActivePowerW * tanPhi
		}
		out.Mode = ModeConstantPF

	case base.OpModFixedPFInjectW != nil:
		// Fixed PF for power injection (over-excited)
		pf := float64(base.OpModFixedPFInjectW.Displacement) / 1000.0
		if pf > 0 && pf <= 1.0 && out.ActivePowerW > 0 {
			tanPhi := tanFromPF(pf)
			out.ReactivePowerVAr = out.ActivePowerW * tanPhi
		}
		out.Mode = ModeConstantPF

	default:
		// Default: constant PF = 1.0 (Q = 0)
		out.ReactivePowerVAr = 0
		out.Mode = ModeConstantPF
	}

	return out
}

// tanFromPF computes tan(acos(pf)) for power factor to var calculation.
func tanFromPF(pf float64) float64 {
	if pf >= 1.0 {
		return 0
	}
	sinSq := 1 - pf*pf
	if sinSq <= 0 {
		return 0
	}
	return math.Sqrt(sinSq) / pf
}
