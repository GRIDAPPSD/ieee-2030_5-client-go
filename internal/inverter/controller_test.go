package inverter

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

func TestApplyControlsNil(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	out := ApplyControls(nil, grid, 8000, Ratings{SetMaxW: RatedW(Rating.RatedW)})

	if out.ActivePowerW != 8000 {
		t.Errorf("nil controls P = %.0f, want 8000", out.ActivePowerW)
	}
	if out.Mode != ModeConstantPF {
		t.Errorf("nil controls mode = %v, want ConstantPF", out.Mode)
	}
}

func TestApplyControlsDisconnect(t *testing.T) {
	connected := false
	base := &sep2.DERControlBase{OpModConnect: &connected}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000, Ratings{SetMaxW: RatedW(Rating.RatedW)})

	if out.Connected {
		t.Error("should be disconnected")
	}
	if out.ActivePowerW != 0 {
		t.Errorf("disconnected P = %.0f, want 0", out.ActivePowerW)
	}
	if out.Mode != ModeDisconnected {
		t.Errorf("mode = %v, want Disconnected", out.Mode)
	}
}

// TestApplyControlsMaxLimW reproduces the security lane's own numbers: a
// 20 kW nameplate makes the PerCent-of-ratedW math diverge from a raw
// hundredths-of-percent-as-watts cast (both read 10000 at the coincidental
// 10 kW default, which is why no prior test caught the regression).
func TestApplyControlsMaxLimW(t *testing.T) {
	maxW := sep2.PerCent(5000) // 50%
	base := &sep2.DERControlBase{OpModMaxLimW: &maxW}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 20000, Ratings{SetMaxW: 20000})

	if out.ActivePowerW != 10000 {
		t.Errorf("P = %.0f, want 10000 (50%% of 20000 W ratedW; a raw cast of the wire value would read 5000)", out.ActivePowerW)
	}
}

// TestApplyControlsFixedW mirrors TestApplyControlsMaxLimW for OpModFixedW,
// again at a nameplate where percent and watts diverge.
func TestApplyControlsFixedW(t *testing.T) {
	fixedW := sep2.SignedPerCent(2500) // 25%
	base := &sep2.DERControlBase{OpModFixedW: &fixedW}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 20000, Ratings{SetMaxW: 20000})

	if out.ActivePowerW != 5000 {
		t.Errorf("P = %.0f, want 5000 (25%% of 20000 W ratedW; a raw cast of the wire value would read 2500)", out.ActivePowerW)
	}
}

// TestApplyControlsTargetW pins OpModTargetW's ActivePower Multiplier
// scaling (IEEE 2030.5-2018 Annex B.2.3.4: watts = Value * 10^Multiplier),
// using the security lane's own probe value.
func TestApplyControlsTargetW(t *testing.T) {
	targetW := sep2.ActivePower{Value: 30, Multiplier: 3} // 30 * 10^3 = 30000 W
	base := &sep2.DERControlBase{OpModTargetW: &targetW}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 40000, Ratings{SetMaxW: 40000})

	if out.ActivePowerW != 30000 {
		t.Errorf("P = %.0f, want 30000 (Value=30 * 10^Multiplier=3; ignoring Multiplier would read 30)", out.ActivePowerW)
	}
}

func TestApplyControlsVoltVar(t *testing.T) {
	base := &sep2.DERControlBase{OpModVoltVar: &sep2.DERCurveLink{Href: "/dc/1"}}
	grid := GridState{VoltsPU: 1.05, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000, Ratings{SetMaxW: RatedW(Rating.RatedW)})

	if out.Mode != ModeVoltVar {
		t.Errorf("mode = %v, want VoltVar", out.Mode)
	}
	// At 1.05 p.u., between V3(1.02,0) and V4(1.08,-1.0): Q ~ -0.5 * 4400 = -2200
	expectedQ := -0.5 * Rating.RatedVAr
	if math.Abs(out.ReactivePowerVAr-expectedQ) > 50 {
		t.Errorf("Q = %.0f, want ~%.0f (volt-var at 1.05 p.u.)", out.ReactivePowerVAr, expectedQ)
	}
}

// TestApplyControlsFixedVar pins opModFixedVar scaling: Value is a
// SignedPerCent (hundredths of a percent) of the quantity refType names.
// The three bases differ from each other and from Value, so reading Value
// as VAr, or scaling against the wrong basis, fails.
func TestApplyControlsFixedVar(t *testing.T) {
	statVarAvail := 3000.0
	zero := 0.0
	ratings := Ratings{SetMaxW: 20000, SetMaxVar: 8000, StatVarAvail: &statVarAvail}
	tests := []struct {
		name    string
		fv      sep2.FixedVar
		ratings Ratings
		wantQ   float64
	}{
		{"refType 1 is percent of setMaxW", sep2.FixedVar{RefType: 1, Value: 5000}, ratings, 10000},
		{"refType 2 is percent of setMaxVar", sep2.FixedVar{RefType: 2, Value: 5000}, ratings, 4000},
		{"refType 3 is percent of statVarAvail", sep2.FixedVar{RefType: 3, Value: 5000}, ratings, 1500},
		{"negative value absorbs", sep2.FixedVar{RefType: 2, Value: -2500}, ratings, -2000},
		{"full scale is the whole basis", sep2.FixedVar{RefType: 2, Value: 10000}, ratings, 8000},
		{"zero statVarAvail is a real reading", sep2.FixedVar{RefType: 3, Value: 5000}, Ratings{SetMaxW: 20000, StatVarAvail: &zero}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fv := tt.fv
			base := &sep2.DERControlBase{OpModFixedVar: &fv}
			out := ApplyControls(base, GridState{VoltsPU: 1.0, FreqHz: 60.0}, 8000, tt.ratings)
			if out.Refused != nil {
				t.Fatalf("Refused = %v, want nil", out.Refused)
			}
			if out.Mode != ModeConstantQ {
				t.Errorf("mode = %v, want ConstantQ", out.Mode)
			}
			if out.ReactivePowerVAr != tt.wantQ {
				t.Errorf("Q = %g VAr, want %g (refType %d, value %d)", out.ReactivePowerVAr, tt.wantQ, tt.fv.RefType, tt.fv.Value)
			}
			if out.ActivePowerW != 8000 {
				t.Errorf("P = %g, want 8000 (fixed var must not touch active power)", out.ActivePowerW)
			}
		})
	}
}

// TestApplyControlsFixedVarRefused: a base whose fixed var cannot be honored
// is refused whole. Nothing in it is applied, including its other modes, so
// the caller falls back to another control.
func TestApplyControlsFixedVarRefused(t *testing.T) {
	negative := -1.0
	tests := []struct {
		name    string
		fv      sep2.FixedVar
		ratings Ratings
		reason  string
	}{
		{"refType 1 with no setMaxW", sep2.FixedVar{RefType: 1, Value: 5000}, Ratings{SetMaxVar: 8000}, "setMaxW"},
		{"refType 2 with no setMaxVar", sep2.FixedVar{RefType: 2, Value: 5000}, Ratings{SetMaxW: 20000}, "setMaxVar"},
		{"refType 3 with no statVarAvail", sep2.FixedVar{RefType: 3, Value: 5000}, Ratings{SetMaxW: 20000, SetMaxVar: 8000}, "statVarAvail"},
		{"refType 3 with a negative statVarAvail", sep2.FixedVar{RefType: 3, Value: 5000}, Ratings{SetMaxW: 20000, SetMaxVar: 8000, StatVarAvail: &negative}, "statVarAvail"},
		{"refType 0", sep2.FixedVar{RefType: 0, Value: 5000}, Ratings{SetMaxW: 20000, SetMaxVar: 8000}, "refType 0"},
		{"refType 4", sep2.FixedVar{RefType: 4, Value: 5000}, Ratings{SetMaxW: 20000, SetMaxVar: 8000}, "refType 4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fv := tt.fv
			fixedW := sep2.SignedPerCent(2500)
			inject := sep2.FixedPowerFactor{Displacement: 900}
			base := &sep2.DERControlBase{OpModFixedVar: &fv, OpModFixedW: &fixedW, OpModFixedPFInjectW: &inject}
			out := ApplyControls(base, GridState{VoltsPU: 1.0, FreqHz: 60.0}, 8000, tt.ratings)
			if !errors.Is(out.Refused, ErrFixedVarRefused) {
				t.Fatalf("Refused = %v, want ErrFixedVarRefused", out.Refused)
			}
			if !strings.Contains(out.Refused.Error(), tt.reason) {
				t.Errorf("Refused = %q, want it to name %q", out.Refused, tt.reason)
			}
			if out != (ControlOutputs{Refused: out.Refused}) {
				t.Errorf("refused base produced outputs %+v; want none applied", out)
			}
		})
	}
}

// TestApplyControlsFreqDroopKOF pins the kOF scale: thousandths of a
// per-unit frequency change per per-unit power change, so 50 is 5% droop.
// At 61.5 Hz a 5% droop on the 10 kW rating sheds 5000 W of 8000 W.
func TestApplyControlsFreqDroopKOF(t *testing.T) {
	tests := []struct {
		name  string
		kOF   uint16
		wantP float64
	}{
		{"kOF 50 is 5 percent", 50, 3000},
		{"kOF 100 is 10 percent", 100, 5500},
		{"kOF 0 uses the default droop", 0, 3000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &sep2.DERControlBase{OpModFreqDroop: &sep2.FreqDroopType{KOF: tt.kOF}}
			out := ApplyControls(base, GridState{VoltsPU: 1.0, FreqHz: 61.5}, 8000, Ratings{SetMaxW: RatedW(Rating.RatedW)})
			if math.Abs(out.ActivePowerW-tt.wantP) > 1e-6 {
				t.Errorf("P = %g, want %g", out.ActivePowerW, tt.wantP)
			}
		})
	}
}

func TestTanFromPF(t *testing.T) {
	// PF = 0.9, tan(acos(0.9)) ~ 0.4843
	got := tanFromPF(0.9)
	if math.Abs(got-0.4843) > 0.001 {
		t.Errorf("tanFromPF(0.9) = %.4f, want ~0.4843", got)
	}

	// PF = 1.0 -> 0
	got = tanFromPF(1.0)
	if got != 0 {
		t.Errorf("tanFromPF(1.0) = %.4f, want 0", got)
	}
}
