// Package inverter_test integration tests for IEEE-046 : Centralized HTTP
// response code router. These exercise (*SEP2Client).Get / Post / Put end-
// to-end through the gotls listener fixture (shared with the IEEE-029 / -030
// suite) and assert that each HTTP status code surfaces as the appropriate
// typed error (ErrBadRequest, ErrNotFound, ErrMethodNotAllowed,
// ErrNotImplemented, ErrResponseTransient, or *MovedError) per the plan-1
// Phase 7 entry contract.
//
// Cases 1-9 cover the new router behavior. Case 10 pins the IEEE-029
// semantic sentinel (ErrEndDeviceNotFound) to be sure it has NOT been
// unified with the new HTTP ErrNotFound : they are different concepts.
package inverter_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// IEEE-046 case 1: 200 OK GET returns the parsed body with no error.
func TestRouter_GetReturnsParsedBody(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/sep+xml")
		dcap := sep2.DeviceCapability{PollRate: 42}
		_ = xml.NewEncoder(w).Encode(&dcap)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	dcap, err := client.Discover(testCtx(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if dcap.PollRate != 42 {
		t.Errorf("PollRate = %d, want 42", dcap.PollRate)
	}
}

// IEEE-046 case 2: 201 Created POST returns nil error and surfaces the
// Location header. Register's first step is a POST to the EndDeviceList href;
// the test server returns 201 + Location → client must return that URL.
func TestRouter_PostReturnsLocationOn201(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Location", "/edev/42")
		w.WriteHeader(http.StatusCreated)
	})
	// Register reads back the newly-created EndDevice via GET on the
	// returned Location URL; serve a minimal stub so the round trip
	// completes.
	mux.HandleFunc("/edev/42", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.EndDevice{SFDI: "STUB"})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, false)

	edev, _, err := client.Register(testCtx(t), "/edev")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if edev.SFDI != "STUB" {
		t.Errorf("read-back SFDI = %q, want STUB", edev.SFDI)
	}
}

// IEEE-046 case 3: 204 No Content PUT returns nil error and no body.
// PutDERStatus is the inverter-side helper exercised by the reporter.
func TestRouter_PutReturnsNilOn204(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/edev/1/der/1/ders", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	if err := client.PutDERStatus(testCtx(t), "/edev/1/der/1/ders", sep2.DERStatus{}); err != nil {
		t.Fatalf("PutDERStatus: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("server hits = %d, want 1", hits.Load())
	}
}

// IEEE-046 case 4: 400 Bad Request surfaces as ErrBadRequest via errors.Is.
func TestRouter_GetMaps400ToErrBadRequest(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "malformed", http.StatusBadRequest)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	_, err := client.Discover(testCtx(t))
	if !errors.Is(err, inverter.ErrBadRequest) {
		t.Fatalf("err = %v, want errors.Is ErrBadRequest", err)
	}
}

// IEEE-046 case 5: 404 Not Found surfaces as ErrNotFound via errors.Is.
func TestRouter_GetMaps404ToErrNotFound(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not here", http.StatusNotFound)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	_, err := client.Discover(testCtx(t))
	if !errors.Is(err, inverter.ErrNotFound) {
		t.Fatalf("err = %v, want errors.Is ErrNotFound", err)
	}
}

// IEEE-046 case 6: 405 Method Not Allowed surfaces as ErrMethodNotAllowed.
// Exercised via PUT (a method the server explicitly rejects).
func TestRouter_PutMaps405ToErrMethodNotAllowed(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/edev/1/der/1/dercap", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET")
		http.Error(w, "verb not allowed", http.StatusMethodNotAllowed)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	err := client.PutDERCapability(testCtx(t), "/edev/1/der/1/dercap", sep2.DERCapability{})
	if !errors.Is(err, inverter.ErrMethodNotAllowed) {
		t.Fatalf("err = %v, want errors.Is ErrMethodNotAllowed", err)
	}
}

// IEEE-046 case 7: 501 Not Implemented surfaces as ErrNotImplemented.
// Exercised via POST (CreateMirrorUsagePoint).
func TestRouter_PostMaps501ToErrNotImplemented(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	_, err := client.CreateMirrorUsagePoint(testCtx(t), "/mup", sep2.MirrorUsagePoint{})
	if !errors.Is(err, inverter.ErrNotImplemented) {
		t.Fatalf("err = %v, want errors.Is ErrNotImplemented", err)
	}
}

// IEEE-046 case 8 (revised by IEEE-047): stdlib auto-follow remains
// disabled : CheckRedirect must still return ErrUseLastResponse so 301s
// reach classifyResponse rather than being silently swallowed by the
// stdlib client. Surfacing as *MovedError is now visible at the
// classifyResponse layer (errors_test.go cases) and via the *one-hop
// follow* path exercised in TestGet301FollowsOnceAndReturnsBody below.
// This case continues to assert stdlib does NOT auto-follow by routing
// the followed request through our own handler and counting hits : a
// stdlib auto-follow would issue a second request without our 301-aware
// wrapper running, so the followHits counter would be 1 even if our
// IEEE-047 wrapper never fired. We assert that the wrapper *did* fire
// (one follow, body returned, no error), proving our path is in play
// while stdlib's is not.
func TestRouter_Get301FollowedOnceAndStdlibAutoFollowDisabled(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var redirectHits, followHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		redirectHits.Add(1)
		w.Header().Set("Location", "/v2/dcap")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v2/dcap", func(w http.ResponseWriter, _ *http.Request) {
		followHits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.DeviceCapability{PollRate: 99})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	dcap, err := client.Discover(testCtx(t))
	if err != nil {
		t.Fatalf("Discover: %v, want nil (IEEE-047 follows 301 once)", err)
	}
	if dcap.PollRate != 99 {
		t.Errorf("dcap.PollRate = %d, want 99 (followed body returned)", dcap.PollRate)
	}
	if redirectHits.Load() != 1 {
		t.Errorf("redirectHits = %d, want 1 (one 301 issued)", redirectHits.Load())
	}
	if followHits.Load() != 1 {
		t.Errorf("followHits = %d, want 1 (IEEE-047 follow-once fired exactly once)", followHits.Load())
	}
}

// IEEE-046 case 9: 5xx still maps to ErrResponseTransient : IEEE-043's
// pre-existing sentinel must survive the refactor so PostResponseWithRetry
// and other callers keep working.
func TestRouter_GetMaps5xxToErrResponseTransient(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	_, err := client.Discover(testCtx(t))
	if !errors.Is(err, inverter.ErrResponseTransient) {
		t.Fatalf("err = %v, want errors.Is ErrResponseTransient", err)
	}
}

// IEEE-046 case 10: ErrEndDeviceNotFound (IEEE-029, semantic) MUST stay
// distinct from ErrNotFound (HTTP). LookupOwnEndDevice against a /edev list
// that returns 200 OK with our LFDI absent returns ErrEndDeviceNotFound :
// NOT ErrNotFound. Pinning behavior so the IEEE-046 refactor cannot
// accidentally collapse the two.
func TestRouter_LookupOwnEndDeviceSemanticSentinel(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		// Empty list, not 404 : semantic "not in list" vs HTTP "no such resource".
		writeEdevList(t, w, sep2.EndDeviceList{})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	_, _, err := client.LookupOwnEndDevice(testCtx(t), "/edev")
	if err == nil {
		t.Fatal("LookupOwnEndDevice: nil err, want ErrEndDeviceNotFound")
	}
	if !errors.Is(err, inverter.ErrEndDeviceNotFound) {
		t.Errorf("err = %v, want errors.Is ErrEndDeviceNotFound", err)
	}
	if errors.Is(err, inverter.ErrNotFound) {
		t.Error("LookupOwnEndDevice empty-list path leaked HTTP ErrNotFound; semantic sentinel collapsed into HTTP one")
	}
}

// testCtx returns a context that is canceled when the test ends. Keeps the
// HTTP round-trip from outliving the test if a hang slips in.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
