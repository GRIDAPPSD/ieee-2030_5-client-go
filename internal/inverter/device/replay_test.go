package device

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// recorderHeader mimics the eight '#' lines GridLAB-D 5.3.0's recorder
// writes before the data rows.
const recorderHeader = "# file...... out.csv\n# date....... Wed Oct  7 10:00:00 2026\n# user....... u\n# host....... h\n# target..... recorder 1\n# trigger.... (none)\n# interval... 60\n# limit...... 1440\n"

func recordingRows(valueAt func(minute int) float64) string {
	var b strings.Builder
	b.WriteString(recorderHeader)
	for m := 0; m < 1440; m++ {
		fmt.Fprintf(&b, "2020-01-01 %02d:%02d:00 UTC,%+g\n", m/60, m%60, valueAt(m))
	}
	return b.String()
}

func writeRecording(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rec.csv")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// clock is a settable time source for the backend.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func at(h, m int) time.Time { return time.Date(2026, 10, 7, h, m, 0, 0, time.UTC) }

func newReplayAt(t *testing.T, cfg ReplayConfig, c *clock) *Replay {
	t.Helper()
	cfg.Now = c.now
	r, err := NewReplay(cfg)
	if err != nil {
		t.Fatalf("NewReplay: %v", err)
	}
	return r
}

func TestReplayReadsRowForCurrentMinuteOfDay(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(m int) float64 { return float64(m) }))
	c := &clock{at(3, 7)}
	r := newReplayAt(t, ReplayConfig{File: f, Type: "pv", Clock: "wall", Scale: 1, RatedW: 10000}, c)
	got, err := r.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := float64(3*60 + 7); got.MaxPowerW != want {
		t.Fatalf("MaxPowerW at 03:07 = %v, want %v", got.MaxPowerW, want)
	}
	if got.Grid.VoltsPU != 1.0 || got.Grid.FreqHz != 60.0 || !got.Grid.Time.Equal(c.t) {
		t.Fatalf("grid = %+v, want 1.0 pu, 60 Hz, time %v", got.Grid, c.t)
	}
	c.t = at(23, 59)
	got, _ = r.ReadState(context.Background())
	if got.MaxPowerW != 1439 {
		t.Fatalf("MaxPowerW at 23:59 = %v, want 1439", got.MaxPowerW)
	}
}

func TestReplayStartClockCountsMinutesSinceStartAndLoops(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(m int) float64 { return float64(m) }))
	c := &clock{at(15, 30)}
	r := newReplayAt(t, ReplayConfig{File: f, Type: "pv", Clock: "start", Scale: 1, RatedW: 10000}, c)
	for _, tc := range []struct {
		after time.Duration
		want  float64
	}{
		{0, 0}, {5*time.Minute + 30*time.Second, 5}, {24*time.Hour + 2*time.Minute, 2},
	} {
		c.t = at(15, 30).Add(tc.after)
		got, _ := r.ReadState(context.Background())
		if got.MaxPowerW != tc.want {
			t.Errorf("after %v MaxPowerW = %v, want %v", tc.after, got.MaxPowerW, tc.want)
		}
	}
}

func TestReplayScaleMultipliesRecordedPower(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 2000 }))
	r := newReplayAt(t, ReplayConfig{File: f, Type: "pv", Clock: "wall", Scale: 0.5, RatedW: 10000}, &clock{at(12, 0)})
	got, _ := r.ReadState(context.Background())
	if got.MaxPowerW != 1000 {
		t.Fatalf("MaxPowerW = %v, want 1000", got.MaxPowerW)
	}
}

func pvControls(p float64) inverter.ControlOutputs {
	return inverter.ControlOutputs{ActivePowerW: p, Connected: true, Energized: true}
}

func TestReplayPVRecordedPowerIsCeilingAndLimitLowersOutput(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 4000 }))
	r := newReplayAt(t, ReplayConfig{File: f, Type: "pv", Clock: "wall", Scale: 1, RatedW: 10000}, &clock{at(12, 0)})
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cmd  float64
		want float64
	}{
		{"no limit reports the recorded ceiling", 4000, 4000},
		{"a limit below the ceiling lowers output", 1500, 1500},
		{"a target above the ceiling cannot exceed it", 9000, 4000},
		{"a negative command cannot make a PV absorb", -500, 0},
	} {
		if _, err := r.ReadState(ctx); err != nil {
			t.Fatal(err)
		}
		st, err := r.ApplySetpoint(ctx, pvControls(tc.cmd))
		if err != nil {
			t.Fatal(err)
		}
		if st.ActivePowerW != tc.want {
			t.Errorf("%s: output = %v, want %v", tc.name, st.ActivePowerW, tc.want)
		}
	}
}

func TestReplayPVDisconnectedReportsZero(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 4000 }))
	r := newReplayAt(t, ReplayConfig{File: f, Type: "pv", Clock: "wall", Scale: 1, RatedW: 10000}, &clock{at(12, 0)})
	_, _ = r.ReadState(context.Background())
	st, _ := r.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 4000})
	if st.ActivePowerW != 0 || st.Connected {
		t.Fatalf("state = %+v, want 0 W and not connected", st)
	}
}

func batteryCfg(f string, soc float64) ReplayConfig {
	return ReplayConfig{File: f, Type: "battery", Clock: "wall", Scale: 1, RatedW: 5000, CapacityWh: 10000, InitialSOC: soc}
}

func TestReplayBatteryBaselineIsRecordedBatteryLoadInDERSign(t *testing.T) {
	t.Parallel()
	// battery_load +2000 is charging in GridLAB-D; the DER convention reports
	// a charging battery as negative active power.
	f := writeRecording(t, recordingRows(func(int) float64 { return 2000 }))
	r := newReplayAt(t, batteryCfg(f, 0.5), &clock{at(12, 0)})
	got, _ := r.ReadState(context.Background())
	if got.MaxPowerW != -2000 {
		t.Fatalf("baseline = %v, want -2000", got.MaxPowerW)
	}
	st, _ := r.ApplySetpoint(context.Background(), pvControls(got.MaxPowerW))
	if st.ActivePowerW != -2000 {
		t.Fatalf("reported output = %v, want the baseline -2000", st.ActivePowerW)
	}
}

func TestReplayBatterySetpointReplacesBaseline(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 2000 }))
	r := newReplayAt(t, batteryCfg(f, 0.5), &clock{at(12, 0)})
	ctx := context.Background()
	_, _ = r.ReadState(ctx)
	st, _ := r.ApplySetpoint(ctx, pvControls(3000))
	if st.ActivePowerW != 3000 {
		t.Fatalf("discharge setpoint output = %v, want 3000 (baseline was -2000)", st.ActivePowerW)
	}
	st, _ = r.ApplySetpoint(ctx, pvControls(9000))
	if st.ActivePowerW != 5000 {
		t.Fatalf("setpoint above rating output = %v, want the 5000 W rating", st.ActivePowerW)
	}
}

func TestReplayBatteryStopsAtEmptyAndFull(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 0 }))
	ctx := context.Background()

	c := &clock{at(12, 0)}
	empty := newReplayAt(t, batteryCfg(f, 0), c)
	_, _ = empty.ReadState(ctx)
	if st, _ := empty.ApplySetpoint(ctx, pvControls(3000)); st.ActivePowerW != 0 {
		t.Errorf("empty battery discharging reports %v, want 0", st.ActivePowerW)
	}
	if st, _ := empty.ApplySetpoint(ctx, pvControls(-3000)); st.ActivePowerW != -3000 {
		t.Errorf("empty battery charging reports %v, want -3000", st.ActivePowerW)
	}

	full := newReplayAt(t, batteryCfg(f, 1), c)
	_, _ = full.ReadState(ctx)
	if st, _ := full.ApplySetpoint(ctx, pvControls(-3000)); st.ActivePowerW != 0 {
		t.Errorf("full battery charging reports %v, want 0", st.ActivePowerW)
	}
	if st, _ := full.ApplySetpoint(ctx, pvControls(3000)); st.ActivePowerW != 3000 {
		t.Errorf("full battery discharging reports %v, want 3000", st.ActivePowerW)
	}
}

func TestReplayBatteryStateOfChargeIntegratesOutputAndHitsEmpty(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 0 }))
	ctx := context.Background()
	c := &clock{at(12, 0)}
	// 10 kWh, 50 percent: 5 kWh. Discharging 4 kW holds for 1 h (1 kWh left)
	// and cannot continue past 1 h 15 min.
	r := newReplayAt(t, batteryCfg(f, 0.5), c)
	_, _ = r.ReadState(ctx)
	if st, _ := r.ApplySetpoint(ctx, pvControls(4000)); st.ActivePowerW != 4000 {
		t.Fatalf("first output = %v, want 4000", st.ActivePowerW)
	}
	c.t = at(13, 0)
	st, _ := r.ApplySetpoint(ctx, pvControls(4000))
	if want := 0.1; math.Abs(r.SOC()-want) > 1e-9 {
		t.Fatalf("SOC after 1 h at 4 kW = %v, want %v", r.SOC(), want)
	}
	if st.ActivePowerW != 4000 {
		t.Fatalf("output with 1 kWh left = %v, want 4000", st.ActivePowerW)
	}
	c.t = at(14, 0)
	st, _ = r.ApplySetpoint(ctx, pvControls(4000))
	if r.SOC() != 0 || st.ActivePowerW != 0 {
		t.Fatalf("after the 2nd hour SOC = %v output = %v, want empty and 0 W", r.SOC(), st.ActivePowerW)
	}
}

func TestNewReplayRejectsMalformedFileNamingTheLine(t *testing.T) {
	t.Parallel()
	good := recordingRows(func(int) float64 { return 1 })
	lines := strings.Split(good, "\n")
	mutate := func(n int, repl string) string {
		cp := append([]string(nil), lines...)
		cp[n-1] = repl
		return strings.Join(cp, "\n")
	}
	for _, tc := range []struct {
		name    string
		content string
		want    []string
	}{
		{"non-numeric value", mutate(20, "2020-01-01 00:11:00 UTC,abc"), []string{"line 20", "abc"}},
		{"NaN value", mutate(20, "2020-01-01 00:11:00 UTC,NaN"), []string{"line 20"}},
		{"Inf value", mutate(20, "2020-01-01 00:11:00 UTC,Inf"), []string{"line 20", "Inf"}},
		{"negative Inf value", mutate(20, "2020-01-01 00:11:00 UTC,-Inf"), []string{"line 20"}},
		{"PST timestamp", mutate(20, "2020-01-01 00:11:00 PST,5"), []string{"line 20", "PST", "UTC"}},
		{"bad timestamp", mutate(30, "not-a-time,5"), []string{"line 30"}},
		{"no comma", mutate(40, "2020-01-01 00:31:00 UTC"), []string{"line 40"}},
		{"duplicate minute", mutate(50, "2020-01-01 00:00:00 UTC,5"), []string{"line 50", "00:00"}},
		{"missing minutes", strings.Join(lines[:100], "\n") + "\n", []string{"1440", "01:32"}},
		{"empty file", recorderHeader, []string{"no data rows"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewReplay(ReplayConfig{File: writeRecording(t, tc.content), Type: "pv", Clock: "wall", Scale: 1, RatedW: 1000})
			if err == nil {
				t.Fatal("NewReplay accepted a malformed file")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
	if _, err := NewReplay(ReplayConfig{File: filepath.Join(t.TempDir(), "absent.csv"), Type: "pv"}); err == nil {
		t.Error("NewReplay accepted a missing file")
	}
}

func TestNewSelectsReplayAndRejectsBadSettings(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 700 }))
	cfg := inverter.SimConfig{Backend: "replay", Replay: inverter.ReplaySettings{File: f, Type: "pv", Clock: "wall", Scale: 1, RatedW: 10000}}
	d, err := New(cfg, inverter.Scenario{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*Replay); !ok {
		t.Fatalf("New(replay) = %T, want *Replay", d)
	}
	cfg.Replay.File = ""
	if _, err := New(cfg, inverter.Scenario{}); err == nil {
		t.Error("New(replay) with no file succeeded")
	}
}

func TestNewReplayRefusesBadSettings(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 1 }))
	pv := ReplayConfig{File: f, Type: "pv", Clock: "wall", Scale: 1, RatedW: 1000}
	bat := batteryCfg(f, 0.5)
	for _, tc := range []struct {
		name string
		cfg  ReplayConfig
		edit func(*ReplayConfig)
		want string
	}{
		{"negative scale", pv, func(c *ReplayConfig) { c.Scale = -1 }, "scale"},
		{"NaN scale", pv, func(c *ReplayConfig) { c.Scale = math.NaN() }, "scale"},
		{"Inf scale", pv, func(c *ReplayConfig) { c.Scale = math.Inf(1) }, "scale"},
		{"zero rating", pv, func(c *ReplayConfig) { c.RatedW = 0 }, "rated power"},
		{"negative rating", pv, func(c *ReplayConfig) { c.RatedW = -5 }, "rated power"},
		{"bad type", pv, func(c *ReplayConfig) { c.Type = "wind" }, "wind"},
		{"bad clock", pv, func(c *ReplayConfig) { c.Clock = "sundial" }, "sundial"},
		{"battery zero capacity", bat, func(c *ReplayConfig) { c.CapacityWh = 0 }, "capacity"},
		{"battery SOC above 1", bat, func(c *ReplayConfig) { c.InitialSOC = 1.1 }, "state of charge"},
		{"battery SOC below 0", bat, func(c *ReplayConfig) { c.InitialSOC = -0.1 }, "state of charge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			tc.edit(&cfg)
			_, err := NewReplay(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if _, err := NewReplay(pv); err != nil {
		t.Fatalf("the unedited PV config was refused: %v", err)
	}
	if _, err := NewReplay(bat); err != nil {
		t.Fatalf("the unedited battery config was refused: %v", err)
	}
}

// Charging must raise the state of charge and stop at full. The battery
// starts below full, so the stop comes from integration reaching 1, not
// from an initial_soc of 1.
func TestReplayBatteryChargesUpToFullAndThenStops(t *testing.T) {
	t.Parallel()
	f := writeRecording(t, recordingRows(func(int) float64 { return 0 }))
	ctx := context.Background()
	c := &clock{at(12, 0)}
	// 10 kWh at 80 percent: 2 kWh of room. Charging 4 kW fills it in 30 min.
	r := newReplayAt(t, batteryCfg(f, 0.8), c)
	_, _ = r.ReadState(ctx)
	if st, _ := r.ApplySetpoint(ctx, pvControls(-4000)); st.ActivePowerW != -4000 {
		t.Fatalf("first charging output = %v, want -4000", st.ActivePowerW)
	}
	c.t = at(12, 15)
	st, _ := r.ApplySetpoint(ctx, pvControls(-4000))
	if want := 0.9; math.Abs(r.SOC()-want) > 1e-9 {
		t.Fatalf("SOC after 15 min at -4 kW = %v, want %v (charging must raise it)", r.SOC(), want)
	}
	if st.ActivePowerW != -4000 {
		t.Fatalf("output with room left = %v, want -4000", st.ActivePowerW)
	}
	c.t = at(13, 0)
	st, _ = r.ApplySetpoint(ctx, pvControls(-4000))
	if r.SOC() != 1 {
		t.Fatalf("SOC after charging past full = %v, want clamped to 1", r.SOC())
	}
	if st.ActivePowerW != 0 {
		t.Fatalf("output once full = %v, want 0 W", st.ActivePowerW)
	}
	if st, _ = r.ApplySetpoint(ctx, pvControls(3000)); st.ActivePowerW != 3000 {
		t.Fatalf("discharge from full = %v, want 3000", st.ActivePowerW)
	}
}
