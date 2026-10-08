package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

const aggLFDI = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// fakeFrq stands in for the SEP2 client: it keeps what was posted and
// serves a settable response list.
type fakeFrq struct {
	posted  []sep2.FlowReservationRequest
	postErr error
	list    sep2.FlowReservationResponseList
	getErr  error
	gets    int
	acks    []sep2.FlowReservationResponseResponse
	ackTo   []string
	ackErr  error
}

func (f *fakeFrq) PostFlowReservationRequest(_ context.Context, _ string, req sep2.FlowReservationRequest) (string, error) {
	if f.postErr != nil {
		return "", f.postErr
	}
	f.posted = append(f.posted, req)
	return "/edev/1/frq/1", nil
}

func (f *fakeFrq) GetFlowReservationResponses(context.Context, string) (sep2.FlowReservationResponseList, error) {
	f.gets++
	return f.list, f.getErr
}

func (f *fakeFrq) PostFlowReservationResponseResponse(_ context.Context, to string, r sep2.FlowReservationResponseResponse) error {
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acks = append(f.acks, r)
	f.ackTo = append(f.ackTo, to)
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func testReserver(t *testing.T, f *fakeFrq, clk *clock, cfgMutate func(*simconfig.Frq)) *reserver {
	t.Helper()
	cfg := simconfig.Frq{
		EnergyWh: 6000, PowerW: 3000, StartInS: 2400, DurationS: 3600, MinLeadS: 60, PollS: 30,
		RequestFile: filepath.Join(t.TempDir(), "frq-request.json"),
	}
	if cfgMutate != nil {
		cfgMutate(&cfg)
	}
	edev := sep2.EndDevice{
		FlowReservationRequestListLink:  &sep2.ListLink{Href: "/edev/1/frq"},
		FlowReservationResponseListLink: &sep2.ListLink{Href: "/edev/1/frp"},
	}
	r, err := newReserver(f, aggLFDI, edev, cfg, newDispatcher(5*time.Second), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewReserver_NeedsBothLinks(t *testing.T) {
	_, err := newReserver(&fakeFrq{}, aggLFDI, sep2.EndDevice{FlowReservationRequestListLink: &sep2.ListLink{Href: "/frq"}}, simconfig.Frq{}, newDispatcher(time.Second), time.Now)
	if err == nil {
		t.Error("reserver built without a FlowReservationResponseListLink")
	}
}

// SIGUSR1 posts one request from the config defaults, overlaid by the
// request file, which is then deleted; the request carries the values, the
// interval from now, and an mRID that is fresh each time.
func TestTrigger_PostsOverlaidRequestAndDeletesTheFile(t *testing.T) {
	f := &fakeFrq{}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	if err := os.WriteFile(r.cfg.RequestFile, []byte(`{"energy_wh": 4000, "power_w": 2000, "start_in_s": 3000}`), 0o600); err != nil {
		t.Fatal(err)
	}

	r.Trigger(context.Background())

	if len(f.posted) != 1 {
		t.Fatalf("%d requests posted, want 1", len(f.posted))
	}
	req := f.posted[0]
	if req.EnergyRequested.Value != 4000 || req.PowerRequested.Value != 2000 {
		t.Errorf("energy %d power %d, want the overlay's 4000 and 2000", req.EnergyRequested.Value, req.PowerRequested.Value)
	}
	if iv := req.IntervalRequested; iv.Start != t0.Unix()+3000 || iv.Duration != 3600 {
		t.Errorf("interval = %+v, want start %d (overlay) duration 3600 (config default)", iv, t0.Unix()+3000)
	}
	if len(req.MRID) != 32 {
		t.Errorf("mRID %q is not a 128-bit hex mRID", req.MRID)
	}
	if _, err := os.Stat(r.cfg.RequestFile); !os.IsNotExist(err) {
		t.Errorf("request file not deleted: %v", err)
	}

	// With no file, the config defaults post, under a fresh mRID.
	r.Trigger(context.Background())
	if len(f.posted) != 2 {
		t.Fatalf("%d requests posted, want 2", len(f.posted))
	}
	second := f.posted[1]
	if second.EnergyRequested.Value != 6000 || second.PowerRequested.Value != 3000 {
		t.Errorf("defaults: energy %d power %d, want 6000 and 3000", second.EnergyRequested.Value, second.PowerRequested.Value)
	}
	if second.MRID == req.MRID {
		t.Errorf("two requests share mRID %s", second.MRID)
	}
	if r.cur.mrid != second.MRID {
		t.Error("the reserver is not following the newest request")
	}
}

func TestTrigger_RefusesLocallyAndPostsNothing(t *testing.T) {
	cases := map[string]struct {
		file   string
		mutate func(*simconfig.Frq)
	}{
		"start too soon":    {file: `{"start_in_s": 30}`},
		"malformed file":    {file: `{`},
		"misspelt key":      {file: `{"energyy_wh": 1}`},
		"config start soon": {mutate: func(c *simconfig.Frq) { c.StartInS = 60 }},
	}
	for name, c := range cases {
		f := &fakeFrq{}
		r := testReserver(t, f, &clock{t0}, c.mutate)
		if c.file != "" {
			if err := os.WriteFile(r.cfg.RequestFile, []byte(c.file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r.Trigger(context.Background())
		if len(f.posted) != 0 || r.cur != nil {
			t.Errorf("%s: posted %d requests (following %v), want none", name, len(f.posted), r.cur != nil)
		}
	}
}

func TestTrigger_FailedPostIsNotFollowed(t *testing.T) {
	f := &fakeFrq{postErr: errors.New("503")}
	r := testReserver(t, f, &clock{t0}, nil)
	r.Trigger(context.Background())
	if r.cur != nil {
		t.Error("a request the server never accepted is being followed")
	}
}

func startOf(r *reserver) time.Time { return r.cur.start }

func grantFor(r *reserver, mrid string, wh int64, w int16) sep2.FlowReservationResponse {
	s := startOf(r)
	return resp("RESP-"+mrid, mrid, s.Unix()-1000, sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: s.Unix(), Duration: 3600}, wh, w)
}

func TestPoll_FollowsTheAnswer(t *testing.T) {
	f := &fakeFrq{}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	r.Trigger(context.Background())
	mrid := r.cur.mrid

	// No answer before the start: pending, nothing dispatched.
	clk.t = t0.Add(time.Minute)
	r.Poll(context.Background())
	if r.cur.kind != answerPending || r.disp.terms != nil {
		t.Fatalf("kind %v terms %v, want pending and no grant", r.cur.kind, r.disp.terms)
	}

	// The poll interval holds: no read before it elapses.
	gets := f.gets
	clk.t = t0.Add(time.Minute + 10*time.Second)
	r.Poll(context.Background())
	if f.gets != gets {
		t.Errorf("polled again after 10 s with poll_s 30")
	}

	// A grant installs the terms the dispatcher will use.
	f.list = listOf(grantFor(r, mrid, 6000, 3000))
	clk.t = t0.Add(2 * time.Minute)
	r.Poll(context.Background())
	if r.cur.kind != answerGranted || r.disp.terms == nil || r.disp.terms.EnergyWh != 6000 || r.disp.terms.PowerW != 3000 {
		t.Fatalf("kind %v terms %+v, want a 6000 Wh 3000 W grant", r.cur.kind, r.disp.terms)
	}

	// A newer response replaces the grant.
	newer := grantFor(r, mrid, 2000, 1000)
	newer.MRID, newer.CreationTime = "RESP-NEWER", newer.CreationTime+500
	f.list = listOf(grantFor(r, mrid, 6000, 3000), newer)
	clk.t = t0.Add(3 * time.Minute)
	r.Poll(context.Background())
	if r.disp.terms == nil || r.disp.terms.EnergyWh != 2000 || r.disp.terms.ResponseMRID != "RESP-NEWER" {
		t.Errorf("terms = %+v, want the newer response's 2000 Wh", r.disp.terms)
	}
}

func TestPoll_DenialIsTerminalAndDispatchesNothing(t *testing.T) {
	f := &fakeFrq{}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	r.Trigger(context.Background())
	denial := resp("D", r.cur.mrid, 100, sep2.EventStatusScheduled, &sep2.DateTimeInterval{Start: r.cur.start.Unix(), Duration: 0}, 0, 0)
	f.list = listOf(denial)
	clk.t = t0.Add(time.Minute)
	r.Poll(context.Background())
	if r.cur.kind != answerDenied || r.disp.terms != nil {
		t.Fatalf("kind %v terms %v, want denied and no grant", r.cur.kind, r.disp.terms)
	}
	gets := f.gets
	clk.t = t0.Add(time.Hour)
	r.Poll(context.Background())
	if f.gets != gets {
		t.Error("a denied request is still being polled")
	}
}

func TestPoll_NoAnswerByTheStartIsNotGrantedAndALateAnswerIsIgnored(t *testing.T) {
	f := &fakeFrq{}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	r.Trigger(context.Background())

	clk.t = r.cur.start
	r.Poll(context.Background())
	if r.cur.kind != answerExpired || r.disp.terms != nil {
		t.Fatalf("kind %v terms %v, want expired and no grant", r.cur.kind, r.disp.terms)
	}
	f.list = listOf(grantFor(r, r.cur.mrid, 6000, 3000))
	clk.t = r.cur.start.Add(time.Minute)
	r.Poll(context.Background())
	if r.cur.kind != answerExpired || r.disp.terms != nil {
		t.Errorf("a late answer revived an expired request: kind %v terms %v", r.cur.kind, r.disp.terms)
	}
}

// An unreadable list is not an empty one: a failed read at the requested
// start must not expire the request.
func TestPoll_FailedReadDoesNotExpireOrChangeState(t *testing.T) {
	f := &fakeFrq{getErr: errors.New("timeout")}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	r.Trigger(context.Background())
	clk.t = r.cur.start.Add(time.Minute)
	r.Poll(context.Background())
	if r.cur.kind != answerPending {
		t.Errorf("kind %v after a failed read past the start, want pending", r.cur.kind)
	}
}

func TestPoll_AcknowledgesResponseRequiredWithItsMRIDAsSubject(t *testing.T) {
	f := &fakeFrq{}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	r.Trigger(context.Background())
	g := grantFor(r, r.cur.mrid, 6000, 3000)
	g.ReplyTo = "/rsps/7/rsp"
	rr := sep2.HexBinary8(0x01)
	g.ResponseRequired = &rr
	f.list = listOf(g)

	clk.t = t0.Add(time.Minute)
	r.Poll(context.Background())
	if len(f.acks) != 1 {
		t.Fatalf("%d acknowledgements, want 1", len(f.acks))
	}
	ack := f.acks[0]
	if ack.Subject != "RESP-"+r.cur.mrid {
		t.Errorf("subject = %q, want the response's mRID %q (not the request's)", ack.Subject, "RESP-"+r.cur.mrid)
	}
	if ack.Status == nil || *ack.Status != sep2.ResponseStatusEventReceived || ack.EndDeviceLFDI != aggLFDI {
		t.Errorf("ack status %v LFDI %q, want received from %s", ack.Status, ack.EndDeviceLFDI, aggLFDI)
	}
	if f.ackTo[0] != "/rsps/7/rsp" {
		t.Errorf("ack posted to %q, want the response's replyTo", f.ackTo[0])
	}

	// Once only.
	clk.t = t0.Add(2 * time.Minute)
	r.Poll(context.Background())
	if len(f.acks) != 1 {
		t.Errorf("%d acknowledgements after a second poll, want still 1", len(f.acks))
	}
}

func TestPoll_NoAcknowledgementUnlessRequired(t *testing.T) {
	for name, mutate := range map[string]func(*sep2.FlowReservationResponse){
		"responseRequired absent": func(g *sep2.FlowReservationResponse) { g.ResponseRequired = nil },
		"received bit clear":      func(g *sep2.FlowReservationResponse) { v := sep2.HexBinary8(0x02); g.ResponseRequired = &v },
		"required but no reply URI": func(g *sep2.FlowReservationResponse) {
			v := sep2.HexBinary8(0x01)
			g.ResponseRequired = &v
			g.ReplyTo = ""
		},
	} {
		f := &fakeFrq{}
		clk := &clock{t0}
		r := testReserver(t, f, clk, nil)
		r.Trigger(context.Background())
		g := grantFor(r, r.cur.mrid, 6000, 3000)
		g.ReplyTo = "/rsps/7/rsp"
		mutate(&g)
		f.list = listOf(g)
		clk.t = t0.Add(time.Minute)
		r.Poll(context.Background())
		if len(f.acks) != 0 {
			t.Errorf("%s: acknowledged", name)
		}
	}
}

func TestPoll_FailedAcknowledgementIsRetried(t *testing.T) {
	f := &fakeFrq{ackErr: errors.New("503")}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	r.Trigger(context.Background())
	g := grantFor(r, r.cur.mrid, 6000, 3000)
	g.ReplyTo = "/rsps/7/rsp"
	rr := sep2.HexBinary8(0x01)
	g.ResponseRequired = &rr
	f.list = listOf(g)

	clk.t = t0.Add(time.Minute)
	r.Poll(context.Background())
	f.ackErr = nil
	clk.t = t0.Add(2 * time.Minute)
	r.Poll(context.Background())
	if len(f.acks) != 1 {
		t.Errorf("%d acknowledgements after the server recovered, want 1", len(f.acks))
	}
}

func TestNilReserverIsInert(t *testing.T) {
	var r *reserver
	r.Trigger(context.Background())
	r.Poll(context.Background())
}

// --- dispatch -------------------------------------------------------------

// fakeBattery is a battery device that follows its command within its
// rating, like the replay backend, and records the last command.
type fakeBattery struct {
	rated     float64
	soc       float64
	baselineW float64 // the DER-sign output it runs at with no command
	commands  []float64
}

func (b *fakeBattery) SOC() float64 { return b.soc }

func (b *fakeBattery) ReadState(context.Context) (device.StateReading, error) {
	return device.StateReading{Grid: inverter.GridState{VoltsPU: 1, FreqHz: 60}, MaxPowerW: b.baselineW}, nil
}

func (b *fakeBattery) ApplySetpoint(_ context.Context, c inverter.ControlOutputs) (inverter.InverterState, error) {
	p := math.Max(-b.rated, math.Min(c.ActivePowerW, b.rated))
	b.commands = append(b.commands, c.ActivePowerW)
	return inverter.InverterState{ActivePowerW: p, Connected: true, Energized: true}, nil
}

// fakePV is a PV device: it delivers its fixed output whatever it is told.
type fakePV struct{ outW float64 }

func (p *fakePV) ReadState(context.Context) (device.StateReading, error) {
	return device.StateReading{Grid: inverter.GridState{VoltsPU: 1, FreqHz: 60}, MaxPowerW: p.outW}, nil
}

func (p *fakePV) ApplySetpoint(_ context.Context, c inverter.ControlOutputs) (inverter.InverterState, error) {
	return inverter.InverterState{ActivePowerW: math.Min(c.ActivePowerW, p.outW), Connected: true, Energized: true}, nil
}

// testFleet builds a fleet of fake devices with a dispatcher and a manual
// clock, without any server: its mirrors are never attempted (no list href)
// and no reading is due.
func testFleet(clk *clock, devs ...*managedDevice) (*managedFleet, *dispatcher) {
	d := newDispatcher(5 * time.Second)
	for _, md := range devs {
		md.now = clk.now
		md.lastReport = clk.t.Add(100 * time.Hour)
	}
	return &managedFleet{devices: devs, reportInterval: time.Hour, dispatch: d, now: clk.now}, d
}

func batteryDev(name string, b *fakeBattery) *managedDevice {
	return &managedDevice{name: name, lfdi: name, kind: "battery", ratedW: b.rated, dev: b}
}

func grantAt(start time.Time, d time.Duration, wh, w float64) *grantTerms {
	return &grantTerms{ResponseMRID: "G", Start: start, End: start.Add(d), EnergyWh: wh, PowerW: w}
}

// Inside a grant the batteries are commanded in proportion to rating, in
// the DER sign; PV is left alone; outside the interval everyone runs free.
func TestFleetTick_DispatchesBatteriesInProportionToRatingInsideTheGrant(t *testing.T) {
	clk := &clock{t0}
	big := &fakeBattery{rated: 6000, soc: 0.5, baselineW: -500}
	small := &fakeBattery{rated: 2000, soc: 0.5, baselineW: -500}
	pv := &fakePV{outW: 1800}
	f, d := testFleet(clk,
		batteryDev("big", big), batteryDev("small", small),
		&managedDevice{name: "pv", lfdi: "pv", kind: "pv", ratedW: 5000, dev: pv})
	d.setGrant(grantAt(t0.Add(time.Minute), time.Hour, 20000, 4000))
	ctx := context.Background()

	// Before the interval: free-running.
	f.Tick(ctx)
	if big.commands[0] != -500 || small.commands[0] != -500 {
		t.Fatalf("before the interval the batteries were commanded %v and %v, want their baseline -500", big.commands, small.commands)
	}

	clk.t = t0.Add(time.Minute)
	f.Tick(ctx)
	// 4000 W charging, split 6000:2000 = 3000 and 1000, charging = negative.
	if got := big.commands[1]; got != -3000 {
		t.Errorf("big commanded %v W, want -3000 (charging 3000 W in the DER sign)", got)
	}
	if got := small.commands[1]; got != -1000 {
		t.Errorf("small commanded %v W, want -1000", got)
	}
	if d.lastAchieved != 4000 {
		t.Errorf("fleet achieved %v W charging, want 4000 (PV excluded)", d.lastAchieved)
	}

	// After the interval: free again.
	clk.t = t0.Add(2 * time.Hour)
	f.Tick(ctx)
	if big.commands[2] != -500 {
		t.Errorf("after the interval big commanded %v, want baseline -500", big.commands[2])
	}
}

// A battery the split gave nothing (full while charging) is held at zero
// inside the grant instead of running its recording against the bound.
func TestFleetTick_FullBatteryIsHeldAtZeroInsideTheGrant(t *testing.T) {
	clk := &clock{t0}
	full := &fakeBattery{rated: 6000, soc: 1, baselineW: -5000}
	ok := &fakeBattery{rated: 2000, soc: 0.5, baselineW: 0}
	f, d := testFleet(clk, batteryDev("full", full), batteryDev("ok", ok))
	d.setGrant(grantAt(t0, time.Hour, 20000, 1500))
	f.Tick(context.Background())
	if full.commands[0] != 0 {
		t.Errorf("full battery commanded %v W, want 0", full.commands[0])
	}
	if ok.commands[0] != -1500 {
		t.Errorf("remaining battery commanded %v W, want -1500", ok.commands[0])
	}
}

// A discharge grant commands a positive (delivering) DER setpoint.
func TestFleetTick_DischargeGrantCommandsPositiveSetpoint(t *testing.T) {
	clk := &clock{t0}
	b := &fakeBattery{rated: 6000, soc: 0.5}
	f, d := testFleet(clk, batteryDev("b", b))
	d.setGrant(grantAt(t0, time.Hour, -20000, 2500))
	f.Tick(context.Background())
	if b.commands[0] != 2500 {
		t.Errorf("commanded %v W, want +2500", b.commands[0])
	}
}

// simulate runs a grant to its end with irregular tick gaps and returns the
// peak fleet power and the energy moved, both integrated independently of
// the dispatcher from what the devices achieved.
func simulate(t *testing.T, terms *grantTerms, rated []float64, soc float64, gaps []time.Duration, run time.Duration) (peakW, movedWh float64) {
	t.Helper()
	clk := &clock{terms.Start.Add(-30 * time.Second)}
	var devs []*managedDevice
	for i, r := range rated {
		devs = append(devs, batteryDev(string(rune('a'+i)), &fakeBattery{rated: r, soc: soc}))
	}
	f, d := testFleet(clk, devs...)
	d.setGrant(terms)

	end := terms.Start.Add(run)
	prevAt, prevP := clk.t, 0.0
	for i := 0; clk.t.Before(end); i++ {
		f.Tick(context.Background())
		p := d.lastAchieved
		if math.Abs(p) > peakW {
			peakW = math.Abs(p)
		}
		// The previous tick's output was held until now; count the part of
		// that span that lies inside the interval.
		prevAt, prevP = clk.t, p
		next := clk.t.Add(gaps[i%len(gaps)])
		from, to := prevAt, next
		if from.Before(terms.Start) {
			from = terms.Start
		}
		if to.After(terms.End) {
			to = terms.End
		}
		if to.After(from) {
			movedWh += prevP * to.Sub(from).Hours()
		}
		clk.t = next
	}
	return peakW, movedWh
}

func TestDispatch_FleetPowerNeverExceedsPowerAvailable(t *testing.T) {
	for _, dir := range []float64{1, -1} {
		terms := grantAt(t0, time.Hour, dir*500000, 3000)
		peak, _ := simulate(t, terms, []float64{4000, 4000, 2000}, 0.5,
			[]time.Duration{5 * time.Second, 7 * time.Second, 30 * time.Second}, time.Hour)
		if peak > 3000+1e-6 {
			t.Errorf("direction %+.0f: fleet peaked at %v W, above powerAvailable 3000", dir, peak)
		}
		if peak < 2999 {
			t.Errorf("direction %+.0f: fleet peaked at only %v W; the bound test needs the power to bind", dir, peak)
		}
	}
}

func TestDispatch_EnergyFromAchievedOutputNeverExceedsEnergyAvailable(t *testing.T) {
	gapSets := [][]time.Duration{
		{5 * time.Second},
		{5 * time.Second, 30 * time.Second, 11 * time.Second},
		{45 * time.Second},
	}
	for _, dir := range []float64{1, -1} {
		for _, wh := range []float64{50, 1000, 2999} {
			for gi, gaps := range gapSets {
				terms := grantAt(t0, time.Hour, dir*wh, 3000)
				_, moved := simulate(t, terms, []float64{4000, 2000}, 0.5, gaps, 90*time.Minute)
				if dir*moved > wh+1e-6 {
					t.Errorf("dir %+.0f energy %v gaps#%d: moved %v Wh, above energyAvailable", dir, wh, gi, dir*moved)
				}
				if dir*moved < wh*0.9 {
					t.Errorf("dir %+.0f energy %v gaps#%d: moved only %v Wh; the bound test needs the energy to be spent", dir, wh, gi, dir*moved)
				}
			}
		}
	}
}

// A device that under-delivers is not counted as having delivered: the
// energy accounted comes from achieved output, so the grant is not spent
// early.
func TestDispatch_EnergyIsIntegratedFromAchievedNotCommanded(t *testing.T) {
	// One 500 W battery cannot move the 3000 W it is asked for.
	terms := grantAt(t0, time.Hour, 3000, 3000)
	_, moved := simulate(t, terms, []float64{500}, 0.5, []time.Duration{5 * time.Second}, time.Hour)
	if moved > 500.5 || moved < 499 {
		t.Errorf("moved %v Wh, want about 500 (500 W for an hour)", moved)
	}
}

// The grant's energy is not reset by a revised answer to the same request,
// and is by a new request.
func TestDispatcher_ResetForgetsDelivered(t *testing.T) {
	d := newDispatcher(5 * time.Second)
	d.setGrant(grantAt(t0, time.Hour, 1000, 3000))
	d.plan(t0, []fleetMember{{key: "a", ratedW: 4000, up: true}})
	d.record(3000)
	d.plan(t0.Add(time.Minute), []fleetMember{{key: "a", ratedW: 4000, up: true}})
	if d.deliveredWh != 50 {
		t.Fatalf("delivered %v Wh, want 50 (3000 W for a minute)", d.deliveredWh)
	}
	d.setGrant(grantAt(t0, time.Hour, 800, 3000))
	if d.deliveredWh != 50 {
		t.Errorf("a revised grant reset delivered to %v", d.deliveredWh)
	}
	d.reset()
	if d.deliveredWh != 0 || d.terms != nil {
		t.Errorf("after reset: delivered %v terms %v", d.deliveredWh, d.terms)
	}
}

// End to end: a reserver following a fake server's grant drives the fleet.
func TestReserverAndFleet_GrantMovesTheBattery(t *testing.T) {
	f := &fakeFrq{}
	clk := &clock{t0}
	r := testReserver(t, f, clk, nil)
	b := &fakeBattery{rated: 6000, soc: 0.5}
	fleet, _ := testFleet(clk, batteryDev("b", b))
	fleet.dispatch = r.disp

	r.Trigger(context.Background())
	f.list = listOf(grantFor(r, r.cur.mrid, 6000, 3000))

	clk.t = r.cur.start.Add(time.Minute)
	r.Poll(context.Background())
	fleet.Tick(context.Background())
	if len(b.commands) != 1 || b.commands[0] != -3000 {
		t.Errorf("battery commands %v, want one of -3000 W (charging 3000 W)", b.commands)
	}
}
