package inverter_test

// Package inverter_test covers
// (*SEP2Client).GetRegistration. This behavior is frozen.
//
// Reuses the ccmTestEnv from client_ccm_test.go
// and startIdleListener from idle_test.go. The test does not need TLS-cipher
// negotiation coverage (GRIDAPPSD/ieee-2030_5-server-go#62 owns that); it
// needs Registration XML parsing, empty-href sentinel error, 404 /
// malformed-XML wrapping.
//
// Cases covered:
//
//  1. TestGetRegistration_HappyPath           : stub returns
//                                                Registration with PIN=111115;
//                                                method returns the parsed
//                                                value.
//  2. TestGetRegistration_EmptyHref           : empty href
//                                                returns error matching
//                                                "registration href required",
//                                                no GET attempted.
//  3. TestGetRegistration_NotFound            : server 404
//                                                produces a wrapped error
//                                                (path + status surfaced),
//                                                no panic.
//  4. TestGetRegistration_MalformedXML        : server emits
//                                                non-XML body, GetRegistration
//                                                returns an unwrappable
//                                                xml.SyntaxError (via %w from
//                                                c.Get's `unmarshal %s: %w`).
//
// Phase 2b log-line / skip behavior (redacted-log + missing-RegistrationLink
// idle/bypass semantics) lives inside `cmd/inverterclient/phase2b_test.go`,
// which drives runPhase2bRegistration directly.

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

// registrationHref is the canonical fixture path for the Registration
// resource. CSIP V1.2 BASIC-001 step 5 binds Registration off
// EndDevice.RegistrationLink; the literal path is server-chosen : `/edev/1/rg`
// matches the convention used elsewhere in the inverter handler tree.
const registrationHref = "/edev/1/rg"

// writeRegistration encodes a Registration to the response writer with the
// SEP+XML content type. Mirrors writeDcap from idle_test.go.
func writeRegistration(t *testing.T, w http.ResponseWriter, rg sep2.Registration) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	if err := xml.NewEncoder(w).Encode(&rg); err != nil {
		t.Errorf("encode registration: %v", err)
	}
}

// newRegistrationTestClient is a small helper that boots a gotls server with
// the supplied handler, builds a production SEP2 client against it, and
// returns both for the caller to drive. Cleanup is wired through t.Cleanup
// via startIdleListener (server) and the client (no explicit Close needed :
// it shares the test's CA pool).
func newRegistrationTestClient(t *testing.T, handler http.Handler) (*inverter.SEP2Client, context.Context) {
	t.Helper()
	env := newCCMTestEnv(t)
	serverURL, _ := startIdleListener(t, env, handler)
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
	t.Cleanup(cancel)
	return client, ctx
}

// TestGetRegistration_HappyPath : case 1.
//
// Stub returns Registration with PIN=111115 (the CSIP V1.2 BASIC-001 step 5
// reference value), DateTimeRegistered, and a PollRate attribute.
// GetRegistration must return all three fields parsed.
func TestGetRegistration_HappyPath(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	const wantPIN uint32 = 111115
	const wantDTR int64 = 1715539200
	const wantPollRate uint32 = 300

	mux := http.NewServeMux()
	mux.HandleFunc(registrationHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeRegistration(t, w, sep2.Registration{
			DateTimeRegistered: wantDTR,
			PIN:                wantPIN,
			PollRate:           wantPollRate,
		})
	})

	client, ctx := newRegistrationTestClient(t, mux)
	rg, err := client.GetRegistration(ctx, registrationHref)
	if err != nil {
		t.Fatalf("GetRegistration: %v", err)
	}
	if rg.PIN != wantPIN {
		t.Errorf("PIN = %d, want %d", rg.PIN, wantPIN)
	}
	if rg.DateTimeRegistered != wantDTR {
		t.Errorf("DateTimeRegistered = %d, want %d", rg.DateTimeRegistered, wantDTR)
	}
	if rg.PollRate != wantPollRate {
		t.Errorf("PollRate = %d, want %d", rg.PollRate, wantPollRate)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("registration GET hits = %d, want exactly 1", got)
	}
}

// TestGetRegistration_EmptyHref : case 2.
//
// An empty href is rejected at the call site without an HTTP round trip.
// The sentinel error string is contractual: the Phase 2b block in main()
// surfaces this exact message via `log.Fatalf("GET Registration: %v", err)`
// when RegistrationLink resolves to an empty href, which is currently the
// only way GetRegistration's empty-href guard fires in production. Future
// callers grepping this message must keep it intact.
func TestGetRegistration_EmptyHref(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	})

	client, ctx := newRegistrationTestClient(t, mux)
	_, err := client.GetRegistration(ctx, "")
	if err == nil {
		t.Fatal("GetRegistration(ctx, \"\") returned nil error; want sentinel")
	}
	if !strings.Contains(err.Error(), "registration href required") {
		t.Errorf("error %q does not contain %q", err.Error(), "registration href required")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("server hits = %d, want 0 (empty-href must short-circuit before any GET)", got)
	}
}

// TestGetRegistration_NotFound : case 3.
//
// Server returns 404 with a small body. GetRegistration must return a
// non-nil error that surfaces both the path and the status code (Pike's
// c.Get currently wraps via fmt.Errorf("GET %s: %d %s") : not a %w, so
// errors.Is on http.StatusNotFound is not the right assertion). What
// matters for the deferred-test contract is "wrapped error, no panic":
// the path is included AND no panic crashes the test binary.
func TestGetRegistration_NotFound(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "registration not found", http.StatusNotFound)
	})

	client, ctx := newRegistrationTestClient(t, mux)
	rg, err := client.GetRegistration(ctx, registrationHref)
	if err == nil {
		t.Fatalf("GetRegistration on 404 returned nil error; rg=%+v", rg)
	}
	if !strings.Contains(err.Error(), "GET registration") {
		t.Errorf("error %q missing outer wrap %q", err.Error(), "GET registration")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not surface status 404", err.Error())
	}
	if !strings.Contains(err.Error(), registrationHref) {
		t.Errorf("error %q does not surface path %q", err.Error(), registrationHref)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want exactly 1", got)
	}
}

// TestGetRegistration_MalformedXML : case 4.
//
// Server returns 200 OK with non-XML body. GetRegistration must wrap the
// xml.SyntaxError via the chain
//   - c.Get: fmt.Errorf("unmarshal %s: %w", path, err)
//   - GetRegistration: fmt.Errorf("GET registration: %w", err)
//
// so errors.As to *xml.SyntaxError succeeds and no panic crashes the test.
func TestGetRegistration_MalformedXML(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(registrationHref, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_, _ = w.Write([]byte("<<<not-xml<<<"))
	})

	client, ctx := newRegistrationTestClient(t, mux)
	rg, err := client.GetRegistration(ctx, registrationHref)
	if err == nil {
		t.Fatalf("GetRegistration on malformed XML returned nil error; rg=%+v", rg)
	}
	var syn *xml.SyntaxError
	if !errors.As(err, &syn) {
		t.Errorf("error %q does not unwrap to *xml.SyntaxError (chain: %T)", err.Error(), err)
	}
	if !strings.Contains(err.Error(), "GET registration") {
		t.Errorf("error %q missing outer GetRegistration wrap", err.Error())
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want exactly 1", got)
	}
}
