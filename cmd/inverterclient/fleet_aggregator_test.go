// Item 2 and item 3: the aggregator role must start a fleet sidecar
// through production's own defaults (no test-added PYTHONPATH beyond what
// main() itself computes), and a fleet given in the aggregator role must
// actually run its Supervisor.

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/sim/gridlabd"
)

// probeGLM and probeFleetJSON are a minimal one-device battery fleet,
// matching models/battery_fleet.py's own template (pinned here as a
// literal fixture, the same reason internal/sim/gridlabd's own
// sidecar_integration_test.go pins one, rather than depending on the
// generator having a stable CLI).
const probeGLM = `clock {
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
`

const probeFleetJSON = `{
  "fleet": "probe",
  "gridlabd_version": "6.0.0a1",
  "aggregator_pen": "12345678",
  "glm": "probe.glm",
  "devices": [
    {
      "name": "probe-000",
      "lfdi": "0FA437FCD2BDADDA3EF8FEFAFF4A3D1612345678",
      "objects": {"inverter": "probe_bat0_inv", "battery": "probe_bat0"}
    }
  ]
}
`

// repoRoot walks up from this file's own directory to find go.mod, so
// PYTHONPATH and cmd.Dir below do not depend on the test's working
// directory (go test always runs with cwd = the package directory).
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

// shortRunDir returns a fresh, short-named directory for the fleet's Unix
// socket: t.TempDir() embeds this package's often-long test names, which
// pushes a socket path past the AF_UNIX 108-byte limit (reproduced here:
// "AF_UNIX path too long" with a plain t.TempDir()).
func shortRunDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "gld")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func gldsidecarPython(t *testing.T) string {
	t.Helper()
	p := os.Getenv("PYTHON_FOR_GLDSIDECAR_TEST")
	if p == "" {
		found, err := exec.LookPath("python3")
		if err != nil {
			t.Skipf("no python3 on PATH and PYTHON_FOR_GLDSIDECAR_TEST unset: %v", err)
		}
		p = found
	}
	if err := exec.Command(p, "-c", "import gridlabd").Run(); err != nil {
		if os.Getenv("GLDSIDECAR_INTEGRATION_REQUIRED") != "" {
			t.Fatalf("gridlabd is not importable via %s: %v", p, err)
		}
		t.Skipf("gridlabd is not importable via %s: %v", p, err)
	}
	return p
}

// TestNewManager_RealSidecarThroughOwnDefaults is item 1's own test,
// rewritten from its round-3 shape: production's own defaults must be
// enough to start the real, pinned sidecar, with NO absolute path the
// test builds itself, which is the exact gap this round closes (the
// round-3 version built one: `filepath.Join(root, defaultSidecarPythonPath)`
// passed straight into fleetSidecarEnv, bypassing resolveSidecarPythonPath
// entirely). t.Chdir to the repo root matches where a deployed binary
// runs from (the same convention --cert/--key/--ca already use); RunDir
// is deliberately outside the repo. buildFleetManagerConfig is the same
// function main() calls, with the --sidecar-pythonpath flag's own default
// value and no Command override, so Supervisor's own default resolves
// "python3" from PATH exactly as production would.
func TestNewManager_RealSidecarThroughOwnDefaults(t *testing.T) {
	gldsidecarPython(t) // skip/fatal gate: gridlabd must be importable via PATH's python3

	root := repoRoot(t)
	t.Chdir(root)

	fleetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fleetDir, "probe.glm"), []byte(probeGLM), 0o600); err != nil {
		t.Fatalf("write glm: %v", err)
	}
	fleetFilePath := filepath.Join(fleetDir, "probe.fleet.json")
	if err := os.WriteFile(fleetFilePath, []byte(probeFleetJSON), 0o600); err != nil {
		t.Fatalf("write fleet file: %v", err)
	}
	runDir := shortRunDir(t)

	fleetCfg, err := buildFleetManagerConfig(string(guard.RoleAggregator), []string{fleetFilePath}, runDir, defaultSidecarPythonPath)
	if err != nil {
		t.Fatalf("buildFleetManagerConfig: %v", err)
	}
	m, err := gridlabd.NewManager(fleetCfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if len(m.Supervisors) != 1 {
		t.Fatalf("len(Supervisors) = %d, want 1", len(m.Supervisors))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- m.Supervisors[0].Run(ctx) }()
	t.Cleanup(func() {
		m.Supervisors[0].Stop()
	})

	deadline := time.Now().Add(15 * time.Second)
	for !m.Supervisors[0].Healthy() && time.Now().Before(deadline) {
		select {
		case err := <-runErr:
			t.Fatalf("Run exited before becoming healthy: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !m.Supervisors[0].Healthy() {
		t.Fatal("sidecar did not become healthy within 15s using production's own defaults (buildFleetManagerConfig, resolveSidecarPythonPath, PATH)")
	}
}

// TestResolveSidecarPythonPath_MissingDirectory is item 1's second test: a
// --sidecar-pythonpath value with no gldsidecar package there is a
// startup error naming the flag and the resolved path, not a "No module
// named gldsidecar" traceback from deep inside a sidecar process.
func TestResolveSidecarPythonPath_MissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := resolveSidecarPythonPath(missing)
	if err == nil {
		t.Fatal("resolveSidecarPythonPath on a directory with no gldsidecar package: want error, got nil")
	}
	if !strings.Contains(err.Error(), "--sidecar-pythonpath") {
		t.Errorf("error %q does not name the flag --sidecar-pythonpath", err.Error())
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the resolved path %s", err.Error(), missing)
	}
}

// A full end-to-end exec of the compiled binary in the aggregator role was
// tried and dropped: main() proceeds past the fleet-starting loop into
// certificate loading and a live IEEE 2030.5 server connection (Discover),
// which this environment has neither of, so the process exits on that
// failure before any SIGINT-driven shutdown assertion could run. Proving
// "supervisors run" and "shutdown stops and waits" against the compiled
// binary would need a fake SEP2 server, which is out of proportion for
// this round; TestNewManager_RealSidecarThroughOwnDefaults above proves
// the construction and Env mechanism main() uses against the real sidecar
// instead, and fleet_start_test.go proves startFleets' own stop and
// wiring behavior against fakes. TestStopFleetSupervisorsWiredIntoMain
// (a source-text scan for the function item 4 removed) is deleted along
// with the function it checked for.
