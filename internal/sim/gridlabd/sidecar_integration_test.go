package gridlabd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
)

// battery2GLM and battery2FleetJSON are the output of
// models/battery_fleet.py's generate("probe", 2, "12345678"), pinned here
// as literal fixtures so this test does not depend on Python being able to
// import that module by path (its generator has no importable console
// entry point; the CLI wrapper is exercised by the sidecar's own pytest
// suite). Two devices, matching the criterion that mirrors and readings
// must be distinguishable per device.
const battery2GLM = `clock {
  starttime '2020-01-01 00:00:00';
  stoptime '2020-01-01 02:00:00';
}
module powerflow;
module generators;

object meter {
  name fleet_meter;
  phases ABCN;
  nominal_voltage 120;
}

object inverter {
  name probe_bat0_inv;
  parent fleet_meter;
  phases ABCN;
  generator_status ONLINE;
  four_quadrant_control_mode CONSTANT_PQ;
  inverter_type FOUR_QUADRANT;
  rated_power 5000;
  P_Out 0;
  Q_Out 0;
}
object battery {
  name probe_bat0;
  parent probe_bat0_inv;
  use_internal_battery_model TRUE;
  battery_type LI_ION;
  battery_capacity 10000;
  state_of_charge 0.5;
  round_trip_efficiency 0.95;
  rated_power 5000;
  V_Max 480;
  base_efficiency 0.95;
}

object inverter {
  name probe_bat1_inv;
  parent fleet_meter;
  phases ABCN;
  generator_status ONLINE;
  four_quadrant_control_mode CONSTANT_PQ;
  inverter_type FOUR_QUADRANT;
  rated_power 5000;
  P_Out 0;
  Q_Out 0;
}
object battery {
  name probe_bat1;
  parent probe_bat1_inv;
  use_internal_battery_model TRUE;
  battery_type LI_ION;
  battery_capacity 10000;
  state_of_charge 0.5;
  round_trip_efficiency 0.95;
  rated_power 5000;
  V_Max 480;
  base_efficiency 0.95;
}
`

const battery2FleetJSON = `{
  "fleet": "probe",
  "gridlabd_version": "6.0.0a1",
  "aggregator_pen": "12345678",
  "glm": "probe.glm",
  "devices": [
    {
      "name": "probe-000",
      "lfdi": "0FA437FCD2BDADDA3EF8FEFAFF4A3D1612345678",
      "objects": {"inverter": "probe_bat0_inv", "battery": "probe_bat0"}
    },
    {
      "name": "probe-001",
      "lfdi": "AD882EC29CFC0A0B500B663AD841670E12345678",
      "objects": {"inverter": "probe_bat1_inv", "battery": "probe_bat1"}
    }
  ]
}
`

// gldsidecarRequiredEnv, when set (to any value), means this test must not
// skip: CI sets it so a green run is evidence the real sidecar path
// actually executed, not that it was silently skipped (go test ./... with
// no -v gives no other sign either way).
const gldsidecarRequiredEnv = "GLDSIDECAR_INTEGRATION_REQUIRED"

// sidecarPython locates a python interpreter with the pinned gridlabd
// package importable, so this test runs the real sidecar exactly as CI's
// ci.yml does (its Python steps put a matching venv on PATH before `go
// test`). PYTHON_FOR_GLDSIDECAR_TEST overrides it for a local run against
// a Scratch-built venv, per this brief's Verification section.
//
// A host with python3 on PATH but no gridlabd installed (this sandbox,
// and likely most dev machines outside CI's venv) skips rather than
// hard-failing with a traceback, UNLESS gldsidecarRequiredEnv is set, in
// which case that would be exactly the silent-pass CI must not allow.
func sidecarPython(t *testing.T) string {
	t.Helper()
	p := os.Getenv("PYTHON_FOR_GLDSIDECAR_TEST")
	if p == "" {
		found, err := exec.LookPath("python3")
		if err != nil {
			failOrSkip(t, "no python3 on PATH and PYTHON_FOR_GLDSIDECAR_TEST unset: %v", err)
			return ""
		}
		p = found
	}
	if err := exec.Command(p, "-c", "import gridlabd").Run(); err != nil {
		failOrSkip(t, "gridlabd is not importable via %s: %v", p, err)
		return ""
	}
	return p
}

func failOrSkip(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(gldsidecarRequiredEnv) != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// repoRoot walks up from this file's directory to find the module root
// (go.mod), so PYTHONPATH can point at sim/gridlabd without depending on
// the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find repo root (go.mod) above " + thisFile)
	return ""
}

// TestSidecarIntegration_RealGridlabd runs the actual pinned gridlabd
// sidecar (not the fake), through Supervisor and FleetDevice, proving the
// full path this package exists for: start, hello, drive two devices'
// setpoints, read them back, and stop cleanly.
func TestSidecarIntegration_RealGridlabd(t *testing.T) {
	python := sidecarPython(t)

	fleetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fleetDir, "probe.glm"), []byte(battery2GLM), 0o600); err != nil {
		t.Fatalf("write glm: %v", err)
	}
	fleetFilePath := filepath.Join(fleetDir, "probe.fleet.json")
	if err := os.WriteFile(fleetFilePath, []byte(battery2FleetJSON), 0o600); err != nil {
		t.Fatalf("write fleet file: %v", err)
	}

	sockDir := shortSockDir(t)
	// The real gldsidecar package is not pip-installed (ci.yml installs
	// only its pinned dependencies, per requirements.lock); PYTHONPATH
	// makes it importable the same way conftest.py does for pytest.
	env := append(os.Environ(), "PYTHONPATH="+filepath.Join(repoRoot(t), "sim", "gridlabd"))
	cfg := SupervisorConfig{
		Fleet:         "probe",
		SocketPath:    filepath.Join(sockDir, "probe.sock"),
		FleetFilePath: fleetFilePath,
		Command:       []string{python, "-m", "gldsidecar"},
		StartTimeout:  15 * time.Second,
		CallTimeout:   10 * time.Second,
		StopGrace:     2 * time.Second,
		Stderr:        testWriter{t},
	}.withDefaults()
	launch := func(ctx context.Context, args []string) (sidecarProcess, error) {
		full := append(append([]string{}, cfg.Command...), args...)
		return startExecProcessWithEnv(ctx, full, env, cfg.Stderr)
	}
	sup := newSupervisor(cfg, launch)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			t.Error("Supervisor.Run did not return after ctx cancel")
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for !sup.Healthy() && time.Now().Before(deadline) {
		select {
		case err := <-runErr:
			t.Fatalf("Run exited before becoming healthy: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !sup.Healthy() {
		t.Fatal("sidecar did not become healthy within 15s")
	}

	transport := sup.Transport()
	dev0, err := device.NewFleetDevice(transport, "0FA437FCD2BDADDA3EF8FEFAFF4A3D1612345678", "probe_bat0_inv", "fleet_meter", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice(dev0): %v", err)
	}
	dev1, err := device.NewFleetDevice(transport, "AD882EC29CFC0A0B500B663AD841670E12345678", "probe_bat1_inv", "fleet_meter", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice(dev1): %v", err)
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()

	state0, err := dev0.ApplySetpoint(callCtx, fleetControls(2500, -100))
	if err != nil {
		t.Fatalf("dev0 ApplySetpoint: %v", err)
	}
	state1, err := dev1.ApplySetpoint(callCtx, fleetControls(1200, 50))
	if err != nil {
		t.Fatalf("dev1 ApplySetpoint: %v", err)
	}

	// This is the invariant the brief names: tests assert values read
	// back through the protocol, not only that the call succeeded, and
	// the two devices' setpoints must not be confused with each other.
	if state0.ActivePowerW != 2500 {
		t.Errorf("dev0 ActivePowerW = %v, want 2500 (read back from gridlabd)", state0.ActivePowerW)
	}
	if state0.ReactivePowerVAr != -100 {
		t.Errorf("dev0 ReactivePowerVAr = %v, want -100", state0.ReactivePowerVAr)
	}
	if state1.ActivePowerW != 1200 {
		t.Errorf("dev1 ActivePowerW = %v, want 1200 (read back, distinct from dev0)", state1.ActivePowerW)
	}
	if state1.ReactivePowerVAr != 50 {
		t.Errorf("dev1 ReactivePowerVAr = %v, want 50", state1.ReactivePowerVAr)
	}

	reading0, err := dev0.ReadState(callCtx)
	if err != nil {
		t.Fatalf("dev0 ReadState: %v", err)
	}
	if reading0.MaxPowerW != 5000 {
		t.Errorf("dev0 MaxPowerW = %v, want 5000 (rated_power from the GLM)", reading0.MaxPowerW)
	}

	// Kill the real process out from under the supervisor: every device on
	// this fleet must refuse rather than report a stale reading.
	proc := sup.process()
	if proc == nil {
		t.Fatal("sup.process() = nil while Healthy() was true")
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill sidecar process: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for sup.Healthy() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sup.Healthy() {
		t.Fatal("Supervisor still reports healthy after the sidecar process was killed")
	}
	if _, err := dev0.ReadState(callCtx); err == nil {
		t.Error("dev0.ReadState after the sidecar died: want error, got nil")
	}
	if _, err := dev1.ApplySetpoint(callCtx, fleetControls(1, 1)); err == nil {
		t.Error("dev1.ApplySetpoint after the sidecar died: want error, got nil")
	}

	// Supervisor.Run auto-restarts (bounded backoff); wait for it, then
	// prove the chosen recovery: dev0's last commanded 2500 W is
	// re-applied to the freshly reloaded model, not left at the GLM's
	// reset default of 0, before ReadState's caller sees "healthy" again.
	deadline = time.Now().Add(20 * time.Second)
	for !sup.Healthy() && time.Now().Before(deadline) {
		select {
		case err := <-runErr:
			t.Fatalf("Run exited instead of restarting: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !sup.Healthy() {
		t.Fatal("sidecar did not auto-restart within 20s")
	}
	if _, err := dev0.ReadState(callCtx); err != nil {
		t.Fatalf("dev0 ReadState after restart (triggers resync): %v", err)
	}
	postRestart, err := transport.Get(callCtx, []device.FleetGetItem{{Object: "probe_bat0_inv", Property: "P_Out"}})
	if err != nil {
		t.Fatalf("raw Get of P_Out after restart: %v", err)
	}
	gotPOut, err := postRestart[0].Float64()
	if err != nil {
		t.Fatalf("P_Out.Float64(): %v", err)
	}
	if gotPOut != 2500 {
		t.Errorf("dev0 P_Out on the restarted model = %v, want 2500 (re-applied, not the GLM's reset default of 0)", gotPOut)
	}
}

func fleetControls(activePowerW, reactivePowerVAr float64) inverter.ControlOutputs {
	return inverter.ControlOutputs{
		ActivePowerW:     activePowerW,
		ReactivePowerVAr: reactivePowerVAr,
		Connected:        true,
		Energized:        true,
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("sidecar stderr: %s", p)
	return len(p), nil
}
