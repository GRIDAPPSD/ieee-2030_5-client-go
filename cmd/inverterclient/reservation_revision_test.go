package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// A revised answer replaces the grant from its own interval start, and the
// energy already moved under the earlier answer counts against the new one.
// The revision starts later than the grant it replaces, so between the two
// the batteries run free: the old grant no longer applies.
func TestRevision_ReplacesTheGrantFromItsIntervalStartAndCarriesDeliveredEnergy(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	b := &fakeBattery{rated: 6000, soc: 0.5, baselineW: -500}
	fleet, _ := testFleet(clk, batteryDev("b", b))
	fleet.dispatch = r.disp
	ctx := context.Background()
	s := r.cur.start

	g1 := resp("G1", r.cur.mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000)
	f.list = listOf(g1)
	clk.t = s
	r.Poll(ctx)
	fleet.Tick(ctx) // enters the grant: -3000 W (charging 3000 W)
	clk.t = s.Add(600 * time.Second)
	fleet.Tick(ctx)
	if got := b.commands[len(b.commands)-1]; got != -3000 {
		t.Fatalf("under the first grant the battery was commanded %v W, want -3000", got)
	}
	if !near(r.disp.deliveredWh, 500, 0.01) {
		t.Fatalf("delivered %v Wh after 600 s at 3000 W, want 500", r.disp.deliveredWh)
	}

	// G2: 700 Wh, from s+1200 to the end of the window.
	g2 := resp("G2", r.cur.mrid, s.Unix()-500, sep2.EventStatusScheduled,
		&sep2.DateTimeInterval{Start: s.Unix() + 1200, Duration: 2400}, 700, 3000)
	f.list = listOf(g1, g2)
	clk.t = s.Add(601 * time.Second)
	r.Poll(ctx)
	if r.disp.terms == nil || r.disp.terms.ResponseMRID != "G2" || r.disp.terms.EnergyWh != 700 || !r.disp.terms.Start.Equal(s.Add(1200*time.Second)) {
		t.Fatalf("terms %+v, want G2: 700 Wh from s+1200", r.disp.terms)
	}

	// Before G2's start the first grant is gone: the battery runs free.
	clk.t = s.Add(660 * time.Second)
	fleet.Tick(ctx)
	if got := b.commands[len(b.commands)-1]; got != -500 {
		t.Errorf("between the revision and its start the battery was commanded %v W, want its baseline -500", got)
	}
	// The 60 s since the last tick were still under G1 and are counted.
	if !near(r.disp.deliveredWh, 550, 0.01) {
		t.Errorf("delivered %v Wh, want 550 carried over", r.disp.deliveredWh)
	}

	// From G2's start: 700 - 550 = 150 Wh left over 2400 s is 225 W.
	clk.t = s.Add(1200 * time.Second)
	fleet.Tick(ctx)
	if got := b.commands[len(b.commands)-1]; !near(got, -225, 0.01) {
		t.Errorf("under the revision the battery was commanded %v W, want -225 (150 Wh left over 2400 s)", got)
	}
	if !near(r.disp.deliveredWh, 550, 0.01) {
		t.Errorf("delivered %v Wh at the revision's start, want 550 unchanged", r.disp.deliveredWh)
	}
}

// A cancelled grant returns every dispatched battery to its own baseline on
// the first dispatch tick after the poll that reads the cancellation.
func TestCancellation_ReturnsEveryDeviceToBaselineWithinOneTick(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	big := &fakeBattery{rated: 6000, soc: 0.5, baselineW: -500}
	small := &fakeBattery{rated: 2000, soc: 0.5, baselineW: 700}
	fleet, _ := testFleet(clk, batteryDev("big", big), batteryDev("small", small))
	fleet.dispatch = r.disp
	ctx := context.Background()
	s := r.cur.start

	g := resp("G", r.cur.mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000)
	f.list = listOf(g)
	clk.t = s.Add(time.Minute)
	r.Poll(ctx)
	fleet.Tick(ctx)
	if big.commands[0] != -2250 || small.commands[0] != -750 {
		t.Fatalf("dispatched %v and %v, want -2250 and -750 (3000 W split 6000:2000)", big.commands, small.commands)
	}

	g.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusCancelled}
	f.list = listOf(g)
	clk.t = s.Add(2 * time.Minute)
	r.Poll(ctx)
	clk.t = s.Add(2*time.Minute + 5*time.Second)
	fleet.Tick(ctx)
	if got := big.commands[len(big.commands)-1]; got != -500 {
		t.Errorf("big commanded %v W after the cancellation, want its baseline -500", got)
	}
	if got := small.commands[len(small.commands)-1]; got != 700 {
		t.Errorf("small commanded %v W after the cancellation, want its baseline 700", got)
	}
	if r.disp.lastAchieved != -200 {
		t.Errorf("fleet achieved %v W charging, want -200 (the baselines, no longer the grant)", r.disp.lastAchieved)
	}
}

// Every answer change is logged with the request mRID and the mRID of the
// response it rests on: a grant, a revision, a cancellation, and a second
// cancellation by a new response. A denial is covered in
// TestAnswerChanges_DenialIsLoggedWithRequestAndResponseMRIDs.
func TestAnswerChanges_AreLoggedWithRequestAndResponseMRIDs(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	buf := captureLog(t)
	ctx := context.Background()
	s := r.cur.start
	mrid := r.cur.mrid
	poll := func(sec int, rs ...sep2.FlowReservationResponse) string {
		buf.Reset()
		f.list = listOf(rs...)
		clk.t = s.Add(time.Duration(sec) * time.Second)
		r.Poll(ctx)
		return buf.String()
	}
	wantLine := func(out string, parts ...string) {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			ok := true
			for _, p := range parts {
				ok = ok && strings.Contains(line, p)
			}
			if ok {
				return
			}
		}
		t.Errorf("no log line holding %q in:\n%s", parts, out)
	}

	g1 := resp("G1", mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000)
	wantLine(poll(100, g1), "request "+mrid, "response G1", "granted")

	if out := poll(200, g1); out != "" {
		t.Errorf("an unchanged answer was logged again: %q", out)
	}

	g2 := resp("G2", mrid, s.Unix()-500, sep2.EventStatusScheduled, windowIV(r), 2000, 1000)
	wantLine(poll(300, g1, g2), "request "+mrid, "response G2", "2000 Wh")

	g2c := g2
	g2c.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusCancelled}
	g1c := g1
	g1c.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusSuperseded}
	wantLine(poll(400, g1c, g2c), "request "+mrid, "response G2", "revoked")

	// A new cancelled response is a new answer even though the state is
	// unchanged.
	g3 := g2c
	g3.MRID, g3.CreationTime = "G3", g2c.CreationTime+10
	wantLine(poll(500, g1c, g2c, g3), "request "+mrid, "response G3", "revoked")
}

// The aggregator's withdrawal is the request as posted with RequestStatus
// cancelled, PUT to the Location the server gave it. Nothing else differs.
func TestWithdraw_PutsTheRequestAsPostedWithStatusCancelled(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	f.location = "/edev/1/frq/77"
	r.Trigger(context.Background())
	posted := f.posted[len(f.posted)-1]
	f.puts, f.putHref = nil, nil // the replacement above withdrew the first request

	clk.t = clk.t.Add(90 * time.Second)
	r.Withdraw(context.Background())

	if len(f.puts) != 1 || f.putHref[0] != "/edev/1/frq/77" {
		t.Fatalf("puts %d to %v, want one to /edev/1/frq/77", len(f.puts), f.putHref)
	}
	got := f.puts[0]
	if got.RequestStatus.RequestStatus != sep2.RequestStatusCancelled || got.RequestStatus.DateTime != clk.t.Unix() {
		t.Errorf("RequestStatus %+v, want cancelled at %d", got.RequestStatus, clk.t.Unix())
	}
	if got.MRID != posted.MRID || *got.EnergyRequested != *posted.EnergyRequested ||
		*got.PowerRequested != *posted.PowerRequested || *got.IntervalRequested != *posted.IntervalRequested {
		t.Errorf("withdrawal %+v differs from the posted request %+v in more than RequestStatus", got, posted)
	}
	if !r.cur.withdrawn {
		t.Error("the request is not marked withdrawn")
	}

	// Withdrawn once: asking again sends nothing.
	r.Withdraw(context.Background())
	if len(f.puts) != 1 {
		t.Errorf("%d PUTs after a second Withdraw, want still 1", len(f.puts))
	}
}

// Withdrawing drops the grant, so the devices go back to baseline on the
// next tick; a withdrawal the server refuses keeps it.
func TestWithdraw_DropsTheGrantOnlyWhenTheServerAccepted(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	s := r.cur.start
	f.list = listOf(resp("G", r.cur.mrid, s.Unix()-1000, sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = s.Add(time.Minute)
	r.Poll(context.Background())
	if r.disp.terms == nil {
		t.Fatal("grant not installed")
	}

	f.putErr = errors.New("503")
	r.Withdraw(context.Background())
	if r.disp.terms == nil || r.cur.withdrawn {
		t.Errorf("a refused withdrawal dropped the grant (terms %v) or marked the request withdrawn (%v)", r.disp.terms, r.cur.withdrawn)
	}

	f.putErr = nil
	r.Withdraw(context.Background())
	if r.disp.terms != nil || !r.cur.withdrawn {
		t.Errorf("after an accepted withdrawal terms %v withdrawn %v, want no grant and withdrawn", r.disp.terms, r.cur.withdrawn)
	}
}

// A new Trigger gives the earlier request's capacity back: the earlier
// request is withdrawn after the new one is posted, and not at all when the
// new post fails.
func TestTrigger_WithdrawsTheRequestItReplaces(t *testing.T) {
	r, f, _ := startedReserver(t, nil)
	f.location = "/edev/1/frq/2"
	first := r.cur.mrid

	f.postErr = errors.New("503")
	r.Trigger(context.Background())
	if len(f.puts) != 0 || r.cur.mrid != first {
		t.Fatalf("a failed post withdrew the earlier request (%d PUTs) or changed what is followed", len(f.puts))
	}

	f.postErr = nil
	r.Trigger(context.Background())
	if len(f.puts) != 1 {
		t.Fatalf("%d PUTs after the replacement, want 1", len(f.puts))
	}
	if f.puts[0].MRID != first || f.puts[0].RequestStatus.RequestStatus != sep2.RequestStatusCancelled || f.putHref[0] != "/edev/1/frq/1" {
		t.Errorf("withdrew %q (%+v) at %q, want the first request %q cancelled at its own href /edev/1/frq/1",
			f.puts[0].MRID, f.puts[0].RequestStatus, f.putHref[0], first)
	}
	if r.cur.mrid == first || r.cur.href != "/edev/1/frq/2" {
		t.Errorf("following %q at %q, want the new request at /edev/1/frq/2", r.cur.mrid, r.cur.href)
	}
}

func TestWithdraw_NothingToWithdrawSendsNothing(t *testing.T) {
	f := &fakeFrq{}
	r := testReserver(t, f, &clock{t0}, nil)
	r.Withdraw(context.Background())
	var nilr *reserver
	nilr.Withdraw(context.Background())
	if len(f.puts) != 0 {
		t.Errorf("%d PUTs with no request", len(f.puts))
	}
}
