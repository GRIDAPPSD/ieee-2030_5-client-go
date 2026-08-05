package inverter

import (
	"math"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// ApplyControls processes a DERControlBase and grid state to determine
// the inverter's output. Implements IEEE 1547 priority ordering per section 4.7:
//
//	a) connect/disconnect (highest)
//	b) trip/ride-through
//	c) volt-watt / freq-droop
//	d) active power limit
//	e) volt-var / watt-var / constant-PF / constant-Q
//
// Curve-typed modes (Volt/Var, Volt/Watt) use the compiled-in IEEE 1547
// default curves. Callers that have fetched server-supplied curves should
// use ApplyControlsWithCurves instead.
func ApplyControls(base *sep2.DERControlBase, grid GridState, maxPW float64) ControlOutputs {
	return ApplyControlsWithCurves(base, grid, maxPW, nil)
}

// ApplyControlsWithCurves is ApplyControls with optional server-supplied
// curve overrides. When `curves` is non-nil and contains a curve for the
// requested CurveType (sep2.CurveTypeOpModVoltVar etc.), that curve is
// used; otherwise the IEEE 1547 compiled-in default applies.
//
// `curves == nil` is equivalent to calling ApplyControls : no behavior
// change for callers that haven't adopted the curve cache yet.
//
// IEEE-042 (Phase 5 closer): plumbs server-fetched DERCurves into the
// control application path. cmd/inverterclient/main.go installs a
// state-machine hook that populates the cache on EVENT_RECEIVED →
// EVENT_STARTED transitions.
func ApplyControlsWithCurves(
	base *sep2.DERControlBase,
	grid GridState,
	maxPW float64,
	curves *DERCurveCache,
) ControlOutputs {
	out := ControlOutputs{
		ActivePowerW: maxPW,
		Connected:    true,
		Energized:    true,
		Mode:         ModeConstantPF,
	}

	if base == nil {
		// No controls : default to PF=1.0
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

	// Priority c: volt-watt (limits active power based on voltage). Uses
	// the server-supplied Volt/Watt curve if cached, else the IEEE 1547
	// default. Pre-existing behavior: this branch fires under the
	// OpModVoltVar guard (a quirk flagged in IEEE-041; out of scope for
	// IEEE-042 to refactor).
	if base.OpModVoltVar != nil {
		curve := lookupCurveOrDefault(curves, sep2.CurveTypeOpModVoltWatt, VoltWattDefaultCurve())
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
		// Volt-var: reactive power as function of voltage. Uses the
		// server-supplied Volt/Var curve if cached, else IEEE 1547 default.
		curve := lookupCurveOrDefault(curves, sep2.CurveTypeOpModVoltVar, VoltVarDefaultCurve())
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

// lookupCurveOrDefault returns the cached curve for `curveType` when
// present, else `fallback`. A nil cache also returns fallback. Extracted so
// the curve-typed branches in ApplyControlsWithCurves stay readable.
func lookupCurveOrDefault(cache *DERCurveCache, curveType uint8, fallback []CurvePoint) []CurvePoint {
	if cache == nil {
		return fallback
	}
	if points, ok := cache.Lookup(curveType); ok && len(points) > 0 {
		return points
	}
	return fallback
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
