package main

// Tests for the aggregator's managed devices (managed.go,
// GRIDAPPSD/ieee-2030_5-client-go#72): the managed set resolved against the
// server's EndDeviceList, one mirror per managed device naming its LFDI,
// readings that carry value and flowDirection, no mirror for the
// aggregator itself, and a refused request for an unmanaged LFDI.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

const (
	pvLFDI        = "1111111111111111111111111111111111111111"
	batLFDI       = "2222222222222222222222222222222222222222"
	strangerLFDI  = "3333333333333333333333333333333333333333"
	notListedLFDI = "4444444444444444444444444444444444444444"
)

// writeFlatRecording writes a 1440-row recorder file with one value.
func writeFlatRecording(t *testing.T, value float64) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# flat recording\n")
	for m := 0; m < 1440; m++ {
		fmt.Fprintf(&b, "2020-01-01 %02d:%02d:00 UTC,%g\n", m/60, m%60, value)
	}
	p := filepath.Join(t.TempDir(), "rec.csv")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	return p
}

func managedCfg(t *testing.T, name, lfdi, typ string, recorded float64) simconfig.Managed {
	t.Helper()
	soc := 0.5
	return simconfig.Managed{
		Name: name, LFDI: lfdi,
		Device: &simconfig.Device{Name: name, Type: typ, RatedW: 5000, CapacityWh: 10000, InitialSOC: &soc},
		Replay: &simconfig.Replay{File: writeFlatRecording(t, recorded), Clock: "start", Scale: 1},
	}
}

// fakeServer lists the given LFDIs as EndDevices, hands out /mup/N
// locations, and keeps what each mirror and reading carried.
type fakeServer struct {
	mu        sync.Mutex
	mupLFDI   map[string]string // mirror Location -> DeviceLFDI it named
	mupBodies []string
	readings  map[string][]sep2.MirrorMeterReading // mirror Location -> readings
}

func newFakeServer(t *testing.T, env *derWalkTestEnv, listed ...string) (*inverter.SEP2Client, *fakeServer) {
	t.Helper()
	fs := &fakeServer{mupLFDI: map[string]string{}, readings: map[string][]sep2.MirrorMeterReading{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		var list sep2.EndDeviceList
		for _, l := range listed {
			list.EndDevice = append(list.EndDevice, sep2.EndDevice{LFDI: l})
		}
		writeSepXML(t, w, list)
	})
	mux.HandleFunc("/mup", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var mup sep2.MirrorUsagePoint
		if err := xml.Unmarshal(b, &mup); err != nil {
			t.Errorf("unmarshal MirrorUsagePoint: %v", err)
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		loc := fmt.Sprintf("/mup/%d", len(fs.mupLFDI)+1)
		fs.mupLFDI[loc] = mup.DeviceLFDI
		fs.mupBodies = append(fs.mupBodies, string(b))
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/mup/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var mmr sep2.MirrorMeterReading
		if err := xml.Unmarshal(b, &mmr); err != nil {
			t.Errorf("unmarshal MirrorMeterReading: %v", err)
		}
		loc := strings.TrimSuffix(r.URL.Path, "/mr")
		fs.mu.Lock()
		fs.readings[loc] = append(fs.readings[loc], mmr)
		fs.mu.Unlock()
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
	return client, fs
}

var dcapWithMirrors = sep2.DeviceCapability{MirrorUsagePointListLink: &sep2.ListLink{Href: "/mup"}}

func TestMissingManaged_NamesEveryUnlistedDevice(t *testing.T) {
	configured := []simconfig.Managed{{Name: "pv", LFDI: pvLFDI}, {Name: "bat", LFDI: batLFDI}, {Name: "ghost", LFDI: notListedLFDI}}
	list := sep2.EndDeviceList{EndDevice: []sep2.EndDevice{{LFDI: strings.ToLower(pvLFDI)}, {LFDI: batLFDI}}}

	err := missingManaged(list, configured)
	if err == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), notListedLFDI) {
		t.Fatalf("missingManaged = %v, want an error naming ghost and %s", err, notListedLFDI)
	}
	if strings.Contains(err.Error(), "pv") || strings.Contains(err.Error(), "bat") {
		t.Errorf("error %q names a listed device (case-insensitive match expected)", err)
	}
	if err := missingManaged(list, configured[:2]); err != nil {
		t.Errorf("all listed: %v", err)
	}
}

func TestStartManaged_UnlistedDeviceStopsTheStartBeforeAnyMirror(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{
		managedCfg(t, "pv", pvLFDI, "pv", 3000),
		managedCfg(t, "ghost", notListedLFDI, "pv", 3000),
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("startManaged = %v, want an error naming ghost", err)
	}
	if n := len(fs.mupLFDI); n != 0 {
		t.Errorf("%d mirrors posted before the start was refused, want 0", n)
	}
}

func TestStartManaged_UnreadableReplayStopsTheStartNamingTheDevice(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, _ := newFakeServer(t, env, pvLFDI)
	m := managedCfg(t, "pv", pvLFDI, "pv", 3000)
	m.Replay.File = filepath.Join(t.TempDir(), "missing.csv")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{m}, 0)
	if err == nil || !strings.Contains(err.Error(), "pv") || !strings.Contains(err.Error(), pvLFDI) {
		t.Fatalf("startManaged = %v, want an error naming the device and its LFDI", err)
	}
}

// Each managed device posts its own mirror naming its LFDI, and its own
// reading carries the value and flowDirection its replay backend produced:
// PV output 3000 W as 19 (received from customer), a battery charging at
// 2000 W as 2000 with 1 (delivered to customer). The aggregator posts no
// mirror naming itself, and a request for an LFDI outside the set is
// refused without reaching the server.
func TestManagedFleet_PostsMirrorAndSignedReadingPerDevice(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI, batLFDI, strangerLFDI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{
		managedCfg(t, "pv", pvLFDI, "pv", 3000),
		managedCfg(t, "bat", batLFDI, "battery", 2000),
	}, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	fleet.Tick(ctx)

	byLFDI := map[string]string{}
	for loc, lfdi := range fs.mupLFDI {
		byLFDI[lfdi] = loc
	}
	if len(fs.mupLFDI) != 2 || byLFDI[pvLFDI] == "" || byLFDI[batLFDI] == "" {
		t.Fatalf("mirrors = %v, want exactly one each for %s and %s", fs.mupLFDI, pvLFDI, batLFDI)
	}
	for _, body := range fs.mupBodies {
		if strings.Contains(body, client.LFDI()) {
			t.Errorf("a mirror names the aggregator's own LFDI %s: %s", client.LFDI(), body)
		}
	}

	for _, want := range []struct {
		name, lfdi string
		value      int64
		dir        uint8
	}{
		{"pv", pvLFDI, 3000, sep2.FlowDirectionReverse},
		{"bat", batLFDI, 2000, sep2.FlowDirectionForward},
	} {
		got := fs.readings[byLFDI[want.lfdi]]
		if len(got) != 1 {
			t.Fatalf("%s: %d readings, want 1", want.name, len(got))
		}
		r := got[0]
		if r.Reading == nil || r.Reading.Value == nil || *r.Reading.Value != want.value {
			t.Errorf("%s: reading value = %v, want %d", want.name, r.Reading, want.value)
		}
		if r.ReadingType == nil || r.ReadingType.FlowDirection == nil || *r.ReadingType.FlowDirection != want.dir {
			t.Errorf("%s: flowDirection = %v, want %d", want.name, r.ReadingType, want.dir)
		}
		if r.ReadingType == nil || r.ReadingType.Uom == nil || *r.ReadingType.Uom != sep2.UomWatts {
			t.Errorf("%s: uom = %v, want watts", want.name, r.ReadingType)
		}
	}
	if fs.readings[byLFDI[pvLFDI]][0].MRID == fs.readings[byLFDI[batLFDI]][0].MRID {
		t.Error("the two devices' readings share an mRID")
	}

	// An LFDI the server lists but the aggregator does not manage is
	// refused before any send.
	before := len(fs.mupBodies)
	_, err = client.CreateMirrorUsagePoint(inverter.WithTarget(ctx, strangerLFDI), "/mup", inverter.DeviceLFDI(strangerLFDI), sep2.MirrorUsagePoint{})
	if err == nil {
		t.Error("mirror for an unmanaged LFDI: want refusal, got nil")
	}
	if len(fs.mupBodies) != before {
		t.Errorf("server received %d mirrors for an unmanaged LFDI, want 0", len(fs.mupBodies)-before)
	}
}

// The report interval gates readings: a second tick inside the interval
// posts nothing more.
func TestManagedFleet_ReportIntervalGatesReadings(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, time.Hour)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	fleet.Tick(ctx)
	fleet.Tick(ctx)
	if n := len(fs.readings["/mup/1"]); n != 1 {
		t.Errorf("%d readings after two ticks inside the interval, want 1", n)
	}
}
