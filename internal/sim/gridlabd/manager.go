package gridlabd

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
)

// ErrNotAggregatorRole is returned when fleet files are given outside the
// aggregator role. ADR-009 decision 1: a fleet file in the der role is a
// startup error, not a partial run; ADR-009 decision 2: the der runner
// never constructs the managed-set walk at all.
var ErrNotAggregatorRole = errors.New("fleet sidecars are only built in the aggregator role")

// AggregatorRole is the client role value that permits building fleets.
const AggregatorRole = "aggregator"

// ManagerConfig builds a Manager from a set of fleet files.
type ManagerConfig struct {
	Role       string   // the process's client role, e.g. "der" or "aggregator"
	FleetFiles []string // one fleet JSON per fleet
	RunDir     string   // directory for per-fleet sockets; never /tmp
	Command    []string // sidecar command template; nil uses the default

	StartTimeout, CallTimeout, StopGrace  time.Duration
	BackoffMin, BackoffMax, RestartWindow time.Duration
	MaxRestarts                           int
	Now                                   func() time.Time
	Log                                   func(format string, args ...any)
}

// Manager owns one Supervisor per fleet file and the FleetDevice backends
// built from it. NewManager is the construction-time gate ADR-009 decision
// 2 requires: called with any role other than "aggregator" and one or more
// fleet files, it refuses before starting any process.
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
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.Role != AggregatorRole {
		if len(cfg.FleetFiles) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: role %q was given %d fleet file(s)", ErrNotAggregatorRole, cfg.Role, len(cfg.FleetFiles))
	}
	if cfg.RunDir == "" {
		return nil, fmt.Errorf("gridlabd: RunDir is required in the aggregator role")
	}

	m := &Manager{}
	for _, path := range cfg.FleetFiles {
		ff, err := LoadFleetFile(path)
		if err != nil {
			return nil, err
		}
		sup := NewSupervisor(SupervisorConfig{
			Fleet:         ff.Fleet,
			SocketPath:    filepath.Join(cfg.RunDir, ff.Fleet+".sock"),
			FleetFilePath: ff.path, // absolute; the sidecar's --fleet-file argument
			Command:       cfg.Command,
			StartTimeout:  cfg.StartTimeout,
			CallTimeout:   cfg.CallTimeout,
			StopGrace:     cfg.StopGrace,
			BackoffMin:    cfg.BackoffMin,
			BackoffMax:    cfg.BackoffMax,
			MaxRestarts:   cfg.MaxRestarts,
			RestartWindow: cfg.RestartWindow,
			Log:           cfg.Log,
		})
		m.Supervisors = append(m.Supervisors, sup)

		transport := sup.Transport()
		for _, dev := range ff.Devices {
			invObj, ok := dev.Objects["inverter"]
			if !ok {
				return nil, fmt.Errorf("fleet %s device %s: no inverter object mapped", ff.Fleet, dev.Name)
			}
			fd, err := device.NewFleetDevice(transport, dev.LFDI, invObj, cfg.Now)
			if err != nil {
				return nil, fmt.Errorf("fleet %s device %s: %w", ff.Fleet, dev.Name, err)
			}
			m.Devices = append(m.Devices, fd)
		}
	}
	return m, nil
}
