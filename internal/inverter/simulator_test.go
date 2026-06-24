package inverter

import (
	"math"
	"testing"
	"time"
)

func TestIrradiance(t *testing.T) {
	tests := []struct {
		hour int
		min  int
		want float64
		tol  float64
	}{
		{0, 0, 0, 0},     // midnight
		{6, 0, 0, 1},     // sunrise (sin(0) = 0)
		{9, 0, 707, 5},   // sin(π/4) ≈ 0.707
		{12, 0, 1000, 1}, // noon peak
		{15, 0, 707, 5},  // afternoon sin(3π/4) ≈ 0.707
		{18, 0, 0, 1},    // sunset
		{21, 0, 0, 0},    // night
		{3, 0, 0, 0},     // night
	}

	for _, tt := range tests {
		tm := time.Date(2024, 6, 21, tt.hour, tt.min, 0, 0, time.UTC)
		got := Irradiance(tm)
		if math.Abs(got-tt.want) > tt.tol {
			t.Errorf("Irradiance(%d:%02d) = %.1f, want %.1f ±%.0f",
				tt.hour, tt.min, got, tt.want, tt.tol)
		}
	}
}

func TestMaxPowerW(t *testing.T) {
	tests := []struct {
		irradiance float64
		want       float64
	}{
		{0, 0},
		{500, 5000},
		{1000, 10000},
		{-100, 0},
	}

	for _, tt := range tests {
		got := MaxPowerW(tt.irradiance)
		if got != tt.want {
			t.Errorf("MaxPowerW(%.0f) = %.0f, want %.0f", tt.irradiance, got, tt.want)
		}
	}
}

func TestEnforceVALimit(t *testing.T) {
	// Within limit
	p, q := EnforceVALimit(8000, 3000)
	s := math.Sqrt(p*p + q*q)
	if s > Rating.RatedVA+1 {
		t.Errorf("S=%.0f exceeds VA rating %.0f", s, Rating.RatedVA)
	}

	// Exceeds limit : Q should be reduced
	p, q = EnforceVALimit(10000, 6000)
	s = math.Sqrt(p*p + q*q)
	if s > Rating.RatedVA+1 {
		t.Errorf("after enforcement S=%.0f exceeds %.0f", s, Rating.RatedVA)
	}
	if p != 10000 {
		t.Errorf("P should stay at 10000, got %.0f", p)
	}

	// P alone exceeds
	p, _ = EnforceVALimit(12000, 0)
	if p > Rating.RatedVA+1 {
		t.Errorf("P=%.0f should be clamped to VA rating", p)
	}
}

func TestComputePowerFactor(t *testing.T) {
	// Unity PF
	pf := ComputePowerFactor(10000, 0)
	if math.Abs(pf-1.0) > 0.001 {
		t.Errorf("PF with Q=0 should be 1.0, got %.3f", pf)
	}

	// Known PF: P=8000, Q=6000, S=10000, PF=0.8
	pf = ComputePowerFactor(8000, 6000)
	if math.Abs(pf-0.8) > 0.001 {
		t.Errorf("PF(8000,6000) = %.3f, want 0.800", pf)
	}

	// Zero output
	pf = ComputePowerFactor(0, 0)
	if pf != 1.0 {
		t.Errorf("PF(0,0) = %.3f, want 1.0", pf)
	}
}

func TestComputeOutputDisconnected(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	controls := ControlOutputs{
		ActivePowerW:     10000,
		ReactivePowerVAr: 1000,
		Connected:        false,
		Energized:        false,
		Mode:             ModeDisconnected,
	}

	state := ComputeOutput(controls, grid)

	if state.ActivePowerW != 0 {
		t.Errorf("disconnected P = %.0f, want 0", state.ActivePowerW)
	}
	if state.ReactivePowerVAr != 0 {
		t.Errorf("disconnected Q = %.0f, want 0", state.ReactivePowerVAr)
	}
	if state.Connected {
		t.Error("should not be connected")
	}
}

func TestComputeOutputNormal(t *testing.T) {
	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	controls := ControlOutputs{
		ActivePowerW:     8000,
		ReactivePowerVAr: 2000,
		Connected:        true,
		Energized:        true,
		Mode:             ModeConstantPF,
	}

	state := ComputeOutput(controls, grid)

	if state.ActivePowerW != 8000 {
		t.Errorf("P = %.0f, want 8000", state.ActivePowerW)
	}
	if state.Mode != ModeConstantPF {
		t.Errorf("mode = %v, want ConstantPF", state.Mode)
	}
}

func TestControlModeString(t *testing.T) {
	if ModeVoltVar.String() != "VoltVar" {
		t.Errorf("VoltVar.String() = %q", ModeVoltVar.String())
	}
	if ModeTripped.String() != "Tripped" {
		t.Errorf("Tripped.String() = %q", ModeTripped.String())
	}
}
