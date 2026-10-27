package main

// Tests for runPhase4Metering (phase4_metering.go): the DER-client identity
// wiring at what used to be main.go:893 (CreateMirrorUsagePoint) and :918
// (NewReporter). A DER client only ever acts for its own EndDevice, so
// both calls must carry client.LFDI() and nothing else
// (GRIDAPPSD/ieee-2030_5-client-go#68).
//
// Fixtures (derWalkTestEnv, newDERWalkClient, startDERWalkListener,
// writeSepXML) are shared with derprogram_phase_test.go : same package,
// same TLS setup, no duplication needed.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// TestRunPhase4Metering_MirrorUsesClientOwnLFDI proves the MirrorUsagePoint
// POST carries client.LFDI() as DeviceLFDI. A mutant that passes another
// LFDI, or "", changes the observed value and fails this assertion; an
// empty value would also fail before any HTTP call (CreateMirrorUsagePoint's
// own guard), which the mmrHits-stays-zero-on-nil-Location branch elsewhere
// already covers, so this test only needs the equality check.
func TestRunPhase4Metering_MirrorUsesClientOwnLFDI(t *testing.T) {
	t.Parallel()
	env := newDERWalkTestEnv(t)

	var gotDeviceLFDI, gotRoleFlags atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read /mup POST body: %v", err)
		}
		var mup sep2.MirrorUsagePoint
		if err := xml.Unmarshal(b, &mup); err != nil {
			t.Errorf("unmarshal /mup POST body: %v", err)
		}
		gotDeviceLFDI.Store(mup.DeviceLFDI)
		gotRoleFlags.Store(mup.RoleFlags)
		w.Header().Set("Location", "/mup/1")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/mup/1/mr", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	dcap := sep2.DeviceCapability{
		MirrorUsagePointListLink: &sep2.ListLink{Href: "/mup"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = runPhase4Metering(ctx, client, dcap, "")

	got, _ := gotDeviceLFDI.Load().(string)
	if got != client.LFDI() {
		t.Errorf("MirrorUsagePoint DeviceLFDI = %q, want client's own LFDI %q", got, client.LFDI())
	}
	if got == "" {
		t.Error("MirrorUsagePoint DeviceLFDI is empty")
	}
	// 9 is isMirror (bit 0) | isDER (bit 3); the server counts a mirror as
	// DER power only with bit 3 set.
	if rf, _ := gotRoleFlags.Load().(sep2.RoleFlagsValue); rf != 9 {
		t.Errorf("MirrorUsagePoint RoleFlags = %d, want 9 (isMirror|isDER)", rf)
	}
}

// TestRunPhase4Metering_ReporterUsesClientOwnLFDI proves the Reporter
// runPhase4Metering returns was constructed with client.LFDI(). readingMRID
// is unexported (internal/inverter), so this cannot recompute the hash
// directly; instead it is differential, using only production API: a
// reference Reporter built directly with client.LFDI() is driven
// immediately before and after the actual call, and the actual mRID must
// equal one of the two reference mRIDs. Two reference calls bracket the
// rare case where a formatted-second boundary falls between them. A
// mutant that passes another LFDI, or an empty one (which NewReporter
// would otherwise refuse), changes every reading this Reporter mints, so
// it would match neither reference.
func TestRunPhase4Metering_ReporterUsesClientOwnLFDI(t *testing.T) {
	t.Parallel()
	env := newDERWalkTestEnv(t)

	var actualBody, refBody atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/mup/1")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/mup/1/mr", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		actualBody.Store(b)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/ref/mr", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		refBody.Store(b)
		w.WriteHeader(http.StatusCreated)
	})

	serverURL, _ := startDERWalkListener(t, env, mux)
	client := newDERWalkClient(t, env, serverURL)

	dcap := sep2.DeviceCapability{
		MirrorUsagePointListLink: &sep2.ListLink{Href: "/mup"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reporter := runPhase4Metering(ctx, client, dcap, "")
	state := inverter.InverterState{ActivePowerW: 1, Time: time.Now()}

	// reportAndReadMRID drives r.ReportMetering and reads the mRID the
	// handler wired to r's mmrHref just captured into bodyVar.
	reportAndReadMRID := func(r *inverter.Reporter, bodyVar *atomic.Value) string {
		t.Helper()
		if err := r.ReportMetering(ctx, state); err != nil {
			t.Fatalf("ReportMetering: %v", err)
		}
		var mmr sep2.MirrorMeterReading
		if err := xml.Unmarshal(bodyVar.Load().([]byte), &mmr); err != nil {
			t.Fatalf("unmarshal MRR POST body: %v", err)
		}
		return mmr.MRID
	}
	newRefReporter := func() *inverter.Reporter {
		t.Helper()
		r, err := inverter.NewReporter(client, client.LFDI(), "", "/ref/mr")
		if err != nil {
			t.Fatalf("NewReporter(reference): %v", err)
		}
		return r
	}

	wantBefore := reportAndReadMRID(newRefReporter(), &refBody)
	got := reportAndReadMRID(reporter, &actualBody)
	wantAfter := reportAndReadMRID(newRefReporter(), &refBody)

	if got != wantBefore && got != wantAfter {
		t.Errorf("actual reading MRID = %q, matches neither client-own-LFDI reference (%q, %q): Reporter was not built with client.LFDI()",
			got, wantBefore, wantAfter)
	}
}

// A single-device client replaces a mirror the server lost the way a managed
// device does, through the report step the main loop calls: the 404 marks the
// mirror lost, the next report creates a new one after the backoff, and
// readings resume at its href.
func TestOwnReporter_LostMirrorIsReplacedAndReadingsResume(t *testing.T) {
	t.Parallel()
	env := newDERWalkTestEnv(t)
	fs := &fakeServer{mupLFDI: map[string]string{}, readings: map[string][]sep2.MirrorMeterReading{}}
	fs.readingFailures, fs.readingFailStatus = 1, http.StatusNotFound
	serverURL, _ := startDERWalkListener(t, env, fs.mux(t))
	client := newDERWalkClient(t, env, serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const interval = 10 * time.Second
	clock := &fakeClock{t: time.Now()}
	o := &ownReporter{client: client, dcap: dcapWithMirrors, base: interval, now: clock.now}
	o.reporter = runPhase4Metering(ctx, client, dcapWithMirrors, "")
	state := inverter.InverterState{ActivePowerW: 1200, Time: time.Unix(1700000000, 0)}

	o.report(ctx, state) // 404: mirror lost
	if len(fs.mupBodies) != 1 {
		t.Fatalf("mirror POSTs = %d after the 404, want 1 (wait for the backoff)", len(fs.mupBodies))
	}
	o.report(ctx, state)
	if len(fs.mupBodies) != 1 {
		t.Fatalf("mirror POSTs = %d inside the backoff, want 1", len(fs.mupBodies))
	}
	clock.advance(interval)
	o.report(ctx, state)

	if len(fs.mupBodies) != 2 {
		t.Fatalf("mirror POSTs = %d, want 2", len(fs.mupBodies))
	}
	var mup sep2.MirrorUsagePoint
	if err := xml.Unmarshal([]byte(fs.mupBodies[1]), &mup); err != nil {
		t.Fatalf("unmarshal re-created mirror: %v", err)
	}
	if mup.RoleFlags != 9 || mup.DeviceLFDI != client.LFDI() {
		t.Errorf("re-created mirror RoleFlags = %d, DeviceLFDI = %s, want 9 and %s", mup.RoleFlags, mup.DeviceLFDI, client.LFDI())
	}
	got := fs.readings["/mup/2"]
	if len(got) != 1 || got[0].Reading == nil || got[0].Reading.Value == nil || *got[0].Reading.Value != 1200 {
		t.Fatalf("readings at /mup/2 = %v, want one of 1200 W", got)
	}
	if n := len(fs.readings["/mup/1"]); n != 0 {
		t.Errorf("%d readings at the lost /mup/1, want 0", n)
	}
}

// A second loss in a row doubles the wait before the next create.
func TestOwnReporter_RepeatedLossBacksOff(t *testing.T) {
	t.Parallel()
	env := newDERWalkTestEnv(t)
	fs := &fakeServer{mupLFDI: map[string]string{}, readings: map[string][]sep2.MirrorMeterReading{}}
	fs.readingFailures, fs.readingFailStatus = 2, http.StatusNotFound
	serverURL, _ := startDERWalkListener(t, env, fs.mux(t))
	client := newDERWalkClient(t, env, serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const interval = 10 * time.Second
	clock := &fakeClock{t: time.Now()}
	o := &ownReporter{client: client, dcap: dcapWithMirrors, base: interval, now: clock.now}
	o.reporter = runPhase4Metering(ctx, client, dcapWithMirrors, "")
	state := inverter.InverterState{ActivePowerW: 1200, Time: time.Unix(1700000000, 0)}

	o.report(ctx, state) // first loss
	clock.advance(interval)
	o.report(ctx, state) // second mirror, second loss: wait two intervals
	clock.advance(interval)
	o.report(ctx, state)
	if len(fs.mupBodies) != 2 {
		t.Fatalf("mirror POSTs = %d one interval after the second loss, want 2", len(fs.mupBodies))
	}
	clock.advance(interval)
	o.report(ctx, state)
	if len(fs.mupBodies) != 3 || len(fs.readings["/mup/3"]) != 1 {
		t.Fatalf("mirror POSTs = %d, readings at /mup/3 = %d, want 3 and 1", len(fs.mupBodies), len(fs.readings["/mup/3"]))
	}
}

// A first mirror create that fails is retried after the backoff, and readings
// land at the new mirror.
func TestOwnReporter_FailedFirstCreateIsRetried(t *testing.T) {
	t.Parallel()
	env := newDERWalkTestEnv(t)
	fs := &fakeServer{mupLFDI: map[string]string{}, readings: map[string][]sep2.MirrorMeterReading{}}
	fs.mupFailures = 1
	serverURL, _ := startDERWalkListener(t, env, fs.mux(t))
	client := newDERWalkClient(t, env, serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const interval = 10 * time.Second
	clock := &fakeClock{t: time.Now()}
	reporter := runPhase4Metering(ctx, client, dcapWithMirrors, "") // first create answers 503
	if reporter.MirrorHref() != "" {
		t.Fatalf("mirror href = %q after a failed create, want empty", reporter.MirrorHref())
	}
	o := newOwnReporter(client, dcapWithMirrors, reporter, interval, clock.now)
	state := inverter.InverterState{ActivePowerW: 1200, Time: time.Unix(1700000000, 0)}

	o.report(ctx, state)
	if n := len(fs.mupBodies); n != 0 {
		t.Fatalf("mirror POSTs = %d inside the backoff, want 0", n)
	}
	clock.advance(interval)
	o.report(ctx, state)

	if n := len(fs.mupBodies); n != 1 {
		t.Fatalf("mirror POSTs = %d after the backoff, want 1", n)
	}
	got := fs.readings["/mup/1"]
	if len(got) != 1 || got[0].Reading == nil || got[0].Reading.Value == nil || *got[0].Reading.Value != 1200 {
		t.Fatalf("readings at /mup/1 = %v, want one of 1200 W", got)
	}
}

// An empty mirror href is not a silent success: nothing is posted, and the
// skip is logged once per streak, not per cycle.
func TestOwnReporter_EmptyHrefSkipIsLoggedOnce(t *testing.T) {
	env := newDERWalkTestEnv(t)
	fs := &fakeServer{mupLFDI: map[string]string{}, readings: map[string][]sep2.MirrorMeterReading{}}
	fs.mupFailures = 1000
	serverURL, _ := startDERWalkListener(t, env, fs.mux(t))
	client := newDERWalkClient(t, env, serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	const interval = 10 * time.Second
	clock := &fakeClock{t: time.Now()}
	o := newOwnReporter(client, dcapWithMirrors, runPhase4Metering(ctx, client, dcapWithMirrors, ""), interval, clock.now)
	state := inverter.InverterState{ActivePowerW: 1200, Time: time.Unix(1700000000, 0)}
	for range 3 {
		o.report(ctx, state)
	}
	if n := strings.Count(buf.String(), "reading skipped"); n != 1 {
		t.Errorf("'reading skipped' logged %d times over 3 cycles, want 1:\n%s", n, buf.String())
	}
	if n := len(fs.readings["/mup/1"]); n != 0 {
		t.Errorf("%d readings with no mirror, want 0", n)
	}
}

func TestMirrorLost_Only404(t *testing.T) {
	t.Parallel()
	if !inverter.MirrorLost(fmt.Errorf("POST /mup/1/mr: %w", inverter.ErrNotFound)) {
		t.Error("wrapped 404: MirrorLost = false, want true")
	}
	for _, err := range []error{nil, inverter.ErrBadRequest, inverter.ErrResponseTransient, errors.New("boom")} {
		if inverter.MirrorLost(err) {
			t.Errorf("MirrorLost(%v) = true, want false", err)
		}
	}
}
