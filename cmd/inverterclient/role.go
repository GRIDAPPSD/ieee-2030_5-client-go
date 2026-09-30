// Phase 2 role wiring: the aggregator holds its own EndDevice and never
// creates it, so an aggregator process always takes the lookup branch of
// EndDevice acquisition regardless of --csip.

package main

import (
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
)

// endDeviceAcquisitionUsesLookup reports whether Phase 2 should GET the
// EndDeviceList and find the process's own record (true) rather than POST
// /edev to self-register (false). An aggregator process always looks up:
// the utility creates its EndDevice, not the client, so Register is never
// called for the aggregator role, whatever --csip is set to.
func endDeviceAcquisitionUsesLookup(cfg inverter.SimConfig) bool {
	return cfg.CSIP || cfg.ClientRole == string(guard.RoleAggregator)
}

// skipDERPipelineForRole reports whether Phase 3 (DER capability/settings
// PUTs) and Phase 4 (MirrorUsagePoint creation) must be skipped for this
// process's role. The aggregator's own EndDevice is not a DER: it has no
// DER capability, settings, status or MirrorUsagePoint of its own. The
// guard refuses these writes for aggregator self too (defense in depth);
// this is what stops the client from even attempting them.
func skipDERPipelineForRole(cfg inverter.SimConfig) bool {
	return cfg.ClientRole == string(guard.RoleAggregator)
}

// derOnlyFlagUsedInAggregatorRole reports the name of a der-only flag set
// while running in the aggregator role, or "" when none is set. --csip
// selects which EndDevice-acquisition branch the der role takes (POST vs
// GET); endDeviceAcquisitionUsesLookup already forces the aggregator role
// onto the GET branch unconditionally, so --csip has no effect there. A
// setting with no effect must be refused at start, not silently ignored.
func derOnlyFlagUsedInAggregatorRole(cfg inverter.SimConfig) string {
	if cfg.ClientRole == string(guard.RoleAggregator) && cfg.CSIP {
		return "--csip"
	}
	return ""
}
