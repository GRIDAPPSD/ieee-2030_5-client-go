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
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
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
	// mupFailures is how many mirror POSTs answer 503 before one succeeds;
	// mupAttempts counts every mirror POST that reached the server.
	mupFailures int
	mupAttempts int
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
		fs.mupAttempts++
		if fs.mupFailures > 0 {
			fs.mupFailures--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
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

// captureManagedLog returns what log wrote until the test ends.
func captureManagedLog(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &b
}

// A mirror that answers 503 at start is retried on later ticks; once it
// answers 201 the device posts readings naming its LFDI.
func TestManagedFleet_FailedMirrorIsRetriedAndThenReports(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	fs.mupFailures = 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	if n := len(fs.mupLFDI); n != 0 {
		t.Fatalf("%d mirrors exist after the 503, want 0", n)
	}
	fleet.Tick(ctx)

	if fs.mupAttempts != 2 || fs.mupLFDI["/mup/1"] != pvLFDI {
		t.Errorf("mirror attempts = %d, mirrors = %v, want 2 attempts and /mup/1 naming %s", fs.mupAttempts, fs.mupLFDI, pvLFDI)
	}
	got := fs.readings["/mup/1"]
	if len(got) != 1 || got[0].Reading == nil || got[0].Reading.Value == nil || *got[0].Reading.Value != 3000 {
		t.Fatalf("readings after the retry = %v, want one of 3000 W", got)
	}
	fleet.Tick(ctx)
	if fs.mupAttempts != 2 {
		t.Errorf("mirror attempts = %d after success, want no further attempts", fs.mupAttempts)
	}
}

// A mirror that never succeeds is retried a bounded number of times, each
// attempt is logged, and every skipped reading is logged.
func TestManagedFleet_MirrorRetryIsBoundedAndSkippedReadingsAreLogged(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	fs.mupFailures = 1000
	logs := captureManagedLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	const ticks = maxMirrorAttempts + 4
	for i := 0; i < ticks; i++ {
		fleet.Tick(ctx)
	}
	if fs.mupAttempts != maxMirrorAttempts {
		t.Errorf("mirror attempts = %d over %d ticks, want %d", fs.mupAttempts, ticks, maxMirrorAttempts)
	}
	if n := len(fs.readings); n != 0 {
		t.Errorf("readings posted without a mirror: %v", fs.readings)
	}
	out := logs.String()
	if n := strings.Count(out, "create MirrorUsagePoint"); n != maxMirrorAttempts {
		t.Errorf("%d logged mirror failures, want %d (one per attempt)", n, maxMirrorAttempts)
	}
	if n := strings.Count(out, "reading skipped: no MirrorUsagePoint"); n != ticks {
		t.Errorf("%d skipped-reading log lines over %d ticks, want one per tick", n, ticks)
	}
}

// The aggregator's own LFDI as a managed device is refused at start,
// naming it, before any request leaves.
func TestStartManaged_RefusesTheAggregatorsOwnLFDI(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	own := strings.ToLower(client.LFDI())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{
		managedCfg(t, "pv", pvLFDI, "pv", 3000),
		managedCfg(t, "self", own, "pv", 3000),
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "self") || !strings.Contains(err.Error(), "own LFDI") {
		t.Fatalf("startManaged = %v, want an error naming self and the aggregator's own LFDI", err)
	}
	if fs.mupAttempts != 0 {
		t.Errorf("%d mirror attempts before the refusal, want 0", fs.mupAttempts)
	}
}

// LFDIs are hex, so the server may list them in a different case than the
// config writes them. The listed check matches either way, and the mirror
// names the device as configured.
func TestManagedLFDI_MixedCaseMatchesTheListAndTheMirrorBody(t *testing.T) {
	const mixed = "AbCdEf0123456789aBcDeF0123456789AbCdEf01"
	list := sep2.EndDeviceList{EndDevice: []sep2.EndDevice{{LFDI: strings.ToUpper(mixed)}}}
	if err := missingManaged(list, []simconfig.Managed{{Name: "mx", LFDI: strings.ToLower(mixed)}}); err != nil {
		t.Errorf("missingManaged on a case difference only: %v", err)
	}
	if err := missingManaged(sep2.EndDeviceList{EndDevice: []sep2.EndDevice{{LFDI: strings.ToLower(mixed)}}}, []simconfig.Managed{{Name: "mx", LFDI: mixed}}); err != nil {
		t.Errorf("missingManaged, list lower and config mixed: %v", err)
	}

	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, strings.ToUpper(mixed))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "mx", mixed, "pv", 3000)}, 0)
	if err != nil {
		t.Fatalf("startManaged with a mixed-case LFDI: %v", err)
	}
	fleet.Tick(ctx)
	if fs.mupLFDI["/mup/1"] != mixed {
		t.Errorf("mirror body LFDI = %q, want the configured %q", fs.mupLFDI["/mup/1"], mixed)
	}
	if n := len(fs.readings["/mup/1"]); n != 1 {
		t.Errorf("%d readings for the mixed-case device, want 1", n)
	}
}

// failingDevice fails the named step and delegates the rest.
type failingDevice struct {
	device.DERDevice
	failRead, failApply bool
}

func (f failingDevice) ReadState(ctx context.Context) (device.StateReading, error) {
	if f.failRead {
		return device.StateReading{}, fmt.Errorf("read down")
	}
	return f.DERDevice.ReadState(ctx)
}

func (f failingDevice) ApplySetpoint(ctx context.Context, c inverter.ControlOutputs) (inverter.InverterState, error) {
	if f.failApply {
		return inverter.InverterState{}, fmt.Errorf("apply down")
	}
	return f.DERDevice.ApplySetpoint(ctx, c)
}

// A device whose ReadState or ApplySetpoint fails is logged and skipped;
// the devices after it still report.
func TestManagedFleet_AFailingDeviceDoesNotStopTheOthers(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI, batLFDI, strangerLFDI)
	logs := captureManagedLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{
		managedCfg(t, "pv", pvLFDI, "pv", 3000),
		managedCfg(t, "bat", batLFDI, "battery", 2000),
		managedCfg(t, "pv2", strangerLFDI, "pv", 1000),
	}, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	fleet.devices[0].dev = failingDevice{DERDevice: fleet.devices[0].dev, failRead: true}
	fleet.devices[1].dev = failingDevice{DERDevice: fleet.devices[1].dev, failApply: true}
	fleet.Tick(ctx)

	if n := len(fs.readings["/mup/1"]) + len(fs.readings["/mup/2"]); n != 0 {
		t.Errorf("%d readings from the two failing devices, want 0", n)
	}
	got := fs.readings["/mup/3"]
	if len(got) != 1 || got[0].Reading == nil || *got[0].Reading.Value != 1000 {
		t.Errorf("the device after the failures reported %v, want one reading of 1000 W", got)
	}
	out := logs.String()
	if !strings.Contains(out, "pv: ReadState failed") || !strings.Contains(out, "bat: ApplySetpoint failed") {
		t.Errorf("failures not logged per device: %q", out)
	}
}

func TestStartManagedForRole_NoSimConfigIsALoggedState(t *testing.T) {
	logs := captureManagedLog(t)
	fleet, err := startManagedForRole(context.Background(), nil, sep2.DeviceCapability{}, "/edev", nil, 0)
	if err != nil || fleet != nil {
		t.Fatalf("startManagedForRole(nil) = %v, %v, want nil, nil", fleet, err)
	}
	if !strings.Contains(logs.String(), "no managed devices") {
		t.Errorf("log %q does not say the aggregator runs no managed devices", logs.String())
	}
}

func TestStartManagedForRole_StartsTheConfiguredDevices(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManagedForRole(ctx, client, dcapWithMirrors, "/edev",
		&simconfig.File{Managed: []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}}, 0)
	if err != nil || fleet == nil || len(fleet.devices) != 1 {
		t.Fatalf("startManagedForRole = %v, %v, want a fleet of 1", fleet, err)
	}
	if fs.mupLFDI["/mup/1"] != pvLFDI {
		t.Errorf("mirrors = %v, want /mup/1 naming %s", fs.mupLFDI, pvLFDI)
	}
}

// main.go must call startManagedForRole and tick the fleet: nothing else
// starts the aggregator's managed devices, and a test of the helper alone
// cannot see the call being removed.
func TestMain_WiresTheManagedFleet(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(src), "\n") {
		if t := strings.TrimSpace(line); !strings.HasPrefix(t, "//") {
			code = append(code, t)
		}
	}
	joined := strings.Join(code, "\n")
	for _, want := range []string{
		"managed, err = startManagedForRole(ctx, client, dcap, edevListHref, simFile, cfg.ReportInterval)",
		"managed.Tick(ctx)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("main.go no longer contains %q", want)
		}
	}
}
