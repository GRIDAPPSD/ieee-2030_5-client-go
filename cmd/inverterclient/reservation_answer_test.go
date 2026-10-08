package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// The request the answer tests follow: start t0+2400 s, one hour, grace 60 s,
// so the window is [t0+2400, t0+6000) and the on-time cutoff t0+2460.

func windowIV(r *reserver) *sep2.DateTimeInterval {
	return &sep2.DateTimeInterval{Start: r.cur.start.Unix(), Duration: 3600}
}

func needAck(g sep2.FlowReservationResponse, replyTo string) sep2.FlowReservationResponse {
	v := sep2.HexBinary8(0x01)
	g.ResponseRequired = &v
	g.ReplyTo = replyTo
	return g
}

func startedReserver(t *testing.T, mutate func(*simconfig.Frq)) (*reserver, *fakeFrq, *clock) {
	t.Helper()
	f := &fakeFrq{}
	clk := &clock{t0}
	r := testReserver(t, f, clk, mutate)
	r.Trigger(context.Background())
	if r.cur == nil {
		t.Fatal("the request was not posted")
	}
	return r, f, clk
}

func at(r *reserver, secondsFromStart int) time.Time {
	return r.cur.start.Add(time.Duration(secondsFromStart) * time.Second)
}

// A grant whose first answer is created at the start (the server's own
// deadline answer) is on time: the request stays pending until the grace has
// passed, and the grant is dispatched once read.
func TestAnswer_FallbackAtTheStartIsOnTime(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	clk.t = at(r, 0)
	r.Poll(context.Background())
	if r.cur.kind != answerPending || r.disp.terms != nil {
		t.Fatalf("no answer at the start: kind %v terms %v, want pending and no grant", r.cur.kind, r.disp.terms)
	}
	g := resp("FALLBACK", r.cur.mrid, at(r, 3).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000)
	f.list = listOf(g)
	clk.t = at(r, 45)
	r.Poll(context.Background())
	if r.cur.kind != answerGranted || r.disp.terms == nil || r.disp.terms.EnergyWh != 6000 || r.disp.terms.PowerW != 3000 {
		t.Fatalf("kind %v terms %+v, want the 6000 Wh 3000 W grant", r.cur.kind, r.disp.terms)
	}
}

// No answer by start + grace is "expired", but it is read again: an answer
// created inside the grace that shows up later still counts.
func TestAnswer_ExpiredIsReJudgedWhenAnOnTimeAnswerAppears(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	clk.t = at(r, 60)
	r.Poll(context.Background())
	if r.cur.kind != answerExpired || r.disp.terms != nil {
		t.Fatalf("kind %v terms %v, want expired and no grant", r.cur.kind, r.disp.terms)
	}
	getsBefore := f.gets
	f.list = listOf(resp("SLOW", r.cur.mrid, at(r, 59).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = at(r, 90)
	r.Poll(context.Background())
	if f.gets == getsBefore {
		t.Fatal("an expired request is no longer read")
	}
	if r.cur.kind != answerGranted || r.disp.terms == nil || r.disp.terms.ResponseMRID != "SLOW" {
		t.Fatalf("kind %v terms %+v, want the late-arriving on-time grant", r.cur.kind, r.disp.terms)
	}
}

// A first answer created after start + grace is not ignored: it is acted on
// for the rest of its window (IEEE 2030.5-2018 10.2.3.3 m).
func TestAnswer_FirstAnswerAfterTheGraceIsActedOnForTheRestOfTheWindow(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	clk.t = at(r, 10)
	r.Poll(context.Background())
	f.list = listOf(resp("LATE", r.cur.mrid, at(r, 65).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = at(r, 80)
	r.Poll(context.Background())
	if r.cur.kind != answerLate {
		t.Errorf("kind %v, want late", r.cur.kind)
	}
	if r.disp.terms == nil || r.disp.terms.EnergyWh != 6000 || r.disp.terms.ResponseMRID != "LATE" {
		t.Fatalf("terms %+v, want the late grant dispatched", r.disp.terms)
	}
	if !r.disp.terms.End.Equal(at(r, 3600)) {
		t.Errorf("late grant ends %v, want the window end %v", r.disp.terms.End, at(r, 3600))
	}
}

// A late first answer that is a denial dispatches nothing.
func TestAnswer_LateDenialDispatchesNothing(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	f.list = listOf(resp("D", r.cur.mrid, at(r, 500).Unix(), sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: r.cur.start.Unix()}, 0, 0))
	clk.t = at(r, 600)
	r.Poll(context.Background())
	if r.cur.kind != answerDenied || r.disp.terms != nil {
		t.Fatalf("kind %v terms %v, want denied and no grant", r.cur.kind, r.disp.terms)
	}
}

// An unreadable or truncated list is not an empty one.
func TestAnswer_TruncatedListLeavesTheStateUnchanged(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	clk.t = at(r, 10)
	r.Poll(context.Background())
	g := resp("G", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000)
	f.list = listOf(g)
	f.list.All = 300
	clk.t = at(r, 100)
	r.Poll(context.Background())
	if r.cur.kind != answerPending || r.disp.terms != nil {
		t.Fatalf("kind %v terms %v after a truncated list, want pending and no grant", r.cur.kind, r.disp.terms)
	}
	f.list.All = 1
	clk.t = at(r, 200)
	r.Poll(context.Background())
	if r.cur.kind != answerGranted {
		t.Errorf("kind %v once the list is complete, want granted", r.cur.kind)
	}
}

// A grant cancelled with no replacement is revoked: nothing is dispatched,
// the state is not "expired", and the request keeps being read.
func TestAnswer_CancelledGrantIsRevokedAndPollingContinues(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	g := resp("G", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000)
	f.list = listOf(g)
	clk.t = at(r, 100)
	r.Poll(context.Background())
	if r.disp.terms == nil {
		t.Fatal("grant not installed")
	}
	g.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusCancelled}
	f.list = listOf(g)
	clk.t = at(r, 200)
	r.Poll(context.Background())
	if r.cur.kind != answerRevoked {
		t.Errorf("kind %v, want revoked", r.cur.kind)
	}
	if r.disp.terms != nil {
		t.Errorf("terms %+v still installed after the grant was cancelled", r.disp.terms)
	}
	gets := f.gets
	clk.t = at(r, 400)
	r.Poll(context.Background())
	if f.gets != gets+1 {
		t.Errorf("reads %d -> %d, want the revoked request read again", gets, f.gets)
	}
}

// A denial is not final: a newer grant on the same subject is followed.
func TestAnswer_DenialThenNewerGrantIsGranted(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	d := resp("D", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: r.cur.start.Unix()}, 0, 0)
	f.list = listOf(d)
	clk.t = at(r, 100)
	r.Poll(context.Background())
	if r.cur.kind != answerDenied || r.disp.terms != nil {
		t.Fatalf("kind %v terms %v, want denied and no grant", r.cur.kind, r.disp.terms)
	}
	g := resp("G2", r.cur.mrid, at(r, 150).Unix(), sep2.EventStatusScheduled, windowIV(r), 4000, 2000)
	f.list = listOf(d, g)
	clk.t = at(r, 200)
	r.Poll(context.Background())
	if r.cur.kind != answerGranted || r.disp.terms == nil || r.disp.terms.ResponseMRID != "G2" || r.disp.terms.EnergyWh != 4000 {
		t.Fatalf("kind %v terms %+v, want the newer 4000 Wh grant", r.cur.kind, r.disp.terms)
	}
}

// Reads stop at the end of the window plus the grace, in every state.
func TestAnswer_ReadsStopAtTheEndPlusGrace(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	f.list = listOf(resp("G", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = at(r, 100)
	r.Poll(context.Background())
	clk.t = at(r, 3600+59)
	r.Poll(context.Background())
	gets := f.gets
	clk.t = at(r, 3600+60)
	r.Poll(context.Background())
	clk.t = at(r, 3600+500)
	r.Poll(context.Background())
	if f.gets != gets {
		t.Errorf("%d reads at or after the end plus grace, want none", f.gets-gets)
	}
	if r.disp.terms != nil {
		t.Errorf("terms %+v still installed after the request finished", r.disp.terms)
	}
}

// A new request drops the old grant and the energy counted under it.
func TestTrigger_NewRequestDropsTheOldGrantAndDelivered(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	f.list = listOf(resp("G", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000))
	clk.t = at(r, 100)
	r.Poll(context.Background())
	r.disp.deliveredWh = 123
	r.Trigger(context.Background())
	if r.disp.terms != nil || r.disp.deliveredWh != 0 {
		t.Errorf("terms %+v delivered %v after a new request, want nothing carried over", r.disp.terms, r.disp.deliveredWh)
	}
}

// A discharge request answered with a positive energy dispatches a discharge:
// the direction comes from the request.
func TestAnswer_DischargeRequestAnsweredPositiveDispatchesDischarge(t *testing.T) {
	r, f, clk := startedReserver(t, func(c *simconfig.Frq) { c.EnergyWh = -3000; c.PowerW = 3000 })
	f.list = listOf(resp("G", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 3000, 3000))
	clk.t = at(r, 100)
	r.Poll(context.Background())
	if r.disp.terms == nil || r.disp.terms.EnergyWh != -3000 || r.disp.terms.PowerW != 3000 {
		t.Fatalf("terms %+v, want -3000 Wh at 3000 W in the request's direction", r.disp.terms)
	}
	b := &fakeBattery{rated: 6000, soc: 0.5}
	fleet, _ := testFleet(clk, batteryDev("b", b))
	fleet.dispatch = r.disp
	fleet.Tick(context.Background())
	if len(b.commands) != 1 || b.commands[0] != 3000 {
		t.Errorf("battery commands %v, want one of +3000 W (discharging)", b.commands)
	}
}

// A grant above the request is dispatched at no more than was asked.
func TestAnswer_GrantAboveTheRequestIsClipped(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	f.list = listOf(resp("G", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 9000, 5000))
	clk.t = at(r, 100)
	r.Poll(context.Background())
	if r.disp.terms == nil || r.disp.terms.EnergyWh != 6000 || r.disp.terms.PowerW != 3000 {
		t.Fatalf("terms %+v, want clipped to the requested 6000 Wh and 3000 W", r.disp.terms)
	}
}

// An interval wider than the window is clipped to it; one outside it, or a
// grant whose energy or power is zero, is unusable and dispatches nothing.
func TestAnswer_GrantIntervalAndTermsAreReadAgainstTheRequest(t *testing.T) {
	win := func(r *reserver) (int64, int64) { return r.cur.start.Unix(), r.cur.start.Unix() + 3600 }

	r, f, clk := startedReserver(t, nil)
	s, e := win(r)
	f.list = listOf(resp("W", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: s - 600, Duration: 7200}, 6000, 3000))
	clk.t = at(r, 100)
	r.Poll(context.Background())
	if r.disp.terms == nil || r.disp.terms.Start.Unix() != s || r.disp.terms.End.Unix() != e {
		t.Fatalf("terms %+v, want the interval clipped to [%d, %d)", r.disp.terms, s, e)
	}

	for name, g := range map[string]func(*reserver) sep2.FlowReservationResponse{
		"entirely before the window": func(r *reserver) sep2.FlowReservationResponse {
			return resp("X", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: s - 7200, Duration: 3600}, 6000, 3000)
		},
		"entirely after the window": func(r *reserver) sep2.FlowReservationResponse {
			return resp("X", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: e + 10, Duration: 3600}, 6000, 3000)
		},
		"zero power": func(r *reserver) sep2.FlowReservationResponse {
			x := resp("X", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000)
			x.PowerAvailable = &sep2.ActivePower{}
			return x
		},
	} {
		r, f, clk := startedReserver(t, nil)
		f.list = listOf(g(r))
		clk.t = at(r, 100)
		r.Poll(context.Background())
		if r.cur.kind != answerInvalid || r.disp.terms != nil {
			t.Errorf("%s: kind %v terms %+v, want invalid and nothing dispatched", name, r.cur.kind, r.disp.terms)
		}
	}
}

// A sign or magnitude mismatch is logged once per response.
func TestAnswer_MismatchIsLoggedOncePerResponse(t *testing.T) {
	buf := captureLog(t)

	r, f, clk := startedReserver(t, func(c *simconfig.Frq) { c.EnergyWh = -3000 })
	f.list = listOf(resp("G", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 3000, 3000))
	for i := 1; i <= 3; i++ {
		clk.t = at(r, 100*i)
		r.Poll(context.Background())
	}
	out := buf.String()
	if n := strings.Count(out, "read against request"); n != 1 {
		t.Errorf("mismatch logged %d times over 3 polls, want 1\n%s", n, out)
	}
	if !strings.Contains(out, "energy sign differs from the request: asked -3000 Wh, granted +3000 Wh") {
		t.Errorf("log does not carry both values\n%s", out)
	}
}

// Every response that asks for it is acknowledged, whatever its state, and
// a failed post is retried until it succeeds.
func TestAck_DenialWhoseFirstPostFailsIsAcknowledgedOnTheNextPoll(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	f.ackErr = errors.New("503")
	d := needAck(resp("D", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: r.cur.start.Unix()}, 0, 0), "/rsps/1/rsp")
	f.list = listOf(d)
	clk.t = at(r, 100)
	r.Poll(context.Background())
	f.ackErr = nil
	clk.t = at(r, 200)
	r.Poll(context.Background())
	if len(f.acks) != 1 || f.acks[0].Subject != "D" {
		t.Fatalf("acks %+v, want exactly one for response D", f.acks)
	}
	clk.t = at(r, 300)
	r.Poll(context.Background())
	if len(f.acks) != 1 {
		t.Errorf("%d acks after a third poll, want still 1", len(f.acks))
	}
}

func TestAck_SupersededAndCancelledResponsesAreAcknowledgedToo(t *testing.T) {
	r, f, clk := startedReserver(t, nil)
	old := needAck(resp("OLD", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusSuperseded, windowIV(r), 6000, 3000), "/rsps/1/rsp")
	dead := needAck(resp("DEAD", r.cur.mrid, at(r, 2).Unix(), sep2.EventStatusCancelled, windowIV(r), 6000, 3000), "/rsps/2/rsp")
	live := needAck(resp("LIVE", r.cur.mrid, at(r, 3).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000), "/rsps/3/rsp")
	f.list = listOf(old, dead, live)
	clk.t = at(r, 100)
	r.Poll(context.Background())
	got := map[string]string{}
	for i, a := range f.acks {
		got[a.Subject] = f.ackTo[i]
	}
	if len(f.acks) != 3 || got["OLD"] != "/rsps/1/rsp" || got["DEAD"] != "/rsps/2/rsp" || got["LIVE"] != "/rsps/3/rsp" {
		t.Errorf("acks %v, want one each for OLD, DEAD and LIVE to their own replyTo", got)
	}
}

// A response that asks for an acknowledgement but cannot receive one is
// logged once and nothing is posted.
func TestAck_RequiredButNoReplyToIsLoggedOnce(t *testing.T) {
	buf := captureLog(t)

	r, f, clk := startedReserver(t, nil)
	f.list = listOf(needAck(resp("NOREPLY", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000), ""))
	for i := 1; i <= 3; i++ {
		clk.t = at(r, 100*i)
		r.Poll(context.Background())
	}
	if len(f.acks) != 0 {
		t.Errorf("%d acks posted with no replyTo", len(f.acks))
	}
	if n := strings.Count(buf.String(), "NOREPLY has no replyTo"); n != 1 {
		t.Errorf("logged %d times over 3 polls, want 1\n%s", n, buf.String())
	}
}

// An acknowledgement still owed when the request finishes is logged once.
func TestAck_UnacknowledgedAtTheEndIsLogged(t *testing.T) {
	buf := captureLog(t)

	r, f, clk := startedReserver(t, nil)
	f.ackErr = errors.New("503")
	f.list = listOf(needAck(resp("OWED", r.cur.mrid, at(r, 1).Unix(), sep2.EventStatusScheduled, windowIV(r), 6000, 3000), "/rsps/1/rsp"))
	clk.t = at(r, 100)
	r.Poll(context.Background())
	clk.t = at(r, 3600+60)
	r.Poll(context.Background())
	clk.t = at(r, 3600+400)
	r.Poll(context.Background())
	if n := strings.Count(buf.String(), "never acknowledged: OWED"); n != 1 {
		t.Errorf("owed acknowledgement logged %d times at the end, want 1\n%s", n, buf.String())
	}
}
