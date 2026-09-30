// Phase 2 role wiring for ADR-009 decision 1: the aggregator holds its own
// EndDevice and never creates it, so an aggregator process always takes the
// lookup branch of EndDevice acquisition regardless of --csip.

package main

import (
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
)

// endDeviceAcquisitionUsesLookup reports whether Phase 2 should GET the
// EndDeviceList and find the process's own record (true) rather than POST
// /edev to self-register (false). An aggregator process always looks up:
// "the utility creates it" (ADR-009 decision 4), so Register is never
// called for the aggregator role, whatever --csip is set to.
func endDeviceAcquisitionUsesLookup(cfg inverter.SimConfig) bool {
	return cfg.CSIP || cfg.ClientRole == string(guard.RoleAggregator)
}
