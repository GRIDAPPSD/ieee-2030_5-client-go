// The aggregator role's fleet sidecars need gldsidecar (sim/gridlabd) on
// PYTHONPATH: it is not pip-installed (ci.yml installs only its pinned
// dependencies, per sim/gridlabd/requirements.lock), the same reason
// sim/gridlabd/conftest.py adds it to sys.path for pytest. Without this,
// NewManager's own default environment (PATH and HOME only) cannot start
// the real sidecar at all.
//
// Design item 1: a relative PYTHONPATH is resolved by the SIDECAR relative
// to ITS OWN cwd (RunDir), not this process's, so passing the flag's
// value straight through used to fail with "No module named gldsidecar"
// whenever RunDir was not this process's own working directory (every
// deployment other than the accidental case of RunDir happening to equal
// the repo root). resolveSidecarPythonPath fixes that by resolving to an
// absolute path, and checking gldsidecar is actually there, before any
// process starts.

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/sim/gridlabd"
)

// defaultSidecarPythonPath is sim/gridlabd's location relative to this
// process's own working directory, matching the convention --cert,
// --key and --ca already use (certs/device.crt, relative to CWD): this
// binary is run from the repository root.
const defaultSidecarPythonPath = "sim/gridlabd"

// fleetSidecarEnv builds the sidecar's environment: gridlabd.DefaultEnv()
// (PATH and HOME) plus PYTHONPATH pointing at pythonPathDir. pythonPathDir
// is a parameter rather than computed here, so a test can supply an
// absolute path independent of its own working directory while production
// uses defaultSidecarPythonPath.
func fleetSidecarEnv(pythonPathDir string) []string {
	if pythonPathDir == "" {
		return gridlabd.DefaultEnv()
	}
	return append(gridlabd.DefaultEnv(), "PYTHONPATH="+pythonPathDir)
}

// resolveSidecarPythonPath resolves pythonPathDir (--sidecar-pythonpath's
// value) to an absolute path and checks that gldsidecar is actually
// there, so a wrong or unresolvable value is a startup error naming the
// flag and the resolved path, not a "No module named gldsidecar"
// traceback from deep inside a sidecar process 30 seconds later. An empty
// pythonPathDir means "gldsidecar is installed in the interpreter's own
// environment": returns "", nil, and fleetSidecarEnv adds no PYTHONPATH.
func resolveSidecarPythonPath(pythonPathDir string) (string, error) {
	if pythonPathDir == "" {
		return "", nil
	}
	abs, err := filepath.Abs(pythonPathDir)
	if err != nil {
		return "", fmt.Errorf("--sidecar-pythonpath %s: %w", pythonPathDir, err)
	}
	marker := filepath.Join(abs, "gldsidecar", "__main__.py")
	if _, err := os.Stat(marker); err != nil {
		return "", fmt.Errorf("--sidecar-pythonpath %s (resolved %s): gldsidecar package not found: %w", pythonPathDir, abs, err)
	}
	return abs, nil
}

// buildFleetManagerConfig builds the gridlabd.ManagerConfig main() passes
// to NewManager, so a test can exercise exactly the construction path
// production uses (item 1) rather than a hand-built config that can drift
// from it. The sidecar python path is resolved and checked only in the
// aggregator role with at least one fleet file: every other combination
// (an ordinary DER-client run, an aggregator run with no fleet files, or
// a fleet file given in the der role) never starts a sidecar and gets no
// sidecar Env at all, either because NewManager itself returns (nil, nil)
// or because NewManager's own ErrNotAggregatorRole refusal is the more
// specific, correct error for that case; resolving the sidecar path first
// would mask it behind an unrelated python-path failure instead.
func buildFleetManagerConfig(role string, fleetFiles []string, runDir, sidecarPythonPath string) (gridlabd.ManagerConfig, error) {
	cfg := gridlabd.ManagerConfig{Role: role, FleetFiles: fleetFiles, RunDir: runDir}
	if role != gridlabd.AggregatorRole || len(fleetFiles) == 0 {
		return cfg, nil
	}
	resolved, err := resolveSidecarPythonPath(sidecarPythonPath)
	if err != nil {
		return gridlabd.ManagerConfig{}, err
	}
	cfg.Env = fleetSidecarEnv(resolved)
	return cfg, nil
}
