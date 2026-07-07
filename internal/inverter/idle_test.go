package inverter_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
)

// startIdleListener boots a gotls-backed HTTPS server backed by the supplied
// handler. It reuses the same NewCCMServerConfig path as the CCM tests so the
// HardwareModuleName SAN tolerant verify hook is in play. Returns the listen
// URL and a stop func.
//
// Used by IEEE-028 idle-poll tests to count and sequence /dcap responses.
func startIdleListener(t *testing.T, env *ccmTestEnv, handler http.Handler) (serverURL string, stop func()) {
	t.Helper()

	cfg, err := sepTLS.NewCCMServerConfig(env.serverCertPath, env.serverKeyPath, env.caCertPath)
	if err != nil {
		t.Fatalf("NewCCMServerConfig: %v", err)
	}

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	tlsL := gotls.NewListener(tcpL, cfg)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})

	return "https://" + tlsL.Addr().String(), func() {
		_ = srv.Close()
		_ = tlsL.Close()
	}
}

func writeDcap(t *testing.T, w http.ResponseWriter, d sep2.DeviceCapability) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&d); err != nil {
		t.Errorf("encode dcap: %v", err)
	}
}

// TestEmptyDcapIdlesAndNeverRegisters asserts that when DeviceCapability
// advertises no function-set links, WaitForAdvertisedLinks re-polls /dcap
// forever and never falls through to /edev (or any other phase-2+ path).
// Context cancel exits the loop cleanly.
//
// IEEE-028 RED: without the wait helper, today's main.go fires Phase 2
// immediately; with the helper, the loop owns the polling cadence.
func TestEmptyDcapIdlesAndNeverRegisters(t *testing.T) {
	env := newCCMTestEnv(t)

	var dcapHits atomic.Int32
	var phase2Hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		dcapHits.Add(1)
		writeDcap(t, w, sep2.DeviceCapability{PollRate: 1})
	})
	// Any of these getting hit is a correctness failure for IEEE-028.
	for _, p := range []string{"/edev", "/mup", "/sdev", "/tm"} {
		mux.HandleFunc(p, func(_ http.ResponseWriter, _ *http.Request) {
			phase2Hits.Add(1)
		})
	}

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	// Shrink the poll cadence so the test runs in tens of ms, not seconds.
	restore := inverter.SetPollDurationForTesting(func(_ uint32) time.Duration {
		return 10 * time.Millisecond
	})
	defer restore()

	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL,
		CertFile:  env.deviceCertPath,
		KeyFile:   env.deviceKeyPath,
		CAFile:    env.caCertPath,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}

	// First Discover establishes the initial empty dcap (counts as hit #1).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial, err := client.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover initial: %v", err)
	}
	if dcapHasAnyLinkForTest(initial) {
		t.Fatalf("initial dcap unexpectedly has links: %+v", initial)
	}

	// Now run the wait loop. After ~80ms we should have at least 3 more
	// dcap hits (1 initial + a few re-polls), then cancel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, werr := client.WaitForAdvertisedLinks(ctx, initial)
		// errors.Is unwraps fmt.Errorf("...: %w", ctx.Err()) wrappers, so
		// it catches both the bare ctx.Err() return and the wrapped
		// "re-discover after idle wait: ... context canceled" path that
		// fires if cancel races with a /dcap GET already in flight.
		if werr != nil && !errors.Is(werr, context.Canceled) {
			t.Errorf("WaitForAdvertisedLinks returned %v, want nil or context.Canceled", werr)
		}
	}()

	// Give the loop time to spin through several polls.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForAdvertisedLinks did not exit within 2s of ctx cancel")
	}

	if got := dcapHits.Load(); got < 3 {
		t.Errorf("dcap hits = %d, want >= 3 (initial + re-polls)", got)
	}
	if got := phase2Hits.Load(); got != 0 {
		t.Errorf("phase-2 path hits = %d, want 0 (must idle on empty dcap)", got)
	}
}

// TestDcapBecomesPopulated_ProceedsToPhase2 asserts that when /dcap returns
// empty on the first two calls and populated on the third, the inverter
// polls /dcap exactly three times then proceeds to POST /edev exactly once.
//
// IEEE-028 RED: today's main.go would POST /edev after the first Discover,
// regardless of advertised links.
func TestDcapBecomesPopulated_ProceedsToPhase2(t *testing.T) {
	env := newCCMTestEnv(t)

	var dcapHits atomic.Int32
	var edevHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		n := dcapHits.Add(1)
		dcap := sep2.DeviceCapability{PollRate: 1}
		if n >= 3 {
			dcap.EndDeviceListLink = &sep2.ListLink{Href: "/edev"}
		}
		writeDcap(t, w, dcap)
	})
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		edevHits.Add(1)
		// Mimic enough of a Created response for the inverter's Register
		// flow to not error. The test only cares that we got here once.
		w.Header().Set("Location", "/edev/1")
		w.WriteHeader(http.StatusCreated)
	})
	// The Register flow does a GET on Location after POST.
	mux.HandleFunc("/edev/1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.EndDevice{})
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	restore := inverter.SetPollDurationForTesting(func(_ uint32) time.Duration {
		return 10 * time.Millisecond
	})
	defer restore()

	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL,
		CertFile:  env.deviceCertPath,
		KeyFile:   env.deviceKeyPath,
		CAFile:    env.caCertPath,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	initial, err := client.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover initial: %v", err)
	}

	final, err := client.WaitForAdvertisedLinks(ctx, initial)
	if err != nil {
		t.Fatalf("WaitForAdvertisedLinks: %v", err)
	}
	if final.EndDeviceListLink == nil {
		t.Fatalf("final dcap missing EndDeviceListLink: %+v", final)
	}

	// Now simulate Phase 2 : this is what main.go would do next. Pass the
	// advertised EndDeviceListLink href per IEEE-030 (no hardcoded "/edev").
	if _, _, err := client.Register(ctx, final.EndDeviceListLink.Href); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := dcapHits.Load(); got != 3 {
		t.Errorf("/dcap hits = %d, want exactly 3", got)
	}
	if got := edevHits.Load(); got != 1 {
		t.Errorf("/edev POST hits = %d, want exactly 1", got)
	}
}

// dcapHasAnyLinkForTest mirrors the production predicate so the test can
// sanity-check the seed payload. Kept in _test.go so it never ships.
func dcapHasAnyLinkForTest(d sep2.DeviceCapability) bool {
	return d.EndDeviceListLink != nil ||
		d.TimeLink != nil ||
		d.SelfDeviceLink != nil ||
		d.MirrorUsagePointListLink != nil ||
		d.ResponseSetListLink != nil
}
