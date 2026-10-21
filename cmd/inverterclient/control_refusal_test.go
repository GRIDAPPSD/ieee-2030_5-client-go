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
	refused := fmt.Errorf("%w: refType 3 (%%statVarAvail) but the device has no valid statVarAvail", inverter.ErrFixedVarRefused)

	l.observe("inverter", nil)
	l.observe("inverter", refused)
	l.observe("inverter", refused)
	l.observe("inverter", nil)
	l.observe("inverter", nil)

	got := logs.String()
	if n := strings.Count(got, "inverter: control refused: opModFixedVar refused: refType 3"); n != 1 {
		t.Errorf("refusal logged %d times, want 1; log:\n%s", n, got)
	}
	if n := strings.Count(got, "inverter: control no longer refused"); n != 1 {
		t.Errorf("clearing logged %d times, want 1; log:\n%s", n, got)
	}
	if lines := strings.Count(got, "\n"); lines != 2 {
		t.Errorf("log has %d lines, want 2 (no line before the first refusal); log:\n%s", lines, got)
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
		{"value above full scale", sep2.FixedVar{RefType: 2, Value: 10001}, full},
		{"value below negative full scale", sep2.FixedVar{RefType: 2, Value: -10001}, full},
	}
}

// A refused base (a DefaultDERControl, or a managed device's control) is
// not applied: the base in force before it keeps driving the device.
func TestControlApplierKeepsWhatWasInForce(t *testing.T) {
	for _, tc := range unhonorableFixedVars() {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSessionLog(t)
			grid := inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0}
			var a controlApplier
			fixedW := sep2.SignedPerCent(2500)
			prior := &sep2.DERControlBase{OpModFixedW: &fixedW}
			before := a.apply("inverter", prior, grid, 8000, tc.ratings, nil)

			fv := tc.fv
			inject := sep2.FixedPowerFactor{Displacement: 900}
			refused := &sep2.DERControlBase{OpModFixedVar: &fv, OpModFixedPFInjectW: &inject}
			for i := 0; i < 3; i++ {
				got := a.apply("inverter", refused, grid, 8000, tc.ratings, nil)
				if got.Refused != nil || got.ActivePowerW != before.ActivePowerW || got.ReactivePowerVAr != before.ReactivePowerVAr || got.Mode != before.Mode {
					t.Fatalf("tick %d: outputs %+v, want the prior base's %+v", i, got, before)
				}
			}
			if n := strings.Count(logs.String(), "inverter: control refused: opModFixedVar refused"); n != 1 {
				t.Errorf("refusal logged %d times over 3 ticks, want 1; log:\n%s", n, logs.String())
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
