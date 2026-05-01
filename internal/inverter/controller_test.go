package inverter

import (
	"math"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

func TestApplyControlsNil(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	out := ApplyControls(nil, grid, 8000)

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

	out := ApplyControls(base, grid, 8000)

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

func TestApplyControlsMaxLimW(t *testing.T) {
	maxW := sep2.ActivePower{Value: 5000}
	base := &sep2.DERControlBase{OpModMaxLimW: &maxW}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000)

	if out.ActivePowerW != 5000 {
		t.Errorf("P = %.0f, want 5000 (limited)", out.ActivePowerW)
	}
}

func TestApplyControlsFixedW(t *testing.T) {
	fixedW := sep2.ActivePower{Value: 3000}
	base := &sep2.DERControlBase{OpModFixedW: &fixedW}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000)

	if out.ActivePowerW != 3000 {
		t.Errorf("P = %.0f, want 3000", out.ActivePowerW)
	}
}

func TestApplyControlsVoltVar(t *testing.T) {
	voltVar := int32(0) // enable volt-var
	base := &sep2.DERControlBase{OpModVoltVar: &voltVar}
	grid := GridState{VoltsPU: 1.05, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000)

	if out.Mode != ModeVoltVar {
		t.Errorf("mode = %v, want VoltVar", out.Mode)
	}
	// At 1.05 p.u., between V3(1.02,0) and V4(1.08,-1.0): Q ≈ -0.5 * 4400 = -2200
	expectedQ := -0.5 * Rating.RatedVAr
	if math.Abs(out.ReactivePowerVAr-expectedQ) > 50 {
		t.Errorf("Q = %.0f, want ~%.0f (volt-var at 1.05 p.u.)", out.ReactivePowerVAr, expectedQ)
	}
}

func TestApplyControlsConstantQ(t *testing.T) {
	fixedQ := sep2.ReactivePower{Value: 2000}
	base := &sep2.DERControlBase{OpModFixedVar: &fixedQ}
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000)

	if out.Mode != ModeConstantQ {
		t.Errorf("mode = %v, want ConstantQ", out.Mode)
	}
	if out.ReactivePowerVAr != 2000 {
		t.Errorf("Q = %.0f, want 2000", out.ReactivePowerVAr)
	}
}

func TestApplyControlsPriorityDisconnectOverAll(t *testing.T) {
	connected := false
	maxW := sep2.ActivePower{Value: 5000}
	voltVar := int32(0)
	base := &sep2.DERControlBase{
		OpModConnect: &connected,
		OpModMaxLimW: &maxW,
		OpModVoltVar: &voltVar,
	}
	grid := GridState{VoltsPU: 1.05, FreqHz: 60.0}

	out := ApplyControls(base, grid, 8000)

	// Disconnect has highest priority — all others ignored
	if out.Connected {
		t.Error("disconnect should override all")
	}
	if out.ActivePowerW != 0 {
		t.Error("disconnected P should be 0")
	}
}

func TestTanFromPF(t *testing.T) {
	// PF = 0.9, tan(acos(0.9)) ≈ 0.4843
	got := tanFromPF(0.9)
	if math.Abs(got-0.4843) > 0.001 {
		t.Errorf("tanFromPF(0.9) = %.4f, want ~0.4843", got)
	}

	// PF = 1.0 → 0
	got = tanFromPF(1.0)
	if got != 0 {
		t.Errorf("tanFromPF(1.0) = %.4f, want 0", got)
	}
}
