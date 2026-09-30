package gridlabd

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
)

// ErrNotAggregatorRole is returned when fleet files are given outside the
// aggregator role: a fleet file in the der role is a startup error, not a
// partial run, and the der role never constructs the managed-set walk at
// all.
var ErrNotAggregatorRole = errors.New("fleet sidecars are only built in the aggregator role")

// AggregatorRole is the client role value that permits building fleets.
const AggregatorRole = "aggregator"

// defaultFleetMeterObject is the shared coupling-point object every fleet
// the one existing generator (models/battery_fleet.py) writes into its
// GLM, literally named "fleet_meter" regardless of fleet name. It is used
// when a fleet file does not name its own meter object.
const defaultFleetMeterObject = "fleet_meter"

// ManagerConfig builds a Manager from a set of fleet files.
type ManagerConfig struct {
	Role       string   // the process's client role, e.g. "der" or "aggregator"
	FleetFiles []string // one fleet JSON per fleet
	RunDir     string   // directory for per-fleet sockets; required only when FleetFiles is non-empty; never /tmp
	Command    []string // sidecar command template; nil uses the default
	// Env is the sidecar's environment, passed to every fleet's
	// Supervisor; nil defaults to DefaultEnv() (PATH and HOME only). A
	// caller whose gldsidecar package is not installed (not on the
	// default PATH-resolved interpreter's own site-packages) must add
	// PYTHONPATH here: this package cannot discover sim/gridlabd's
	// location on its own, since a deployed binary need not be
	// colocated with that source tree.
	Env []string

	StartTimeout, CallTimeout, StopGrace  time.Duration
	BackoffMin, BackoffMax, RestartWindow time.Duration
	MaxRestarts                           int
	Now                                   func() time.Time

	// Stderr and Log are diagnostics: without them a sidecar's startup
	// traceback, and every restart and health-transition log line, are
	// silently discarded, which is exactly what made an earlier startup
	// failure invisible. Nil defaults to os.Stderr and log.Printf, not to
	// io.Discard: a caller must opt OUT of diagnostics explicitly (an
	// io.Discard Stderr, a no-op Log), not get silence by omission.
	Stderr io.Writer
	Log    func(format string, args ...any)
}

// Manager owns one Supervisor per fleet file and the FleetDevice backends
// built from it. NewManager is the construction-time gate: called with any
// role other than "aggregator" and one or more fleet files, it refuses
// before starting any process.
type Manager struct {
	Supervisors []*Supervisor
	Devices     []*device.FleetDevice
}

// NewManager loads and validates every fleet file and builds a Supervisor
// and FleetDevice set per fleet. It does not start any supervisor; call
// Run on each Supervisor (typically one goroutine per fleet) separately.
//
// Given a role other than AggregatorRole:
//   - with no fleet files, returns (nil, nil): an ordinary DER client run.
//   - with one or more fleet files, returns ErrNotAggregatorRole and
//     builds nothing.
//
// In the aggregator role with no fleet files, also returns (nil, nil):
// --client-role aggregator with no --fleet-file is a valid, existing
// invocation (#71) that must keep working unchanged, and RunDir is only
// ever needed to hold a fleet's socket.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.Role != AggregatorRole {
		if len(cfg.FleetFiles) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: role %q was given %d fleet file(s)", ErrNotAggregatorRole, cfg.Role, len(cfg.FleetFiles))
	}
	if len(cfg.FleetFiles) == 0 {
		return nil, nil
	}
	if cfg.RunDir == "" {
		return nil, fmt.Errorf("gridlabd: RunDir is required when FleetFiles is non-empty")
	}
	if err := prepareRunDir(cfg.RunDir); err != nil {
		return nil, err
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Log == nil {
		cfg.Log = log.Printf
	}

	m := &Manager{}
	seenFleetNames := make(map[string]string, len(cfg.FleetFiles)) // fleet name -> source path
	seenLFDIs := make(map[string]string, len(cfg.FleetFiles))      // normalized LFDI -> "fleet/device"
	for _, path := range cfg.FleetFiles {
		ff, err := LoadFleetFile(path)
		if err != nil {
			return nil, err
		}
		if other, dup := seenFleetNames[ff.Fleet]; dup {
			return nil, fmt.Errorf("fleet %s: duplicate fleet name, also used by %s", ff.Fleet, other)
		}
		seenFleetNames[ff.Fleet] = path
		for _, dev := range ff.Devices {
			// dev.LFDI is already uppercased by LoadFleetFile; ToUpper
			// again here costs nothing and does not depend on that.
			norm := strings.ToUpper(dev.LFDI)
			if other, dup := seenLFDIs[norm]; dup {
				return nil, fmt.Errorf("LFDI %s is used by two devices across fleet files: %s and %s/%s", norm, other, ff.Fleet, dev.Name)
			}
			seenLFDIs[norm] = ff.Fleet + "/" + dev.Name
		}

		sup := NewSupervisor(SupervisorConfig{
			Fleet:         ff.Fleet,
			SocketPath:    filepath.Join(cfg.RunDir, ff.Fleet+".sock"),
			FleetFilePath: ff.path, // absolute; the sidecar's --fleet-file argument
			Command:       cfg.Command,
			Env:           cfg.Env,
			Dir:           cfg.RunDir,
			StartTimeout:  cfg.StartTimeout,
			CallTimeout:   cfg.CallTimeout,
			StopGrace:     cfg.StopGrace,
			BackoffMin:    cfg.BackoffMin,
			BackoffMax:    cfg.BackoffMax,
			MaxRestarts:   cfg.MaxRestarts,
			RestartWindow: cfg.RestartWindow,
			Stderr:        cfg.Stderr,
			Log:           cfg.Log,
		})
		m.Supervisors = append(m.Supervisors, sup)

		meterObj := ff.Meter
		if meterObj == "" {
			meterObj = defaultFleetMeterObject
		}
		transport := sup.Transport()
		for _, dev := range ff.Devices {
			invObj, ok := dev.Objects["inverter"]
			if !ok {
				return nil, fmt.Errorf("fleet %s device %s: no inverter object mapped", ff.Fleet, dev.Name)
			}
			fd, err := device.NewFleetDevice(transport, dev.LFDI, invObj, meterObj, cfg.Now)
			if err != nil {
				return nil, fmt.Errorf("fleet %s device %s: %w", ff.Fleet, dev.Name, err)
			}
			m.Devices = append(m.Devices, fd)
		}
	}
	return m, nil
}

// prepareRunDir creates dir with mode 0700 if it does not exist yet, and
// otherwise refuses to use it as a run directory if it is a symlink, not a
// directory, or group- or world-writable: a socket for every fleet lives
// here, and a writable-by-others directory lets another local user replace
// a socket between our own bind and a later dial.
func prepareRunDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(dir, 0o700)
	}
	if err != nil {
		return fmt.Errorf("run dir %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("run dir %s: refusing a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("run dir %s: not a directory", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("run dir %s: group- or world-writable (mode %o), refusing", dir, info.Mode().Perm())
	}
	owned, err := ownedByCurrentUser(info)
	if err != nil {
		return fmt.Errorf("run dir %s: %w", dir, err)
	}
	if !owned {
		return fmt.Errorf("run dir %s: not owned by the current user, refusing", dir)
	}
	return nil
}
