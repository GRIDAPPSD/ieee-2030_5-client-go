package main

// Tests for the DER resources a managed device reports
// (managed_status.go, GRIDAPPSD/ieee-2030_5-client-go#76): capability and
// settings once from the rating, status and availability at the status
// interval, availability values for a full and an empty battery, nothing
// from a device that failed its tick, and every PUT judged for its device.

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
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

// derServer lists each LFDI with a DERList of one DER whose four resources
// live under /der/<lfdi>/, and keeps every PUT body by path.
type derServer struct {
	mu   sync.Mutex
	puts map[string][]string
	// failing makes a PUT to the path answer 500; the attempt is still kept.
	failing map[string]bool
}

func (s *derServer) bodies(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts[path]...)
}

func (s *derServer) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.puts {
		n += len(b)
	}
	return n
}

func newDERServer(t *testing.T, lfdis ...string) (*inverter.SEP2Client, *derServer) {
	t.Helper()
	env := newDERWalkTestEnv(t)
	srv := &derServer{puts: map[string][]string{}, failing: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		var list sep2.EndDeviceList
		for _, l := range lfdis {
			list.EndDevice = append(list.EndDevice, sep2.EndDevice{LFDI: l, DERListLink: &sep2.ListLink{Href: "/derlist/" + l}})
		}
		writeSepXML(t, w, list)
	})
	mux.HandleFunc("/mup", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/mup/1")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/mup/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	for _, l := range lfdis {
		mux.HandleFunc("/derlist/"+l, func(w http.ResponseWriter, _ *http.Request) {
			base := "/der/" + l + "/"
			writeSepXML(t, w, sep2.DERList{
				ListResource: sep2.ListResource{All: 1, Results: 1},
				DER: []sep2.DER{{
					DERCapabilityLink:   &sep2.Link{Href: base + "cap"},
					DERSettingsLink:     &sep2.Link{Href: base + "set"},
					DERStatusLink:       &sep2.Link{Href: base + "stat"},
					DERAvailabilityLink: &sep2.Link{Href: base + "avail"},
				}},
			})
		})
	}
	mux.HandleFunc("/der/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		srv.mu.Lock()
		srv.puts[r.URL.Path] = append(srv.puts[r.URL.Path], string(b))
		fail := srv.failing[r.URL.Path]
		srv.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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

func (s *derServer) setFailing(path string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[path] = v
}

// scriptedDevice returns the reading and state it is given, so a test picks
// the tick's maximum possible output, current output and connection state.
type scriptedDevice struct {
	device.DERDevice
	reading device.StateReading
	state   inverter.InverterState
	soc     float64
}

func (d scriptedDevice) ReadState(context.Context) (device.StateReading, error) {
	return d.reading, nil
}

func (d scriptedDevice) ApplySetpoint(context.Context, inverter.ControlOutputs) (inverter.InverterState, error) {
	return d.state, nil
}

func (d scriptedDevice) SOC() float64 { return d.soc }

func batteryAt(t *testing.T, lfdi string, soc float64) simconfig.Managed {
	m := managedCfg(t, "bat", lfdi, "battery", 0)
	m.Device.InitialSOC = &soc
	return m
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := xml.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("unmarshal %T from %q: %v", v, body, err)
	}
	return v
}

func startDERFleet(t *testing.T, client *inverter.SEP2Client, every time.Duration, cfg ...simconfig.Managed) (*managedFleet, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", cfg, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	fleet.setStatusInterval(every)
	return fleet, ctx
}

// A full battery offers its rating for discharge, 7200 s of it (10 kWh at
// 5 kW), and no charge room; its status carries a full state of charge.
func TestManagedStatus_FullBatteryAvailabilityAndStatus(t *testing.T) {
	client, srv := newDERServer(t, batLFDI)
	fleet, ctx := startDERFleet(t, client, time.Minute, batteryAt(t, batLFDI, 1))
	fleet.Tick(ctx)

	avails := srv.bodies("/der/" + batLFDI + "/avail")
	if len(avails) != 1 {
		t.Fatalf("%d availability PUTs, want 1", len(avails))
	}
	a := decode[sep2.DERAvailability](t, avails[0])
	if a.StatWAvail == nil || a.StatWAvail.Value != 5000 || a.StatWAvail.Multiplier != 0 {
		t.Errorf("statWAvail = %+v, want 5000 W", a.StatWAvail)
	}
	if a.AvailabilityDuration == nil || *a.AvailabilityDuration != 7200 {
		t.Errorf("availabilityDuration = %v, want 7200 s", a.AvailabilityDuration)
	}
	if a.MaxChargeDuration == nil || *a.MaxChargeDuration != 0 {
		t.Errorf("maxChargeDuration = %v, want 0 (full)", a.MaxChargeDuration)
	}
	if a.ReadingTime == 0 {
		t.Error("availability readingTime = 0, want the synchronized clock")
	}

	st := decode[sep2.DERStatus](t, srv.bodies("/der/" + batLFDI + "/stat")[0])
	if st.StateOfChargeStatus == nil || st.StateOfChargeStatus.Value != 10000 {
		t.Errorf("stateOfChargeStatus = %+v, want 10000 (100%%)", st.StateOfChargeStatus)
	}
	if st.GenConnectStatus == nil || st.OperationalModeStatus == nil || st.ReadingTime == 0 {
		t.Errorf("status = %+v, want connection, operational mode and readingTime", st)
	}
}

// An empty battery offers nothing for discharge, stated as an explicit zero
// rather than an absent element, and 7200 s of charge room.
func TestManagedStatus_EmptyBatteryAvailabilityAndStatus(t *testing.T) {
	client, srv := newDERServer(t, batLFDI)
	fleet, ctx := startDERFleet(t, client, time.Minute, batteryAt(t, batLFDI, 0))
	fleet.Tick(ctx)

	raw := srv.bodies("/der/" + batLFDI + "/avail")
	if len(raw) != 1 {
		t.Fatalf("%d availability PUTs, want 1", len(raw))
	}
	if !strings.Contains(raw[0], "statWAvail") {
		t.Errorf("statWAvail absent from %q, want an explicit zero", raw[0])
	}
	a := decode[sep2.DERAvailability](t, raw[0])
	if a.StatWAvail == nil || a.StatWAvail.Value != 0 {
		t.Errorf("statWAvail = %+v, want 0 W", a.StatWAvail)
	}
	if a.AvailabilityDuration == nil || *a.AvailabilityDuration != 0 {
		t.Errorf("availabilityDuration = %v, want 0 s", a.AvailabilityDuration)
	}
	if a.MaxChargeDuration == nil || *a.MaxChargeDuration != 7200 {
		t.Errorf("maxChargeDuration = %v, want 7200 s", a.MaxChargeDuration)
	}

	st := decode[sep2.DERStatus](t, srv.bodies("/der/" + batLFDI + "/stat")[0])
	if st.StateOfChargeStatus == nil || st.StateOfChargeStatus.Value != 0 {
		t.Errorf("stateOfChargeStatus = %+v, want an explicit 0", st.StateOfChargeStatus)
	}
}

// Capability and settings come from the configured rating, once, and each
// device writes to its own DER.
func TestManagedStatus_CapabilityAndSettingsOnceFromTheRating(t *testing.T) {
	client, srv := newDERServer(t, pvLFDI, batLFDI)
	fleet, ctx := startDERFleet(t, client, time.Minute,
		managedCfg(t, "pv", pvLFDI, "pv", 3000), batteryAt(t, batLFDI, 0.5))
	clk := useFakeClock(fleet)
	fleet.Tick(ctx)
	clk.advance(2 * time.Minute)
	fleet.Tick(ctx)

	for _, lfdi := range []string{pvLFDI, batLFDI} {
		if n := len(srv.bodies("/der/" + lfdi + "/cap")); n != 1 {
			t.Errorf("%s: %d capability PUTs over two ticks, want 1", lfdi, n)
		}
		if n := len(srv.bodies("/der/" + lfdi + "/set")); n != 1 {
			t.Errorf("%s: %d settings PUTs over two ticks, want 1", lfdi, n)
		}
	}

	pv := decode[sep2.DERCapability](t, srv.bodies("/der/" + pvLFDI + "/cap")[0])
	if pv.RTGMaxW == nil || pv.RTGMaxW.Value != 5000 || pv.Type == nil || *pv.Type != 4 {
		t.Errorf("pv capability = %+v, want rtgMaxW 5000 and type 4", pv)
	}
	if pv.RTGMaxChargeRateW != nil {
		t.Error("pv capability carries a charge rate")
	}
	// connect 2, energize 3, fixed W 7, max limit W 20; a battery adds
	// charge 0 and discharge 1.
	const pvModes, batModes = sep2.DERControlType(1<<2 | 1<<3 | 1<<7 | 1<<20), sep2.DERControlType(1<<2 | 1<<3 | 1<<7 | 1<<20 | 1<<0 | 1<<1)
	if pv.ModesSupported == nil || *pv.ModesSupported != pvModes {
		t.Errorf("pv modesSupported = %v, want %#x", pv.ModesSupported, uint32(pvModes))
	}
	bat := decode[sep2.DERCapability](t, srv.bodies("/der/" + batLFDI + "/cap")[0])
	if bat.RTGMaxW == nil || bat.RTGMaxW.Value != 5000 || bat.Type == nil || *bat.Type != 80 ||
		bat.RTGMaxChargeRateW == nil || bat.RTGMaxChargeRateW.Value != 5000 ||
		bat.RTGMaxDischargeRateW == nil || bat.RTGMaxDischargeRateW.Value != 5000 {
		t.Errorf("battery capability = %+v, want 5000 W both ways and type 80", bat)
	}
	if bat.ModesSupported == nil || *bat.ModesSupported != batModes {
		t.Errorf("battery modesSupported = %v, want %#x", bat.ModesSupported, uint32(batModes))
	}
	set := decode[sep2.DERSettings](t, srv.bodies("/der/" + batLFDI + "/set")[0])
	if set.SetMaxW == nil || set.SetMaxW.Value != 5000 || set.UpdatedTime == 0 {
		t.Errorf("battery settings = %+v, want setMaxW 5000 and an updatedTime", set)
	}

	// A PV device recording 3000 W delivers all of it, so it has no reserve,
	// and reports no durations.
	a := decode[sep2.DERAvailability](t, srv.bodies("/der/" + pvLFDI + "/avail")[0])
	if a.StatWAvail == nil || a.StatWAvail.Value != 0 || a.AvailabilityDuration != nil {
		t.Errorf("pv availability = %+v, want statWAvail 0 and no durations", a)
	}
	if st := decode[sep2.DERStatus](t, srv.bodies("/der/" + pvLFDI + "/stat")[0]); st.StateOfChargeStatus != nil {
		t.Errorf("pv status carries a state of charge: %+v", st.StateOfChargeStatus)
	}
}

// Status and availability follow the status interval, not the tick.
func TestManagedStatus_FollowsTheStatusInterval(t *testing.T) {
	client, srv := newDERServer(t, pvLFDI)
	fleet, ctx := startDERFleet(t, client, 30*time.Second, managedCfg(t, "pv", pvLFDI, "pv", 3000))
	clk := useFakeClock(fleet)
	stat := "/der/" + pvLFDI + "/stat"

	fleet.Tick(ctx)
	clk.advance(10 * time.Second)
	fleet.Tick(ctx)
	if n := len(srv.bodies(stat)); n != 1 {
		t.Fatalf("%d status PUTs inside the interval, want 1", n)
	}
	clk.advance(25 * time.Second)
	fleet.Tick(ctx)
	if n, a := len(srv.bodies(stat)), len(srv.bodies("/der/"+pvLFDI+"/avail")); n != 2 || a != 2 {
		t.Errorf("after the interval: %d status and %d availability PUTs, want 2 and 2", n, a)
	}
}

// A device whose source fails sends neither status nor availability, and
// resumes with fresh values when it recovers.
func TestManagedStatus_FailedDeviceSendsNothing(t *testing.T) {
	for _, mode := range []string{"read", "apply"} {
		t.Run(mode, func(t *testing.T) {
			client, srv := newDERServer(t, batLFDI)
			fleet, ctx := startDERFleet(t, client, 30*time.Second, batteryAt(t, batLFDI, 0.5))
			clk := useFakeClock(fleet)
			md := fleet.devices[0]
			good := md.dev

			fleet.Tick(ctx)
			before := srv.total()
			if before == 0 {
				t.Fatal("the healthy tick sent nothing")
			}

			md.dev = failingDevice{DERDevice: good, failRead: mode == "read", failApply: mode == "apply"}
			clk.advance(time.Minute)
			fleet.Tick(ctx)
			clk.advance(time.Minute)
			fleet.Tick(ctx)
			if got := srv.total(); got != before {
				t.Errorf("a failed device sent %d more PUTs, want 0", got-before)
			}

			md.dev = good
			clk.advance(time.Minute)
			fleet.Tick(ctx)
			if n := len(srv.bodies("/der/" + batLFDI + "/stat")); n != 2 {
				t.Errorf("%d status PUTs after recovery, want 2", n)
			}
		})
	}
}

// Every PUT carries its device as the guard target: a device's resources
// are written only under its own DER, and a request for an unmanaged LFDI
// is refused before it is sent.
func TestManagedStatus_PutsAreJudgedForTheirDevice(t *testing.T) {
	client, srv := newDERServer(t, pvLFDI, batLFDI, strangerLFDI)
	fleet, ctx := startDERFleet(t, client, time.Minute,
		managedCfg(t, "pv", pvLFDI, "pv", 3000), batteryAt(t, batLFDI, 0.5))
	fleet.Tick(ctx)

	srv.mu.Lock()
	for path := range srv.puts {
		if !strings.HasPrefix(path, "/der/"+pvLFDI+"/") && !strings.HasPrefix(path, "/der/"+batLFDI+"/") {
			t.Errorf("PUT to %s, outside the managed devices' DERs", path)
		}
	}
	paths := len(srv.puts)
	srv.mu.Unlock()
	if paths != 8 {
		t.Errorf("%d distinct resources written, want 8 (four per managed device)", paths)
	}

	before := srv.total()
	err := client.PutDERAvailability(inverter.WithTarget(ctx, strangerLFDI), "/der/"+strangerLFDI+"/avail", sep2.DERAvailability{})
	if err == nil {
		t.Error("availability PUT for an unmanaged LFDI: want refusal, got nil")
	}
	if srv.total() != before {
		t.Errorf("server received %d PUTs for an unmanaged LFDI, want 0", srv.total()-before)
	}
	if err := client.PutDERAvailability(ctx, "", sep2.DERAvailability{}); err == nil {
		t.Error("empty availability href: want an error")
	}
}

// A rating past the int16 wire range is carried with a multiplier, not
// wrapped.
func TestActivePowerOf_CarriesLargeRatingsWithAMultiplier(t *testing.T) {
	for _, tc := range []struct {
		w    float64
		want sep2.ActivePower
	}{
		{0, sep2.ActivePower{}},
		{5000, sep2.ActivePower{Value: 5000}},
		{32767, sep2.ActivePower{Value: 32767}},
		{40000, sep2.ActivePower{Multiplier: 1, Value: 4000}},
		{2500000, sep2.ActivePower{Multiplier: 2, Value: 25000}},
	} {
		if got := activePowerOf(tc.w); got != tc.want {
			t.Errorf("activePowerOf(%v) = %+v, want %+v", tc.w, got, tc.want)
		}
	}
}

// A device whose EndDevice advertises no DERList reports nothing, does not
// stop its readings, and says so once, naming the device.
func TestManagedStatus_NoDERListLinkIsLoggedOnce(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	logs := captureManagedLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	fleet.Tick(ctx)
	fleet.Tick(ctx)
	fleet.Tick(ctx)
	if n := len(fs.readings["/mup/1"]); n != 3 {
		t.Errorf("%d readings, want 3: a missing DERList must not stop metering", n)
	}
	if fleet.devices[0].der != nil {
		t.Error("device resolved a DER without a DERListLink")
	}
	const line = "managed device pv: EndDevice advertises no DERListLink"
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Errorf("%d log lines %q over three ticks, want 1", n, line)
	}
}

// A DER with no status or availability link is logged once per link, naming
// the device, and no PUT goes to the missing resource.
func TestManagedStatus_MissingStatusAndAvailabilityLinksAreLoggedOnce(t *testing.T) {
	env := newDERWalkTestEnv(t)
	srv := &derServer{puts: map[string][]string{}, failing: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		writeSepXML(t, w, sep2.EndDeviceList{EndDevice: []sep2.EndDevice{{LFDI: pvLFDI, DERListLink: &sep2.ListLink{Href: "/derlist"}}}})
	})
	mux.HandleFunc("/mup", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/mup/1")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/mup/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	mux.HandleFunc("/derlist", func(w http.ResponseWriter, _ *http.Request) {
		writeSepXML(t, w, sep2.DERList{
			ListResource: sep2.ListResource{All: 1, Results: 1},
			DER:          []sep2.DER{{DERCapabilityLink: &sep2.Link{Href: "/der/cap"}, DERSettingsLink: &sep2.Link{Href: "/der/set"}}},
		})
	})
	mux.HandleFunc("/der/", func(w http.ResponseWriter, r *http.Request) {
		srv.mu.Lock()
		srv.puts[r.URL.Path] = append(srv.puts[r.URL.Path], "")
		srv.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	serverURL, _ := startDERWalkListener(t, env, mux)
	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL, CertFile: env.deviceCertPath, KeyFile: env.deviceKeyPath,
		CAFile: env.caCertPath, ClientRole: "aggregator",
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}
	logs := captureManagedLog(t)
	fleet, ctx := startDERFleet(t, client, time.Minute, managedCfg(t, "pv", pvLFDI, "pv", 3000))
	clk := useFakeClock(fleet)
	for range 3 {
		fleet.Tick(ctx)
		clk.advance(2 * time.Minute)
	}
	for _, line := range []string{
		"managed device pv: DER has no DERStatusLink",
		"managed device pv: DER has no DERAvailabilityLink",
	} {
		if n := strings.Count(logs.String(), line); n != 1 {
			t.Errorf("%d log lines %q over three status intervals, want 1", n, line)
		}
	}
	if n := srv.total(); n != 2 {
		t.Errorf("%d PUTs, want 2 (capability and settings only)", n)
	}
}

// Availability is the reserve: the most the device could output minus what
// it outputs, never negative. A PV at night has none, a PV curtailed to
// half its recorded maximum has the other half, a battery discharging at
// its rating has none, and a half-charged idle battery has its full rating
// with durations from its state of charge.
func TestManagedStatus_AvailabilityIsTheReserve(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         string
		soc          float64
		maxW, outW   float64
		wantW        int16
		wantDischarg uint32
		wantCharge   uint32
	}{
		{name: "pv at night", kind: "pv", maxW: 0, outW: 0, wantW: 0},
		{name: "pv curtailed to half", kind: "pv", maxW: 4000, outW: 2000, wantW: 2000},
		{name: "pv above its rating", kind: "pv", maxW: 9000, outW: 0, wantW: 5000},
		{name: "pv output past its maximum", kind: "pv", maxW: 1000, outW: 2000, wantW: 0},
		{name: "battery discharging at full rate", kind: "battery", soc: 0.5, maxW: 0, outW: 5000, wantW: 0, wantDischarg: 3600, wantCharge: 3600},
		{name: "battery charging", kind: "battery", soc: 0.5, maxW: 0, outW: -3000, wantW: 5000, wantDischarg: 3600, wantCharge: 3600},
		{name: "battery 40 percent idle", kind: "battery", soc: 0.4, maxW: 0, outW: 0, wantW: 5000, wantDischarg: 2880, wantCharge: 4320},
		{name: "battery above full", kind: "battery", soc: 1.5, maxW: 0, outW: 0, wantW: 5000, wantDischarg: 7200, wantCharge: 0},
		{name: "battery below empty", kind: "battery", soc: -0.5, maxW: 0, outW: 0, wantW: 0, wantDischarg: 0, wantCharge: 7200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lfdi := pvLFDI
			if tc.kind == "battery" {
				lfdi = batLFDI
			}
			client, srv := newDERServer(t, lfdi)
			fleet, ctx := startDERFleet(t, client, time.Minute, managedCfg(t, tc.kind, lfdi, tc.kind, 0))
			fleet.devices[0].dev = scriptedDevice{
				DERDevice: fleet.devices[0].dev, soc: tc.soc,
				reading: device.StateReading{MaxPowerW: tc.maxW},
				state:   inverter.InverterState{ActivePowerW: tc.outW, Connected: true, Energized: true},
			}
			fleet.Tick(ctx)

			bodies := srv.bodies("/der/" + lfdi + "/avail")
			if len(bodies) != 1 {
				t.Fatalf("%d availability PUTs, want 1", len(bodies))
			}
			a := decode[sep2.DERAvailability](t, bodies[0])
			if a.StatWAvail == nil || a.StatWAvail.Value != tc.wantW || a.StatWAvail.Multiplier != 0 {
				t.Errorf("statWAvail = %+v, want %d W", a.StatWAvail, tc.wantW)
			}
			if tc.kind == "pv" {
				if a.AvailabilityDuration != nil || a.MaxChargeDuration != nil {
					t.Errorf("pv availability carries durations: %+v", a)
				}
				return
			}
			if a.AvailabilityDuration == nil || *a.AvailabilityDuration != tc.wantDischarg {
				t.Errorf("availabilityDuration = %v, want %d s", a.AvailabilityDuration, tc.wantDischarg)
			}
			if a.MaxChargeDuration == nil || *a.MaxChargeDuration != tc.wantCharge {
				t.Errorf("maxChargeDuration = %v, want %d s", a.MaxChargeDuration, tc.wantCharge)
			}
		})
	}
}

// The status body carries the device's own connection and operational state
// and state of charge, not a zero state.
func TestManagedStatus_StatusCarriesTheDeviceState(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		connected, energized bool
		wantConnect          uint8
		wantMode             uint8
	}{
		{"connected and energized", true, true, 0x05, 2},
		{"connected only", true, false, 0x01, 1},
		{"neither", false, false, 0x00, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := newDERServer(t, batLFDI)
			fleet, ctx := startDERFleet(t, client, time.Minute, batteryAt(t, batLFDI, 0.5))
			fleet.devices[0].dev = scriptedDevice{
				DERDevice: fleet.devices[0].dev, soc: 0.25,
				state: inverter.InverterState{Connected: tc.connected, Energized: tc.energized},
			}
			fleet.Tick(ctx)

			bodies := srv.bodies("/der/" + batLFDI + "/stat")
			if len(bodies) != 1 {
				t.Fatalf("%d status PUTs, want 1", len(bodies))
			}
			st := decode[sep2.DERStatus](t, bodies[0])
			if st.GenConnectStatus == nil || uint8(st.GenConnectStatus.Value) != tc.wantConnect {
				t.Errorf("genConnectStatus = %+v, want %#x", st.GenConnectStatus, tc.wantConnect)
			}
			if st.OperationalModeStatus == nil || st.OperationalModeStatus.Value != tc.wantMode {
				t.Errorf("operationalModeStatus = %+v, want %d", st.OperationalModeStatus, tc.wantMode)
			}
			if st.StateOfChargeStatus == nil || st.StateOfChargeStatus.Value != 2500 {
				t.Errorf("stateOfChargeStatus = %+v, want 2500 (25%%)", st.StateOfChargeStatus)
			}
			if st.ReadingTime == 0 || st.GenConnectStatus.DateTime != st.ReadingTime {
				t.Errorf("status times = %d and %d, want the synchronized clock on both", st.ReadingTime, st.GenConnectStatus.DateTime)
			}
		})
	}
}

// A capability or settings PUT the server refuses is retried with the
// mirror's backoff, not on every tick, and each failed attempt is logged
// with its number.
func TestManagedStatus_FailedSetupIsRetriedWithBackoff(t *testing.T) {
	client, srv := newDERServer(t, pvLFDI)
	logs := captureManagedLog(t)
	capPath := "/der/" + pvLFDI + "/cap"
	srv.setFailing(capPath, true)
	fleet, ctx := startDERFleet(t, client, time.Hour, managedCfg(t, "pv", pvLFDI, "pv", 3000))
	md := fleet.devices[0]
	md.mirrorBase = 10 * time.Second
	clk := useFakeClock(fleet)

	// Attempt 1 at t=0 waits 10 s, attempt 2 waits 20 s, attempt 3 waits 40 s.
	steps := []struct {
		advance time.Duration
		want    int
	}{
		{0, 1}, {5 * time.Second, 1}, {5 * time.Second, 2}, {15 * time.Second, 2},
		{5 * time.Second, 3}, {35 * time.Second, 3}, {5 * time.Second, 4},
	}
	for i, st := range steps {
		clk.advance(st.advance)
		fleet.Tick(ctx)
		if got := len(srv.bodies(capPath)); got != st.want {
			t.Fatalf("step %d: %d capability PUTs, want %d", i, got, st.want)
		}
	}
	for _, line := range []string{"PUT DERCapability attempt 1:", "PUT DERCapability attempt 2:", "PUT DERCapability attempt 3:"} {
		if n := strings.Count(logs.String(), "managed device pv: "+line); n != 1 {
			t.Errorf("%d log lines %q, want 1", n, line)
		}
	}

	srv.setFailing(capPath, false)
	clk.advance(10 * time.Minute)
	fleet.Tick(ctx)
	before := len(srv.bodies(capPath))
	clk.advance(10 * time.Minute)
	fleet.Tick(ctx)
	if got := len(srv.bodies(capPath)); got != before {
		t.Errorf("%d capability PUTs after success, want %d: a PUT that succeeded is not repeated", got, before)
	}
}

// Each device's requests are judged for that device: the guard sees as many
// requests for the PV as the PV made (its DERList read, four PUTs and its reading POST), and
// the same for the battery, so a PUT that carried another device's target
// moves a count.
func TestManagedStatus_EachPutIsJudgedForItsOwnDevice(t *testing.T) {
	client, srv := newDERServer(t, pvLFDI, batLFDI)
	fleet, ctx := startDERFleet(t, client, time.Minute,
		managedCfg(t, "pv", pvLFDI, "pv", 3000), batteryAt(t, batLFDI, 0.5))
	rec := &recordingSet{inner: guard.NewStaticManagedSet(pvLFDI, batLFDI), calls: map[string]int{}}
	client.SetManagedSet(rec)
	fleet.Tick(ctx)

	for _, lfdi := range []string{pvLFDI, batLFDI} {
		puts := 0
		for _, leaf := range []string{"cap", "set", "stat", "avail"} {
			puts += len(srv.bodies("/der/" + lfdi + "/" + leaf))
		}
		if puts != 4 {
			t.Fatalf("%s: %d PUTs, want 4", lfdi, puts)
		}
		if got, want := rec.judged(lfdi), 1+puts+1; got != want {
			t.Errorf("%s: guard judged %d requests for it, want %d (DERList read, %d PUTs, reading POST)", lfdi, got, want, puts)
		}
	}
}
