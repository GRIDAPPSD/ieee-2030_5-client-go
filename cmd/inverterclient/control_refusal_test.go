package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

func TestRefusalLogLogsEachChangeOnce(t *testing.T) {
	logs := captureSessionLog(t)
	var l refusalLog
	const refused = "control refused: opModFixedVar refused: refType 3; applying the default control"
	const noControl = "control refused: opModFixedVar refused: refType 3; applying no control"

	l.observe("inverter", "")
	l.observe("inverter", refused)
	l.observe("inverter", refused)
	l.observe("inverter", noControl)
	l.observe("inverter", noControl)
	l.observe("inverter", "")
	l.observe("inverter", "")

	got := logs.String()
	for _, want := range []string{"inverter: " + refused, "inverter: " + noControl, "inverter: control no longer refused"} {
		if n := strings.Count(got, want+"\n"); n != 1 {
			t.Errorf("%q logged %d times, want 1; log:\n%s", want, n, got)
		}
	}
	if lines := strings.Count(got, "\n"); lines != 3 {
		t.Errorf("log has %d lines, want 3 (one per change, none before the first refusal); log:\n%s", lines, got)
	}
}

// ownRatings must follow the nameplate a sim config sets, and must leave
// statVarAvail unknown rather than zero, which would scale a %statVarAvail
// fixed var to 0 VAr instead of refusing it.
func TestOwnRatingsFollowTheNameplate(t *testing.T) {
	prev := inverter.Rating
	t.Cleanup(func() { inverter.Rating = prev })
	applyNameplate(5000)

	r := ownRatings()
	if r.SetMaxW != 5000 || r.SetMaxVar != 2200 {
		t.Errorf("ownRatings = SetMaxW %g, SetMaxVar %g; want 5000 and 2200", float64(r.SetMaxW), r.SetMaxVar)
	}
	if r.StatVarAvail != nil {
		t.Errorf("StatVarAvail = %g, want nil (not tracked)", *r.StatVarAvail)
	}
}

// unhonorableFixedVars lists one FixedVar per refusal reason with ratings
// that make it impossible.
func unhonorableFixedVars() []struct {
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
	}
}

// A refused base falls back to the DefaultDERControl's outputs, and to
// the nil-base output when the default is refused or absent. An event that
// was applied earlier is never what a refused base falls back to.
func TestControlApplierFallsBackToTheDefault(t *testing.T) {
	grid := inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0}
	target := sep2.SignedPerCent(2500)
	honorableDefault := &sep2.DefaultDERControl{DERControlBase: &sep2.DERControlBase{OpModFixedW: &target}}
	for _, tc := range unhonorableFixedVars() {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.ratings
			nilBase := inverter.ApplyControls(nil, grid, 8000, r)
			wantDefault := inverter.ApplyControls(honorableDefault.DERControlBase, grid, 8000, r)
			if wantDefault.Refused != nil || wantDefault.ActivePowerW == nilBase.ActivePowerW {
				t.Fatalf("setup: the default must be honorable and differ from the nil base; got %+v vs %+v", wantDefault, nilBase)
			}
			fv := tc.fv
			refused := &sep2.DERControlBase{OpModFixedVar: &fv}
			refusedDefault := &sep2.DefaultDERControl{DERControlBase: refused}
			zero := sep2.SignedPerCent(0)
			event := &sep2.DERControlBase{OpModFixedW: &zero}

			var a controlApplier
			logs := captureSessionLog(t)
			a.apply("inverter", event, honorableDefault, grid, 8000, r, nil)
			for i := 0; i < 3; i++ {
				if got := a.apply("inverter", refused, honorableDefault, grid, 8000, r, nil); got != wantDefault {
					t.Fatalf("refused base with an honorable default, tick %d: %+v, want the default's %+v", i, got, wantDefault)
				}
			}
			// The event ended and the default itself is refused: nothing
			// valid remains, so the nil base applies, not the ended event.
			for i := 0; i < 3; i++ {
				if got := a.apply("inverter", refused, refusedDefault, grid, 8000, r, nil); got != nilBase {
					t.Fatalf("refused default, tick %d: %+v, want the nil base %+v", i, got, nilBase)
				}
			}
			if got := a.apply("inverter", refused, nil, grid, 8000, r, nil); got != nilBase {
				t.Errorf("refused base with no default: %+v, want the nil base %+v", got, nilBase)
			}
			out := logs.String()
			if n := strings.Count(out, "applying the default control"); n != 1 {
				t.Errorf("fallback to the default logged %d times over 3 ticks, want 1; log:\n%s", n, out)
			}
			if n := strings.Count(out, "applying no control"); n != 1 {
				t.Errorf("fallback to the nil base logged %d times over 4 ticks, want 1; log:\n%s", n, out)
			}
		})
	}
}

// A rejected event posts exactly one Response, status 253, when
// responseRequired bit 1 is set, and none when it is not. The state in
// force stays DEFAULT.
func TestResponsePOSTHook_RejectedEventPosts253(t *testing.T) {
	const replyTo = "https://server.example/rsps"
	const lfdi = "0011223344556677889900aabbccddeeff001122"
	for _, tc := range unhonorableFixedVars() {
		for _, m := range []struct {
			mask  sep2.HexBinary8
			posts int
		}{{0x02, 1}, {0x03, 1}, {0x01, 0}} {
			t.Run(fmt.Sprintf("%s mask 0x%02x", tc.name, uint8(m.mask)), func(t *testing.T) {
				poster := &fakePoster{}
				clock := func() time.Time { return fixedTestNow }
				sm, sched := newTestStateMachine(clock)
				ratings := tc.ratings
				sm.SetValidator(controlValidator(func() inverter.Ratings { return ratings }))
				sm.AddTransitionHook(responsePOSTHook(poster, lfdi, fixedNowFn))

				mask := m.mask
				dc := buildControl("EVT-REJ", replyTo, &mask, 0, 300)
				fv := tc.fv
				dc.DERControlBase = &sep2.DERControlBase{OpModFixedVar: &fv}
				sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
				sm.Tick(clock().Add(time.Minute), nil, nil, sched)

				if st := sm.Current().State; st != inverter.StateDefault {
					t.Errorf("state = %s, want DEFAULT (the rejected event is never in force)", st)
				}
				calls := poster.Calls()
				if len(calls) != m.posts {
					t.Fatalf("PostResponse calls = %d, want %d; calls=%+v", len(calls), m.posts, calls)
				}
				for _, c := range calls {
					if c.Resp.Status == nil || *c.Resp.Status != 253 || c.Resp.Subject != "EVT-REJ" || c.ReplyTo != replyTo {
						t.Errorf("Response = %+v to %q, want status 253 for EVT-REJ to %q", c.Resp, c.ReplyTo, replyTo)
					}
				}
			})
		}
	}
}

// The single-device state machine rejects an event the device cannot honor.
func TestNewEventStateMachineRejectsUnhonorable(t *testing.T) {
	clock := func() time.Time { return fixedTestNow }
	sm := newEventStateMachine()
	_, sched := newTestStateMachine(clock)
	var nexts []inverter.EventState
	sm.AddTransitionHook(func(_, next inverter.EventState, _ *sep2.DERControl) { nexts = append(nexts, next) })
	fv := sep2.FixedVar{RefType: 3, Value: 5000}
	dc := buildControl("EVT-REJ", "", nil, 0, 300)
	dc.DERControlBase = &sep2.DERControlBase{OpModFixedVar: &fv}
	sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	if len(nexts) != 1 || nexts[0] != inverter.StateEventRejected || sm.Current().State != inverter.StateDefault {
		t.Errorf("transitions %v, state %s; want one EVENT_REJECTED and DEFAULT", nexts, sm.Current().State)
	}
}

// One rejected event gets one 253, however often it is added again: after
// dropping out of the list for a poll, from a fresh cache on a re-walk, or
// with only its status changed.
func TestResponsePOSTHook_RejectedEventPosts253Once(t *testing.T) {
	const replyTo = "https://server.example/rsps"
	poster := &fakePoster{}
	clock := func() time.Time { return fixedTestNow }
	sm, sched := newTestStateMachine(clock)
	sm.SetValidator(controlValidator(func() inverter.Ratings { return inverter.Ratings{SetMaxW: 20000} }))
	sm.AddTransitionHook(responsePOSTHook(poster, "lfdi", fixedNowFn))

	mask := sep2.HexBinary8(0x03)
	dc := buildControl("EVT-REJ", replyTo, &mask, time.Minute, 300)
	fv := sep2.FixedVar{RefType: 2, Value: 5000}
	dc.DERControlBase = &sep2.DERControlBase{OpModFixedVar: &fv}
	snap := func(cs ...sep2.DERControl) map[string]sep2.DERControl {
		m := map[string]sep2.DERControl{}
		for _, c := range cs {
			m[c.MRID] = c
		}
		return m
	}
	tick := func(prev, curr map[string]sep2.DERControl) {
		added, cancelled := diffSnapshots(prev, curr)
		sm.Tick(clock(), added, cancelled, sched)
	}

	// Received, gone for one poll, back.
	tick(snap(), snap(dc))
	tick(snap(dc), snap())
	tick(snap(), snap(dc))
	// A re-walk's fresh cache adds it again.
	tick(snap(), snap(dc))
	// Only its status changed.
	active := dc.Copy()
	active.EventStatus = &sep2.EventStatus{CurrentStatus: 1}
	tick(snap(), snap(active))

	calls := poster.Calls()
	if len(calls) != 1 || calls[0].Resp.Status == nil || *calls[0].Resp.Status != 253 {
		t.Errorf("Responses = %+v, want exactly one 253", calls)
	}
}
