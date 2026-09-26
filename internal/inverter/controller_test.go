package inverter

import (
	"math"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

func TestApplyControlsNil(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	out := ApplyControls(nil, grid, 8000, Rating.RatedW)

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

	out := ApplyControls(base, grid, 8000, Rating.RatedW)

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

	out := ApplyControls(base, grid, 20000, 20000)

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

	out := ApplyControls(base, grid, 20000, 20000)

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

	out := ApplyControls(base, grid, 40000, 40000)

	if out.ActivePowerW != 30000 {
		t.Errorf("P = %.0f, want 30000 (Value=30 * 10^Multiplier=3; ignoring Multiplier would read 30)", out.ActivePowerW)
	}
}

func TestApplyControlsVoltVar(t *testing.T) {
	voltVar := int32(0) // enable volt-var
	base := &sep2.DERControlBase{OpModVoltVar: &voltVar}
	grid := GridState{VoltsPU: 1.05, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000, Rating.RatedW)

	if out.Mode != ModeVoltVar {
		t.Errorf("mode = %v, want VoltVar", out.Mode)
	}
	// At 1.05 p.u., between V3(1.02,0) and V4(1.08,-1.0): Q ~ -0.5 * 4400 = -2200
	expectedQ := -0.5 * Rating.RatedVAr
	if math.Abs(out.ReactivePowerVAr-expectedQ) > 50 {
		t.Errorf("Q = %.0f, want ~%.0f (volt-var at 1.05 p.u.)", out.ReactivePowerVAr, expectedQ)
	}
}

func TestApplyControlsConstantQ(t *testing.T) {
	fixedQ := sep2.ReactivePower{Value: 2000}
	base := &sep2.DERControlBase{OpModFixedVar: &fixedQ}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000, Rating.RatedW)

	if out.Mode != ModeConstantQ {
		t.Errorf("mode = %v, want ConstantQ", out.Mode)
	}
	if out.ReactivePowerVAr != 2000 {
		t.Errorf("Q = %.0f, want 2000", out.ReactivePowerVAr)
	}
}

func TestApplyControlsPriorityDisconnectOverAll(t *testing.T) {
	connected := false
	maxWPct := sep2.PerCent(5000)
	voltVar := int32(0)
	base := &sep2.DERControlBase{
		OpModConnect: &connected,
		OpModMaxLimW: &maxWPct,
		OpModVoltVar: &voltVar,
	}
	grid := GridState{VoltsPU: 1.05, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000, Rating.RatedW)

	// Disconnect has highest priority : all others ignored
	if out.Connected {
		t.Error("disconnect should override all")
	}
	if out.ActivePowerW != 0 {
		t.Error("disconnected P should be 0")
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
