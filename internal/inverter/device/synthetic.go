package device

import (
	"context"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// Synthetic is the DERDevice backend backed by the scenario harness. It
// produces physical state by walking scenario steps and computing irradiance,
// then applies setpoints via ComputeOutput math. No I/O; context is accepted
// for interface conformance but not checked.
//
// This is the existing tick-loop logic extracted verbatim: no behavior change.
type Synthetic struct {
	scenario  inverter.Scenario
	simStart  time.Time
	simTime   time.Time
	stepIdx   int
	grid      inverter.GridState
	timeScale float64
	tick      time.Duration
}

// NewSynthetic constructs a Synthetic backend.
// sc is the scenario to walk; tickInterval and timeScale match the SimConfig
// fields used by the tick loop.
func NewSynthetic(sc inverter.Scenario, tickInterval time.Duration, timeScale float64) *Synthetic {
	start := time.Date(2024, 6, 21, 6, 0, 0, 0, time.UTC) // sunrise, matches main.go line 893
	return &Synthetic{
		scenario:  sc,
		simStart:  start,
		simTime:   start,
		grid:      inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0, Time: start},
		timeScale: timeScale,
		tick:      tickInterval,
	}
}

// ReadState advances simulation time by one tick (using the configured
// timeScale), walks scenario steps, computes irradiance and MaxPowerW, and
// returns the current StateReading. This is the extracted equivalent of
// main.go lines 917-942.
func (s *Synthetic) ReadState(_ context.Context) (StateReading, error) {
	// Advance sim time exactly as the original tick loop did.
	simDelta := time.Duration(float64(s.tick) * s.timeScale)
	s.simTime = s.simTime.Add(simDelta)
	s.grid.Time = s.simTime

	// Walk scenario steps.
	elapsed := s.simTime.Sub(s.simStart)
	for s.stepIdx < len(s.scenario.Steps) && elapsed >= s.scenario.Steps[s.stepIdx].AtTime {
		step := s.scenario.Steps[s.stepIdx]
		if step.Grid != nil {
			s.grid.VoltsPU = step.Grid.VoltsPU
			s.grid.FreqHz = step.Grid.FreqHz
		}
		s.stepIdx++
	}

	irr := inverter.Irradiance(s.simTime)
	maxP := inverter.MaxPowerW(irr)

	return StateReading{
		Grid:       s.grid,
		MaxPowerW:  maxP,
		Irradiance: irr,
	}, nil
}

// ApplySetpoint applies the commanded control output via ComputeOutput and
// returns the resulting InverterState. This is the extracted equivalent of
// main.go line 958: state := inverter.ComputeOutput(controls, currentGrid).
func (s *Synthetic) ApplySetpoint(_ context.Context, controls inverter.ControlOutputs) (inverter.InverterState, error) {
	return inverter.ComputeOutput(controls, s.grid), nil
}

// SimTime returns the current simulation time. Exposed for tests that need
// to assert scenario step transitions.
func (s *Synthetic) SimTime() time.Time {
	return s.simTime
}

// ScenarioDone reports whether the scenario duration has been exceeded.
// The tick loop can use this to break out of the simulation.
func (s *Synthetic) ScenarioDone() bool {
	return s.simTime.Sub(s.simStart) >= s.scenario.Duration
}
