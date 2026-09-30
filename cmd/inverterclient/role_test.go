// Cmd-side wiring test: the aggregator role always acquires its own
// EndDevice by lookup and never POSTs to create one.

package main

import (
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
)

func TestEndDeviceAcquisitionUsesLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		csip bool
		role string
		want bool
	}{
		{"der, no csip: registers (today's default)", false, string(guard.RoleDER), false},
		{"der, csip: looks up", true, string(guard.RoleDER), true},
		{"aggregator, no csip: still looks up, never registers", false, string(guard.RoleAggregator), true},
		{"aggregator, csip: looks up", true, string(guard.RoleAggregator), true},
	}
	for _, tt := range tests {
		cfg := inverter.SimConfig{CSIP: tt.csip, ClientRole: tt.role}
		if got := endDeviceAcquisitionUsesLookup(cfg); got != tt.want {
			t.Errorf("%s: endDeviceAcquisitionUsesLookup(csip=%v, role=%q) = %v, want %v",
				tt.name, tt.csip, tt.role, got, tt.want)
		}
	}
}

// TestSkipDERPipelineForRole is fix-round-1 finding 3: the aggregator's own
// EndDevice is not a DER, so Phase 3 (DER capability/settings PUTs) and
// Phase 4 (MirrorUsagePoint creation) must be skipped for it, and must stay
// unchanged for der.
func TestSkipDERPipelineForRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role string
		want bool
	}{
		{string(guard.RoleDER), false},
		{string(guard.RoleAggregator), true},
	}
	for _, tt := range tests {
		cfg := inverter.SimConfig{ClientRole: tt.role}
		if got := skipDERPipelineForRole(cfg); got != tt.want {
			t.Errorf("skipDERPipelineForRole(role=%q) = %v, want %v", tt.role, got, tt.want)
		}
	}
}

// TestRunDERSessionForRole is fix-round-3 finding 3: the control poll,
// event engine, response hook, simulated device tick and alarm detector
// must not start for the aggregator role even when Phase 2c selected a
// DERProgram (a program erroneously assigned to a non-DER EndDevice).
func TestRunDERSessionForRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		role     string
		selected bool
		want     bool
	}{
		{"der, program selected: runs", string(guard.RoleDER), true, true},
		{"der, no program: does not run", string(guard.RoleDER), false, false},
		{"aggregator, program selected: still refused", string(guard.RoleAggregator), true, false},
		{"aggregator, no program: does not run", string(guard.RoleAggregator), false, false},
	}
	for _, tt := range tests {
		cfg := inverter.SimConfig{ClientRole: tt.role}
		if got := runDERSessionForRole(cfg, tt.selected); got != tt.want {
			t.Errorf("%s: runDERSessionForRole(role=%q, selected=%v) = %v, want %v",
				tt.name, tt.role, tt.selected, got, tt.want)
		}
	}
}

// TestDEROnlyFlagUsedInAggregatorRole is fix-round-1 finding 6: --csip is
// refused at start for the aggregator role rather than silently ignored,
// and stays unaffected for der (in either --csip state).
func TestDEROnlyFlagUsedInAggregatorRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		csip bool
		role string
		want string
	}{
		{"der, csip off", false, string(guard.RoleDER), ""},
		{"der, csip on: unaffected, still der's own setting", true, string(guard.RoleDER), ""},
		{"aggregator, csip off: nothing to refuse", false, string(guard.RoleAggregator), ""},
		{"aggregator, csip on: refused", true, string(guard.RoleAggregator), "--csip"},
	}
	for _, tt := range tests {
		cfg := inverter.SimConfig{CSIP: tt.csip, ClientRole: tt.role}
		if got := derOnlyFlagUsedInAggregatorRole(cfg); got != tt.want {
			t.Errorf("%s: derOnlyFlagUsedInAggregatorRole(csip=%v, role=%q) = %q, want %q",
				tt.name, tt.csip, tt.role, got, tt.want)
		}
	}
}
