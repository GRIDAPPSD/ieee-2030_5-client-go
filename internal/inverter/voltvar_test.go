package inverter

import (
	"math"
	"testing"
)

func TestEvaluateCurveAtVertices(t *testing.T) {
	curve := VoltVarDefaultCurve()

	tests := []struct {
		x    float64
		want float64
	}{
		{0.92, 1.0},   // V1
		{0.98, 0.0},   // V2
		{1.02, 0.0},   // V3
		{1.08, -1.0},  // V4
	}

	for _, tt := range tests {
		got := EvaluateCurve(curve, tt.x)
		if math.Abs(got-tt.want) > 0.001 {
			t.Errorf("EvaluateCurve(%.2f) = %.3f, want %.3f", tt.x, got, tt.want)
		}
	}
}

func TestEvaluateCurveMidpoints(t *testing.T) {
	curve := VoltVarDefaultCurve()

	// Midpoint between V1(0.92,1.0) and V2(0.98,0.0) at 0.95 → 0.5
	got := EvaluateCurve(curve, 0.95)
	if math.Abs(got-0.5) > 0.001 {
		t.Errorf("at 0.95 p.u. got %.3f, want 0.500", got)
	}

	// Midpoint between V3(1.02,0.0) and V4(1.08,-1.0) at 1.05 → -0.5
	got = EvaluateCurve(curve, 1.05)
	if math.Abs(got-(-0.5)) > 0.001 {
		t.Errorf("at 1.05 p.u. got %.3f, want -0.500", got)
	}
}

func TestEvaluateCurveClamp(t *testing.T) {
	curve := VoltVarDefaultCurve()

	// Below first point
	got := EvaluateCurve(curve, 0.80)
	if got != 1.0 {
		t.Errorf("below curve got %.3f, want 1.0", got)
	}

	// Above last point
	got = EvaluateCurve(curve, 1.20)
	if got != -1.0 {
		t.Errorf("above curve got %.3f, want -1.0", got)
	}
}

func TestEvaluateCurveEmpty(t *testing.T) {
	got := EvaluateCurve(nil, 1.0)
	if got != 0 {
		t.Errorf("empty curve got %.3f, want 0", got)
	}
}

func TestVoltWattCurve(t *testing.T) {
	curve := VoltWattDefaultCurve()

	// At 1.06 (V1): full power
	got := EvaluateCurve(curve, 1.06)
	if math.Abs(got-1.0) > 0.001 {
		t.Errorf("at V1 got %.3f, want 1.0", got)
	}

	// At 1.10 (V2): 20% power
	got = EvaluateCurve(curve, 1.10)
	if math.Abs(got-0.2) > 0.001 {
		t.Errorf("at V2 got %.3f, want 0.2", got)
	}

	// At 1.08 (midpoint): 60%
	got = EvaluateCurve(curve, 1.08)
	if math.Abs(got-0.6) > 0.001 {
		t.Errorf("at midpoint got %.3f, want 0.6", got)
	}
}
