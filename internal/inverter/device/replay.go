package device

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

const minutesPerDay = 1440

// recorderTimeLayout is the timestamp GridLAB-D's recorder writes
// ("2020-01-01 12:00:00 UTC").
const recorderTimeLayout = "2006-01-02 15:04:05 MST"

// ReplayConfig configures the replay backend.
type ReplayConfig struct {
	File string
	// Type is "pv" or "battery". A PV file holds the inverter's P_Out in
	// the DER sign (positive delivering). A battery file holds the
	// recorded battery_load, which GridLAB-D signs positive for charging.
	Type string
	// Clock is "wall" (minute of day, UTC) or "start" (minutes since the
	// backend was built, looping daily).
	Clock      string
	Scale      float64
	RatedW     float64
	CapacityWh float64
	InitialSOC float64
	// Now is the time source; nil means time.Now.
	Now func() time.Time
}

// Replay is the DERDevice backend that reports recorded GridLAB-D output.
// The recording is the baseline: for PV the available ceiling, for a
// battery the power it would run at with no command. ApplySetpoint turns
// what the controller commands into the achieved output.
//
// A Replay is used from the single tick loop and is not safe for
// concurrent use.
type Replay struct {
	cfg   ReplayConfig
	rows  [minutesPerDay]float64
	start time.Time

	grid  inverter.GridState
	soc   float64
	lastT time.Time
	lastP float64
}

// NewReplay loads cfg.File and validates cfg. The file is read in
// GridLAB-D's recorder format: lines starting with '#' are skipped and
// every other line is "<timestamp>,<value>". Each minute of the day must
// appear exactly once, so a recording that does not cover a day is refused
// at start instead of reporting zeros for the gap.
func NewReplay(cfg ReplayConfig) (*Replay, error) {
	if cfg.File == "" {
		return nil, errors.New("replay backend needs a recording file")
	}
	if cfg.Type != "pv" && cfg.Type != "battery" {
		return nil, fmt.Errorf("replay device type %q must be pv or battery", cfg.Type)
	}
	if cfg.Clock != "wall" && cfg.Clock != "start" {
		return nil, fmt.Errorf("replay clock %q must be wall or start", cfg.Clock)
	}
	if cfg.Scale < 0 || math.IsNaN(cfg.Scale) || math.IsInf(cfg.Scale, 0) {
		return nil, fmt.Errorf("replay scale %v must be a non-negative number", cfg.Scale)
	}
	if cfg.RatedW <= 0 {
		return nil, fmt.Errorf("replay rated power %v W must be positive", cfg.RatedW)
	}
	if cfg.Type == "battery" {
		if cfg.CapacityWh <= 0 {
			return nil, fmt.Errorf("battery capacity %v Wh must be positive", cfg.CapacityWh)
		}
		if cfg.InitialSOC < 0 || cfg.InitialSOC > 1 {
			return nil, fmt.Errorf("battery initial state of charge %v must be within 0 to 1", cfg.InitialSOC)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Replay{cfg: cfg, soc: cfg.InitialSOC}
	if err := r.load(); err != nil {
		return nil, err
	}
	r.start = cfg.Now()
	return r, nil
}

func (r *Replay) load() error {
	f, err := os.Open(r.cfg.File)
	if err != nil {
		return fmt.Errorf("replay file: %w", err)
	}
	defer f.Close()

	var seen [minutesPerDay]bool
	rows := 0
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ',')
		if i < 0 {
			return fmt.Errorf("replay file %s line %d: want \"<timestamp>,<value>\", got %q", r.cfg.File, n, line)
		}
		ts, err := time.Parse(recorderTimeLayout, strings.TrimSpace(line[:i]))
		if err != nil {
			return fmt.Errorf("replay file %s line %d: bad timestamp %q: %w", r.cfg.File, n, line[:i], err)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("replay file %s line %d: bad value %q", r.cfg.File, n, line[i+1:])
		}
		m := ts.Hour()*60 + ts.Minute()
		if seen[m] {
			return fmt.Errorf("replay file %s line %d: minute %02d:%02d appears twice", r.cfg.File, n, m/60, m%60)
		}
		seen[m] = true
		r.rows[m] = v
		rows++
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("replay file %s: %w", r.cfg.File, err)
	}
	if rows == 0 {
		return fmt.Errorf("replay file %s: no data rows", r.cfg.File)
	}
	for m, ok := range seen {
		if !ok {
			return fmt.Errorf("replay file %s: has %d rows, want 1440 (one per minute of the day); first missing minute is %02d:%02d", r.cfg.File, rows, m/60, m%60)
		}
	}
	return nil
}

// minute returns the row index for now.
func (r *Replay) minute(now time.Time) int {
	if r.cfg.Clock == "start" {
		m := int(now.Sub(r.start) / time.Minute)
		return ((m % minutesPerDay) + minutesPerDay) % minutesPerDay
	}
	u := now.UTC()
	return u.Hour()*60 + u.Minute()
}

// baseline is the recorded power for now in the DER sign (positive
// delivering), scaled.
func (r *Replay) baseline(now time.Time) float64 {
	v := r.rows[r.minute(now)] * r.cfg.Scale
	if r.cfg.Type == "battery" {
		// GridLAB-D's battery_load is positive while charging.
		return -v
	}
	return v
}

// ReadState reports 1.0 pu and 60 Hz and the recorded baseline as the
// available power; the controller starts from it and a command replaces or
// lowers it.
func (r *Replay) ReadState(_ context.Context) (StateReading, error) {
	now := r.cfg.Now()
	r.grid = inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0, Time: now}
	return StateReading{Grid: r.grid, MaxPowerW: r.baseline(now)}, nil
}

// SOC is the battery's current state of charge, 0 to 1. For PV it is the
// configured initial value and never changes.
func (r *Replay) SOC() float64 { return r.soc }

// ApplySetpoint turns the commanded power into the achieved output. PV
// delivers between 0 and the recorded ceiling. A battery follows the
// command within its rating, except that it cannot discharge when empty or
// charge when full; its state of charge integrates the reported output.
func (r *Replay) ApplySetpoint(_ context.Context, controls inverter.ControlOutputs) (inverter.InverterState, error) {
	now := r.cfg.Now()
	p := controls.ActivePowerW
	switch r.cfg.Type {
	case "pv":
		p = math.Max(0, math.Min(p, r.baseline(now)))
	case "battery":
		r.integrate(now)
		p = math.Max(-r.cfg.RatedW, math.Min(p, r.cfg.RatedW))
		if (p > 0 && r.soc <= 0) || (p < 0 && r.soc >= 1) {
			p = 0
		}
	}
	controls.ActivePowerW = p
	grid := r.grid
	if grid.Time.IsZero() {
		grid = inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0, Time: now}
	}
	st := inverter.ComputeOutput(controls, grid)
	if r.cfg.Type == "battery" {
		r.lastT, r.lastP = now, st.ActivePowerW
	}
	return st, nil
}

// integrate advances the state of charge by the previous reported output
// over the time since it was reported. Round-trip efficiency is not
// modelled: the recording already carries it in the baseline.
func (r *Replay) integrate(now time.Time) {
	if r.lastT.IsZero() {
		return
	}
	hours := now.Sub(r.lastT).Hours()
	if hours <= 0 {
		return
	}
	r.soc -= r.lastP * hours / r.cfg.CapacityWh
	r.soc = math.Max(0, math.Min(1, r.soc))
}
