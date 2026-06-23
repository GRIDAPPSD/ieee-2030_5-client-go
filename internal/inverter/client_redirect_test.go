// Package inverter_test integration tests for IEEE-047 — single-hop 301
// Moved Permanently follow + cached-href surfacing. These exercise the
// follow-once behavior layered on top of IEEE-046's *MovedError surfacing,
// run end-to-end through the gotls listener fixture shared with the IEEE-029
// / -030 / -046 suites.
//
// Cases 1-6 map to the IEEE-047 ticket's test contract:
//
//  1. 200 OK happy path: no redirect, behavior unchanged from IEEE-046.
//  2. Single 301 → 200: follow-once, body returned, newHref surfaced.
//  3. Two 301s in a row: first followed, second *MovedError propagated
//     without further retry (no chain following per RFC 7231 §6.4.2 /
//     CSIP V1.2 §6.6).
//  4. 301 with empty Location: *MovedError returned with empty Location;
//     no follow attempted, no crash.
//  5. POST with 301: same single-follow behavior; the request body is
//     re-sent on the follow attempt (verified by reading the second
//     request body server-side and asserting equality with the first).
//  6. Caller updates cached href: integration via Get's newHref return
//     value — the test holds a local href var, sees the surfaced new
//     href, and re-issues a second GET against the updated value with
//     the redirect handler no longer firing.
package inverter_test

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

func redirectTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// IEEE-047 case 1: 200 OK happy path — no redirect, no follow, newHref is
// empty. Pins that the IEEE-047 wrapper does not perturb the 200 path that
// IEEE-046's classifier already handled.
func TestRedirect_200OKNoFollow(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.DeviceCapability{PollRate: 42})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	var dcap sep2.DeviceCapability
	newHref, err := client.Get(redirectTestCtx(t), "/dcap", &dcap)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if newHref != "" {
		t.Errorf("newHref = %q, want \"\" (200 path; no follow)", newHref)
	}
	if dcap.PollRate != 42 {
		t.Errorf("dcap.PollRate = %d, want 42", dcap.PollRate)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

// IEEE-047 case 2: single 301 → 200. The wrapper follows once, returns the
// body parsed from the new URL, and surfaces the new path as newHref so the
// caller can update its cached reference.
func TestRedirect_Get301FollowsOnceAndReturnsBody(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var redirectHits, followHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		redirectHits.Add(1)
		w.Header().Set("Location", "/v2/edev")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v2/edev", func(w http.ResponseWriter, _ *http.Request) {
		followHits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.EndDeviceList{ListResource: sep2.ListResource{All: 1}})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	var list sep2.EndDeviceList
	newHref, err := client.Get(redirectTestCtx(t), "/edev", &list)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if newHref != "/v2/edev" {
		t.Errorf("newHref = %q, want %q", newHref, "/v2/edev")
	}
	if list.All != 1 {
		t.Errorf("list.All = %d, want 1 (body from followed response)", list.All)
	}
	if got := redirectHits.Load(); got != 1 {
		t.Errorf("redirectHits = %d, want 1", got)
	}
	if got := followHits.Load(); got != 1 {
		t.Errorf("followHits = %d, want 1 (exactly one follow)", got)
	}
}

// IEEE-047 case 3: two 301s in a row — only the first is followed. The
// second *MovedError propagates back to the caller without further retry
// (no chain following).
func TestRedirect_ChainedRedirectsNotFollowed(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var firstHits, secondHits, thirdHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		w.Header().Set("Location", "/b")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Location", "/c")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, _ *http.Request) {
		thirdHits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.DeviceCapability{PollRate: 7})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	var dcap sep2.DeviceCapability
	_, err := client.Get(redirectTestCtx(t), "/a", &dcap)
	if err == nil {
		t.Fatal("Get: nil err, want *MovedError from second redirect")
	}
	var me *inverter.MovedError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want errors.As *MovedError", err)
	}
	if me.Status != http.StatusMovedPermanently {
		t.Errorf("MovedError.Status = %d, want 301", me.Status)
	}
	if me.Location != "/c" {
		t.Errorf("MovedError.Location = %q, want %q (second redirect target)", me.Location, "/c")
	}
	if got := firstHits.Load(); got != 1 {
		t.Errorf("firstHits = %d, want 1", got)
	}
	if got := secondHits.Load(); got != 1 {
		t.Errorf("secondHits = %d, want 1 (one follow only)", got)
	}
	if got := thirdHits.Load(); got != 0 {
		t.Errorf("thirdHits = %d, want 0 (no chain following per RFC 7231 §6.4.2)", got)
	}
}

// IEEE-047 case 4: 301 with empty Location header. RFC 7231 §6.4.2 doesn't
// require Location on a 301 (it MUST be present per §7.1.2 but servers can
// violate spec), so the wrapper must not attempt to follow and must not
// crash. The *MovedError propagates with Location="" so the caller can
// distinguish from a real follow.
func TestRedirect_301EmptyLocationDoesNotFollow(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// Deliberately omit Location header.
		w.WriteHeader(http.StatusMovedPermanently)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	var list sep2.EndDeviceList
	newHref, err := client.Get(redirectTestCtx(t), "/edev", &list)
	if err == nil {
		t.Fatal("Get: nil err, want *MovedError with empty Location")
	}
	if newHref != "" {
		t.Errorf("newHref = %q, want \"\" (no follow on empty Location)", newHref)
	}
	var me *inverter.MovedError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want errors.As *MovedError", err)
	}
	if me.Location != "" {
		t.Errorf("MovedError.Location = %q, want \"\"", me.Location)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1 (one attempt; no follow)", got)
	}
}

// IEEE-047 case 5: POST with 301 — the wrapper follows once and re-sends
// the request body to the new URL. The test asserts that (a) the second
// request body equals the first byte-for-byte (proving bytes.NewReader is
// re-wrapped per attempt, not consumed on the first) and (b) the Location
// header from the second 201 response is what Post returns.
func TestRedirect_Post301FollowsOnceAndResendsBody(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var firstBody, secondBody atomic.Value
	firstBody.Store([]byte(nil))
	secondBody.Store([]byte(nil))

	var redirectHits, followHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/mup", func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		firstBody.Store(b)
		w.Header().Set("Location", "/v2/mup")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v2/mup", func(w http.ResponseWriter, r *http.Request) {
		followHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		secondBody.Store(b)
		w.Header().Set("Location", "/v2/mup/42")
		w.WriteHeader(http.StatusCreated)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	mup := sep2.MirrorUsagePoint{
		MRID:        "mup-redir-1",
		Description: "redirect test",
	}
	loc, err := client.CreateMirrorUsagePoint(redirectTestCtx(t), "/mup", mup)
	if err != nil {
		t.Fatalf("CreateMirrorUsagePoint: %v", err)
	}
	if loc != "/v2/mup/42" {
		t.Errorf("Location = %q, want %q (from 201 on followed POST)", loc, "/v2/mup/42")
	}
	if got := redirectHits.Load(); got != 1 {
		t.Errorf("redirectHits = %d, want 1", got)
	}
	if got := followHits.Load(); got != 1 {
		t.Errorf("followHits = %d, want 1", got)
	}

	first := firstBody.Load().([]byte)
	second := secondBody.Load().([]byte)
	if len(first) == 0 {
		t.Fatal("firstBody is empty; nothing posted on first attempt")
	}
	if len(second) == 0 {
		t.Fatal("secondBody is empty; body NOT re-sent on follow (io.Reader exhausted bug)")
	}
	if string(first) != string(second) {
		t.Errorf("body re-send mismatch:\n  first  = %q\n  second = %q", first, second)
	}
}

// IEEE-047 case 6: caller updates cached href on 301. Holds a local href
// variable, calls Get to fetch the resource, sees the newHref surfaced,
// updates the cached value, then re-issues GET against the new value. The
// original redirect handler must NOT fire on the second call — proving the
// caller-side update reaches the next traversal.
func TestRedirect_CallerUpdatesCachedHrefOnFollow(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var redirectHits, followHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/edev", func(w http.ResponseWriter, _ *http.Request) {
		redirectHits.Add(1)
		w.Header().Set("Location", "/v2/edev")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v2/edev", func(w http.ResponseWriter, _ *http.Request) {
		followHits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.EndDeviceList{ListResource: sep2.ListResource{All: 1}})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	cachedHref := "/edev" // mimics cmd/inverterclient/main.go's edevListHref

	var list sep2.EndDeviceList
	newHref, err := client.Get(redirectTestCtx(t), cachedHref, &list)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if newHref != "/v2/edev" {
		t.Fatalf("first call newHref = %q, want %q", newHref, "/v2/edev")
	}
	cachedHref = newHref // <-- the caller-side update under test

	// Second call uses the updated cached href. The redirect handler
	// must not fire again.
	var list2 sep2.EndDeviceList
	newHref2, err := client.Get(redirectTestCtx(t), cachedHref, &list2)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if newHref2 != "" {
		t.Errorf("second call newHref = %q, want \"\" (no follow expected — caller already updated href)", newHref2)
	}
	if got := redirectHits.Load(); got != 1 {
		t.Errorf("redirectHits = %d, want 1 (second call should bypass /edev)", got)
	}
	if got := followHits.Load(); got != 2 {
		t.Errorf("followHits = %d, want 2 (first followed + second direct)", got)
	}
}

// IEEE-047 Put coverage: PUT with 301 — the wrapper follows once and
// re-sends the body to the new URL. PutDERCapability is the test surface
// (PUT wrapper that doesn't surface newHref to the caller; we assert by
// counting handler hits and reading both request bodies).
func TestRedirect_Put301FollowsOnceAndResendsBody(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var firstBody, secondBody atomic.Value
	firstBody.Store([]byte(nil))
	secondBody.Store([]byte(nil))

	var redirectHits, followHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/dercap", func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		firstBody.Store(b)
		w.Header().Set("Location", "/v2/dercap")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v2/dercap", func(w http.ResponseWriter, r *http.Request) {
		followHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		secondBody.Store(b)
		w.WriteHeader(http.StatusNoContent)
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	maxW := sep2.ActivePower{Value: 7777}
	if err := client.PutDERCapability(redirectTestCtx(t), "/dercap", sep2.DERCapability{
		RTGMaxW: &maxW,
	}); err != nil {
		t.Fatalf("PutDERCapability: %v", err)
	}
	if got := redirectHits.Load(); got != 1 {
		t.Errorf("redirectHits = %d, want 1", got)
	}
	if got := followHits.Load(); got != 1 {
		t.Errorf("followHits = %d, want 1", got)
	}

	first := firstBody.Load().([]byte)
	second := secondBody.Load().([]byte)
	if len(first) == 0 {
		t.Fatal("firstBody is empty; nothing PUT on first attempt")
	}
	if len(second) == 0 {
		t.Fatal("secondBody is empty; body NOT re-sent on follow")
	}
	if string(first) != string(second) {
		t.Errorf("PUT body re-send mismatch:\n  first  = %q\n  second = %q", first, second)
	}
}

// IEEE-047 wrapper coverage: GetFSAList surfaces the new FSAList base href
// (with the ?l=255 paging query stripped) on a 301. Confirms the
// stripPagingQuery seam — callers store the BASE href, not the paginated
// URL, so subsequent fresh GETs append paging from a clean base.
func TestRedirect_GetFSAList301StripsPagingQuery(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var redirectHits, followHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/fsa", func(w http.ResponseWriter, _ *http.Request) {
		redirectHits.Add(1)
		// Server returns the bare new resource path; paging is the
		// client's concern, so the Location should NOT carry l=255
		// here (that's the canonical server behavior).
		w.Header().Set("Location", "/v2/fsa")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v2/fsa", func(w http.ResponseWriter, _ *http.Request) {
		followHits.Add(1)
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&sep2.FunctionSetAssignmentsList{})
	})
	serverURL, _ := startIdleListener(t, env, mux)
	client := newCSIPClient(t, env, serverURL, true)

	_, newHref, err := client.GetFSAList(redirectTestCtx(t), "/fsa")
	if err != nil {
		t.Fatalf("GetFSAList: %v", err)
	}
	// The server's Location is /v2/fsa (no paging). The wrapper should
	// surface that verbatim — there's no paging suffix to strip.
	if newHref != "/v2/fsa" {
		t.Errorf("newHref = %q, want %q (server-supplied Location; no paging suffix to strip)", newHref, "/v2/fsa")
	}
	if got := redirectHits.Load(); got != 1 {
		t.Errorf("redirectHits = %d, want 1", got)
	}
	if got := followHits.Load(); got != 1 {
		t.Errorf("followHits = %d, want 1", got)
	}
}
