package device

import (
	"context"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// TestSyntheticReadState_ScenarioStep asserts that ReadState applies scenario
// steps: after the simulation clock passes a step's AtTime, the returned
// StateReading carries the step's grid values (exact field values, not just
// non-crash).
func TestSyntheticReadState_ScenarioStep(t *testing.T) {
	sc := inverter.Scenario{
		Name:     "step-test",
		Duration: 1 * time.Hour,
		Steps: []inverter.ScenarioStep{
			{AtTime: 0, Grid: &inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0}},
			{AtTime: 10 * time.Minute, Grid: &inverter.GridState{VoltsPU: 1.05, FreqHz: 60.0}},
		},
	}
	// timeScale=600: 1 real second = 10 sim minutes, so one tick crosses
	// the 10-minute boundary.
	s := NewSynthetic(sc, 1*time.Second, 600)

	ctx := context.Background()

	// Tick 1: simTime = 0 + 10min -> exactly reaches step 2 boundary.
	reading, err := s.ReadState(ctx)
	if err != nil {
		t.Fatalf("ReadState tick 1: %v", err)
	}
	if reading.Grid.VoltsPU != 1.05 {
		t.Errorf("tick 1: want Grid.VoltsPU=1.05, got %v", reading.Grid.VoltsPU)
	}
	if reading.Grid.FreqHz != 60.0 {
		t.Errorf("tick 1: want Grid.FreqHz=60.0, got %v", reading.Grid.FreqHz)
	}
}

// TestSyntheticReadState_Irradiance asserts that the Irradiance field on the
// returned StateReading is non-zero during peak solar hours.
func TestSyntheticReadState_Irradiance(t *testing.T) {
	sc := inverter.NormalScenario()
	// timeScale=3600: 1 real second = 1 sim hour. Start is 06:00; after one
	// tick the clock is 07:00, well inside the solar window with non-zero
	// irradiance (sin(pi*1/12) > 0).
	s := NewSynthetic(sc, 1*time.Second, 3600)

	ctx := context.Background()
	reading, err := s.ReadState(ctx)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if reading.Irradiance <= 0 {
		t.Errorf("want Irradiance > 0 at 07:00 sim time, got %v", reading.Irradiance)
	}
	if reading.MaxPowerW <= 0 {
		t.Errorf("want MaxPowerW > 0 at 07:00 sim time, got %v", reading.MaxPowerW)
	}
}

// TestSyntheticApplySetpoint_Disconnect asserts that ApplySetpoint returns
// InverterState with ActivePowerW=0 and Connected=false when the control
// orders a disconnect. Exact field values, not just non-crash.
func TestSyntheticApplySetpoint_Disconnect(t *testing.T) {
	sc := inverter.NormalScenario()
	s := NewSynthetic(sc, 1*time.Second, 1)
	ctx := context.Background()

	controls := inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: 0,
		Connected:        false,
		Energized:        false,
		Mode:             inverter.ModeDisconnected,
	}
	state, err := s.ApplySetpoint(ctx, controls)
	if err != nil {
		t.Fatalf("ApplySetpoint: %v", err)
	}
	if state.ActivePowerW != 0 {
		t.Errorf("disconnect: want ActivePowerW=0, got %v", state.ActivePowerW)
	}
	if state.ReactivePowerVAr != 0 {
		t.Errorf("disconnect: want ReactivePowerVAr=0, got %v", state.ReactivePowerVAr)
	}
	if state.Connected {
		t.Errorf("disconnect: want Connected=false, got true")
	}
	if state.Energized {
		t.Errorf("disconnect: want Energized=false, got true")
	}
}

// TestSyntheticGolden_Normal is the no-behavior-change gate for the normal
// scenario. It runs a fixed number of ticks through the Synthetic backend and
// asserts the resulting InverterState sequence equals the pre-refactor output
// computed directly from ComputeOutput (identical math, different call site).
func TestSyntheticGolden_Normal(t *testing.T) {
	sc := inverter.NormalScenario()
	// timeScale=3600: 1 real second = 1 sim hour, so 4 ticks cover 4 sim
	// hours of the sunny-day curve.
	const timeScale = 3600.0
	const tick = time.Second
	s := NewSynthetic(sc, tick, timeScale)
	ctx := context.Background()

	// Build the pre-refactor reference sequence by driving the same math
	// directly (the old inline loop, extracted). Base is nil -> ConstantPF.
	simStart := time.Date(2024, 6, 21, 6, 0, 0, 0, time.UTC)
	refGrid := inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0, Time: simStart}
	refSimTime := simStart
	const nTicks = 4

	type tickResult struct {
		state     inverter.InverterState
		maxPowerW float64
		irr       float64
	}
	refs := make([]tickResult, nTicks)
	for i := range nTicks {
		simDelta := time.Duration(float64(tick) * timeScale)
		refSimTime = refSimTime.Add(simDelta)
		refGrid.Time = refSimTime
		irr := inverter.Irradiance(refSimTime)
		maxP := inverter.MaxPowerW(irr)
		controls := inverter.ApplyControlsWithCurves(nil, refGrid, maxP, nil)
		refs[i] = tickResult{
			state:     inverter.ComputeOutput(controls, refGrid),
			maxPowerW: maxP,
			irr:       irr,
		}
	}

	// Now drive the same ticks through the Synthetic backend and compare.
	for i := range nTicks {
		reading, err := s.ReadState(ctx)
		if err != nil {
			t.Fatalf("tick %d ReadState: %v", i, err)
		}
		controls := inverter.ApplyControlsWithCurves(nil, reading.Grid, reading.MaxPowerW, nil)
		state, err := s.ApplySetpoint(ctx, controls)
		if err != nil {
			t.Fatalf("tick %d ApplySetpoint: %v", i, err)
		}

		ref := refs[i]
		if state.ActivePowerW != ref.state.ActivePowerW {
			t.Errorf("tick %d: ActivePowerW: want %.4f, got %.4f", i, ref.state.ActivePowerW, state.ActivePowerW)
		}
		if state.ReactivePowerVAr != ref.state.ReactivePowerVAr {
			t.Errorf("tick %d: ReactivePowerVAr: want %.4f, got %.4f", i, ref.state.ReactivePowerVAr, state.ReactivePowerVAr)
		}
		if state.Connected != ref.state.Connected {
			t.Errorf("tick %d: Connected: want %v, got %v", i, ref.state.Connected, state.Connected)
		}
		if state.Energized != ref.state.Energized {
			t.Errorf("tick %d: Energized: want %v, got %v", i, ref.state.Energized, state.Energized)
		}
		if state.Mode != ref.state.Mode {
			t.Errorf("tick %d: Mode: want %v, got %v", i, ref.state.Mode, state.Mode)
		}
		if reading.MaxPowerW != ref.maxPowerW {
			t.Errorf("tick %d: MaxPowerW: want %.4f, got %.4f", i, ref.maxPowerW, reading.MaxPowerW)
		}
		if reading.Irradiance != ref.irr {
			t.Errorf("tick %d: Irradiance: want %.4f, got %.4f", i, ref.irr, reading.Irradiance)
		}
	}
}

// TestSyntheticGolden_VoltVar is the no-behavior-change gate for the voltvar
// scenario. Runs through all scenario steps and compares field-by-field.
func TestSyntheticGolden_VoltVar(t *testing.T) {
	sc := inverter.VoltVarScenario()
	// timeScale=600: 1 real second = 10 sim minutes; 6 ticks cover 1 sim
	// hour (the full voltvar scenario duration).
	const timeScale = 600.0
	const tick = time.Second
	s := NewSynthetic(sc, tick, timeScale)
	ctx := context.Background()

	simStart := time.Date(2024, 6, 21, 6, 0, 0, 0, time.UTC)
	refSimTime := simStart
	refStepIdx := 0
	refGrid := inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0, Time: simStart}
	nTicks := int(sc.Duration / time.Duration(float64(tick)*timeScale))

	for i := range nTicks {
		simDelta := time.Duration(float64(tick) * timeScale)
		refSimTime = refSimTime.Add(simDelta)
		refGrid.Time = refSimTime
		elapsed := refSimTime.Sub(simStart)
		for refStepIdx < len(sc.Steps) && elapsed >= sc.Steps[refStepIdx].AtTime {
			if sc.Steps[refStepIdx].Grid != nil {
				refGrid.VoltsPU = sc.Steps[refStepIdx].Grid.VoltsPU
				refGrid.FreqHz = sc.Steps[refStepIdx].Grid.FreqHz
			}
			refStepIdx++
		}
		irr := inverter.Irradiance(refSimTime)
		maxP := inverter.MaxPowerW(irr)
		controls := inverter.ApplyControlsWithCurves(nil, refGrid, maxP, nil)
		refState := inverter.ComputeOutput(controls, refGrid)

		reading, err := s.ReadState(ctx)
		if err != nil {
			t.Fatalf("tick %d ReadState: %v", i, err)
		}
		controls2 := inverter.ApplyControlsWithCurves(nil, reading.Grid, reading.MaxPowerW, nil)
		state, err := s.ApplySetpoint(ctx, controls2)
		if err != nil {
			t.Fatalf("tick %d ApplySetpoint: %v", i, err)
		}

		if state.ActivePowerW != refState.ActivePowerW {
			t.Errorf("tick %d: ActivePowerW: want %.4f, got %.4f", i, refState.ActivePowerW, state.ActivePowerW)
		}
		if state.ReactivePowerVAr != refState.ReactivePowerVAr {
			t.Errorf("tick %d: ReactivePowerVAr: want %.4f, got %.4f", i, refState.ReactivePowerVAr, state.ReactivePowerVAr)
		}
		if reading.Grid.VoltsPU != refGrid.VoltsPU {
			t.Errorf("tick %d: Grid.VoltsPU: want %v, got %v", i, refGrid.VoltsPU, reading.Grid.VoltsPU)
		}
	}
}
