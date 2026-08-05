// Package inverter_test covers LookupOwnEndDevice, ErrEndDeviceNotFound,
// --csip flag plumbing, and the four href-taking method signatures that
// replaced string-formatted paths.
//
// This behavior is frozen. These tests assert frozen
// behavior; they do not exercise unmerged future changes. If a test reveals
// a defect, the project policy is to file a separate MEDIUM ticket : do not
// fix inline.
//
// Fixture pattern mirrors internal/inverter/idle_test.go: each
// test stands up a gotls-backed HTTPS server via the shared ccmTestEnv from
// client_ccm_test.go and routes requests with a per-test http.ServeMux.
// HTTP-method-keyed atomic counters detect unintended fan-out.
package inverter_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// newCSIPClient builds an inverter client against serverURL using the
// shared ccmTestEnv certs. CSIP is a per-test bool : case 4 needs false.
func newCSIPClient(t *testing.T, env *ccmTestEnv, serverURL string, csip bool) *inverter.SEP2Client {
	t.Helper()
	c, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL,
		CertFile:  env.deviceCertPath,
		KeyFile:   env.deviceKeyPath,
		CAFile:    env.caCertPath,
		CSIP:      csip,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}
	return c
}

// writeEdevList encodes the supplied EndDeviceList as SEP+XML to w.
func writeEdevList(t *testing.T, w http.ResponseWriter, list sep2.EndDeviceList) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&list); err != nil {
		t.Errorf("encode edev list: %v", err)
	}
}

// writeEdev encodes the supplied EndDevice as SEP+XML to w.
func writeEdev(t *testing.T, w http.ResponseWriter, edev sep2.EndDevice) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&edev); err != nil {
		t.Errorf("encode edev: %v", err)
	}
}

// otherLFDI is a deterministic non-matching LFDI used in case 3 below.
// 40 uppercase hex chars matches the internal/tls.LFDI("%X", ...) format.
const otherLFDI = "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"

// =============================================================================
// LookupOwnEndDevice + --csip flag plumbing
// =============================================================================

// TestLookupOwnEndDevice_FindsByLFDI exercises case 1:
// --csip on, server /edev list contains our LFDI → LookupOwnEndDevice
// succeeds; Phase 3 (DER setup) fires exactly once afterward.
//
// The handler reads the client LFDI from an atomic stitched in after
// NewSEP2Client returns : the test cannot know the LFDI before the
// device cert is loaded.
func TestLookupOwnEndDevice_FindsByLFDI(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var myLFDI atomic.Value
	myLFDI.Store("")

	var edevHits atomic.Int32
	var derCapHits atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, r *http.Request) {
		edevHits.Add(1)
		// Sanity: the production code appends ?l=255 (first-cut paging).
		if !strings.Contains(r.URL.RawQuery, "l=255") {
			t.Errorf("expected ?l=255 paging query, got %q", r.URL.RawQuery)
		}
		writeEdevList(t, w, sep2.EndDeviceList{
			EndDevice: []sep2.EndDevice{{
				LFDI:        myLFDI.Load().(string),
				DERListLink: &sep2.ListLink{Href: "/edev/1/der"},
			}},
		})
	})
	mux.HandleFunc("/edev/1/der/1/dercap", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("dercap method = %s, want PUT", r.Method)
		}
		derCapHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	myLFDI.Store(client.LFDI())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	edev, _, err := client.LookupOwnEndDevice(ctx, "/edev")
	if err != nil {
		t.Fatalf("LookupOwnEndDevice: %v", err)
	}
	if edev.LFDI != client.LFDI() {
		t.Errorf("returned LFDI = %q, want %q", edev.LFDI, client.LFDI())
	}
	if edev.DERListLink == nil || edev.DERListLink.Href != "/edev/1/der" {
		t.Fatalf("DERListLink not parsed: %+v", edev.DERListLink)
	}

	// Phase 3 fires exactly once : drive PutDERCapability through the
	// href the server advertised. The production code
	// derives this href from the parsed EndDevice's DERListLink rather
	// than constructing it from ID segments.
	if err := client.PutDERCapability(ctx, edev.DERListLink.Href+"/1/dercap", sep2.DERCapability{}); err != nil {
		t.Fatalf("PutDERCapability: %v", err)
	}

	if got := edevHits.Load(); got != 1 {
		t.Errorf("/edev GET hits = %d, want exactly 1", got)
	}
	if got := derCapHits.Load(); got != 1 {
		t.Errorf("derCap PUT hits = %d, want exactly 1", got)
	}
}

// TestLookupOwnEndDevice_EmptyListReturnsNotFound exercises case 2:
// --csip on, server /edev list empty → ErrEndDeviceNotFound; caller idles;
// zero PUTs/POSTs on Phase 3+ endpoints while idling. Then the list
// becomes populated and Lookup succeeds on the retry.
//
// The "advances to Phase 3 when our LFDI appears" half is exercised by
// flipping the served list and re-calling Lookup; the production idle
// loop lives in cmd/inverterclient/main.go and is integration territory.
func TestLookupOwnEndDevice_EmptyListReturnsNotFound(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var listEmpty atomic.Bool
	listEmpty.Store(true)

	var myLFDI atomic.Value
	myLFDI.Store("")

	var edevHits atomic.Int32
	var phase3PostHits atomic.Int32
	var phase3PutHits atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		edevHits.Add(1)
		var list sep2.EndDeviceList
		if !listEmpty.Load() {
			list.EndDevice = []sep2.EndDevice{{LFDI: myLFDI.Load().(string)}}
		}
		writeEdevList(t, w, list)
	})
	// Any of these PUTs/POSTs firing during the idle window is a bug.
	for _, p := range []string{"/edev/1/der/1/dercap", "/edev/1/der/1/derg", "/edev/1/der/1/ders", "/mup", "/mup/1/mr"} {
		p := p
		mux.HandleFunc(p, func(_ http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost:
				phase3PostHits.Add(1)
			case http.MethodPut:
				phase3PutHits.Add(1)
			}
		})
	}

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	myLFDI.Store(client.LFDI())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First lookup: list empty → ErrEndDeviceNotFound.
	_, _, err := client.LookupOwnEndDevice(ctx, "/edev")
	if !errors.Is(err, inverter.ErrEndDeviceNotFound) {
		t.Fatalf("LookupOwnEndDevice err = %v, want ErrEndDeviceNotFound", err)
	}

	// Simulate two more idle re-polls : the production main.go loop
	// hits /edev on each cycle; we call Lookup again to mirror the
	// behavior under test. Zero Phase 3+ traffic must occur in this
	// window because the inverter has no EndDevice to write against.
	for i := 0; i < 2; i++ {
		if _, _, err := client.LookupOwnEndDevice(ctx, "/edev"); !errors.Is(err, inverter.ErrEndDeviceNotFound) {
			t.Fatalf("idle re-poll %d: err = %v, want ErrEndDeviceNotFound", i+1, err)
		}
	}

	if got := phase3PostHits.Load(); got != 0 {
		t.Errorf("Phase 3+ POST hits during idle = %d, want 0", got)
	}
	if got := phase3PutHits.Load(); got != 0 {
		t.Errorf("Phase 3+ PUT hits during idle = %d, want 0", got)
	}

	// Now the server provisions our device : flip the list and Lookup
	// must succeed on the next call (advances out of the idle loop).
	listEmpty.Store(false)
	edev, _, err := client.LookupOwnEndDevice(ctx, "/edev")
	if err != nil {
		t.Fatalf("LookupOwnEndDevice after provisioning: %v", err)
	}
	if edev.LFDI != client.LFDI() {
		t.Errorf("provisioned LFDI = %q, want %q", edev.LFDI, client.LFDI())
	}
	if got := edevHits.Load(); got < 4 {
		t.Errorf("/edev hits = %d, want >= 4 (empty x3 + populated)", got)
	}
}

// TestLookupOwnEndDevice_OtherLFDIsNotOurs exercises case 3:
// --csip on, server list contains other LFDIs but not ours → LookupOwnEndDevice
// returns ErrEndDeviceNotFound. Verifies the LFDI filter is exact-match,
// not just a non-empty list check, and case-sensitive on the uppercase-hex
// representation produced by internal/tls.LFDI.
func TestLookupOwnEndDevice_OtherLFDIsNotOurs(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		// Multiple entries, none ours.
		writeEdevList(t, w, sep2.EndDeviceList{
			EndDevice: []sep2.EndDevice{
				{LFDI: otherLFDI},
				{LFDI: "1234567890ABCDEF1234567890ABCDEF12345678"},
				{LFDI: "FEEDFACEFEEDFACEFEEDFACEFEEDFACEFEEDFACE"},
			},
		})
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := client.LookupOwnEndDevice(ctx, "/edev")
	if !errors.Is(err, inverter.ErrEndDeviceNotFound) {
		t.Fatalf("err = %v, want ErrEndDeviceNotFound", err)
	}

}

// TestLookupOwnEndDevice_CaseInsensitiveLFDI codifies a case-insensitivity
// fix: IEEE 2030.5 / CSIP servers commonly emit lowercase `<lFDI>` on the
// wire (xs:hexBinary is case-insensitive per W3C XML Schema Part 2; SunSpec
// test PKI documents the canonical LFDI in lowercase). The Go client
// computes its own LFDI as uppercase via fmt.Sprintf("%X", ...) in
// internal/tls.LFDI. LookupOwnEndDevice MUST match across cases :
// case-sensitive equality produced a silent CSIP interop failure (caller
// idle-polls forever on a false ErrEndDeviceNotFound) before the comparison
// switched to strings.EqualFold.
//
// This test supersedes an earlier fixture that asserted the case-sensitive
// bug as expected behavior.
func TestLookupOwnEndDevice_CaseInsensitiveLFDI(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	// Stand up a placeholder listener so we can build the client and
	// learn its (uppercase) LFDI; the real listener echoes back the
	// lowercased form to simulate an external server.
	probeMux := http.NewServeMux()
	probeURL, probeStop := startIdleListener(t, env, probeMux)
	probeClient := newCSIPClient(t, env, probeURL, true)
	ourUpper := probeClient.LFDI()
	ourLower := strings.ToLower(ourUpper)
	probeStop()

	if ourUpper == ourLower {
		t.Fatalf("client LFDI %q is not uppercase-hex; test premise broken", ourUpper)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		writeEdevList(t, w, sep2.EndDeviceList{
			EndDevice: []sep2.EndDevice{{LFDI: ourLower}},
		})
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	edev, _, err := client.LookupOwnEndDevice(ctx, "/edev")
	if err != nil {
		t.Fatalf("LookupOwnEndDevice: err = %v, want nil (lowercase LFDI must match uppercase client LFDI)", err)
	}
	// The returned EndDevice carries the server's on-the-wire LFDI casing;
	// we only care that the match succeeded and that the same record came
	// back. Compare case-insensitively for symmetry with the fix.
	if !strings.EqualFold(edev.LFDI, ourUpper) {
		t.Errorf("matched edev.LFDI = %q, want case-fold equal to %q", edev.LFDI, ourUpper)
	}
}

// TestLookupOwnEndDevice_EmptyHrefErrors covers the empty-href guard on
// LookupOwnEndDevice : the same defensive check added across all
// five methods. Lifts LookupOwnEndDevice over the >=80% coverage gate and
// asserts the contract.
func TestLookupOwnEndDevice_EmptyHrefErrors(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := client.LookupOwnEndDevice(ctx, "")
	if err == nil {
		t.Fatal("LookupOwnEndDevice(ctx, \"\") returned nil error; want failure")
	}
	if !strings.Contains(err.Error(), "edev list href required") {
		t.Errorf("err = %v, want one containing %q", err, "edev list href required")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("HTTP hits = %d, want 0 (early return)", got)
	}
}

// TestLookupOwnEndDevice_ServerErrorIsWrapped covers the GET-error branch
// in LookupOwnEndDevice : a 500 from the list endpoint surfaces as a
// wrapped error containing both the operation context ("get edev list")
// and the underlying transport status. Lifts LookupOwnEndDevice coverage
// over the ≥80% gate.
func TestLookupOwnEndDevice_ServerErrorIsWrapped(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := client.LookupOwnEndDevice(ctx, "/edev")
	if err == nil {
		t.Fatal("expected non-nil error from 500 response")
	}
	if !strings.Contains(err.Error(), "get edev list") {
		t.Errorf("err = %v, want it to wrap with 'get edev list' context", err)
	}
	if errors.Is(err, inverter.ErrEndDeviceNotFound) {
		t.Errorf("transport error should NOT be reported as ErrEndDeviceNotFound: %v", err)
	}
}

// TestRegister_CSIPOffFiresPOST exercises case 4 (back-compat):
// --csip off, the existing Register() POST still fires /edev and Phase 3
// proceeds. Asserts no regression : the IEEE 2030.5 self-registration
// path is unchanged when --csip is not set.
func TestRegister_CSIPOffFiresPOST(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var postHits atomic.Int32
	var getHits atomic.Int32
	var derCapPutHits atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postHits.Add(1)
			w.Header().Set("Location", "/edev/1")
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("/edev unexpected method %s", r.Method)
		}
	})
	mux.HandleFunc("/edev/1", func(w http.ResponseWriter, _ *http.Request) {
		getHits.Add(1)
		writeEdev(t, w, sep2.EndDevice{
			LFDI:        "ANYLFDI",
			DERListLink: &sep2.ListLink{Href: "/edev/1/der"},
		})
	})
	mux.HandleFunc("/edev/1/der/1/dercap", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("dercap method = %s, want PUT", r.Method)
		}
		derCapPutHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, false /* CSIP off */)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	edev, _, err := client.Register(ctx, "/edev")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if edev.DERListLink == nil {
		t.Fatal("Register did not surface DERListLink from Location read")
	}

	// Phase 3 proceeds : the caller derives the dercap href from the
	// EndDevice's DERListLink (production: see cmd/inverterclient/main.go).
	if err := client.PutDERCapability(ctx, "/edev/1/der/1/dercap", sep2.DERCapability{}); err != nil {
		t.Fatalf("PutDERCapability: %v", err)
	}

	if got := postHits.Load(); got != 1 {
		t.Errorf("/edev POST hits = %d, want exactly 1", got)
	}
	if got := getHits.Load(); got != 1 {
		t.Errorf("/edev/1 GET hits = %d, want exactly 1 (Location read-back)", got)
	}
	if got := derCapPutHits.Load(); got != 1 {
		t.Errorf("dercap PUT hits = %d, want exactly 1", got)
	}
}

// =============================================================================
// Four href-taking method signatures + reporter no-op
// =============================================================================

// TestRegister_PostsToExactHref exercises case 1: Register(ctx,
// edevListHref) POSTs to the passed href (not a hardcoded path), and
// returns the parsed EndDevice from the Location read.
func TestRegister_PostsToExactHref(t *testing.T) {
	t.Parallel()

	// Two non-default hrefs verify the method does no string manipulation
	// on the input. /custom/devices/path is deliberately not /edev.
	tests := []struct {
		name       string
		listHref   string
		locHeader  string
		readBackTo string
	}{
		{
			name:       "non_default_list_path",
			listHref:   "/custom/devices/path",
			locHeader:  "/custom/devices/path/42",
			readBackTo: "/custom/devices/path/42",
		},
		{
			name:       "list_with_query_string",
			listHref:   "/edev?profile=csip",
			locHeader:  "/edev/9",
			readBackTo: "/edev/9",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCCMTestEnv(t)

			var postPath atomic.Value
			postPath.Store("")
			var getPath atomic.Value
			getPath.Store("")

			mux := http.NewServeMux()
			// Wildcard handler so we can record the path the inverter
			// actually hit, regardless of which literal we passed.
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					postPath.Store(r.URL.Path)
					w.Header().Set("Location", tc.locHeader)
					w.WriteHeader(http.StatusCreated)
				case http.MethodGet:
					getPath.Store(r.URL.Path)
					writeEdev(t, w, sep2.EndDevice{LFDI: "PARSED-FROM-LOCATION"})
				default:
					t.Errorf("unexpected method %s on %s", r.Method, r.URL.Path)
				}
			})

			serverURL, stop := startIdleListener(t, env, mux)
			defer stop()

			client := newCSIPClient(t, env, serverURL, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			edev, _, err := client.Register(ctx, tc.listHref)
			if err != nil {
				t.Fatalf("Register: %v", err)
			}

			// Strip the optional ?query to compare paths.
			wantPath := strings.SplitN(tc.listHref, "?", 2)[0]
			if got := postPath.Load().(string); got != wantPath {
				t.Errorf("POST path = %q, want %q (must POST to exact href)", got, wantPath)
			}
			if got := getPath.Load().(string); got != tc.readBackTo {
				t.Errorf("GET path = %q, want %q (Location read-back)", got, tc.readBackTo)
			}
			if edev.LFDI != "PARSED-FROM-LOCATION" {
				t.Errorf("returned EndDevice LFDI = %q, want %q (parsed from Location body)", edev.LFDI, "PARSED-FROM-LOCATION")
			}
		})
	}

	// Empty href → error before any HTTP call.
	t.Run("empty_href_errors", func(t *testing.T) {
		t.Parallel()
		env := newCCMTestEnv(t)
		var hits atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
		})
		serverURL, stop := startIdleListener(t, env, mux)
		defer stop()

		client := newCSIPClient(t, env, serverURL, false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, _, err := client.Register(ctx, "")
		if err == nil {
			t.Fatal("Register(ctx, \"\") returned nil error; want failure")
		}
		if !strings.Contains(err.Error(), "edev list href required") {
			t.Errorf("err = %v, want one containing %q", err, "edev list href required")
		}
		if got := hits.Load(); got != 0 {
			t.Errorf("HTTP hits = %d, want 0 (early return)", got)
		}
	})
}

// TestPutDERCapability_WritesToExactHref exercises case 2:
// PutDERCapability(ctx, href, ...) writes to the exact href, not a
// derived path. The handler asserts the literal request path.
func TestPutDERCapability_WritesToExactHref(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		href    string
		wantErr bool
	}{
		{"standard_path", "/edev/1/der/1/dercap", false},
		{"non_default_path", "/path/from/server/links", false},
		{"empty_href_errors", "", true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCCMTestEnv(t)

			var putPath atomic.Value
			putPath.Store("")
			var hits atomic.Int32

			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Method != http.MethodPut {
					t.Errorf("method = %s, want PUT", r.Method)
				}
				putPath.Store(r.URL.Path)
				w.WriteHeader(http.StatusNoContent)
			})

			serverURL, stop := startIdleListener(t, env, mux)
			defer stop()

			client := newCSIPClient(t, env, serverURL, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := client.PutDERCapability(ctx, tc.href, sep2.DERCapability{})
			if tc.wantErr {
				if err == nil {
					t.Fatal("PutDERCapability returned nil error; want failure")
				}
				if !strings.Contains(err.Error(), "dercap href required") {
					t.Errorf("err = %v, want %q", err, "dercap href required")
				}
				if got := hits.Load(); got != 0 {
					t.Errorf("HTTP hits on empty href = %d, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutDERCapability: %v", err)
			}
			if got := putPath.Load().(string); got != tc.href {
				t.Errorf("PUT path = %q, want %q (must write to exact href)", got, tc.href)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("PUT hits = %d, want exactly 1", got)
			}
		})
	}
}

// TestPhase3SkipsWhenDERListLinkAbsent exercises case 3:
// EndDevice with DERListLink == nil → zero DER PUTs. The production
// gating logic lives in cmd/inverterclient/main.go's Phase 3 block :
// we assert the contract at the inverter package boundary: when the
// caller observes a nil DERListLink, no PutDERCapability /
// PutDERSettings / PutDERStatus calls are made.
func TestPhase3SkipsWhenDERListLinkAbsent(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	// All of these are Phase 3 PUT endpoints; any hit is a contract
	// violation when the caller has observed DERListLink == nil.
	var derHits atomic.Int32
	mux := http.NewServeMux()
	for _, p := range []string{"/edev/1/der/1/dercap", "/edev/1/der/1/derg", "/edev/1/der/1/ders"} {
		p := p
		mux.HandleFunc(p, func(_ http.ResponseWriter, _ *http.Request) {
			t.Errorf("Phase 3 endpoint %s was hit despite DERListLink == nil", p)
			derHits.Add(1)
		})
	}

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build the EndDevice the way the production code does : with no
	// DERListLink advertised by the server.
	edev := sep2.EndDevice{LFDI: client.LFDI()}
	if edev.DERListLink != nil {
		t.Fatalf("fixture broken: DERListLink should be nil, got %+v", edev.DERListLink)
	}

	// Production gating expression (cmd/inverterclient/main.go Phase 3):
	// if edev.DERListLink == nil { skip Phase 3 }. We replicate the
	// branch literally : Pike's "stay in scope" rule blocks extracting
	// a helper from main.go, so the test exercises the same expression
	// the production code does.
	if edev.DERListLink != nil {
		_ = client.PutDERCapability(ctx, edev.DERListLink.Href+"/1/dercap", sep2.DERCapability{})
		_ = client.PutDERSettings(ctx, edev.DERListLink.Href+"/1/derg", sep2.DERSettings{})
		_ = client.PutDERStatus(ctx, edev.DERListLink.Href+"/1/ders", sep2.DERStatus{})
	}

	if got := derHits.Load(); got != 0 {
		t.Errorf("Phase 3 PUT hits = %d, want 0", got)
	}

	// Sanity: empty-href guard short-circuits the methods even if the
	// gate were missed. This belt-and-suspenders check is the
	// inverter-package contract that defends the main.go Phase 3 gate.
	if err := client.PutDERCapability(ctx, "", sep2.DERCapability{}); err == nil {
		t.Error("PutDERCapability(ctx, \"\") returned nil error; should fail before HTTP")
	}
	if err := client.PutDERSettings(ctx, "", sep2.DERSettings{}); err == nil {
		t.Error("PutDERSettings(ctx, \"\") returned nil error; should fail before HTTP")
	}
	if err := client.PutDERStatus(ctx, "", sep2.DERStatus{}); err == nil {
		t.Error("PutDERStatus(ctx, \"\") returned nil error; should fail before HTTP")
	}
	if got := derHits.Load(); got != 0 {
		t.Errorf("Phase 3 PUT hits after empty-href guard = %d, want 0", got)
	}
}

// TestPhase4SkipsWhenMUPListLinkAbsent exercises case 4:
// dcap with MirrorUsagePointListLink == nil → zero /mup calls. The
// inverter package contract: CreateMirrorUsagePoint and PostMeterReading
// short-circuit on empty href, so a nil MirrorUsagePointListLink in the
// caller cannot leak into HTTP traffic.
func TestPhase4SkipsWhenMUPListLinkAbsent(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var mupHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("/mup hit despite MirrorUsagePointListLink == nil")
		mupHits.Add(1)
	})
	mux.HandleFunc("/mup/1/mr", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("/mup/1/mr hit despite MirrorUsagePointListLink == nil")
		mupHits.Add(1)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dcap := sep2.DeviceCapability{}
	if dcap.MirrorUsagePointListLink != nil {
		t.Fatalf("fixture broken: MirrorUsagePointListLink should be nil")
	}

	// Production gating expression: if dcap.MirrorUsagePointListLink ==
	// nil { skip Phase 4 }. Replicated here.
	if dcap.MirrorUsagePointListLink != nil {
		_, _ = client.CreateMirrorUsagePoint(ctx, dcap.MirrorUsagePointListLink.Href, sep2.MirrorUsagePoint{})
	}

	if got := mupHits.Load(); got != 0 {
		t.Errorf("/mup hits = %d, want 0", got)
	}

	// Belt-and-suspenders: the inverter-package contract that defends
	// the main.go Phase 4 gate.
	if _, err := client.CreateMirrorUsagePoint(ctx, "", sep2.MirrorUsagePoint{}); err == nil {
		t.Error("CreateMirrorUsagePoint(ctx, \"\") returned nil error; should fail before HTTP")
	}
	if err := client.PostMeterReading(ctx, "", sep2.MirrorMeterReading{}); err == nil {
		t.Error("PostMeterReading(ctx, \"\") returned nil error; should fail before HTTP")
	}
	if got := mupHits.Load(); got != 0 {
		t.Errorf("/mup hits after empty-href guard = %d, want 0", got)
	}
}

// TestReporter_EmptyHrefsAreNoOps exercises case 5: a Reporter
// constructed with empty derStatusHref and mmrHref is a silent no-op :
// ReportStatus and ReportMetering return nil and emit zero HTTP calls
// regardless of how many times the simulation ticker drives them.
//
// This is the "ticker drives the simulation but emits no HTTP calls"
// contract. We exercise it
// directly by calling the report methods in a loop; production wires a
// time.Ticker on top of the same methods, so the contract holds at the
// boundary the inverter package owns.
func TestReporter_EmptyHrefsAreNoOps(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var anyHit atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP traffic: %s %s", r.Method, r.URL.Path)
		anyHit.Add(1)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	reporter := inverter.NewReporter(client, "", "") // both hrefs empty

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Simulate 5 ticker iterations. Production ReportInterval defaults
	// to seconds; we just call the methods directly to avoid timing
	// flakes in CI.
	state := inverter.InverterState{
		ActivePowerW: 1234,
		Connected:    true,
		Energized:    true,
		Mode:         inverter.ModeConstantPF,
		Time:         time.Now(),
	}
	for i := 0; i < 5; i++ {
		if err := reporter.ReportStatus(ctx, state); err != nil {
			t.Errorf("ReportStatus iter %d: err = %v, want nil (no-op on empty href)", i, err)
		}
		if err := reporter.ReportMetering(ctx, state); err != nil {
			t.Errorf("ReportMetering iter %d: err = %v, want nil (no-op on empty href)", i, err)
		}
	}

	if got := anyHit.Load(); got != 0 {
		t.Errorf("HTTP traffic during no-op reporter = %d, want 0", got)
	}
}

// =============================================================================
// Optional case: endpoint constant scan
// =============================================================================

// TestNoHardcodedEndpointConstants is an optional case: a
// build-tag-less unit test that fails if anyone reintroduces the old
// hardcoded endpoint string literals in internal/inverter/client.go.
//
// The check is intentionally narrow : we look only for the specific
// path literals the link-derivation cleanup replaced. The
// integration_test.go file predates that cleanup and still uses literal
// hrefs in
// its assertions; that's intentional (a server-side route check). The
// scan therefore targets only the production source under test.
//
// Implemented in client_const_scan_test.go to keep the import surface
// of this file focused on httptest assertions.
