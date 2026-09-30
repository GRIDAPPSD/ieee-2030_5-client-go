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
