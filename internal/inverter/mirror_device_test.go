// Package inverter_test covers how a mirror and a reading name the device
// they describe (GRIDAPPSD/ieee-2030_5-client-go#68): CreateMirrorUsagePoint
// takes the described device's LFDI as a parameter instead of always using
// the client's own certificate LFDI, and the reading MRID includes the
// device so two devices reporting through one client in the same second do
// not collide.
package inverter_test

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// deviceA and deviceB stand in for two DERs a manager acts for. Distinct
// from testDeviceLFDI so a mixup between "the device under test" and "some
// other device" would show up as a wrong assertion rather than a
// coincidental match.
const (
	deviceALFDI = "1111111111111111111111111111111111AAAA"
	deviceBLFDI = "2222222222222222222222222222222222BBBB"
)

// TestCreateMirrorUsagePoint_DeviceLFDI posts a mirror for two devices
// through one client and asserts each encoded body carries its own
// deviceLFDI, not the client's certificate LFDI : the collision
// CreateMirrorUsagePoint's old c.lfdi assignment could not avoid.
func TestCreateMirrorUsagePoint_DeviceLFDI(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var mu atomicBodies
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read POST body: %v", err)
		}
		mu.add(b)
		w.Header().Set("Location", "/mup/x")
		w.WriteHeader(http.StatusCreated)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)
	ctx := testCtx(t)

	for _, dev := range []struct {
		lfdi string
		mrid string
	}{
		{deviceALFDI, "mup-a"},
		{deviceBLFDI, "mup-b"},
	} {
		if _, err := client.CreateMirrorUsagePoint(ctx, "/mup", dev.lfdi, sep2.MirrorUsagePoint{
			MRID:                dev.mrid,
			Description:         "PV Inverter Metering",
			ServiceCategoryKind: 0,
			Status:              1,
		}); err != nil {
			t.Fatalf("CreateMirrorUsagePoint(%s): %v", dev.lfdi, err)
		}
	}

	bodies := mu.get()
	if len(bodies) != 2 {
		t.Fatalf("got %d POST bodies, want 2", len(bodies))
	}

	var gotA, gotB sep2.MirrorUsagePoint
	if err := xml.Unmarshal(bodies[0], &gotA); err != nil {
		t.Fatalf("unmarshal body A: %v", err)
	}
	if err := xml.Unmarshal(bodies[1], &gotB); err != nil {
		t.Fatalf("unmarshal body B: %v", err)
	}

	if gotA.DeviceLFDI != deviceALFDI {
		t.Errorf("device A: DeviceLFDI = %q, want %q", gotA.DeviceLFDI, deviceALFDI)
	}
	if gotB.DeviceLFDI != deviceBLFDI {
		t.Errorf("device B: DeviceLFDI = %q, want %q", gotB.DeviceLFDI, deviceBLFDI)
	}
	if gotA.DeviceLFDI == client.LFDI() {
		t.Errorf("device A DeviceLFDI equals the client's own certificate LFDI; deviceLFDI param was ignored")
	}
	if bytes.Equal(bodies[0], bodies[1]) {
		t.Error("device A and device B POST bodies are byte-identical; want distinct")
	}
}

// TestCreateMirrorUsagePoint_EmptyDeviceLFDIFails proves the deviceLFDI
// guard rejects an empty value before any HTTP call, matching the existing
// mupListHref guard : deviceLFDI is required (schema minOccurs=1,
// core-go schema_gate_test.go "omitempty-required MirrorUsagePoint.DeviceLFDI"),
// so an empty value must never reach the wire.
func TestCreateMirrorUsagePoint_EmptyDeviceLFDIFails(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(_ http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	if _, err := client.CreateMirrorUsagePoint(testCtx(t), "/mup", "", sep2.MirrorUsagePoint{}); err == nil {
		t.Error("CreateMirrorUsagePoint(_, _, \"\", _) returned nil error; should fail before HTTP")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("/mup hits = %d, want 0", got)
	}
}

// TestCreateMirrorUsagePoint_DERClientByteIdentical proves the invariant
// that a DER client acting for itself still sends the exact body it sent
// before this change: passing client.LFDI() as deviceLFDI reproduces the
// old c.lfdi-assignment behavior byte for byte.
func TestCreateMirrorUsagePoint_DERClientByteIdentical(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var got []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read POST body: %v", err)
		}
		got = b
		w.WriteHeader(http.StatusCreated)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	mup := sep2.MirrorUsagePoint{
		MRID:                "mup-abcdefgh",
		Description:         "PV Inverter Metering",
		ServiceCategoryKind: 0,
		Status:              1,
	}

	// The pre-#68 production line was `mup.DeviceLFDI = c.lfdi`, applied
	// internally with no caller-visible parameter. Reproduce that shape
	// directly to get the "before" body to compare against.
	want := mup
	want.DeviceLFDI = client.LFDI()
	wantBody, err := xml.Marshal(&want)
	if err != nil {
		t.Fatalf("marshal want body: %v", err)
	}

	if _, err := client.CreateMirrorUsagePoint(testCtx(t), "/mup", client.LFDI(), mup); err != nil {
		t.Fatalf("CreateMirrorUsagePoint: %v", err)
	}

	if !bytes.Equal(got, wantBody) {
		t.Errorf("POST body not byte-identical to today's DER-client output.\ngot:  %s\nwant: %s", got, wantBody)
	}
}

// TestReadingMRID_UniquePerDeviceSameSecond fixes the timestamp so both
// devices land in the identical second : the exact collision case the
// issue describes (reading-<second>, no device component) : and asserts
// the MRIDs still differ because the device is now part of the format.
func TestReadingMRID_UniquePerDeviceSameSecond(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local)

	mridA := inverter.ReadingMRIDForTesting(deviceALFDI, at)
	mridB := inverter.ReadingMRIDForTesting(deviceBLFDI, at)

	if mridA == mridB {
		t.Fatalf("mridA == mridB == %q for two devices at the identical timestamp", mridA)
	}
	const wantPrefixA = "reading-" + deviceALFDI + "-"
	const wantPrefixB = "reading-" + deviceBLFDI + "-"
	if got := mridA; len(got) < len(wantPrefixA) || got[:len(wantPrefixA)] != wantPrefixA {
		t.Errorf("mridA = %q, want prefix %q", got, wantPrefixA)
	}
	if got := mridB; len(got) < len(wantPrefixB) || got[:len(wantPrefixB)] != wantPrefixB {
		t.Errorf("mridB = %q, want prefix %q", got, wantPrefixB)
	}

	// Same device, same second: still deterministic (not randomized), so
	// two calls for the same device at the same instant collide exactly
	// as before. This issue does not change that; it only fixes cross-
	// device collisions.
	if got := inverter.ReadingMRIDForTesting(deviceALFDI, at); got != mridA {
		t.Errorf("readingMRID is not deterministic for the same device and time: %q != %q", got, mridA)
	}
}

// TestReporter_ReadingMRIDPerDevice runs ReportMetering for two devices
// through separate Reporters sharing one client, and asserts the posted
// MRIDs differ and each carries its own device's LFDI. Complements
// TestReadingMRID_UniquePerDeviceSameSecond's fixed-time proof with a real
// POST round-trip.
func TestReporter_ReadingMRIDPerDevice(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var bodyA, bodyB atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/mup/a/mr", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyA.Store(b)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/mup/b/mr", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyB.Store(b)
		w.WriteHeader(http.StatusCreated)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)
	ctx := testCtx(t)

	reporterA := inverter.NewReporter(client, deviceALFDI, "", "/mup/a/mr")
	reporterB := inverter.NewReporter(client, deviceBLFDI, "", "/mup/b/mr")

	state := inverter.InverterState{ActivePowerW: 100, Connected: true, Time: time.Now()}
	if err := reporterA.ReportMetering(ctx, state); err != nil {
		t.Fatalf("reporterA.ReportMetering: %v", err)
	}
	if err := reporterB.ReportMetering(ctx, state); err != nil {
		t.Fatalf("reporterB.ReportMetering: %v", err)
	}

	var gotA, gotB sep2.MirrorMeterReading
	if err := xml.Unmarshal(bodyA.Load().([]byte), &gotA); err != nil {
		t.Fatalf("unmarshal body A: %v", err)
	}
	if err := xml.Unmarshal(bodyB.Load().([]byte), &gotB); err != nil {
		t.Fatalf("unmarshal body B: %v", err)
	}

	if gotA.MRID == gotB.MRID {
		t.Fatalf("device A and device B MRID both = %q", gotA.MRID)
	}
	wantPrefixA := "reading-" + deviceALFDI + "-"
	wantPrefixB := "reading-" + deviceBLFDI + "-"
	if len(gotA.MRID) < len(wantPrefixA) || gotA.MRID[:len(wantPrefixA)] != wantPrefixA {
		t.Errorf("device A MRID = %q, want prefix %q", gotA.MRID, wantPrefixA)
	}
	if len(gotB.MRID) < len(wantPrefixB) || gotB.MRID[:len(wantPrefixB)] != wantPrefixB {
		t.Errorf("device B MRID = %q, want prefix %q", gotB.MRID, wantPrefixB)
	}
}

// atomicBodies collects POST bodies in call order under a mutex-free
// atomic.Value, since the two CreateMirrorUsagePoint calls in
// TestCreateMirrorUsagePoint_DeviceLFDI run sequentially on the test
// goroutine but the handler runs on the server's own goroutine.
type atomicBodies struct {
	v atomic.Value
}

func (a *atomicBodies) add(b []byte) {
	cur, _ := a.v.Load().([][]byte)
	a.v.Store(append(cur, b))
}

func (a *atomicBodies) get() [][]byte {
	cur, _ := a.v.Load().([][]byte)
	return cur
}
