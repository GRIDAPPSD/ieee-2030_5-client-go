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
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// derServer lists each LFDI with a DERList of one DER whose four resources
// live under /der/<lfdi>/, and keeps every PUT body by path.
type derServer struct {
	mu   sync.Mutex
	puts map[string][]string
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
	srv := &derServer{puts: map[string][]string{}}
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
	return client, srv
}

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
	bat := decode[sep2.DERCapability](t, srv.bodies("/der/" + batLFDI + "/cap")[0])
	if bat.RTGMaxW == nil || bat.RTGMaxW.Value != 5000 || bat.Type == nil || *bat.Type != 80 ||
		bat.RTGMaxChargeRateW == nil || bat.RTGMaxChargeRateW.Value != 5000 ||
		bat.RTGMaxDischargeRateW == nil || bat.RTGMaxDischargeRateW.Value != 5000 {
		t.Errorf("battery capability = %+v, want 5000 W both ways and type 80", bat)
	}
	set := decode[sep2.DERSettings](t, srv.bodies("/der/" + batLFDI + "/set")[0])
	if set.SetMaxW == nil || set.SetMaxW.Value != 5000 || set.UpdatedTime == 0 {
		t.Errorf("battery settings = %+v, want setMaxW 5000 and an updatedTime", set)
	}

	// A PV device offers its rating, and reports no state of charge.
	a := decode[sep2.DERAvailability](t, srv.bodies("/der/" + pvLFDI + "/avail")[0])
	if a.StatWAvail == nil || a.StatWAvail.Value != 5000 || a.AvailabilityDuration != nil {
		t.Errorf("pv availability = %+v, want statWAvail 5000 and no durations", a)
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

// A device whose EndDevice advertises no DERList reports nothing and does
// not stop its readings.
func TestManagedStatus_NoDERListLinkIsQuiet(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	fleet.Tick(ctx)
	if n := len(fs.readings["/mup/1"]); n != 1 {
		t.Errorf("%d readings, want 1: a missing DERList must not stop metering", n)
	}
	if fleet.devices[0].der != nil {
		t.Error("device resolved a DER without a DERListLink")
	}
}
