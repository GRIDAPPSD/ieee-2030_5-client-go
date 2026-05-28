package inverter

import "sort"

// EvaluateCurve performs piecewise linear interpolation on a set of curve points.
// Below the first point's X, returns the first Y. Above the last X, returns the last Y.
// Points must be sorted by X (ascending).
func EvaluateCurve(points []CurvePoint, x float64) float64 {
	if len(points) == 0 {
		return 0
	}

	// Clamp below first point
	if x <= points[0].X {
		return points[0].Y
	}
	// Clamp above last point
	if x >= points[len(points)-1].X {
		return points[len(points)-1].Y
	}

	// Find the segment and interpolate
	idx := sort.Search(len(points), func(i int) bool {
		return points[i].X > x
	})

	p0 := points[idx-1]
	p1 := points[idx]

	if p1.X == p0.X {
		return p0.Y
	}

	t := (x - p0.X) / (p1.X - p0.X)
	return p0.Y + t*(p1.Y-p0.Y)
}

// VoltVarDefaultCurve returns the IEEE 1547 Table 8 default volt-var curve
// for Category B. X = voltage (p.u.), Y = reactive power (fraction of rated VAr).
// Positive Y = over-excited (injecting vars), negative = under-excited (absorbing).
func VoltVarDefaultCurve() []CurvePoint {
	return []CurvePoint{
		{X: 0.92, Y: 1.0},  // V1: full var injection (over-excited)
		{X: 0.98, Y: 0.0},  // V2: deadband start
		{X: 1.02, Y: 0.0},  // V3: deadband end
		{X: 1.08, Y: -1.0}, // V4: full var absorption (under-excited)
	}
}

// VoltWattDefaultCurve returns the IEEE 1547 Table 10 default volt-watt curve
// for Category B. X = voltage (p.u.), Y = active power (fraction of rated W).
func VoltWattDefaultCurve() []CurvePoint {
	return []CurvePoint{
		{X: 1.06, Y: 1.0}, // V1: rated power
		{X: 1.10, Y: 0.2}, // V2: minimum power (20%)
	}
}

// WattVarDefaultCurve returns the IEEE 1547 Table 9 default watt-var curve.
// X = active power (fraction of rated W), Y = reactive power (fraction of rated VAr).
func WattVarDefaultCurve() []CurvePoint {
	return []CurvePoint{
		{X: 0.2, Y: 0.0},   // P1: no vars below 20% output
		{X: 0.5, Y: 0.0},   // P2: deadband
		{X: 1.0, Y: -0.44}, // P3: 44% absorption at rated (Category B)
	}
}
