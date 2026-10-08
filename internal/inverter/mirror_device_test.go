// Package inverter_test covers how a mirror and a reading name the device
// they describe (GRIDAPPSD/ieee-2030_5-client-go#68): CreateMirrorUsagePoint
// takes the described device's LFDI as a parameter instead of always using
// the client's own certificate LFDI, and both the mirror mRID and the
// reading mRID conform to mRIDType (IEEE 2030.5-2018, HexBinary128: at
// most 32 hex characters) and stay unique per device and per reading, since
// this client mints them with no IANA PEN configured.
package inverter_test

import (
	"bytes"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// deviceA and deviceB stand in for two DERs a manager acts for. Distinct
// from testDeviceLFDI so a mixup between "the device under test" and "some
// other device" would show up as a wrong assertion rather than a
// coincidental match. 40 hex characters, matching a real LFDI's length
// (HexBinary160).
const (
	deviceALFDI = "111111111111111111111111111111111111AAAA"
	deviceBLFDI = "222222222222222222222222222222222222BBBB"
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
	// A manager acts for other devices; a der client may only name itself.
	client := newGuardTestClient(t, serverURL, env, "aggregator")
	client.SetManagedSet(guard.NewStaticManagedSet(deviceALFDI, deviceBLFDI))
	ctx := testCtx(t)

	for _, dev := range []struct {
		lfdi string
		mrid string
	}{
		{deviceALFDI, inverter.MirrorUsagePointMRID(deviceALFDI)},
		{deviceBLFDI, inverter.MirrorUsagePointMRID(deviceBLFDI)},
	} {
		if _, err := client.CreateMirrorUsagePoint(inverter.WithTarget(ctx, dev.lfdi), "/mup", inverter.DeviceLFDI(dev.lfdi), sep2.MirrorUsagePoint{
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
// old c.lfdi-assignment behavior byte for byte. MRID is held fixed here on
// purpose : this test is about DeviceLFDI wiring, not MRID format, which
// TestMirrorUsagePointMRID_* covers separately.
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

	if _, err := client.CreateMirrorUsagePoint(testCtx(t), "/mup", inverter.DeviceLFDI(client.LFDI()), mup); err != nil {
		t.Fatalf("CreateMirrorUsagePoint: %v", err)
	}

	if !bytes.Equal(got, wantBody) {
		t.Errorf("POST body not byte-identical to today's DER-client output.\ngot:  %s\nwant: %s", got, wantBody)
	}
}

// isHex128 reports whether s is exactly 32 upper-case hex characters, the
// mRIDType wire format (IEEE 2030.5-2018, HexBinary128).
func isHex128(s string) bool {
	if len(s) != 32 {
		return false
	}
	if s != strings.ToUpper(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// TestMirrorUsagePointMRID_Format proves the mirror mRID conforms to
// mRIDType and is unique per device : the "mup-"+SFDI[:8] format it
// replaces was neither.
func TestMirrorUsagePointMRID_Format(t *testing.T) {
	t.Parallel()

	mridA := inverter.MirrorUsagePointMRID(deviceALFDI)
	mridB := inverter.MirrorUsagePointMRID(deviceBLFDI)

	if !isHex128(mridA) {
		t.Errorf("mridA = %q, want 32 upper-case hex characters", mridA)
	}
	if !isHex128(mridB) {
		t.Errorf("mridB = %q, want 32 upper-case hex characters", mridB)
	}
	if mridA == mridB {
		t.Errorf("mridA == mridB == %q for two different devices", mridA)
	}
	if got := inverter.MirrorUsagePointMRID(deviceALFDI); got != mridA {
		t.Errorf("MirrorUsagePointMRID is not deterministic for the same device: %q != %q", got, mridA)
	}
}

// TestReadingMRID_UniquePerDeviceSameSecond fixes the timestamp so both
// devices land in the identical second : the exact collision case the
// issue describes : and asserts the MRIDs still differ, conform to
// mRIDType, and that a per-Reporter counter (seq) also separates two
// readings for the SAME device in the same second, which deviceLFDI alone
// cannot.
func TestReadingMRID_UniquePerDeviceSameSecond(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local)

	mridA := inverter.ReadingMRIDForTesting(deviceALFDI, at, 1)
	mridB := inverter.ReadingMRIDForTesting(deviceBLFDI, at, 1)
	mridA2 := inverter.ReadingMRIDForTesting(deviceALFDI, at, 2)

	if !isHex128(mridA) {
		t.Errorf("mridA = %q, want 32 upper-case hex characters", mridA)
	}
	if !isHex128(mridB) {
		t.Errorf("mridB = %q, want 32 upper-case hex characters", mridB)
	}
	if mridA == mridB {
		t.Fatalf("mridA == mridB == %q for two devices at the identical timestamp and seq", mridA)
	}
	if mridA == mridA2 {
		t.Fatalf("same device, same second, different seq: mridA == mridA2 == %q", mridA)
	}

	// Same device, same second, same seq: deterministic.
	if got := inverter.ReadingMRIDForTesting(deviceALFDI, at, 1); got != mridA {
		t.Errorf("readingMRID is not deterministic for identical inputs: %q != %q", got, mridA)
	}
}

// TestAvoidReservedAllF proves the reserved-value guard directly against a
// crafted all-0xFF input : IEEE 2030.5-2018 reserves
// 0xFFFFFFFFFFFFFFFFFFFFFFFF[PEN] for an in-progress accumulator record,
// so deriveMRID must never return it. A hash is one-way, so this is
// proven against the guard function itself rather than hoping a SHA-256
// output collides with all-F.
func TestAvoidReservedAllF(t *testing.T) {
	t.Parallel()

	allFF := bytes.Repeat([]byte{0xFF}, 16)
	got := inverter.AvoidReservedAllFForTesting(allFF)
	if bytes.Equal(got, allFF) {
		t.Errorf("avoidReservedAllF(all-0xFF) = %x, want it changed away from the reserved value", got)
	}
	if len(got) != 16 {
		t.Errorf("avoidReservedAllF(all-0xFF) changed length: got %d bytes, want 16", len(got))
	}

	notAllFF := append(bytes.Repeat([]byte{0xFF}, 15), 0x00)
	if got := inverter.AvoidReservedAllFForTesting(notAllFF); !bytes.Equal(got, notAllFF) {
		t.Errorf("avoidReservedAllF(non-reserved input) = %x, want it returned unchanged (%x)", got, notAllFF)
	}
}

// TestReporter_ReadingMRIDPerDevice runs ReportMetering for two devices
// through separate Reporters sharing one client, and asserts the posted
// MRIDs differ, conform to mRIDType, and neither equals the other device's.
// Complements TestReadingMRID_UniquePerDeviceSameSecond's fixed-time proof
// with a real POST round-trip.
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

	reporterA, err := inverter.NewReporter(client, deviceALFDI, "", "/mup/a/mr")
	if err != nil {
		t.Fatalf("NewReporter(A): %v", err)
	}
	reporterB, err := inverter.NewReporter(client, deviceBLFDI, "", "/mup/b/mr")
	if err != nil {
		t.Fatalf("NewReporter(B): %v", err)
	}

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

	if !isHex128(gotA.MRID) {
		t.Errorf("device A MRID = %q, want 32 upper-case hex characters", gotA.MRID)
	}
	if !isHex128(gotB.MRID) {
		t.Errorf("device B MRID = %q, want 32 upper-case hex characters", gotB.MRID)
	}
	if gotA.MRID == gotB.MRID {
		t.Fatalf("device A and device B MRID both = %q", gotA.MRID)
	}
}

// TestNewReporter_EmptyDeviceLFDIFails proves NewReporter refuses an empty
// deviceLFDI, matching CreateMirrorUsagePoint's guard : every mRID this
// Reporter would mint names its device, so an empty value must never be
// accepted.
func TestNewReporter_EmptyDeviceLFDIFails(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	serverURL, _ := startIdleListener(t, env, http.NewServeMux())
	client := newCSIPClient(t, env, serverURL, true)

	reporter, err := inverter.NewReporter(client, "", "", "")
	if err == nil {
		t.Error("NewReporter(_, \"\", _, _) returned nil error; should refuse an empty deviceLFDI")
	}
	if reporter != nil {
		t.Errorf("NewReporter(_, \"\", _, _) returned a non-nil Reporter alongside the error: %+v", reporter)
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
