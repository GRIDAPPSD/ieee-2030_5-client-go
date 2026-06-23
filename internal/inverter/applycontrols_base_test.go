package inverter_test

import (
	"testing"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// TestActiveControlBase walks the three-rule decision tree in
// ActiveControlBase. Each subtest names the rule it pins so a regression
// is easy to localize.
//
// IEEE-041 (Phase 5 ticket 4 of 5). Closes the long-standing
// ApplyControls(nil, ...) defect at cmd/inverterclient/main.go.
func TestActiveControlBase(t *testing.T) {
	t.Parallel()

	// Common fixtures — distinct pointer identities let the assertions
	// distinguish "returned the event base" from "returned the default base."
	eventW := sep2.ActivePower{Value: 750}
	eventBase := &sep2.DERControlBase{OpModFixedW: &eventW}

	defaultW := sep2.ActivePower{Value: 250}
	defaultBase := &sep2.DERControlBase{OpModFixedW: &defaultW}

	defaultCtl := &sep2.DefaultDERControl{DERControlBase: defaultBase}
	defaultCtlNilBase := &sep2.DefaultDERControl{DERControlBase: nil}

	type tc struct {
		name        string
		snap        inverter.EventStateSnapshot
		defaultCtl  *sep2.DefaultDERControl
		wantBase    *sep2.DERControlBase
		description string
	}

	cases := []tc{
		// Rule 3: fall-through.
		{
			name:        "no_csip_no_events_no_default",
			snap:        inverter.EventStateSnapshot{State: inverter.StateDefault},
			defaultCtl:  nil,
			wantBase:    nil,
			description: "rule 3: no CSIP server / no default provisioned → nil (ApplyControls handles nil as no-op)",
		},
		{
			name: "default_nil_base_field",
			snap: inverter.EventStateSnapshot{State: inverter.StateDefault},
			// DefaultDERControl object exists but its DERControlBase field is
			// nil (server sent an empty default). Rule 2 falls through to
			// Rule 3.
			defaultCtl:  defaultCtlNilBase,
			wantBase:    nil,
			description: "rule 2 guard: defaultCtl non-nil but DERControlBase nil → falls through to nil",
		},

		// Rule 2: default base when no active event.
		{
			name:        "default_active",
			snap:        inverter.EventStateSnapshot{State: inverter.StateDefault},
			defaultCtl:  defaultCtl,
			wantBase:    defaultBase,
			description: "rule 2: DEFAULT state with non-nil default base → default base",
		},

		// Rule 1: event base when EVENT_STARTED with a non-nil base.
		{
			name: "event_started_with_base",
			snap: inverter.EventStateSnapshot{
				State:            inverter.StateEventStarted,
				ActiveMRID:       "evt-1",
				ActiveDERControl: &sep2.DERControl{DERControlBase: eventBase},
			},
			defaultCtl:  defaultCtl,
			wantBase:    eventBase, // rule 1 beats rule 2
			description: "rule 1: EVENT_STARTED with valid event base wins over default",
		},

		// Rule 1 safety: nil event base falls through to Rule 2.
		{
			name: "event_started_nil_base_falls_to_default",
			snap: inverter.EventStateSnapshot{
				State:            inverter.StateEventStarted,
				ActiveMRID:       "evt-2",
				ActiveDERControl: &sep2.DERControl{DERControlBase: nil},
			},
			defaultCtl:  defaultCtl,
			wantBase:    defaultBase,
			description: "rule 1 guard: EVENT_STARTED but event DERControlBase nil → falls to rule 2 (default)",
		},
		{
			name: "event_started_nil_active_der_control",
			snap: inverter.EventStateSnapshot{
				State:            inverter.StateEventStarted,
				ActiveMRID:       "evt-3",
				ActiveDERControl: nil,
			},
			defaultCtl:  defaultCtl,
			wantBase:    defaultBase,
			description: "rule 1 guard: EVENT_STARTED but ActiveDERControl pointer nil → falls to rule 2",
		},

		// Transient states: never treated as "active event" — return default.
		{
			name: "event_received_uses_default",
			snap: inverter.EventStateSnapshot{
				State:            inverter.StateEventReceived,
				ActiveMRID:       "evt-pending",
				ActiveDERControl: &sep2.DERControl{DERControlBase: eventBase},
			},
			defaultCtl:  defaultCtl,
			wantBase:    defaultBase,
			description: "EVENT_RECEIVED: event known but not yet started → keep applying default base",
		},
		{
			name: "event_completed_uses_default",
			snap: inverter.EventStateSnapshot{
				State:            inverter.StateEventCompleted,
				ActiveMRID:       "evt-done",
				ActiveDERControl: &sep2.DERControl{DERControlBase: eventBase},
			},
			defaultCtl:  defaultCtl,
			wantBase:    defaultBase,
			description: "EVENT_COMPLETED: transient terminal state → default (state machine auto-reverts to DEFAULT same tick)",
		},
		{
			name: "event_cancelled_uses_default",
			snap: inverter.EventStateSnapshot{
				State:            inverter.StateEventCancelled,
				ActiveMRID:       "evt-cancel",
				ActiveDERControl: &sep2.DERControl{DERControlBase: eventBase},
			},
			defaultCtl:  defaultCtl,
			wantBase:    defaultBase,
			description: "EVENT_CANCELLED: transient terminal state → default",
		},

		// Revert path: post-EVENT_STARTED → EVENT_COMPLETED → DEFAULT,
		// Current() reports DEFAULT and the helper returns the default
		// base again. Verifies the round-trip back to default works as the
		// state machine clears ActiveDERControl.
		{
			name: "revert_to_default_after_event_completes",
			snap: inverter.EventStateSnapshot{
				State:            inverter.StateDefault,
				ActiveMRID:       "",
				ActiveDERControl: nil, // state machine clears these on revert
			},
			defaultCtl:  defaultCtl,
			wantBase:    defaultBase,
			description: "post-completion revert: Current() shows DEFAULT with nil active → default base resumes",
		},
	}

	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := inverter.ActiveControlBase(tt.snap, tt.defaultCtl)
			if got != tt.wantBase {
				t.Fatalf("%s: ActiveControlBase = %p, want %p (%s)",
					tt.name, got, tt.wantBase, tt.description)
			}
		})
	}
}

// TestActiveControlBase_DrivesApplyControls is the wiring smoke test: a
// rule-2 default base with OpModFixedW=500W flows through ApplyControls
// and the controller honors it. Proves the helper's output is usable by
// ApplyControls without any further adaptation.
//
// We do NOT re-test ApplyControls's full priority ladder here — that's
// controller_test.go's job. This is the one-line check that says "the
// helper return value plugs into ApplyControls."
func TestActiveControlBase_DrivesApplyControls(t *testing.T) {
	t.Parallel()

	fixedW := sep2.ActivePower{Value: 500}
	defaultBase := &sep2.DERControlBase{OpModFixedW: &fixedW}
	defaultCtl := &sep2.DefaultDERControl{DERControlBase: defaultBase}

	snap := inverter.EventStateSnapshot{State: inverter.StateDefault}
	base := inverter.ActiveControlBase(snap, defaultCtl)
	if base != defaultBase {
		t.Fatalf("helper returned %p, want %p (default base)", base, defaultBase)
	}

	// Now plumb through ApplyControls — same call shape the simulation tick
	// loop uses post-IEEE-041.
	grid := inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0}
	const maxP = 8000.0
	out := inverter.ApplyControls(base, grid, maxP)

	if out.ActivePowerW != 500 {
		t.Fatalf("ApplyControls(default base with OpModFixedW=500, ...) ActivePowerW = %.0f, want 500 (controller honored the helper-supplied base)", out.ActivePowerW)
	}
}
