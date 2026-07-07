package dispatch

import (
	"fmt"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// New constructs the RegisterableDispatcher for the configured role.
// Selection happens once at startup; the receiver wiring never branches on
// dispatcher identity after construction. Per ADR-003, the concrete TYPE
// is the role: no --mode runtime flag, no Role field on the returned value.
//
// Supported values for cfg.Role:
//   - "" or "simulator": the SimulatorDispatcher (default, cache-refresh policy).
//   - "production": the ProductionDispatcher (scaffold with TBD extension points).
//
// An unknown role is a loud startup error (fail-closed discipline).
func New(cfg inverter.SimConfig) (RegisterableDispatcher, error) {
	switch cfg.Role {
	case "", "simulator":
		return NewSimulatorDispatcher(), nil
	case "production":
		return NewProductionDispatcher(), nil
	default:
		return nil, fmt.Errorf("unknown role %q (want simulator|production)", cfg.Role)
	}
}

// CheckRoleBackendCoherence validates the (role, backend) pair and returns
// (warned, err):
//   - (false, nil): clean production pair (production+realdevice) or any
//     simulator combination.
//   - (true, nil): permitted dry-run (production+synthetic or
//     production+gridlabd): a legitimate commissioning step per the
//     operationalizing doc Section 5.4 stage 1, but unusual enough to
//     warrant a loud warning at startup.
//   - (false, error): never returned; all pairs are permitted with at most a
//     warning. Reserved for future fail-closed policy.
//
// The caller (main) is responsible for logging the warning; this function
// only classifies the pair so it can be tested independently.
func CheckRoleBackendCoherence(role, backend string) (warned bool, err error) {
	if role != "production" {
		// Simulator role pairs freely with any backend.
		return false, nil
	}
	// Production role with a synthetic or gridlabd backend is an unusual
	// (but permitted) dry-run combination. Per Fork C decision: warn, do
	// not fail-closed, because a production dry-run against synthetic is a
	// legitimate commissioning step.
	switch backend {
	case "realdevice":
		return false, nil
	default:
		// synthetic, gridlabd, or empty: permitted with warning.
		return true, nil
	}
}
