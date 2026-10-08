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
	// noLocation makes a successful mirror POST answer 201 with no Location.
	noLocation bool
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
		if !fs.noLocation {
			w.Header().Set("Location", loc)
		}
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

// fakeClock is a manual clock for the retry and report-interval timing.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// useFakeClock moves every device of the fleet onto a clock that starts at
// the real time, so the backoff the start attempt set is on the same scale.
func useFakeClock(fleet *managedFleet) *fakeClock {
	c := &fakeClock{t: time.Now()}
	for _, md := range fleet.devices {
		md.now = c.now
	}
	return c
}

func TestMirrorBackoff_DoublesFromTheReportIntervalAndIsCapped(t *testing.T) {
	for _, tc := range []struct {
		base     time.Duration
		failures int
		want     time.Duration
	}{
		{time.Second, 1, time.Second},
		{time.Second, 2, 2 * time.Second},
		{time.Second, 3, 4 * time.Second},
		{time.Second, 9, 256 * time.Second},
		{time.Second, 10, maxMirrorBackoff},
		{time.Second, 500, maxMirrorBackoff},
		{time.Hour, 1, maxMirrorBackoff},
		{0, 1, 0},
		{0, 500, 0},
	} {
		if got := mirrorBackoff(tc.base, tc.failures); got != tc.want {
			t.Errorf("mirrorBackoff(%v, %d) = %v, want %v", tc.base, tc.failures, got, tc.want)
		}
	}
}

// A mirror that fails for far longer than the old five-attempt window still
// ends up reporting once the server answers, the retries are spaced by the
// backoff rather than made on every tick, and nothing is posted meanwhile.
func TestManagedFleet_LongMirrorOutageStillReportsAfterRecovery(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	const outage = 8 // more than the five attempts the old cap allowed
	fs.mupFailures = outage
	logs := captureManagedLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, time.Second)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	clock := useFakeClock(fleet)
	if fs.mupAttempts != 1 {
		t.Fatalf("mirror attempts at start = %d, want 1", fs.mupAttempts)
	}

	// Ticks inside the backoff make no attempt.
	for i := 0; i < 3; i++ {
		fleet.Tick(ctx)
	}
	if fs.mupAttempts != 1 {
		t.Fatalf("mirror attempts after 3 ticks inside the backoff = %d, want 1", fs.mupAttempts)
	}

	// Each step is longer than the backoff, so every tick retries.
	for i := 0; i < outage-1; i++ {
		clock.advance(maxMirrorBackoff)
		fleet.Tick(ctx)
		if fs.mupAttempts != i+2 {
			t.Fatalf("after step %d: mirror attempts = %d, want %d", i, fs.mupAttempts, i+2)
		}
	}
	if n := len(fs.readings); n != 0 {
		t.Fatalf("readings posted before the mirror existed: %v", fs.readings)
	}

	clock.advance(maxMirrorBackoff)
	fleet.Tick(ctx)
	if fs.mupLFDI["/mup/1"] != pvLFDI {
		t.Fatalf("mirrors after recovery = %v, want /mup/1 naming %s", fs.mupLFDI, pvLFDI)
	}
	got := fs.readings["/mup/1"]
	if len(got) != 1 || got[0].Reading == nil || got[0].Reading.Value == nil || *got[0].Reading.Value != 3000 {
		t.Fatalf("readings after recovery = %v, want one of 3000 W", got)
	}
	if n := strings.Count(logs.String(), "create MirrorUsagePoint attempt"); n != outage {
		t.Errorf("%d logged mirror failures, want %d (one per failed attempt)", n, outage)
	}
	if strings.Contains(logs.String(), "gave up") {
		t.Errorf("log says the device gave up: %q", logs.String())
	}
}

// The wait after a failure is the backoff: a tick just short of it makes no
// attempt, a tick at it does.
func TestManagedFleet_MirrorRetryWaitsForTheBackoff(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	fs.mupFailures = 1000
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, 10*time.Second)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	clock := useFakeClock(fleet)
	clock.advance(9 * time.Second)
	fleet.Tick(ctx)
	if fs.mupAttempts != 1 {
		t.Errorf("attempts 9 s into a 10 s backoff = %d, want 1", fs.mupAttempts)
	}
	clock.advance(2 * time.Second)
	fleet.Tick(ctx)
	if fs.mupAttempts != 2 {
		t.Errorf("attempts after the first backoff = %d, want 2", fs.mupAttempts)
	}
	// The second failure doubles the wait to 20 s.
	clock.advance(19 * time.Second)
	fleet.Tick(ctx)
	if fs.mupAttempts != 2 {
		t.Errorf("attempts 19 s into a 20 s backoff = %d, want 2", fs.mupAttempts)
	}
	clock.advance(2 * time.Second)
	fleet.Tick(ctx)
	if fs.mupAttempts != 3 {
		t.Errorf("attempts after the second backoff = %d, want 3", fs.mupAttempts)
	}
}

// A device with no mirror logs its skipped reading once per report
// interval, not once per tick.
func TestManagedFleet_SkippedReadingIsLoggedOncePerInterval(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	fs.mupFailures = 1000
	logs := captureManagedLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, time.Hour)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	clock := useFakeClock(fleet)
	const skipped = "reading skipped: no MirrorUsagePoint"
	for i := 0; i < 5; i++ {
		fleet.Tick(ctx)
	}
	if n := strings.Count(logs.String(), skipped); n != 1 {
		t.Errorf("%d skipped-reading lines over 5 ticks inside one interval, want 1", n)
	}
	clock.advance(time.Hour + time.Second)
	fleet.Tick(ctx)
	fleet.Tick(ctx)
	if n := strings.Count(logs.String(), skipped); n != 2 {
		t.Errorf("%d skipped-reading lines after the interval passed, want 2", n)
	}
	if len(fs.readings) != 0 {
		t.Errorf("readings posted without a mirror: %v", fs.readings)
	}
}

// A 201 with no Location does not create a usable mirror: it is logged as
// the failure it is, no reading is posted to a guessed "/mr" path, and the
// next attempt can still succeed.
func TestManagedFleet_EmptyLocationIsAFailureNotAMirror(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	fs.noLocation = true
	logs := captureManagedLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet, err := startManaged(ctx, client, dcapWithMirrors, "/edev", []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}, 0)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	fleet.Tick(ctx)
	if fleet.devices[0].hasMirror {
		t.Error("device has a mirror after a POST that returned no Location")
	}
	if len(fs.readings) != 0 {
		t.Errorf("readings posted after a POST with no Location: %v", fs.readings)
	}
	if !strings.Contains(logs.String(), "returned empty Location") {
		t.Errorf("log %q does not report the empty Location", logs.String())
	}

	fs.noLocation = false
	fleet.Tick(ctx)
	// The two location-less POSTs still created server-side points 1 and 2,
	// so the third is the one the device now reports to.
	if !fleet.devices[0].hasMirror || len(fs.readings["/mup/3"]) != 1 {
		t.Errorf("after a good Location: hasMirror=%v readings=%v, want a mirror and one reading at /mup/3", fleet.devices[0].hasMirror, fs.readings)
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
	if !strings.Contains(logs.String(), "no sim config") {
		t.Errorf("log %q does not say there is no sim config", logs.String())
	}
}

// A sim config that exists but lists no managed devices is a different
// state from no config, and the log says which.
func TestStartManagedForRole_ConfigWithNoManagedDevicesIsALoggedState(t *testing.T) {
	logs := captureManagedLog(t)
	fleet, err := startManagedForRole(context.Background(), nil, sep2.DeviceCapability{}, "/edev", &simconfig.File{}, 0)
	if err != nil || fleet != nil {
		t.Fatalf("startManagedForRole(empty config) = %v, %v, want nil, nil", fleet, err)
	}
	out := logs.String()
	if !strings.Contains(out, "lists no managed devices") || strings.Contains(out, "no sim config") {
		t.Errorf("log %q does not say the config lists no managed devices", out)
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

// A start error reaches the exit path (fatalf in main) with the device
// named, and no fleet comes back; a good start never calls it.
func TestStartManagedOrExit_PassesAStartErrorToExit(t *testing.T) {
	env := newDERWalkTestEnv(t)
	client, _ := newFakeServer(t, env, pvLFDI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exits []string
	exit := func(format string, args ...any) { exits = append(exits, fmt.Sprintf(format, args...)) }

	bad := &simconfig.File{Managed: []simconfig.Managed{managedCfg(t, "ghost", notListedLFDI, "pv", 3000)}}
	if fleet := startManagedOrExit(ctx, client, dcapWithMirrors, "/edev", bad, 0, exit); fleet != nil {
		t.Errorf("fleet = %v after a start error, want nil", fleet)
	}
	if len(exits) != 1 || !strings.Contains(exits[0], "ghost") || !strings.Contains(exits[0], "managed devices") {
		t.Fatalf("exit calls = %q, want one naming ghost", exits)
	}

	good := &simconfig.File{Managed: []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}}
	fleet := startManagedOrExit(ctx, client, dcapWithMirrors, "/edev", good, 0, exit)
	if fleet == nil || len(fleet.devices) != 1 || len(exits) != 1 {
		t.Errorf("good start: fleet = %v, exit calls = %q, want a fleet of 1 and no new exit call", fleet, exits)
	}
}

// Ticking the fleet main holds posts the readings; a nil fleet (no managed
// devices) ticks nothing and does not panic.
func TestManagedFleet_TickDrivesReadingsAndIsNilSafe(t *testing.T) {
	var none *managedFleet
	none.Tick(context.Background())

	env := newDERWalkTestEnv(t)
	client, fs := newFakeServer(t, env, pvLFDI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fleet := startManagedOrExit(ctx, client, dcapWithMirrors, "/edev",
		&simconfig.File{Managed: []simconfig.Managed{managedCfg(t, "pv", pvLFDI, "pv", 3000)}}, 0,
		func(format string, args ...any) { t.Fatalf(format, args...) })
	fleet.Tick(ctx)
	if n := len(fs.readings["/mup/1"]); n != 1 {
		t.Errorf("%d readings after one tick, want 1", n)
	}
}

// The wiring in main.go is the part no helper test reaches. It must hand
// fatalf to startManagedOrExit, so a start error exits, and tick the fleet
// as the first statement of the aggregator branch, not inside a condition
// that can be made dead.
func TestMain_WiresTheManagedFleet(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(src), "\n") {
		if tr := strings.TrimSpace(line); tr != "" && !strings.HasPrefix(tr, "//") {
			code = append(code, tr)
		}
	}
	var startCalls, tickAt []int
	for i, l := range code {
		if strings.Contains(l, "startManagedOrExit(") {
			startCalls = append(startCalls, i)
		}
		if l == "managed.Tick(ctx)" {
			tickAt = append(tickAt, i)
		}
	}
	if len(startCalls) != 1 || !strings.HasSuffix(code[startCalls[0]], "cfg.ReportInterval, fatalf)") {
		t.Errorf("main.go must call startManagedOrExit once, ending in cfg.ReportInterval, fatalf); got lines %v", startCalls)
	}
	if len(tickAt) != 1 || code[tickAt[0]-1] != "if skipDERPipelineForRole(cfg) {" {
		t.Errorf("main.go must hold exactly one unconditional managed.Tick(ctx) directly under the aggregator role branch; got %v", tickAt)
	}
}
