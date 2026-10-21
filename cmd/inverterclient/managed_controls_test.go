package main

// Tests for the managed devices' control sessions (managed_controls.go,
// GRIDAPPSD/ieee-2030_5-client-go#74): each device follows the DER control
// as its own target, posts Responses naming itself, and a device a control
// is driving leaves the grant split while its output still counts against
// the grant.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
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

	// failFSA lists the devices whose FSA list answers 503; failDefault
	// makes the DefaultDERControl answer 503; defaultW is the target the
	// default control asks for.
	failFSA     map[string]bool
	failDefault bool
	defaultW    int

	// failControls makes the DERControlList answer 503; progMode picks what
	// the DERProgramList holds (progNormal, progEmpty, progNoList or
	// progNoListAndNormal); times keeps when each path was hit.
	failControls bool
	progMode     int
	times        map[string][]time.Time

	// subscribed is the resources the devices subscribed to, by the
	// SubscriptionList they posted to.
	subscribed map[string][]string

	// subStatus, when set, is the status every Subscription POST answers;
	// subAttempts counts the POSTs; respMask, when set, is the
	// responseRequired mask of the controls.
	subStatus   int
	subAttempts map[string]int
	respMask    sep2.HexBinary8

	// failFSAAfterServe makes a device's FSA list answer once more and then
	// fail.
	failFSAAfterServe map[string]bool

	// ctlFixedVar, when set, is added to CTL1's control base.
	ctlFixedVar *sep2.FixedVar

	// withBadControl adds BAD, whose base core cannot decode, to the list.
	withBadControl bool
}

func (s *controlsServer) addBadControl() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.withBadControl = true
}

func (s *controlsServer) setControlFixedVar(fv sep2.FixedVar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctlFixedVar = &fv
}

func (s *controlsServer) setSubStatus(code int) {
	s.mu.Lock()
	s.subStatus = code
	s.mu.Unlock()
}

func (s *controlsServer) subAttemptsTo(list string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subAttempts[list]
}

func (s *controlsServer) setRespMask(m sep2.HexBinary8) {
	s.mu.Lock()
	s.respMask = m
	s.mu.Unlock()
}

func (s *controlsServer) failFSANextRead(lfdi string) {
	s.mu.Lock()
	s.failFSAAfterServe[lfdi] = true
	s.mu.Unlock()
}

func (s *controlsServer) subscriptionsTo(list string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.subscribed[list]...)
}

const (
	progNormal           = iota // PROG1, with a DERControlList
	progEmpty                   // no program at all
	progNoList                  // only PROG0, which has no DERControlList link
	progNoListAndNormal         // PROG0 (higher priority, no list) and PROG1
	progWithPrimaryAdded        // PROG1 and PROG2, which outranks it and has its own list
	progListMoved               // PROG1 alone, its control list moved to /derc2
	progBrokenSibling           // PROG1, PROG2 (outranks it) and PROG3, whose control list answers 503
	progBrokenHigher            // PROG1 and PROG4, which outranks it but whose control list answers 404
)

// secondControlW is the target of the control PROG2 serves at /derc2.
const secondControlW = 700

func (s *controlsServer) setFailControls(fail bool) {
	s.mu.Lock()
	s.failControls = fail
	s.mu.Unlock()
}

func (s *controlsServer) setProgMode(m int) {
	s.mu.Lock()
	s.progMode = m
	s.mu.Unlock()
}

// hitTimes is when path was hit, oldest first.
func (s *controlsServer) hitTimes(path string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times[path]...)
}

func (s *controlsServer) setFailFSA(lfdi string, fail bool) {
	s.mu.Lock()
	s.failFSA[lfdi] = fail
	s.mu.Unlock()
}

func (s *controlsServer) setDefault(fail bool, w int) {
	s.mu.Lock()
	s.failDefault, s.defaultW = fail, w
	s.mu.Unlock()
}

func (s *controlsServer) hitsOf(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *controlsServer) hit(path string) {
	s.mu.Lock()
	s.hits[path]++
	s.times[path] = append(s.times[path], time.Now())
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
	return newControlsFixtureWith(t, false, startOffset, durationS, lfdis...)
}

// newControlsFixtureWith is newControlsFixture, and with withDefault the
// program also links a DefaultDERControl served at /dc.
func newControlsFixtureWith(t *testing.T, withDefault bool, startOffset time.Duration, durationS uint32, lfdis ...string) (*inverter.SEP2Client, *controlsServer) {
	t.Helper()
	env := newDERWalkTestEnv(t)
	srv := &controlsServer{hits: map[string]int{}, failFSA: map[string]bool{}, times: map[string][]time.Time{}, subscribed: map[string][]string{}, subAttempts: map[string]int{}, failFSAAfterServe: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		var list sep2.EndDeviceList
		for _, l := range lfdis {
			list.EndDevice = append(list.EndDevice, sep2.EndDevice{
				LFDI:                           l,
				FunctionSetAssignmentsListLink: &sep2.ListLink{Href: "/fsa/" + l},
				SubscriptionListLink:           &sep2.ListLink{Href: "/sub/" + l},
			})
		}
		writeSepXML(t, w, list)
	})
	for _, l := range lfdis {
		mux.HandleFunc("/sub/"+l, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var sub sep2.Subscription
			if err := xml.Unmarshal(b, &sub); err != nil {
				t.Errorf("unmarshal Subscription: %v", err)
			}
			srv.mu.Lock()
			srv.subAttempts["/sub/"+l]++
			if code := srv.subStatus; code != 0 {
				srv.mu.Unlock()
				http.Error(w, "refused", code)
				return
			}
			srv.subscribed["/sub/"+l] = append(srv.subscribed["/sub/"+l], sub.SubscribedResource)
			srv.mu.Unlock()
			w.Header().Set("Location", fmt.Sprintf("/sub/%s/%d", l, len(srv.subscriptionsTo("/sub/"+l))))
			w.WriteHeader(http.StatusCreated)
		})
		mux.HandleFunc("/fsa/"+l, func(w http.ResponseWriter, _ *http.Request) {
			srv.hit("/fsa/" + l)
			srv.mu.Lock()
			fail := srv.failFSA[l]
			if srv.failFSAAfterServe[l] && !fail {
				srv.failFSA[l], srv.failFSAAfterServe[l] = true, false
			}
			srv.mu.Unlock()
			if fail {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
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
		prog := sep2.DERProgram{MRID: "PROG1", Primacy: 1, DERControlListLink: &sep2.ListLink{Href: "/derc"}}
		if withDefault {
			prog.DefaultDERControlLink = &sep2.Link{Href: "/dc"}
		}
		srv.mu.Lock()
		mode := srv.progMode
		srv.mu.Unlock()
		var progs []sep2.DERProgram
		switch mode {
		case progNormal:
			progs = []sep2.DERProgram{prog}
		case progNoList:
			progs = []sep2.DERProgram{{MRID: "PROG0", Primacy: 0}}
		case progNoListAndNormal:
			progs = []sep2.DERProgram{{MRID: "PROG0", Primacy: 0}, prog}
		case progWithPrimaryAdded:
			progs = []sep2.DERProgram{prog, {MRID: "PROG2", Primacy: 0, DERControlListLink: &sep2.ListLink{Href: "/derc2"}}}
		case progListMoved:
			prog.DERControlListLink = &sep2.ListLink{Href: "/derc2"}
			progs = []sep2.DERProgram{prog}
		case progBrokenHigher:
			progs = []sep2.DERProgram{prog, {MRID: "PROG4", Primacy: 0, DERControlListLink: &sep2.ListLink{Href: "/derc3"}}}
		case progBrokenSibling:
			progs = []sep2.DERProgram{prog,
				{MRID: "PROG2", Primacy: 0, DERControlListLink: &sep2.ListLink{Href: "/derc2"}},
				{MRID: "PROG3", Primacy: 5, DERControlListLink: &sep2.ListLink{Href: "/derc3"}}}
		}
		writeSepXML(t, w, sep2.DERProgramList{
			ListResource: sep2.ListResource{All: uint32(len(progs)), Results: uint32(len(progs))},
			DERProgram:   progs,
		})
	})
	mux.HandleFunc("/dc", func(w http.ResponseWriter, _ *http.Request) {
		srv.hit("/dc")
		srv.mu.Lock()
		fail, wv := srv.failDefault, srv.defaultW
		srv.mu.Unlock()
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var ddc sep2.DefaultDERControl
		ddc.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: int16(wv)}}
		writeSepXML(t, w, ddc)
	})
	mux.HandleFunc("/derc3", func(w http.ResponseWriter, _ *http.Request) {
		srv.hit("/derc3")
		srv.mu.Lock()
		mode := srv.progMode
		srv.mu.Unlock()
		if mode == progBrokenHigher {
			http.NotFound(w, nil)
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/derc2", func(w http.ResponseWriter, _ *http.Request) {
		srv.hit("/derc2")
		var dc sep2.DERControl
		dc.MRID = "CTL2"
		dc.Interval = &sep2.DateTimeInterval{Start: time.Now().Add(-startOffset).Unix(), Duration: durationS}
		dc.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: secondControlW}}
		writeSepXML(t, w, sep2.DERControlList{
			ListResource: sep2.ListResource{All: 1, Results: 1},
			DERControl:   []sep2.DERControl{dc},
		})
	})
	mux.HandleFunc("/derc", func(w http.ResponseWriter, _ *http.Request) {
		srv.hit("/derc")
		srv.mu.Lock()
		failList := srv.failControls
		srv.mu.Unlock()
		if failList {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		srv.mu.Lock()
		mask := sep2.HexBinary8(0x07)
		if srv.respMask != 0 {
			mask = srv.respMask
		}
		fixedVar := srv.ctlFixedVar
		withBad := srv.withBadControl
		srv.mu.Unlock()
		var dc sep2.DERControl
		dc.MRID = "CTL1"
		dc.ReplyTo = "/rsp"
		dc.ResponseRequired = &mask
		dc.Interval = &sep2.DateTimeInterval{Start: time.Now().Add(-startOffset).Unix(), Duration: durationS}
		dc.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: controlTargetW}, OpModFixedVar: fixedVar}
		list := sep2.DERControlList{
			ListResource: sep2.ListResource{All: 1, Results: 1},
			DERControl:   []sep2.DERControl{dc},
		}
		if !withBad {
			writeSepXML(t, w, list)
			return
		}
		bad := dc.Copy()
		bad.MRID = "BAD"
		fv := badFixedVar
		bad.DERControlBase = &sep2.DERControlBase{OpModFixedVar: &fv}
		list.DERControl = append(list.DERControl, bad)
		list.All, list.Results = 2, 2
		writeListWithBadItems(t, w, list, 1)
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

// recordingSet is the guard's managed set, counting how often each device
// was judged as a request target. A request with no target is the client's
// own and is never counted.
type recordingSet struct {
	inner guard.ManagedSet
	mu    sync.Mutex
	calls map[string]int
}

func (r *recordingSet) IsManaged(lfdi string) bool {
	r.mu.Lock()
	r.calls[strings.ToUpper(lfdi)]++
	r.mu.Unlock()
	return r.inner.IsManaged(lfdi)
}

func (r *recordingSet) judged(lfdi string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[strings.ToUpper(lfdi)]
}

// startControlFleet starts a PV and a battery under mode, with the control
// poll shortened, and returns the fleet, the probes, and the cancel.
func startControlFleet(t *testing.T, client *inverter.SEP2Client, mode string) (*managedFleet, map[string]*probeDevice, context.CancelFunc) {
	t.Helper()
	fleet, probes, cancel, _ := startControlFleetEvery(t, client, mode, 20*time.Millisecond)
	return fleet, probes, cancel
}

// startControlFleetEvery is startControlFleet with the control poll set to
// interval, and the managed set replaced by one that counts the targets it
// is asked about.
func startControlFleetEvery(t *testing.T, client *inverter.SEP2Client, mode string, interval time.Duration, opts ...func(*controlSession)) (*managedFleet, map[string]*probeDevice, context.CancelFunc, *recordingSet) {
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
	rec := &recordingSet{inner: guard.NewStaticManagedSet(pvLFDI, batLFDI), calls: map[string]int{}}
	client.SetManagedSet(rec)
	probes := map[string]*probeDevice{}
	for _, md := range fleet.devices {
		p := &probeDevice{DERDevice: md.dev}
		md.dev = p
		md.ctl.interval = interval
		for _, o := range opts {
			o(md.ctl)
		}
		probes[md.lfdi] = p
	}
	fleet.StartControls(ctx, controlsEnv{client: client, cfg: inverter.SimConfig{}})
	// Cleanups run last-in first-out: cancel, then wait for the sessions.
	t.Cleanup(func() { waitControlsBounded(t, fleet, "the test ended and its context was cancelled") })
	t.Cleanup(cancel)
	return fleet, probes, cancel, rec
}

// waitControlsBounded waits for the fleet's session goroutines, and fails
// the test naming what should have ended them when they outlive 5s, so a
// session that does not exit fails by assertion and not at the package
// timeout.
func waitControlsBounded(t *testing.T, fleet *managedFleet, why string) {
	t.Helper()
	done := make(chan struct{})
	go func() { fleet.WaitControls(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("control sessions still running 5s after %s", why)
	}
}

func deviceByLFDI(f *managedFleet, lfdi string) *managedDevice {
	for _, md := range f.devices {
		if md.lfdi == lfdi {
			return md
		}
	}
	return nil
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

	// No session goroutine exists, so this returns at once.
	waitControlsBounded(t, fleet, "none started a session (it must start none)")
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
	waitControlsBounded(t, fleet, "the context was cancelled")
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

// syncLog is a log destination that session goroutines may write while the
// test reads it.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureSessionLog returns what log wrote until the test ends.
func captureSessionLog(t *testing.T) *syncLog {
	t.Helper()
	l := &syncLog{}
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

// A failed FSA list is retried with backoff until it succeeds, naming the
// device, while the other device's session is unaffected throughout.
func TestManagedControls_FailedDiscoveryIsRetriedWhileAnotherDeviceKeepsRunning(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setFailFSA(pvLFDI, true)
	logs := captureSessionLog(t)
	fleet, _, cancel, rec := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
	pv, bat := deviceByLFDI(fleet, pvLFDI), deviceByLFDI(fleet, batLFDI)

	waitFor(t, "the battery to start the control", func() bool { return bat.ctl.sm.Current().State == inverter.StateEventStarted })
	waitFor(t, "three failed FSA lists for the PV", func() bool { return srv.hitsOf("/fsa/"+pvLFDI) >= 3 })

	// The PV is still failing: no control, no Response. The battery ran once.
	if got := pv.ctl.sm.Current().State; got != inverter.StateDefault {
		t.Errorf("PV state while its FSA list fails = %v, want DEFAULT", got)
	}
	if got := srv.statusesFor(pvLFDI, "CTL1"); len(got) != 0 {
		t.Errorf("PV posted statuses %v while its FSA list fails, want none", got)
	}
	waitFor(t, "the battery's Responses", func() bool { return len(srv.statusesFor(batLFDI, "CTL1")) >= 2 })
	if got := srv.statusesFor(batLFDI, "CTL1"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("battery posted statuses %v, want [1 2]", got)
	}
	if n := srv.hitsOf("/fsa/" + batLFDI); n != 1 {
		t.Errorf("battery read its FSA list %d times, want 1: the PV's failures must not touch it", n)
	}
	if !strings.Contains(logs.String(), "managed device pv ("+pvLFDI+"): FSA list failed") {
		t.Errorf("no log line names the PV and its failed FSA list; got:\n%s", logs.String())
	}

	// The server recovers: the PV starts the same control.
	srv.setFailFSA(pvLFDI, false)
	waitFor(t, "the PV to start the control after recovery", func() bool { return pv.ctl.sm.Current().State == inverter.StateEventStarted })
	waitFor(t, "the PV's Responses", func() bool { return len(srv.statusesFor(pvLFDI, "CTL1")) >= 2 })
	if got := srv.statusesFor(pvLFDI, "CTL1"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("PV posted statuses %v, want [1 2]", got)
	}
	if n := srv.hitsOf("/fsa/" + batLFDI); n != 1 {
		t.Errorf("battery read its FSA list %d times after the PV recovered, want 1", n)
	}

	// Every read and Response of a device was judged as that device: once
	// the sessions are done, each device was the target of at least its FSA
	// reads plus its Responses.
	cancel()
	waitControlsBounded(t, fleet, "the context was cancelled")
	for _, lfdi := range []string{pvLFDI, batLFDI} {
		want := srv.hitsOf("/fsa/"+lfdi) + len(srv.statusesFor(lfdi, "CTL1"))
		if got := rec.judged(lfdi); got < want {
			t.Errorf("device %s was the request target %d times, want at least %d (its FSA reads and Responses)", lfdi, got, want)
		}
	}
}

// A session in discovery backoff ends with the process context, even when
// the backoff is long.
func TestManagedControls_SessionInDiscoveryBackoffEndsWithTheProcessContext(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setFailFSA(pvLFDI, true)
	srv.setFailFSA(batLFDI, true)
	// A ten minute backoff after the first failure: only the context can
	// end the wait inside the test's time.
	fleet, _, cancel, _ := startControlFleetEvery(t, client, "follow", 10*time.Minute)

	waitFor(t, "both devices to fail their first FSA list", func() bool {
		return srv.hitsOf("/fsa/"+pvLFDI) >= 1 && srv.hitsOf("/fsa/"+batLFDI) >= 1
	})
	cancel()
	waitControlsBounded(t, fleet, "the context was cancelled during discovery backoff")
}

// A default control that fails first and appears later is followed, and a
// later change of it is followed too.
func TestManagedControls_DefaultControlThatAppearsLaterIsFollowed(t *testing.T) {
	// The control starts in an hour, so only the default drives the output.
	client, srv := newControlsFixtureWith(t, true, -time.Hour, 3600, pvLFDI, batLFDI)
	srv.setDefault(true, 0)
	fleet, probes, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)

	waitFor(t, "two failed DefaultDERControl reads", func() bool { return srv.hitsOf("/dc") >= 4 })
	fleet.Tick(context.Background())
	if got := probes[pvLFDI].achievedW(); got != 4000 {
		t.Errorf("PV achieved %v W with no default readable, want its recording 4000 W", got)
	}

	srv.setDefault(false, 700)
	waitFor(t, "the PV to read the default", func() bool { return defaultTargetW(deviceByLFDI(fleet, pvLFDI)) == 700 })
	fleet.Tick(context.Background())
	if got := probes[pvLFDI].achievedW(); got != 700 {
		t.Errorf("PV achieved %v W under a 700 W default, want 700 W", got)
	}
	if got := deviceByLFDI(fleet, pvLFDI).ctl.sm.Current().State; got == inverter.StateEventStarted {
		t.Errorf("PV state = %v, want the control not started so only the default drives it", got)
	}

	srv.setDefault(false, 900)
	waitFor(t, "the PV to follow the changed default", func() bool { return defaultTargetW(deviceByLFDI(fleet, pvLFDI)) == 900 })
	fleet.Tick(context.Background())
	if got := probes[pvLFDI].achievedW(); got != 900 {
		t.Errorf("PV achieved %v W under a 900 W default, want 900 W", got)
	}
}

// defaultTargetW is the target of the default control the device holds, or
// -1 when it holds none.
func defaultTargetW(md *managedDevice) int {
	d := md.ctl.defaultCtl.Load()
	if d == nil || d.DERControlBase == nil || d.DERControlBase.OpModTargetW == nil {
		return -1
	}
	return int(d.DERControlBase.OpModTargetW.Value)
}

// receiveControlOn puts device md in EVENT_RECEIVED on a control that
// starts in an hour.
func receiveControlOn(md *managedDevice, clock func() time.Time) *inverter.Scheduler {
	var dc sep2.DERControl
	dc.MRID = "CTL1"
	dc.Interval = &sep2.DateTimeInterval{Start: time.Now().Add(time.Hour).Unix(), Duration: 3600}
	dc.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: controlTargetW}}
	sched := inverter.NewScheduler(clock, rand.New(rand.NewPCG(1, 2)))
	md.ctl.sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	return sched
}

// A control that is received but has not started does not drive the
// device, so the device keeps its share of the grant; once a started
// control completes the device rejoins the split.
func TestManagedControls_DeviceLeavesTheSplitOnlyWhileAControlIsStarted(t *testing.T) {
	fleet, _ := batteryFleet(t, "follow")
	md := fleet.devices[0]

	at := time.Now()
	sched := receiveControlOn(md, func() time.Time { return at })
	if got := md.ctl.sm.Current().State; got != inverter.StateEventReceived {
		t.Fatalf("state = %v, want EVENT_RECEIVED", got)
	}
	if members := fleet.batteries(); len(members) != 2 {
		t.Errorf("grant members with a received control = %+v, want both: it is not driving the device yet", members)
	}

	// The control starts: the device leaves the split.
	at = at.Add(90 * time.Minute)
	md.ctl.sm.Tick(at, nil, nil, sched)
	if got := md.ctl.sm.Current().State; got != inverter.StateEventStarted {
		t.Fatalf("state = %v, want EVENT_STARTED", got)
	}
	if members := fleet.batteries(); len(members) != 1 || members[0].key != batLFDI {
		t.Errorf("grant members while the control runs = %+v, want only %s", members, batLFDI)
	}

	// It completes: the device rejoins.
	at = at.Add(2 * time.Hour)
	md.ctl.sm.Tick(at, nil, nil, sched)
	if got := md.ctl.sm.Current().State; got != inverter.StateDefault {
		t.Fatalf("state = %v, want DEFAULT after completion", got)
	}
	if members := fleet.batteries(); len(members) != 2 {
		t.Errorf("grant members after completion = %+v, want both", members)
	}
}

// A failed Response names the device that failed to send it, on both the
// failure line and the dead-letter line.
func TestResponsePOSTHook_FailureLogsNameTheManagedDevice(t *testing.T) {
	mask := sep2.HexBinary8(0x07)
	dc := buildControl("EVT-LOG-001", "/rsps", &mask, time.Minute, 60)
	cfg := inverter.ResponseRetryConfig{MaxAttempts: 2, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, BackoffMultiplier: 2}

	for name, errs := range map[string][]error{
		"response POST failed": {fmt.Errorf("simulated 400")},
		"DEAD-LETTER": {
			fmt.Errorf("simulated 503: %w", inverter.ErrResponseTransient),
			fmt.Errorf("simulated 503: %w", inverter.ErrResponseTransient),
		},
	} {
		sm, sched := newTestStateMachine(func() time.Time { return fixedTestNow })
		sm.AddTransitionHook(responsePOSTHook(&fakePoster{errsLeft: errs}, batLFDI, fixedNowFn, cfg))
		logs := captureManagedLog(t)
		sm.Tick(fixedTestNow, []sep2.DERControl{dc}, nil, sched)
		var line string
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, name) {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no %q line logged; got:\n%s", name, logs.String())
			continue
		}
		if want := fmt.Sprintf("lfdi=%q", batLFDI); !strings.Contains(line, want) {
			t.Errorf("%q line = %q, want it to contain %s", name, line, want)
		}
	}
}

// An active event ends on time even while the DERControlList cannot be read.
func TestManagedControls_ActiveEventEndsOnTimeWhileThePollFails(t *testing.T) {
	// Started 1s ago, runs 3s.
	client, srv := newControlsFixture(t, time.Second, 3, pvLFDI, batLFDI)
	fleet, _, _ := startControlFleet(t, client, "follow")
	waitFor(t, "both devices to start the control", func() bool { return allStarted(fleet) })

	srv.setFailControls(true)
	failedFrom := srv.hitsOf("/derc")
	waitFor(t, "the PV to leave EVENT_STARTED with its polls failing", func() bool {
		return deviceByLFDI(fleet, pvLFDI).ctl.sm.Current().State != inverter.StateEventStarted
	})
	if n := srv.hitsOf("/derc") - failedFrom; n < 1 {
		t.Errorf("only %d failing polls were made, so the event did not end while polls failed", n)
	}
	if deviceByLFDI(fleet, pvLFDI).ctl.active() {
		t.Errorf("PV still reports an active event after its end time")
	}
	if got := defaultTargetW(deviceByLFDI(fleet, pvLFDI)); got != -1 {
		t.Errorf("fixture holds a default of %d W, want none", got)
	}
}

// A program added after the session started is followed, the wait is logged
// naming the device and its LFDI, and the session still ends with the context.
func TestManagedControls_ProgramAddedAfterStartIsFollowed(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setProgMode(progEmpty)
	logs := captureSessionLog(t)
	fleet, _, cancel, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)

	waitFor(t, "three program list reads", func() bool { return srv.hitsOf("/dp") >= 3 })
	if got := deviceByLFDI(fleet, pvLFDI).ctl.sm.Current().State; got != inverter.StateDefault {
		t.Fatalf("PV state with no program = %v, want DEFAULT", got)
	}
	want := "managed device pv (" + pvLFDI + "): no DERProgram with a DERControlList"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("no log line %q; got:\n%s", want, logs.String())
	}

	srv.setProgMode(progNormal)
	waitFor(t, "both devices to follow the added program", func() bool { return allStarted(fleet) })
	for _, lfdi := range []string{pvLFDI, batLFDI} {
		waitFor(t, "Responses from "+lfdi, func() bool { return len(srv.statusesFor(lfdi, "CTL1")) >= 2 })
	}
	cancel()
	waitControlsBounded(t, fleet, "the context was cancelled")
}

// A program with no DERControlList link is skipped, without a nil
// dereference, in favour of one that has it; and when it is the only one the
// session waits instead of ending.
func TestManagedControls_ProgramWithoutAControlListIsSkipped(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setProgMode(progNoList)
	fleet, _, cancel, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)

	waitFor(t, "three program list reads", func() bool { return srv.hitsOf("/dp") >= 3 })
	if n := srv.hitsOf("/derc"); n != 0 {
		t.Errorf("/derc was read %d times with no program offering a control list", n)
	}

	srv.setProgMode(progNoListAndNormal)
	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	cancel()
	waitControlsBounded(t, fleet, "the context was cancelled")
}

// A default control that cannot be read does not stop the events being
// followed, is retried on each poll, and the last one read keeps driving the
// device when a later read fails.
func TestManagedControls_UnreadableDefaultDoesNotStopEventsAndKeepsTheLast(t *testing.T) {
	client, srv := newControlsFixtureWith(t, true, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setDefault(true, 0)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
	pv := deviceByLFDI(fleet, pvLFDI)

	waitFor(t, "both devices to follow the event with the default unreadable", func() bool { return allStarted(fleet) })
	waitFor(t, "the default to be retried", func() bool { return srv.hitsOf("/dc") >= 6 })
	if got := defaultTargetW(pv); got != -1 {
		t.Errorf("PV holds a %d W default that was never readable", got)
	}

	srv.setDefault(false, 700)
	waitFor(t, "the PV to read the default", func() bool { return defaultTargetW(pv) == 700 })

	// The default fails again: the 700 W one stays.
	srv.setDefault(true, 0)
	before := srv.hitsOf("/dc")
	waitFor(t, "failing default reads", func() bool { return srv.hitsOf("/dc") >= before+4 })
	if got := defaultTargetW(pv); got != 700 {
		t.Errorf("PV default after failed reads = %d W, want the last one read, 700 W", got)
	}
}

// Every read and Response of the sessions is made as the device: each one is
// judged against the managed set, so the judgements cover every request the
// server saw (FSA lists, programs, defaults, control lists and Responses).
func TestManagedControls_EveryReadIsMadeAsTheDevice(t *testing.T) {
	client, srv := newControlsFixtureWith(t, true, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, cancel, rec := startControlFleetEvery(t, client, "follow", 10*time.Millisecond)

	waitFor(t, "control list and default reads", func() bool { return srv.hitsOf("/derc") >= 30 && srv.hitsOf("/dc") >= 30 })
	for _, lfdi := range []string{pvLFDI, batLFDI} {
		waitFor(t, "Responses from "+lfdi, func() bool { return len(srv.statusesFor(lfdi, "CTL1")) >= 2 })
	}
	cancel()
	waitControlsBounded(t, fleet, "the context was cancelled")

	want := srv.hitsOf("/fsa/"+pvLFDI) + srv.hitsOf("/fsa/"+batLFDI) + srv.hitsOf("/dp") +
		srv.hitsOf("/dc") + srv.hitsOf("/derc") +
		len(srv.statusesFor(pvLFDI, "CTL1")) + len(srv.statusesFor(batLFDI, "CTL1"))
	if got := rec.judged(pvLFDI) + rec.judged(batLFDI); got < want {
		t.Errorf("the devices were the request target %d times, want at least %d (every read and Response the server saw)", got, want)
	}
}

// Retries of a failing discovery back off: each wait doubles.
func TestManagedControls_DiscoveryBackoffGrows(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setFailFSA(pvLFDI, true)
	_, _, cancel, _ := startControlFleetEvery(t, client, "follow", 40*time.Millisecond)
	defer cancel()

	waitFor(t, "four failed FSA lists for the PV", func() bool { return len(srv.hitTimes("/fsa/"+pvLFDI)) >= 4 })
	ts := srv.hitTimes("/fsa/" + pvLFDI)
	// Waits of 40, 80 and 160 ms; a timer never fires early, so lower bounds
	// with room for server-side jitter.
	for i, min := range []time.Duration{35 * time.Millisecond, 70 * time.Millisecond, 140 * time.Millisecond} {
		if gap := ts[i+1].Sub(ts[i]); gap < min {
			t.Errorf("retry %d came %v after the previous attempt, want at least %v (the wait must double)", i+1, gap, min)
		}
	}
}

func TestControlSession_PollIntervalPrecedence(t *testing.T) {
	dcap := sep2.DeviceCapability{PollRate: 3600}
	cases := []struct {
		name string
		cs   *controlSession
		dcap sep2.DeviceCapability
		want time.Duration
	}{
		{"test seam wins", &controlSession{interval: 5 * time.Millisecond, pollS: 7}, dcap, 5 * time.Millisecond},
		{"poll_s beats the server pollRate", &controlSession{pollS: 7}, dcap, 7 * time.Second},
		{"server pollRate", &controlSession{}, dcap, time.Hour},
		{"nothing set", &controlSession{}, sep2.DeviceCapability{}, defaultControlPoll},
	}
	for _, c := range cases {
		if got := c.cs.pollInterval(c.dcap); got != c.want {
			t.Errorf("%s: pollInterval = %v, want %v", c.name, got, c.want)
		}
	}
}

// WaitControls returns only after every session goroutine has finished, not
// when the context is cancelled.
func TestManagedControls_WaitControlsWaitsForEverySession(t *testing.T) {
	client, _ := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	pv := managedCfg(t, "pv", pvLFDI, "pv", 4000)
	pv.Controls = &simconfig.Controls{Response: "follow"}
	ctx, cancel := context.WithCancel(context.Background())
	fleet, err := startManaged(ctx, client, sep2.DeviceCapability{}, "/edev", []simconfig.Managed{pv}, time.Hour)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	release := make(chan struct{})
	exited := make(chan struct{}, 1)
	fleet.devices[0].ctl.interval = 20 * time.Millisecond
	fleet.devices[0].ctl.onExit = func() { exited <- struct{}{}; <-release }
	fleet.StartControls(ctx, controlsEnv{client: client, cfg: inverter.SimConfig{}})
	waitFor(t, "the PV to start the control", func() bool { return allStarted(fleet) })

	cancel()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not run onExit within 5s of the context being cancelled")
	}
	done := make(chan struct{})
	go func() { fleet.WaitControls(); close(done) }()
	select {
	case <-done:
		t.Fatal("WaitControls returned while a session goroutine was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitControls did not return after the session finished")
	}
}

// A walk that finds no program waits one interval before looking again, and
// does not spin against the server.
func TestManagedControls_NoProgramRewalkWaitsAnInterval(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setProgMode(progEmpty)
	_, _, cancel, _ := startControlFleetEvery(t, client, "follow", 100*time.Millisecond)
	defer cancel()

	waitFor(t, "six program list reads", func() bool { return len(srv.hitTimes("/dp")) >= 6 })
	ts := srv.hitTimes("/dp")
	// Two devices share /dp. Whichever hit first reads again at least two
	// intervals later, so the last of six hits trails the first by that much.
	if span := ts[len(ts)-1].Sub(ts[0]); span < 190*time.Millisecond {
		t.Errorf("six re-walks took %v, want at least 190ms for two 100ms intervals", span)
	}
}

// targetW is the target the device's output follows right now, or -1 when
// nothing drives it.
func targetW(md *managedDevice) int {
	b := md.ctl.base()
	if b == nil || b.OpModTargetW == nil {
		return -1
	}
	return int(b.OpModTargetW.Value)
}

// A program that outranks the followed one, added while the session runs,
// is switched to when the session is told the program list changed: the old
// program's control ends and the new program's control list drives the
// device.
func TestManagedControls_OutrankingProgramAddedAfterStartIsSwitchedTo(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)

	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	for _, md := range fleet.devices {
		if got := targetW(md); got != controlTargetW {
			t.Fatalf("device %s target = %d W before the change, want %d W", md.name, got, controlTargetW)
		}
	}

	srv.setProgMode(progWithPrimaryAdded)
	for _, md := range fleet.devices {
		md.ctl.kick()
	}

	waitFor(t, "both devices to follow PROG2", func() bool {
		for _, md := range fleet.devices {
			if targetW(md) != secondControlW {
				return false
			}
		}
		return true
	})
	for _, md := range fleet.devices {
		active := md.ctl.sm.Current().ActiveDERControl
		if active == nil || active.MRID != "CTL2" {
			t.Errorf("device %s active control = %+v, want CTL2", md.name, active)
		}
	}
	if srv.hitsOf("/derc2") == 0 {
		t.Error("PROG2's control list was never read")
	}
}

// A program deleted on the server ends its session's polling of the control
// list, ends the control it was driving, drops the program's default, and
// returns the session to discovery.
func TestManagedControls_DeletedProgramStopsPollingAndReturnsToDiscovery(t *testing.T) {
	client, srv := newControlsFixtureWith(t, true, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setDefault(false, 900)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)

	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })

	srv.setProgMode(progEmpty)
	for _, md := range fleet.devices {
		md.ctl.kick()
	}
	waitFor(t, "both devices to leave the deleted program", func() bool {
		for _, md := range fleet.devices {
			if md.ctl.sm.Current().State == inverter.StateEventStarted || md.ctl.defaultCtl.Load() != nil {
				return false
			}
		}
		return true
	})
	for _, md := range fleet.devices {
		if got := targetW(md); got != -1 {
			t.Errorf("device %s still follows a %d W target after its program was deleted", md.name, got)
		}
	}

	controlReads, programReads := srv.hitsOf("/derc"), srv.hitsOf("/dp")
	waitFor(t, "discovery to keep walking", func() bool { return srv.hitsOf("/dp") >= programReads+4 })
	if got := srv.hitsOf("/derc"); got != controlReads {
		t.Errorf("control list read %d more times after the program was deleted", got-controlReads)
	}
}

// The slow periodic re-walk finds a change nobody notified.
func TestManagedControls_PeriodicRewalkFindsAnUnnotifiedProgram(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond,
		func(cs *controlSession) { cs.rewalk = 40 * time.Millisecond })

	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	srv.setProgMode(progWithPrimaryAdded)
	waitFor(t, "both devices to follow PROG2", func() bool {
		for _, md := range fleet.devices {
			if targetW(md) != secondControlW {
				return false
			}
		}
		return true
	})
}

// A program whose walk fails keeps being followed: a failed re-walk is not
// a deleted program.
func TestManagedControls_FailedRewalkKeepsTheFollowedProgram(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)

	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	srv.setFailFSA(pvLFDI, true)
	pv := deviceByLFDI(fleet, pvLFDI)
	reads := srv.hitsOf("/fsa/" + pvLFDI)
	pv.ctl.kick()
	waitFor(t, "the failing re-walk", func() bool { return srv.hitsOf("/fsa/"+pvLFDI) > reads })
	time.Sleep(100 * time.Millisecond)
	if got := targetW(pv); got != controlTargetW {
		t.Errorf("target after a failed re-walk = %d W, want %d W", got, controlTargetW)
	}
}

// startSubscribedFleet starts a PV and a battery whose sessions subscribe
// through router, with the control poll at interval.
func startSubscribedFleet(t *testing.T, client *inverter.SEP2Client, router *notifyRouter, interval time.Duration) *managedFleet {
	t.Helper()
	pv := managedCfg(t, "pv", pvLFDI, "pv", 4000)
	bat := managedCfg(t, "bat", batLFDI, "battery", 0)
	pv.Controls = &simconfig.Controls{Response: "follow"}
	bat.Controls = &simconfig.Controls{Response: "follow"}
	ctx, cancel := context.WithCancel(context.Background())
	fleet, err := startManaged(ctx, client, sep2.DeviceCapability{}, "/edev", []simconfig.Managed{pv, bat}, time.Hour)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	for _, md := range fleet.devices {
		md.ctl.interval = interval
	}
	fleet.StartControls(ctx, controlsEnv{client: client, notify: &sessionNotify{poster: client, url: "https://client.example/notify", router: router}})
	t.Cleanup(func() { waitControlsBounded(t, fleet, "the test ended") })
	t.Cleanup(cancel)
	return fleet
}

// Sessions subscribe to their own device's FSA list and to each program
// list they walked, once, as that device; a notification for a subscribed
// list wakes only the session that subscribed to it.
func TestManagedControls_SessionsSubscribeAndNotificationsWakeThem(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	router := newNotifyRouter()
	fleet := startSubscribedFleet(t, client, router, 20*time.Millisecond)

	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	for _, lfdi := range []string{pvLFDI, batLFDI} {
		waitFor(t, "subscriptions of "+lfdi, func() bool { return len(srv.subscriptionsTo("/sub/"+lfdi)) >= 2 })
		got := append([]string(nil), srv.subscriptionsTo("/sub/"+lfdi)...)
		sort.Strings(got)
		want := []string{"/dp", "/fsa/" + lfdi}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("device %s subscribed to %v, want %v", lfdi, got, want)
		}
	}

	// Another walk subscribes to nothing it already has.
	srv.setProgMode(progWithPrimaryAdded)
	deviceByLFDI(fleet, pvLFDI).ctl.kick()
	waitFor(t, "the PV to follow PROG2", func() bool { return targetW(deviceByLFDI(fleet, pvLFDI)) == secondControlW })
	if got := len(srv.subscriptionsTo("/sub/" + pvLFDI)); got != 2 {
		t.Errorf("PV holds %d subscriptions after a re-walk, want 2", got)
	}

	// The bat has not been woken, so it still follows PROG1; a notification
	// for the bat's FSA list wakes it alone.
	bt := deviceByLFDI(fleet, batLFDI)
	time.Sleep(100 * time.Millisecond)
	if got := targetW(bt); got != controlTargetW {
		t.Fatalf("battery target = %d W before its notification, want %d W", got, controlTargetW)
	}
	if !router.route(sep2.Notification{SubscribedResource: "/fsa/" + batLFDI, Status: sep2.NotificationStatusChanged}) {
		t.Fatal("notification for the battery's FSA list was not routed")
	}
	waitFor(t, "the battery to follow PROG2", func() bool { return targetW(bt) == secondControlW })
}

// A subscription the server ended is made again on the next walk.
func TestManagedControls_CancelledSubscriptionIsMadeAgain(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	router := newNotifyRouter()
	fleet := startSubscribedFleet(t, client, router, 20*time.Millisecond)
	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	waitFor(t, "the PV's subscriptions", func() bool { return len(srv.subscriptionsTo("/sub/"+pvLFDI)) >= 2 })

	router.route(sep2.Notification{SubscribedResource: "/dp", Status: sep2.NotificationStatusCanceledNoInfo})
	waitFor(t, "the PV to subscribe to /dp again", func() bool {
		n := 0
		for _, h := range srv.subscriptionsTo("/sub/" + pvLFDI) {
			if h == "/dp" {
				n++
			}
		}
		return n >= 2
	})
}

// A 405 means the server takes no subscriptions: the session asks once and
// relies on the timer from then on.
func TestManagedControls_RefusedSubscriptionsAreNotAskedForAgain(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setSubStatus(http.StatusMethodNotAllowed)
	fleet := startSubscribedFleet(t, client, newNotifyRouter(), 20*time.Millisecond)
	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	pv := deviceByLFDI(fleet, pvLFDI)
	for range 3 {
		pv.ctl.kick()
		time.Sleep(60 * time.Millisecond)
	}
	if got := srv.subAttemptsTo("/sub/" + pvLFDI); got != 1 {
		t.Errorf("PV posted %d Subscriptions to a server that answered 405, want 1", got)
	}
}

// A subscription that failed is retried on a later walk.
func TestManagedControls_FailedSubscriptionIsRetriedOnALaterWalk(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setSubStatus(http.StatusInternalServerError)
	fleet := startSubscribedFleet(t, client, newNotifyRouter(), 20*time.Millisecond)
	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })
	waitFor(t, "the first attempts", func() bool { return srv.subAttemptsTo("/sub/"+pvLFDI) >= 2 })
	if got := len(srv.subscriptionsTo("/sub/" + pvLFDI)); got != 0 {
		t.Fatalf("PV holds %d subscriptions from a server that refused them", got)
	}

	srv.setSubStatus(0)
	deviceByLFDI(fleet, pvLFDI).ctl.kick()
	waitFor(t, "the PV's subscriptions after the server recovered", func() bool { return len(srv.subscriptionsTo("/sub/"+pvLFDI)) >= 2 })
}

// A program that keeps its mRID but moves its control list is followed at the
// new list.
func TestManagedControls_ProgramWithAMovedControlListIsFollowedAtTheNewList(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })

	srv.setProgMode(progListMoved)
	pv := deviceByLFDI(fleet, pvLFDI)
	pv.ctl.kick()
	waitFor(t, "the PV to follow the moved list", func() bool { return targetW(pv) == secondControlW })
}

// A device with no program at start is woken by a notification, not left to
// wait out the poll interval.
func TestManagedControls_NotificationWakesASessionStillInDiscovery(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setProgMode(progEmpty)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 30*time.Second)
	waitFor(t, "the first walks", func() bool { return srv.hitsOf("/dp") >= 2 })

	srv.setProgMode(progNormal)
	for _, md := range fleet.devices {
		md.ctl.kick()
	}
	waitFor(t, "both devices to follow PROG1 well before the 30s poll", func() bool { return allStarted(fleet) })
}

// A switch hands the replacement straight to the follower: a walk that fails
// after the switch leaves the device on the new program, not on none.
func TestManagedControls_SwitchNeedsNoSecondWalk(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
	waitFor(t, "both devices to follow PROG1", func() bool { return allStarted(fleet) })

	srv.setProgMode(progWithPrimaryAdded)
	srv.failFSANextRead(pvLFDI)
	pv := deviceByLFDI(fleet, pvLFDI)
	reads := srv.hitsOf("/fsa/" + pvLFDI)
	pv.ctl.kick()
	waitFor(t, "the PV to follow PROG2", func() bool { return targetW(pv) == secondControlW })
	if got := srv.hitsOf("/fsa/"+pvLFDI) - reads; got != 1 {
		t.Errorf("the switch read the PV's FSA list %d times, want 1", got)
	}
}

// A control dropped because a different program outranks its program is
// reported as superseded by an alternate program (status 14); one whose
// program was deleted is reported as cancelled (status 6).
func TestManagedControls_DroppedControlResponseStatus(t *testing.T) {
	tests := []struct {
		name string
		mode int
		want []int
	}{
		{"outranked by another program", progWithPrimaryAdded, []int{1, 2, 14}},
		{"program deleted", progEmpty, []int{1, 2, 6}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
			srv.setRespMask(0x03)
			fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
			pv := deviceByLFDI(fleet, pvLFDI)
			waitFor(t, "the PV to start CTL1", func() bool { return pv.ctl.sm.Current().State == inverter.StateEventStarted })

			srv.setProgMode(tc.mode)
			pv.ctl.kick()
			waitFor(t, "the third Response", func() bool { return len(srv.statusesFor(pvLFDI, "CTL1")) >= 3 })
			time.Sleep(100 * time.Millisecond)
			if got := srv.statusesFor(pvLFDI, "CTL1"); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("CTL1 statuses = %v, want %v", got, tc.want)
			}
		})
	}
}

// One program whose control list cannot be read does not stop a re-walk from
// seeing the others.
func TestManagedControls_BrokenSiblingProgramDoesNotBlindTheRewalk(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
	pv := deviceByLFDI(fleet, pvLFDI)
	waitFor(t, "the PV to follow PROG1", func() bool { return targetW(pv) == controlTargetW })

	srv.setProgMode(progBrokenSibling)
	pv.ctl.kick()
	waitFor(t, "the PV to follow PROG2", func() bool { return targetW(pv) == secondControlW })
	if srv.hitsOf("/derc3") == 0 {
		t.Error("the broken sibling's control list was never read, so the walk did not meet it")
	}
}

// The receiver's callback gives a notification to the session that
// subscribed to it and to nobody else, and every other one to the fallback.
func TestRouteThenDispatch(t *testing.T) {
	r := newNotifyRouter()
	cs := newControlSession(nil)
	r.watch("/edev/1/fsa", cs)
	var fell []string
	h := routeThenDispatch(r, func(_ context.Context, n sep2.Notification) { fell = append(fell, n.SubscribedResource) })

	h(context.Background(), sep2.Notification{SubscribedResource: "/edev/1/fsa"})
	h(context.Background(), sep2.Notification{SubscribedResource: "/derp/own"})

	if len(cs.wake) != 1 {
		t.Error("the subscribed session was not woken")
	}
	if fmt.Sprint(fell) != "[/derp/own]" {
		t.Errorf("fallback got %v, want only [/derp/own]", fell)
	}
}

func TestNotifyRouter_RoutesByResourceAndReportsUnknown(t *testing.T) {
	r := newNotifyRouter()
	a, b := newControlSession(nil), newControlSession(nil)
	r.watch("/edev/1/fsa", a)
	r.watch("/edev/1/fsa/1/derp", a)
	r.watch("/edev/2/fsa", b)

	tests := []struct {
		name     string
		n        sep2.Notification
		handled  bool
		wantA    bool
		wantB    bool
		unwatchA bool
	}{
		{"subscribed resource wakes its watcher", sep2.Notification{SubscribedResource: "/edev/1/fsa"}, true, true, false, false},
		{"href is the fallback", sep2.Notification{Resource: sep2.Resource{Href: "/edev/2/fsa"}}, true, false, true, false},
		{"unknown resource is not handled", sep2.Notification{SubscribedResource: "/edev/9/fsa"}, false, false, false, false},
		{"cancelled subscription still wakes and is forgotten", sep2.Notification{SubscribedResource: "/edev/1/fsa/1/derp", Status: sep2.NotificationStatusCanceledNoInfo}, true, true, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drain(a)
			drain(b)
			a.setSubscribed("/edev/1/fsa/1/derp", true)
			if got := r.route(tc.n); got != tc.handled {
				t.Errorf("route handled = %v, want %v", got, tc.handled)
			}
			if got := len(a.wake) == 1; got != tc.wantA {
				t.Errorf("session A woken = %v, want %v", got, tc.wantA)
			}
			if got := len(b.wake) == 1; got != tc.wantB {
				t.Errorf("session B woken = %v, want %v", got, tc.wantB)
			}
			if got := a.subscribedTo("/edev/1/fsa/1/derp"); got == tc.unwatchA {
				t.Errorf("session A holds the subscription = %v after the notification, want %v", got, !tc.unwatchA)
			}
		})
	}
}

func drain(cs *controlSession) {
	select {
	case <-cs.wake:
	default:
	}
}

// A higher-primacy program whose control list cannot be read is not chosen
// over a readable one, whether the session is discovering or re-walking.
func TestManagedControls_UnreadableHigherPrimacyProgramIsNotChosen(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setProgMode(progBrokenHigher)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
	pv := deviceByLFDI(fleet, pvLFDI)
	waitFor(t, "the PV to follow the readable PROG1", func() bool { return targetW(pv) == controlTargetW })

	reads := srv.hitsOf("/fsa/" + pvLFDI)
	pv.ctl.kick()
	waitFor(t, "the re-walk that meets the broken program", func() bool { return srv.hitsOf("/fsa/"+pvLFDI) > reads })
	time.Sleep(100 * time.Millisecond)
	if got := targetW(pv); got != controlTargetW {
		t.Errorf("PV target = %d W with an unreadable higher-primacy program, want %d W", got, controlTargetW)
	}
	if srv.hitsOf("/derc3") == 0 {
		t.Error("the unreadable program's control list was never read")
	}
}

// A control list that cannot be read during a re-walk does not make the
// followed program look gone.
func TestManagedControls_FollowedProgramWithAnUnreadableListIsKept(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	fleet, _, _, _ := startControlFleetEvery(t, client, "follow", 20*time.Millisecond)
	pv := deviceByLFDI(fleet, pvLFDI)
	waitFor(t, "the PV to follow PROG1", func() bool { return targetW(pv) == controlTargetW })

	srv.setFailControls(true)
	reads := srv.hitsOf("/fsa/" + pvLFDI)
	pv.ctl.kick()
	waitFor(t, "the re-walk", func() bool { return srv.hitsOf("/fsa/"+pvLFDI) > reads })
	time.Sleep(100 * time.Millisecond)
	if got := targetW(pv); got != controlTargetW {
		t.Errorf("PV target = %d W after a transient control list failure, want %d W", got, controlTargetW)
	}
	if got := pv.ctl.sm.Current().State; got != inverter.StateEventStarted {
		t.Errorf("PV state = %v, want the control still started", got)
	}
}

// A managed device has only a watt rating, so a %setMaxVar fixed var is
// rejected at receipt: each device posts one 253 for it and never starts it.
func TestManagedControls_UnhonorableControlIsRejectedWith253(t *testing.T) {
	client, srv := newControlsFixture(t, 5*time.Second, 3600, pvLFDI, batLFDI)
	srv.setControlFixedVar(sep2.FixedVar{RefType: 2, Value: 5000})
	fleet, _, _ := startControlFleet(t, client, "follow")

	for _, l := range []string{pvLFDI, batLFDI} {
		waitFor(t, "a Response from "+l, func() bool { return len(srv.statusesFor(l, "CTL1")) > 0 })
	}
	// Further polls re-read the same list; none may post again or start it.
	time.Sleep(200 * time.Millisecond)
	for _, l := range []string{pvLFDI, batLFDI} {
		if got := srv.statusesFor(l, "CTL1"); len(got) != 1 || got[0] != 253 {
			t.Errorf("%s posted %v for CTL1, want exactly [253]", l, got)
		}
		if st := deviceByLFDI(fleet, l).ctl.sm.Current().State; st == inverter.StateEventStarted {
			t.Errorf("%s started the rejected control", l)
		}
	}
}

func TestManagedDevice_RatingsHoldOnlyTheWattRating(t *testing.T) {
	md := &managedDevice{ratedW: 4000}
	if got := md.ratings(); got != (inverter.Ratings{SetMaxW: 4000}) {
		t.Errorf("ratings = %+v, want only SetMaxW 4000: a managed device has no reactive rating or reserve", got)
	}
}

// A managed device whose control is refused falls back to its default
// control instead of handing the device the empty refused output.
func TestManagedControls_RefusedControlFallsBackToTheDefault(t *testing.T) {
	fleet, probes := batteryFleet(t, "follow")
	md := fleet.devices[0]
	md.ctl.defaultCtl.Store(&sep2.DefaultDERControl{DERControlBase: &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: 700}}})
	// No session validator runs here, so the refused event reaches the tick.
	fv := sep2.FixedVar{RefType: 2, Value: 5000}
	var dc sep2.DERControl
	dc.MRID = "CTL1"
	dc.Interval = &sep2.DateTimeInterval{Start: time.Now().Add(-time.Minute).Unix(), Duration: 3600}
	dc.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: controlTargetW}, OpModFixedVar: &fv}
	md.ctl.sm.Tick(time.Now(), []sep2.DERControl{dc}, nil, inverter.NewScheduler(time.Now, rand.New(rand.NewPCG(1, 2))))
	if st := md.ctl.sm.Current().State; st != inverter.StateEventStarted {
		t.Fatalf("setup: state %s, want EVENT_STARTED", st)
	}

	fleet.Tick(context.Background())
	if got := probes[md.lfdi].achievedW(); got != 700 {
		t.Errorf("achieved %v W, want 700 W from the default control", got)
	}
}
