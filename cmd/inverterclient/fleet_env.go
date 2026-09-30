// The aggregator role's fleet sidecars need gldsidecar (sim/gridlabd) on
// PYTHONPATH: it is not pip-installed (ci.yml installs only its pinned
// dependencies, per sim/gridlabd/requirements.lock), the same reason
// sim/gridlabd/conftest.py adds it to sys.path for pytest. Without this,
// NewManager's own default environment (PATH and HOME only) cannot start
// the real sidecar at all.

package main

import "github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/sim/gridlabd"

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
