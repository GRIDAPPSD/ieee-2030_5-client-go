// #70's construction-time gate: fleet sidecars are aggregator-only. A
// fleet file in the der role is a startup error, before any device,
// dispatcher, listener or sidecar process exists.

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/sim/gridlabd"
)

// TestFleetManagerConstruction_DERRoleWithFleetFileRefuses is the unit
// level: the same construction shape main() uses, given the der role and
// a fleet file, refuses before touching the fleet file at all (the role
// check runs first), so a nonexistent path here does not confuse what is
// being proven.
func TestFleetManagerConstruction_DERRoleWithFleetFileRefuses(t *testing.T) {
	_, err := gridlabd.NewManager(gridlabd.ManagerConfig{
		Role:       string(guard.RoleDER),
		FleetFiles: []string{"does-not-need-to-exist.json"},
	})
	if !errors.Is(err, gridlabd.ErrNotAggregatorRole) {
		t.Fatalf("NewManager(der role, 1 fleet file) error = %v, want ErrNotAggregatorRole", err)
	}
}

func TestFleetManagerConstruction_DERRoleNoFleetFiles_Unaffected(t *testing.T) {
	m, err := gridlabd.NewManager(gridlabd.ManagerConfig{Role: string(guard.RoleDER)})
	if err != nil {
		t.Fatalf("NewManager(der role, no fleet files): %v", err)
	}
	if m != nil {
		t.Errorf("NewManager(der role, no fleet files) = %+v, want nil", m)
	}
}

// TestFleetManagerWiredIntoMain is the wiring seam, the same shape
// log_fatalf_scan_test.go and der_session_wiring_test.go use: proves
// main() actually calls gridlabd.NewManager and exits on its error, and
// that this happens before device.New (so no device, dispatcher or
// sidecar exists yet), not just that the construction logic exists on its
// own.
func TestFleetManagerWiredIntoMain(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)
	for _, line := range []string{
		// Item 1: main builds the ManagerConfig through
		// buildFleetManagerConfig (the same function a test calls), not a
		// literal ManagerConfig{} passed straight to NewManager, so the
		// --sidecar-pythonpath resolution a test exercises is the same
		// path production takes.
		"fleetCfg, err := buildFleetManagerConfig(cfg.ClientRole,",
		"fleetMgr, err := gridlabd.NewManager(fleetCfg)",
		"os.Exit(1)",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("main.go missing expected line: %q", line)
		}
	}
	fleetIdx := strings.Index(text, "gridlabd.NewManager(")
	deviceIdx := strings.Index(text, "device.New(cfg, scenario)")
	if fleetIdx < 0 {
		t.Fatal("main.go does not call gridlabd.NewManager")
	}
	if deviceIdx < 0 {
		t.Fatal("main.go does not call device.New(cfg, scenario) (has it been renamed?)")
	}
	if fleetIdx > deviceIdx {
		t.Error("gridlabd.NewManager must be called before device.New, so the der-role refusal happens before any device exists")
	}
}

// TestShutdownWiredIntoMain is item 4 (round 5): a source-text check that
// `defer shutdown()` is present and positioned after startFleets' success
// path, so the deferred call covers every ordinary return main makes from
// there on (a ctx-cancel idle loop, Phase 5's own ctx.Done() case,
// scenario completion). This is a text check, not a behavioral one: it
// cannot tell a live `defer shutdown()` from one some other edit made
// dead code, the same limit TestFleetManagerWiredIntoMain and the old,
// now-deleted TestStopFleetSupervisorsWiredIntoMain always had.
// TestMain_CreateClientFailureAfterFleetHealthy_RemovesFleetSocket (round
// 4) is the behavioral proof for shutdown() itself, through fatalf's own
// explicit call to the same function value; driving main() to a genuine
// ordinary return instead of a fatal exit needs a live or fake IEEE
// 2030.5 server, out of proportion for this round.
func TestShutdownWiredIntoMain(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Line by line, comments skipped: a commented-out `// defer shutdown()`
	// must not satisfy the wiring check.
	startFleetsLine, deferLine := -1, -1
	for i, line := range strings.Split(string(src), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if startFleetsLine < 0 && strings.Contains(code, "fleetStop, err := startFleets(ctx, fleets)") {
			startFleetsLine = i
		}
		if deferLine < 0 && code == "defer shutdown()" {
			deferLine = i
		}
	}
	if startFleetsLine < 0 {
		t.Fatal("main.go does not call startFleets(ctx, fleets) (has it been renamed?)")
	}
	if deferLine < 0 {
		t.Fatal("main.go has no uncommented `defer shutdown()`")
	}
	if deferLine < startFleetsLine {
		t.Error("defer shutdown() must be positioned after startFleets succeeds, not before")
	}
}

// TestBinary_DERRoleWithFleetFile_ExitsWithError is #70's literal
// criterion: the real compiled binary, started in the der role with a
// fleet file, exits with an error and starts no sidecar. No other flag is
// needed: the role/fleet-file check runs before certificate, server or
// scenario validation.
func TestBinary_DERRoleWithFleetFile_ExitsWithError(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "inverterclient")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	fleetFile := filepath.Join(t.TempDir(), "nonexistent.fleet.json") // never read: role is checked first
	cmd := exec.Command(bin, "--client-role", "der", "--fleet-file", fleetFile)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("binary in der role with --fleet-file: want a nonzero exit, got err=%v output=%s", err, out)
	}
	if exitErr.ExitCode() == 0 {
		t.Errorf("exit code = 0, want nonzero; output: %s", out)
	}
	if !strings.Contains(string(out), gridlabd.ErrNotAggregatorRole.Error()) {
		t.Errorf("output = %q, want it to mention %q", out, gridlabd.ErrNotAggregatorRole.Error())
	}
}
