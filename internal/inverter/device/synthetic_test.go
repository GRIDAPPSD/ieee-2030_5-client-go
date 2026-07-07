package device

import (
	"context"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
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

// TestSyntheticScenarioDone asserts ScenarioDone is false before the scenario
// duration is exceeded and true after (item 12).
func TestSyntheticScenarioDone(t *testing.T) {
	// Very short scenario: duration=10 minutes, timeScale=600 per second.
	sc := inverter.Scenario{
		Name:     "done-test",
		Duration: 10 * time.Minute,
		Steps:    []inverter.ScenarioStep{{AtTime: 0, Grid: &inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0}}},
	}
	s := NewSynthetic(sc, 1*time.Second, 600) // 1 tick = 10 sim minutes

	ctx := context.Background()

	// Before ticking: not done.
	if s.ScenarioDone() {
		t.Error("ScenarioDone should be false before any tick")
	}

	// After one tick: simTime = start + 10 min = exactly duration -> done.
	if _, err := s.ReadState(ctx); err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if !s.ScenarioDone() {
		t.Error("ScenarioDone should be true after duration is reached")
	}
}

// TestSyntheticSimTime asserts SimTime advances correctly each tick (item 12).
func TestSyntheticSimTime(t *testing.T) {
	sc := inverter.NormalScenario()
	const tick = time.Second
	const timeScale = 3600.0
	s := NewSynthetic(sc, tick, timeScale)
	ctx := context.Background()

	simStart := time.Date(2024, 6, 21, 6, 0, 0, 0, time.UTC)
	expectedAfterTick1 := simStart.Add(time.Duration(float64(tick) * timeScale))

	if _, err := s.ReadState(ctx); err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	got := s.SimTime()
	if !got.Equal(expectedAfterTick1) {
		t.Errorf("SimTime after tick 1: want %v, got %v", expectedAfterTick1, got)
	}
}

// goldenInverterStateFields is a helper that compares two InverterState values
// field by field and reports failures via t.Errorf. Used by all golden tests.
func goldenInverterStateFields(t *testing.T, label string, want, got inverter.InverterState) {
	t.Helper()
	if got.ActivePowerW != want.ActivePowerW {
		t.Errorf("%s ActivePowerW: want %.4f, got %.4f", label, want.ActivePowerW, got.ActivePowerW)
	}
	if got.ReactivePowerVAr != want.ReactivePowerVAr {
		t.Errorf("%s ReactivePowerVAr: want %.4f, got %.4f", label, want.ReactivePowerVAr, got.ReactivePowerVAr)
	}
	if got.PowerFactor != want.PowerFactor {
		t.Errorf("%s PowerFactor: want %.6f, got %.6f", label, want.PowerFactor, got.PowerFactor)
	}
	if got.VoltsPU != want.VoltsPU {
		t.Errorf("%s VoltsPU: want %.4f, got %.4f", label, want.VoltsPU, got.VoltsPU)
	}
	if got.Connected != want.Connected {
		t.Errorf("%s Connected: want %v, got %v", label, want.Connected, got.Connected)
	}
	if got.Energized != want.Energized {
		t.Errorf("%s Energized: want %v, got %v", label, want.Energized, got.Energized)
	}
	if got.Mode != want.Mode {
		t.Errorf("%s Mode: want %v, got %v", label, want.Mode, got.Mode)
	}
}

// runGoldenScenario is the shared harness for all golden scenario tests. It
// drives both the reference (inline math) and the Synthetic backend for nTicks,
// then compares field-by-field.
func runGoldenScenario(t *testing.T, sc inverter.Scenario, tick time.Duration, timeScale float64) {
	t.Helper()
	s := NewSynthetic(sc, tick, timeScale)
	ctx := context.Background()

	simStart := time.Date(2024, 6, 21, 6, 0, 0, 0, time.UTC)
	refSimTime := simStart
	refStepIdx := 0
	refGrid := inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0, Time: simStart}
	nTicks := int(sc.Duration / time.Duration(float64(tick)*timeScale))
	if nTicks == 0 {
		nTicks = 4 // fallback for scenarios shorter than one tick
	}

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

		goldenInverterStateFields(t, "tick "+itoa(i), refState, state)

		if reading.MaxPowerW != maxP {
			t.Errorf("tick %d MaxPowerW: want %.4f, got %.4f", i, maxP, reading.MaxPowerW)
		}
		if reading.Irradiance != irr {
			t.Errorf("tick %d Irradiance: want %.4f, got %.4f", i, irr, reading.Irradiance)
		}
		if reading.Grid.VoltsPU != refGrid.VoltsPU {
			t.Errorf("tick %d Grid.VoltsPU: want %.4f, got %.4f", i, refGrid.VoltsPU, reading.Grid.VoltsPU)
		}
	}
}

// itoa is a tiny helper to avoid importing strconv just for a label.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// TestSyntheticGolden_Normal is the no-behavior-change gate for the normal
// scenario (items 13, 14).
func TestSyntheticGolden_Normal(t *testing.T) {
	runGoldenScenario(t, inverter.NormalScenario(), time.Second, 3600.0)
}

// TestSyntheticGolden_VoltVar is the no-behavior-change gate for the voltvar
// scenario (items 13, 14).
func TestSyntheticGolden_VoltVar(t *testing.T) {
	runGoldenScenario(t, inverter.VoltVarScenario(), time.Second, 600.0)
}

// TestSyntheticGolden_FreqDroop is the no-behavior-change gate for the
// freqdroop scenario, exercising the frequency step path (item 13).
func TestSyntheticGolden_FreqDroop(t *testing.T) {
	runGoldenScenario(t, inverter.FreqDroopScenario(), time.Second, 600.0)
}

// TestSyntheticGolden_VoltageRide is the no-behavior-change gate for the
// voltageride scenario, exercising the trip / ride-through step path (item 13).
func TestSyntheticGolden_VoltageRide(t *testing.T) {
	runGoldenScenario(t, inverter.VoltageRideScenario(), time.Second, 600.0)
}
