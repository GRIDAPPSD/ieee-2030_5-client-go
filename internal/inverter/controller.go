package inverter

import (
	"errors"
	"fmt"
	"math"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// percentFullScale is the PerCent/SignedPerCent divisor: IEEE 2030.5-2018
// Annex B.2.3.4 defines both as hundredths of a percent, 10000 = 100%.
const percentFullScale = 10000.0

// ErrFixedVarRefused marks an opModFixedVar the device cannot honor: its
// refType names a basis the device does not know, or its setpoint is out of
// the schema's range.
var ErrFixedVarRefused = errors.New("opModFixedVar refused")

// RatedW is the device's own %setMaxW basis, in watts: the same value
// published as DERSettings.SetMaxW, against which OpModMaxLimW and
// OpModFixedW are scaled as percentages. A named type, rather than a plain
// float64, so it cannot be swapped with maxPW (the instantaneously
// available power ceiling, e.g. irradiance-limited) without a compile error.
type RatedW float64

// Ratings are the device's own bases for percentage controls. A basis the
// device does not know is left at its zero value, and a control that needs
// it is refused rather than scaled against a guess.
type Ratings struct {
	// SetMaxW is the %setMaxW basis.
	SetMaxW RatedW
	// SetMaxVar is the %setMaxVar basis in VAr; zero means not known.
	SetMaxVar float64
	// StatVarAvail is the %statVarAvail basis in VAr, nil when not known:
	// zero is a real reading (no reactive reserve), not an absent one.
	StatVarAvail *float64
}

// ApplyControls processes a DERControlBase and grid state to determine
// the inverter's output. Implements IEEE 1547 priority ordering per section 4.7:
//
//	a) connect/disconnect (highest)
//	b) trip/ride-through
//	c) volt-watt / freq-droop
//	d) active power limit
//	e) volt-var / watt-var / constant-PF / constant-Q
//
// ratings.SetMaxW is the device's own %setMaxW basis (the same value
// published as DERSettings.SetMaxW): OpModMaxLimW and OpModFixedW are
// percentages of it, not watts, per core's field docs. It is distinct from
// maxPW, which is the instantaneously available power ceiling (e.g.
// irradiance-limited), not the nameplate.
//
// Curve-typed modes (Volt/Var, Volt/Watt) use the compiled-in IEEE 1547
// default curves. Callers that have fetched server-supplied curves should
// use ApplyControlsWithCurves instead.
func ApplyControls(base *sep2.DERControlBase, grid GridState, maxPW float64, ratings Ratings) ControlOutputs {
	return ApplyControlsWithCurves(base, grid, maxPW, ratings, nil)
}

// ApplyControlsWithCurves is ApplyControls with optional server-supplied
// curve overrides. When `curves` is non-nil and contains a curve for the
// requested CurveType (sep2.CurveTypeOpModVoltVar etc.), that curve is
// used; otherwise the IEEE 1547 compiled-in default applies.
//
// `curves == nil` is equivalent to calling ApplyControls : no behavior
// change for callers that haven't adopted the curve cache yet.
//
// Phase 5 closer: plumbs server-fetched DERCurves into the
// control application path. cmd/inverterclient/main.go installs a
// state-machine hook that populates the cache on EVENT_RECEIVED ->
// EVENT_STARTED transitions.
func ApplyControlsWithCurves(
	base *sep2.DERControlBase,
	grid GridState,
	maxPW float64,
	ratings Ratings,
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

	// A base that cannot be honored is refused whole: nothing in it is
	// applied, and the caller keeps the controls in force before it.
	var fixedVAr float64
	if base.OpModFixedVar != nil {
		q, err := fixedVarVAr(*base.OpModFixedVar, ratings)
		if err != nil {
			return ControlOutputs{Refused: err}
		}
		fixedVAr = q
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
	// OpModVoltVar guard (a known quirk; out of scope here to refactor).
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
		// kOF is the per-unit frequency change per per-unit power change,
		// in thousandths (IEEE 2030.5-2018 FreqDroopType): 50 is 5% droop.
		// This model has no under-frequency response, so kUF is not read.
		droopPct := float64(base.OpModFreqDroop.KOF) / 10.0
		if droopPct <= 0 {
			droopPct = DefaultDroopPct
		}
		adjustedP := FreqDroop(grid.FreqHz, Rating.NominalHz, droopPct, out.ActivePowerW, Rating.RatedW)
		if adjustedP < out.ActivePowerW {
			out.ActivePowerW = adjustedP
			out.Mode = ModeFreqDroop
		}
	}

	// Priority d: active power limit. OpModMaxLimW and OpModFixedW are
	// percentages of ratings.SetMaxW (%setMaxW), not watts. OpModFixedW is signed
	// over %setMaxChargeRateW when negative per core's field doc; this
	// client has no separate charge-rate setting (PV-only, no battery) and
	// publishes only SetMaxW, so both signs scale against SetMaxW.
	if base.OpModMaxLimW != nil {
		limitW := float64(*base.OpModMaxLimW) / percentFullScale * float64(ratings.SetMaxW)
		if limitW < out.ActivePowerW {
			out.ActivePowerW = limitW
		}
	}
	if base.OpModFixedW != nil {
		out.ActivePowerW = float64(*base.OpModFixedW) / percentFullScale * float64(ratings.SetMaxW)
	}
	if base.OpModTargetW != nil {
		// ActivePower is Value * 10^Multiplier watts (IEEE 2030.5-2018
		// Annex B.2.3.4), already absolute, not a percentage of SetMaxW.
		out.ActivePowerW = float64(base.OpModTargetW.Value) * math.Pow(10, float64(base.OpModTargetW.Multiplier))
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
		out.ReactivePowerVAr = fixedVAr
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

// fixedVarVAr converts an opModFixedVar setpoint to VAr. Value is a
// SignedPerCent of the basis RefType names (IEEE 2030.5-2018 FixedVar and
// DERUnitRefType): 1 %setMaxW, 2 %setMaxVar, 3 %statVarAvail. An unknown
// basis or an out-of-range setpoint returns an error wrapping
// ErrFixedVarRefused, never a guessed or zero VAr.
func fixedVarVAr(fv sep2.FixedVar, r Ratings) (float64, error) {
	if fv.Value < -percentFullScale || fv.Value > percentFullScale {
		return 0, fmt.Errorf("%w: value %d is outside [-10000, 10000]", ErrFixedVarRefused, fv.Value)
	}
	var basis float64
	switch fv.RefType {
	case 1:
		if r.SetMaxW <= 0 {
			return 0, fmt.Errorf("%w: refType 1 (%%setMaxW) but the device has no setMaxW", ErrFixedVarRefused)
		}
		basis = float64(r.SetMaxW)
	case 2:
		if r.SetMaxVar <= 0 {
			return 0, fmt.Errorf("%w: refType 2 (%%setMaxVar) but the device has no setMaxVar", ErrFixedVarRefused)
		}
		basis = r.SetMaxVar
	case 3:
		if r.StatVarAvail == nil || *r.StatVarAvail < 0 {
			return 0, fmt.Errorf("%w: refType 3 (%%statVarAvail) but the device has no valid statVarAvail", ErrFixedVarRefused)
		}
		basis = *r.StatVarAvail
	default:
		return 0, fmt.Errorf("%w: refType %d is not %%setMaxW, %%setMaxVar or %%statVarAvail", ErrFixedVarRefused, fv.RefType)
	}
	return float64(fv.Value) / percentFullScale * basis, nil
}

// CheckControl reports whether the device can honor base as a whole, with
// the same rule ApplyControls refuses by. A nil base is honored.
func CheckControl(base *sep2.DERControlBase, r Ratings) error {
	if base == nil || base.OpModFixedVar == nil {
		return nil
	}
	_, err := fixedVarVAr(*base.OpModFixedVar, r)
	return err
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
