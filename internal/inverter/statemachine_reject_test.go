package inverter_test

import (
	"errors"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// unhonorable lists one FixedVar per refusal reason, each with ratings that
// make it impossible for the device.
func unhonorable() []struct {
	name    string
	fv      sep2.FixedVar
	ratings inverter.Ratings
} {
	statVarAvail := 3000.0
	full := inverter.Ratings{SetMaxW: 20000, SetMaxVar: 8000, StatVarAvail: &statVarAvail}
	return []struct {
		name    string
		fv      sep2.FixedVar
		ratings inverter.Ratings
	}{
		{"refType 1 with no setMaxW", sep2.FixedVar{RefType: 1, Value: 5000}, inverter.Ratings{SetMaxVar: 8000}},
		{"refType 2 with no setMaxVar", sep2.FixedVar{RefType: 2, Value: 5000}, inverter.Ratings{SetMaxW: 20000}},
		{"refType 3 with no statVarAvail", sep2.FixedVar{RefType: 3, Value: 5000}, inverter.Ratings{SetMaxW: 20000, SetMaxVar: 8000}},
		{"refType 0", sep2.FixedVar{RefType: 0, Value: 5000}, full},
		{"refType 4", sep2.FixedVar{RefType: 4, Value: 5000}, full},
		{"value above full scale", sep2.FixedVar{RefType: 2, Value: 10001}, full},
		{"value below negative full scale", sep2.FixedVar{RefType: 2, Value: -10001}, full},
	}
}

func controlAt(mrid string, start time.Time, durationSec uint32, base sep2.DERControlBase) sep2.DERControl {
	dc := sep2.DERControl{DERControlBase: &base}
	dc.MRID = mrid
	dc.Interval = &sep2.DateTimeInterval{Start: start.Unix(), Duration: durationSec}
	return dc
}

// TestStateMachine_RejectsUnhonorableEvent: an event the device cannot
// honor is refused at receipt with one EVENT_REJECTED transition, is never
// scheduled, and leaves the event already in force running.
func TestStateMachine_RejectsUnhonorableEvent(t *testing.T) {
	for _, tc := range unhonorable() {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0)
			clock := func() time.Time { return now }
			sched := inverter.NewScheduler(clock, inverter.SeededRandForTesting(1, 2))
			sm := inverter.NewStateMachine()
			sm.SetValidator(func(evt *sep2.DERControl) error {
				return inverter.CheckControl(evt.DERControlBase, tc.ratings)
			})
			type seen struct {
				prev, next inverter.EventState
				mrid       string
			}
			var trail []seen
			sm.AddTransitionHook(func(prev, next inverter.EventState, evt *sep2.DERControl) {
				m := ""
				if evt != nil {
					m = evt.MRID
				}
				trail = append(trail, seen{prev, next, m})
			})

			fixedW := sep2.SignedPerCent(2500)
			inForce := controlAt("A", now.Add(-time.Minute), 600, sep2.DERControlBase{OpModFixedW: &fixedW})
			sm.Tick(now, []sep2.DERControl{inForce}, nil, sched)
			if cur := sm.Current(); cur.State != inverter.StateEventStarted || cur.ActiveMRID != "A" {
				t.Fatalf("setup: state %s mRID %q, want EVENT_STARTED A", cur.State, cur.ActiveMRID)
			}
			trail = nil

			fv := tc.fv
			bad := controlAt("B", now, 300, sep2.DERControlBase{OpModFixedVar: &fv})
			sm.Tick(now, []sep2.DERControl{bad}, nil, sched)

			if cur := sm.Current(); cur.State != inverter.StateEventStarted || cur.ActiveMRID != "A" {
				t.Errorf("after reject: state %s mRID %q, want EVENT_STARTED A still in force", cur.State, cur.ActiveMRID)
			}
			if len(trail) != 1 || trail[0] != (seen{inverter.StateEventStarted, inverter.StateEventRejected, "B"}) {
				t.Errorf("transitions = %+v, want exactly one EVENT_STARTED -> EVENT_REJECTED for B", trail)
			}

			// After A ends, B must not start: it was never scheduled.
			now = now.Add(20 * time.Minute)
			sm.Tick(now, nil, nil, sched)
			for _, s := range trail[1:] {
				if s.mrid == "B" {
					t.Errorf("rejected event B reached %s", s.next)
				}
			}
			if cur := sm.Current(); cur.State != inverter.StateDefault {
				t.Errorf("after A ends: state %s, want DEFAULT", cur.State)
			}
		})
	}
}

// TestCheckControl names each refusal reason and accepts what it can honor.
func TestCheckControl(t *testing.T) {
	for _, tc := range unhonorable() {
		fv := tc.fv
		err := inverter.CheckControl(&sep2.DERControlBase{OpModFixedVar: &fv}, tc.ratings)
		if !errors.Is(err, inverter.ErrFixedVarRefused) {
			t.Errorf("%s: CheckControl = %v, want ErrFixedVarRefused", tc.name, err)
		}
	}
	ok := sep2.FixedVar{RefType: 1, Value: 5000}
	if err := inverter.CheckControl(&sep2.DERControlBase{OpModFixedVar: &ok}, inverter.Ratings{SetMaxW: 20000}); err != nil {
		t.Errorf("honorable FixedVar: CheckControl = %v, want nil", err)
	}
	if err := inverter.CheckControl(nil, inverter.Ratings{}); err != nil {
		t.Errorf("nil base: CheckControl = %v, want nil", err)
	}
}
