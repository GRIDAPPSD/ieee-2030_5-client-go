package main

// Tests for the managed devices' control sessions (managed_controls.go,
// GRIDAPPSD/ieee-2030_5-client-go#74): each device follows the DER control
// as its own target, posts Responses naming itself, and a device a control
// is driving leaves the grant split while its output still counts against
// the grant.

import (
	"context"
	"encoding/xml"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

const controlTargetW = 1500

// controlsServer serves one DERProgram, shared by every device, whose
// DERControlList holds one control, and keeps the Responses posted to it.
type controlsServer struct {
	mu        sync.Mutex
	responses []sep2.DERControlResponse
	hits      map[string]int
}

func (s *controlsServer) hit(path string) {
	s.mu.Lock()
	s.hits[path]++
	s.mu.Unlock()
}

func (s *controlsServer) totalHits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.hits {
		n += v
	}
	return n
}

// statusesFor is the sorted Response statuses posted by the device lfdi for
// the control subject.
func (s *controlsServer) statusesFor(lfdi, subject string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, r := range s.responses {
		if r.EndDeviceLFDI == lfdi && r.Subject == subject && r.Status != nil {
			out = append(out, int(*r.Status))
		}
	}
	sort.Ints(out)
	return out
}

// newControlsFixture starts a server listing the given devices, each with
// its own FSA list, all pointing at one program with one control that
// started startOffset ago and runs for durationS seconds, asking for a
// Response on receipt, start and completion.
func newControlsFixture(t *testing.T, startOffset time.Duration, durationS uint32, lfdis ...string) (*inverter.SEP2Client, *controlsServer) {
	t.Helper()
	env := newDERWalkTestEnv(t)
	srv := &controlsServer{hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		var list sep2.EndDeviceList
		for _, l := range lfdis {
			list.EndDevice = append(list.EndDevice, sep2.EndDevice{LFDI: l, FunctionSetAssignmentsListLink: &sep2.ListLink{Href: "/fsa/" + l}})
		}
		writeSepXML(t, w, list)
	})
	for _, l := range lfdis {
		mux.HandleFunc("/fsa/"+l, func(w http.ResponseWriter, _ *http.Request) {
			srv.hit("/fsa/" + l)
			writeSepXML(t, w, sep2.FunctionSetAssignmentsList{
				ListResource: sep2.ListResource{All: 1, Results: 1},
				FunctionSetAssignments: []sep2.FunctionSetAssignments{
					{MRID: "FSA1", DERProgramListLink: &sep2.ListLink{Href: "/dp"}},
				},
			})
		})
	}
	mux.HandleFunc("/dp", func(w http.ResponseWriter, _ *http.Request) {
		srv.hit("/dp")
		writeSepXML(t, w, sep2.DERProgramList{
			ListResource: sep2.ListResource{All: 1, Results: 1},
			DERProgram:   []sep2.DERProgram{{MRID: "PROG1", Primacy: 1, DERControlListLink: &sep2.ListLink{Href: "/derc"}}},
		})
	})
	mux.HandleFunc("/derc", func(w http.ResponseWriter, _ *http.Request) {
		srv.hit("/derc")
		mask := sep2.HexBinary8(0x07)
		var dc sep2.DERControl
		dc.MRID = "CTL1"
		dc.ReplyTo = "/rsp"
		dc.ResponseRequired = &mask
		dc.Interval = &sep2.DateTimeInterval{Start: time.Now().Add(-startOffset).Unix(), Duration: durationS}
		dc.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: controlTargetW}}
		writeSepXML(t, w, sep2.DERControlList{
			ListResource: sep2.ListResource{All: 1, Results: 1},
			DERControl:   []sep2.DERControl{dc},
		})
	})
	mux.HandleFunc("/rsp", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var resp sep2.DERControlResponse
		if err := xml.Unmarshal(b, &resp); err != nil {
			t.Errorf("unmarshal DERControlResponse: %v", err)
		}
		srv.mu.Lock()
		srv.responses = append(srv.responses, resp)
		srv.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	serverURL, _ := startDERWalkListener(t, env, mux)
	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL, CertFile: env.deviceCertPath, KeyFile: env.deviceKeyPath,
		CAFile: env.caCertPath, ClientRole: "aggregator",
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}
	return client, srv
}

// probeDevice records what the fleet tick last achieved on a device.
type probeDevice struct {
	device.DERDevice
	mu   sync.Mutex
	last inverter.InverterState
}

func (p *probeDevice) ApplySetpoint(ctx context.Context, c inverter.ControlOutputs) (inverter.InverterState, error) {
	st, err := p.DERDevice.ApplySetpoint(ctx, c)
	p.mu.Lock()
	p.last = st
	p.mu.Unlock()
	return st, err
}

func (p *probeDevice) SOC() float64 { return p.DERDevice.(interface{ SOC() float64 }).SOC() }

func (p *probeDevice) achievedW() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last.ActivePowerW
}

// startControlFleet starts a PV and a battery under mode, with the control
// poll shortened, and returns the fleet, the probes, and the cancel.
func startControlFleet(t *testing.T, client *inverter.SEP2Client, mode string) (*managedFleet, map[string]*probeDevice, context.CancelFunc) {
	t.Helper()
	pv := managedCfg(t, "pv", pvLFDI, "pv", 4000)
	bat := managedCfg(t, "bat", batLFDI, "battery", 0)
	pv.Controls = &simconfig.Controls{Response: mode}
	bat.Controls = &simconfig.Controls{Response: mode}
	ctx, cancel := context.WithCancel(context.Background())
	fleet, err := startManaged(ctx, client, sep2.DeviceCapability{}, "/edev", []simconfig.Managed{pv, bat}, time.Hour)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	probes := map[string]*probeDevice{}
	for _, md := range fleet.devices {
		p := &probeDevice{DERDevice: md.dev}
		md.dev = p
		md.ctl.interval = 20 * time.Millisecond
		probes[md.lfdi] = p
	}
	fleet.StartControls(ctx, controlsEnv{client: client, cfg: inverter.SimConfig{}})
	// Cleanups run last-in first-out: cancel, then wait for the sessions.
	t.Cleanup(fleet.WaitControls)
	t.Cleanup(cancel)
	return fleet, probes, cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func allStarted(f *managedFleet) bool {
	for _, md := range f.devices {
		if md.ctl.sm.Current().State != inverter.StateEventStarted {
			return false
		}
	}
	return true
}

func TestManagedControls_FollowGivesEachDeviceTheFullTargetAndPostsResponses(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, probes, _ := startControlFleet(t, client, "follow")

	waitFor(t, "both devices to start the control", func() bool { return allStarted(fleet) })
	fleet.Tick(context.Background())

	// One control, read by two devices: each is held to the full target,
	// not to half of it. The PV's recording alone would give 4000.
	for lfdi, p := range probes {
		if got := p.achievedW(); got != controlTargetW {
			t.Errorf("device %s achieved %v W, want %d W", lfdi, got, controlTargetW)
		}
	}
	// Received (1) and started (2), each naming the device that posted it.
	for _, lfdi := range []string{pvLFDI, batLFDI} {
		waitFor(t, "Responses from "+lfdi, func() bool { return len(srv.statusesFor(lfdi, "CTL1")) >= 2 })
		if got := srv.statusesFor(lfdi, "CTL1"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Errorf("device %s posted statuses %v, want [1 2]", lfdi, got)
		}
	}
}

func TestManagedControls_CompletedResponseNamesTheDevice(t *testing.T) {
	// Started 1s ago, runs 2s: it completes while the sessions poll.
	client, srv := newControlsFixture(t, time.Second, 2, pvLFDI, batLFDI)
	startControlFleet(t, client, "follow")

	for _, lfdi := range []string{pvLFDI, batLFDI} {
		waitFor(t, "completed Response from "+lfdi, func() bool { return len(srv.statusesFor(lfdi, "CTL1")) >= 3 })
		if got := srv.statusesFor(lfdi, "CTL1"); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
			t.Errorf("device %s posted statuses %v, want [1 2 3]", lfdi, got)
		}
	}
}

func TestManagedControls_AckPostsResponsesAndLeavesOutputAlone(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, probes, _ := startControlFleet(t, client, "ack")

	waitFor(t, "both devices to start the control", func() bool { return allStarted(fleet) })
	waitFor(t, "Responses from both", func() bool {
		return len(srv.statusesFor(pvLFDI, "CTL1")) >= 2 && len(srv.statusesFor(batLFDI, "CTL1")) >= 2
	})
	fleet.Tick(context.Background())

	if got := probes[pvLFDI].achievedW(); got != 4000 {
		t.Errorf("ack PV achieved %v W, want its recording 4000 W unchanged", got)
	}
	if got := probes[batLFDI].achievedW(); got != 0 {
		t.Errorf("ack battery achieved %v W, want its recording 0 W unchanged", got)
	}
}

func TestManagedControls_NoneReadsNothingAndPostsNothing(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, probes, _ := startControlFleet(t, client, "none")

	fleet.WaitControls() // no session goroutine exists, so this returns at once
	time.Sleep(200 * time.Millisecond)
	fleet.Tick(context.Background())

	if n := srv.totalHits(); n != 0 {
		t.Errorf("none made %d requests to the program tree, want 0", n)
	}
	srv.mu.Lock()
	posted := len(srv.responses)
	srv.mu.Unlock()
	if posted != 0 {
		t.Errorf("none posted %d Responses, want 0", posted)
	}
	if got := probes[pvLFDI].achievedW(); got != 4000 {
		t.Errorf("none PV achieved %v W, want 4000 W", got)
	}
}

func TestManagedControls_SessionsEndWithTheProcessContext(t *testing.T) {
	client, _ := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, cancel := startControlFleet(t, client, "follow")
	waitFor(t, "both devices to start the control", func() bool { return allStarted(fleet) })

	cancel()
	done := make(chan struct{})
	go func() { fleet.WaitControls(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("control sessions still running 5s after the context was cancelled")
	}
}

// batteryFleet builds two batteries by hand, each starting idle, so the
// grant split can be observed without a server.
func batteryFleet(t *testing.T, mode string) (*managedFleet, map[string]*probeDevice) {
	t.Helper()
	fleet := &managedFleet{reportInterval: time.Hour}
	probes := map[string]*probeDevice{}
	for _, c := range []struct{ name, lfdi string }{{"a", pvLFDI}, {"b", batLFDI}} {
		m := managedCfg(t, c.name, c.lfdi, "battery", 0)
		rep, err := device.NewReplay(device.ReplayConfig{
			File: m.Replay.File, Type: "battery", Clock: "start", Scale: 1,
			RatedW: 5000, CapacityWh: 10000, InitialSOC: 0.5,
		})
		if err != nil {
			t.Fatalf("NewReplay: %v", err)
		}
		p := &probeDevice{DERDevice: rep}
		probes[c.lfdi] = p
		fleet.devices = append(fleet.devices, &managedDevice{
			name: c.name, lfdi: c.lfdi, kind: "battery", ratedW: 5000, dev: p, now: time.Now,
			ctl: newControlSession(&simconfig.Controls{Response: mode}),
		})
	}
	return fleet, probes
}

// startControlOn puts device md in EVENT_STARTED on a control that asks for
// controlTargetW.
func startControlOn(md *managedDevice) {
	var dc sep2.DERControl
	dc.MRID = "CTL1"
	dc.Interval = &sep2.DateTimeInterval{Start: time.Now().Add(-time.Minute).Unix(), Duration: 3600}
	dc.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: controlTargetW}}
	sched := inverter.NewScheduler(time.Now, rand.New(rand.NewPCG(1, 2)))
	md.ctl.sm.Tick(time.Now(), []sep2.DERControl{dc}, nil, sched)
}

func TestManagedControls_ControlledDeviceLeavesTheSplitButCountsAgainstTheGrant(t *testing.T) {
	fleet, probes := batteryFleet(t, "follow")
	startControlOn(fleet.devices[0])

	members := fleet.batteries()
	if len(members) != 1 || members[0].key != batLFDI {
		t.Fatalf("grant members = %+v, want only the device no control is driving (%s)", members, batLFDI)
	}

	// A 2000 W charging grant over the next hour.
	disp := newDispatcher(time.Second)
	disp.setGrant(&grantTerms{Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Hour), EnergyWh: 100000, PowerW: 2000})
	fleet.setDispatcher(disp)
	fleet.Tick(context.Background())

	// The controlled device runs its control (discharging 1500 W), and the
	// other takes the whole grant (charging 2000 W).
	if got := probes[pvLFDI].achievedW(); got != controlTargetW {
		t.Errorf("controlled device achieved %v W, want %d W", got, controlTargetW)
	}
	if got := probes[batLFDI].achievedW(); got != -2000 {
		t.Errorf("other device achieved %v W, want -2000 W (the whole grant)", got)
	}
	// Against the grant the fleet moved 2000 W charging by one device and
	// 1500 W discharging by the other: 500 W net charging.
	if got := disp.lastAchieved; got != 500 {
		t.Errorf("achieved against the grant = %v W charging, want 500 W", got)
	}
}

func TestManagedControls_AckDeviceStaysInTheSplit(t *testing.T) {
	fleet, _ := batteryFleet(t, "ack")
	startControlOn(fleet.devices[0])

	if members := fleet.batteries(); len(members) != 2 {
		t.Errorf("grant members = %+v, want both: an ack device's output is not driven by the control", members)
	}
}
