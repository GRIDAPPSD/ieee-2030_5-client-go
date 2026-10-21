package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/invertertest"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

type listRejectRig struct {
	sm     *inverter.StateMachine
	sched  *inverter.Scheduler
	cache  *inverter.DERControlCache
	poster *fakePoster
	prev   map[string]sep2.DERControl
	trail  []string // "<mRID> <state>" per transition carrying an event
}

func newListRejectRig() *listRejectRig {
	r := &listRejectRig{cache: inverter.NewDERControlCache(), poster: &fakePoster{}}
	_, r.sched = newTestStateMachine(fixedNowFn)
	r.sm = newEventStateMachine()
	r.sm.AddTransitionHook(responsePOSTHook(r.poster, "0011223344556677889900aabbccddeeff001122", fixedNowFn))
	r.sm.AddTransitionHook(func(_, next inverter.EventState, evt *sep2.DERControl) {
		if evt != nil {
			r.trail = append(r.trail, evt.MRID+" "+next.String())
		}
	})
	r.prev = r.cache.Snapshot()
	return r
}

// poll is one list read followed by one state-machine step, as the single
// device's poll and tick goroutines do it.
func (r *listRejectRig) poll(list sep2.DERControlListResult) {
	r.cache.Refresh(list)
	r.prev = stepEvents(r.sm, r.sched, fixedTestNow, r.cache, r.prev)
}

// statuses is each Response posted for subject, as "<status>@<replyTo>".
func (r *listRejectRig) statuses(subject string) []string {
	var out []string
	for _, c := range r.poster.Calls() {
		if c.Resp.Subject == subject && c.Resp.Status != nil {
			out = append(out, fmt.Sprintf("%d@%s", *c.Resp.Status, c.ReplyTo))
		}
	}
	return out
}

func hasTrail(trail []string, want string) bool {
	for _, s := range trail {
		if s == want {
			return true
		}
	}
	return false
}

// A list with one valid control and one undecodable control: the valid one
// is scheduled, the bad one gets one 253 to its replyTo, the next poll
// neither cancels it nor answers it again, and removing another event is
// still seen as a cancellation.
func TestStepEvents_UndecodableControlIsRejectedAndTheListKept(t *testing.T) {
	r := newListRejectRig()
	mask := sep2.HexBinary8(0x03)
	a := buildControl("A", "/rsp/A", &mask, 10*time.Minute, 300)
	c := buildControl("C", "/rsp/C", &mask, 20*time.Minute, 300)
	bad := invertertest.Rejected("B", "/rsp/B", 0x02)

	r.poll(invertertest.ListResult([]sep2.DERControl{a, c}, bad))
	if snap := r.sm.Current(); snap.State != inverter.StateEventReceived || snap.ActiveMRID != "A" {
		t.Fatalf("after poll 1: %s mRID %q, want EVENT_RECEIVED for A", snap.State, snap.ActiveMRID)
	}
	if got := r.statuses("B"); len(got) != 1 || got[0] != "253@/rsp/B" {
		t.Fatalf("B responses %v, want exactly [253@/rsp/B]", got)
	}

	// Poll 2: the same list. Poll 3: A removed by the server.
	r.poll(invertertest.ListResult([]sep2.DERControl{a, c}, bad))
	r.poll(invertertest.ListResult([]sep2.DERControl{c}, bad))

	if got := r.statuses("B"); len(got) != 1 {
		t.Errorf("B responses after 3 polls %v, want one 253", got)
	}
	if hasTrail(r.trail, "B EVENT_CANCELLED") {
		t.Errorf("rejected B reported as cancelled; trail %v", r.trail)
	}
	if !hasTrail(r.trail, "A EVENT_CANCELLED") {
		t.Errorf("removal of A not seen as a cancellation; trail %v", r.trail)
	}
	if got := r.statuses("A"); len(got) != 2 || got[0] != "1@/rsp/A" || got[1] != "6@/rsp/A" {
		t.Errorf("A responses %v, want [1@/rsp/A 6@/rsp/A]", got)
	}
	if snap := r.sm.Current(); snap.ActiveMRID != "C" {
		t.Errorf("after A's removal the active mRID is %q, want C", snap.ActiveMRID)
	}
}

// When the same mRID is both a control and a rejected item, the rejection
// wins: the control never starts, it gets one 253, and the device runs the
// DefaultDERControl, or no control when there is none.
func TestStepEvents_RejectionWinsAndTheDeviceFallsBack(t *testing.T) {
	r := newListRejectRig()
	mask := sep2.HexBinary8(0x03)
	x := buildControl("X", "/rsp/X", &mask, -time.Minute, 3600)
	target := sep2.SignedPerCent(2500)
	x.DERControlBase = &sep2.DERControlBase{OpModFixedW: &target}

	for i := 0; i < 3; i++ {
		r.poll(invertertest.ListResult([]sep2.DERControl{x}, invertertest.Rejected("X", "/rsp/X", 0x03)))
	}
	if got := r.statuses("X"); len(got) != 1 || got[0] != "253@/rsp/X" {
		t.Errorf("X responses %v, want exactly [253@/rsp/X]", got)
	}
	if hasTrail(r.trail, "X EVENT_STARTED") || hasTrail(r.trail, "X EVENT_RECEIVED") {
		t.Errorf("rejected X was scheduled; trail %v", r.trail)
	}
	snap := r.sm.Current()
	if snap.State != inverter.StateDefault {
		t.Fatalf("state %s, want DEFAULT", snap.State)
	}
	def := &sep2.DefaultDERControl{DERControlBase: &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: 700}}}
	if got := inverter.ActiveControlBase(snap, def); got != def.DERControlBase {
		t.Errorf("applied base %+v, want the default's", got)
	}
	if got := inverter.ActiveControlBase(snap, nil); got != nil {
		t.Errorf("applied base %+v with no default, want none", got)
	}
}

// A rejected item with no mRID cannot be answered: it gets no Response and
// is logged, and two such items are both logged rather than merged.
func TestStepEvents_RejectedItemWithNoMRIDIsLoggedOnly(t *testing.T) {
	logs := captureSessionLog(t)
	r := newListRejectRig()
	one := invertertest.Rejected("", "/rsp/1", 0x03)
	one.Err = errors.New("first bad item")
	two := invertertest.Rejected("", "/rsp/2", 0x03)
	two.Err = errors.New("second bad item")

	r.poll(invertertest.ListResult(nil, one, two))
	if calls := r.poster.Calls(); len(calls) != 0 {
		t.Errorf("posted %d Responses for items with no mRID, want 0", len(calls))
	}
	out := logs.String()
	for _, want := range []string{"first bad item", "second bad item"} {
		if n := strings.Count(out, "no mRID, no response possible: "+want); n != 1 {
			t.Errorf("%q logged %d times, want 1; log:\n%s", want, n, out)
		}
	}
}

// No 253 when responseRequired bit 1 is clear.
func TestStepEvents_RejectedWithoutBit1PostsNothing(t *testing.T) {
	r := newListRejectRig()
	r.poll(invertertest.ListResult(nil, invertertest.Rejected("B", "/rsp/B", 0x01)))
	if calls := r.poster.Calls(); len(calls) != 0 {
		t.Errorf("posted %d Responses with responseRequired 0x01, want 0", len(calls))
	}
	if !hasTrail(r.trail, "B EVENT_REJECTED") {
		t.Errorf("B not rejected; trail %v", r.trail)
	}
}

// A managed device polling a list that holds one undecodable control keeps
// following the valid control, posts one 253 for the bad one, and never
// logs a poll failure.
func TestManagedControls_UndecodableControlIsRejectedAndTheListKept(t *testing.T) {
	logs := captureSessionLog(t)
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.addBadControl()
	fleet, _, _ := startControlFleet(t, client, "follow")

	waitFor(t, "every device started CTL1", func() bool { return allStarted(fleet) })
	for _, l := range []string{pvLFDI, batLFDI} {
		waitFor(t, "a 253 from "+l, func() bool { return len(srv.statusesFor(l, "BAD")) > 0 })
	}
	time.Sleep(200 * time.Millisecond)
	for _, l := range []string{pvLFDI, batLFDI} {
		if got := srv.statusesFor(l, "BAD"); len(got) != 1 || got[0] != 253 {
			t.Errorf("%s posted %v for BAD, want exactly [253]", l, got)
		}
		if got := srv.statusesFor(l, "CTL1"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Errorf("%s posted %v for CTL1, want [1 2]", l, got)
		}
		if m := deviceByLFDI(fleet, l).ctl.sm.Current().ActiveMRID; m != "CTL1" {
			t.Errorf("%s active mRID %q, want CTL1", l, m)
		}
	}
	if out := logs.String(); strings.Contains(out, "DERControlList poll failed") {
		t.Errorf("a poll logged a failure:\n%s", out)
	}
}

// An event received valid that comes back undecodable under the same mRID
// is answered with 253 only: the server did not cancel it, so no 6. It
// stops applying, and the device falls back to the default.
func TestStepEvents_ValidEventTurnedUndecodablePostsNoCancel(t *testing.T) {
	mask := sep2.HexBinary8(0x03)
	target := sep2.SignedPerCent(2500)
	cases := []struct {
		name  string
		start time.Duration
		other time.Duration // start of a second event Y, 0 for none
		want  []string
	}{
		{"active", -time.Minute, 0, []string{"1@/rsp/X", "2@/rsp/X", "253@/rsp/X"}},
		{"pending head", 10 * time.Minute, 20 * time.Minute, []string{"1@/rsp/X", "253@/rsp/X"}},
		{"pending behind the head", 20 * time.Minute, 10 * time.Minute, []string{"253@/rsp/X"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newListRejectRig()
			x := buildControl("X", "/rsp/X", &mask, tc.start, 3600)
			x.DERControlBase = &sep2.DERControlBase{OpModFixedW: &target}
			controls := []sep2.DERControl{x}
			var rest []sep2.DERControl
			if tc.other != 0 {
				y := buildControl("Y", "/rsp/Y", &mask, tc.other, 300)
				controls = append(controls, y)
				rest = append(rest, y)
			}
			r.poll(invertertest.ListResult(controls))
			for i := 0; i < 2; i++ {
				r.poll(invertertest.ListResult(rest, invertertest.Rejected("X", "/rsp/X", 0x03)))
			}
			if got := r.statuses("X"); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("X responses %v, want %v", got, tc.want)
			}
			snap := r.sm.Current()
			if snap.ActiveMRID == "X" {
				t.Fatalf("X still active (%s)", snap.State)
			}
			if tc.other == 0 {
				if snap.State != inverter.StateDefault {
					t.Errorf("state %s, want DEFAULT", snap.State)
				}
				def := &sep2.DefaultDERControl{DERControlBase: &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: 700}}}
				if got := inverter.ActiveControlBase(snap, def); got != def.DERControlBase {
					t.Errorf("applied base %+v, want the default's", got)
				}
			} else if snap.ActiveMRID != "Y" {
				t.Errorf("active mRID %q, want Y", snap.ActiveMRID)
			}
		})
	}
}

// A bad item the server has already cancelled is not answered with 253.
func TestStepEvents_CancelledUndecodableItemPostsNo253(t *testing.T) {
	r := newListRejectRig()
	bad := invertertest.Rejected("B", "/rsp/B", 0x03)
	bad.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusCancelled}
	r.poll(invertertest.ListResult(nil, bad))
	r.poll(invertertest.ListResult(nil, bad))
	if calls := r.poster.Calls(); len(calls) != 0 {
		t.Errorf("posted %d Responses for a cancelled bad item, want 0", len(calls))
	}
	if hasTrail(r.trail, "B EVENT_REJECTED") {
		t.Errorf("cancelled B was rejected; trail %v", r.trail)
	}
}
