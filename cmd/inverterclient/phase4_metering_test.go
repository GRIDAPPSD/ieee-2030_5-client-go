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
	"context"
	"encoding/xml"
	"io"
	"net/http"
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

	var gotDeviceLFDI atomic.Value
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
