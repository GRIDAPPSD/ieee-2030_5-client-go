// Package inverter_test covers the
// (*SEP2Client).PostResponse method that wires the client side of the
// IEEE 2030.5 Response Function Set (CSIP V1.2 CORE-022):
//
//  1. 201 Created -> returns nil; XML body well-formed; Location captured.
//  2. 204 No Content -> returns nil.
//  3. Relative replyToHref resolves under c.baseURL.
//  4. Absolute replyToHref bypasses c.baseURL (no double-prefix).
//  5. 4xx -> wrapped error; response body NOT logged verbatim.
//  6. Transient 5xx + happy retry -> first attempt 500, second 201 -> nil.
//  7. Persistent 5xx -> both attempts 500 -> wrapped ErrResponseTransient.
//  8. Context cancellation mid-request -> ctx error propagates.
//  9. Content-Type header is `application/sep+xml` on the wire.
//
// Fixture pattern matches the rest of the inverter package: gotls-backed
// HTTPS listener via ccmTestEnv + startIdleListener, per-test mux, atomic
// hit counters, t.Parallel() where safe.
package inverter_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// captureLogsMu serializes concurrent captureLogs calls. The helper swaps
// log.Default()'s output writer, which is a single global slot : two
// t.Parallel() tests calling captureLogs would otherwise interleave their
// SetOutput calls AND their Printf writes, producing intermittent -race
// failures (Pike Y3 / Pike DD trail). Holding the mutex for the
// duration of fn keeps the global swap atomic from the test's perspective
// without changing the production log path or the inverter package API.
var captureLogsMu sync.Mutex

// sampleDERControlResponse returns a populated DERControlResponse the test
// server will see on the wire. Status=2 (Started) per IEEE 2030.5-2023
// §10.10 Table 31, with a Subject mRID
// that identifies which event the Response acknowledges.
func sampleDERControlResponse() sep2.DERControlResponse {
	status := sep2.ResponseStatusEventStarted
	return sep2.DERControlResponse{
		Response: sep2.Response{
			CreatedDateTime: 1736000000,
			EndDeviceLFDI:   "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF",
			Status:          &status,
			Subject:         "event-mrid-001",
		},
	}
}

// captureLogs swaps log.Default()'s output for the duration of fn so the
// test can assert what was (or was NOT) written. log.Printf is what the
// production PostResponse path uses for both the 201-Location trace and
// the 4xx redacted warning; capturing here is the only way to assert body
// redaction without rebuilding the logger seam.
//
// Contract: callers MUST be serial (no t.Parallel()).
// log.Default() is process-global : any other t.Parallel() test running
// concurrently with a captureLogs caller will log.Printf into the captured
// buffer (because we've SetOutput'd it) AND race buf.String(). Go's test
// runtime runs serial tests in a single goroutine BEFORE resuming queued
// parallel tests, so serial captureLogs callers complete with no
// concurrent log writers. The captureLogsMu mutex below is a secondary
// guard against future captureLogs-from-multiple-goroutines misuse; it is
// NOT sufficient on its own (see response_retry_test.go dead-letter
// comment).
//
// Refactoring production log.Printf to an injectable logger would lift
// this constraint but is out of scope here.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	captureLogsMu.Lock()
	defer captureLogsMu.Unlock()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

// readBodyBytes reads-and-restores r.Body so the handler can verify the
// XML payload it received without the production code path observing a
// drained body. The Body is re-buffered before the handler returns so any
// follow-on logic continues to see the original bytes.
func readBodyBytes(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

// =============================================================================
// Case 1: 201 Created happy path (XML body + Location header)
// =============================================================================

func TestPostResponse_201CreatedHappyPath(t *testing.T) {
	// captureLogs callers must be serial : log.Default() is
	// process-global. See response_post_test.go captureLogs godoc.
	env := newCCMTestEnv(t)

	var hits atomic.Int32
	var contentType atomic.Value
	contentType.Store("")
	var rawBody atomic.Value
	rawBody.Store([]byte(nil))

	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		contentType.Store(r.Header.Get("Content-Type"))
		rawBody.Store(readBodyBytes(t, r))
		w.Header().Set("Location", "/rsps/1/rsp/42")
		w.WriteHeader(http.StatusCreated)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logs := captureLogs(t, func() {
		if err := client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse()); err != nil {
			t.Fatalf("PostResponse: %v", err)
		}
	})

	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want exactly 1", got)
	}
	if got := contentType.Load().(string); got != "application/sep+xml" {
		t.Errorf("Content-Type = %q, want application/sep+xml", got)
	}
	body := rawBody.Load().([]byte)
	var decoded sep2.DERControlResponse
	if err := xml.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode posted body: %v\nbody=%s", err, string(body))
	}
	if decoded.Subject != "event-mrid-001" {
		t.Errorf("decoded Subject = %q, want event-mrid-001", decoded.Subject)
	}
	if decoded.Status == nil || *decoded.Status != sep2.ResponseStatusEventStarted {
		t.Errorf("decoded Status = %v, want Started(2)", decoded.Status)
	}
	if !strings.Contains(logs, "/rsps/1/rsp/42") {
		t.Errorf("expected Location header trace in log; got: %s", logs)
	}
}

// =============================================================================
// Case 2: 204 No Content happy path
// =============================================================================

func TestPostResponse_204NoContentHappyPath(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse()); err != nil {
		t.Fatalf("PostResponse(204): %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want exactly 1", got)
	}
}

// =============================================================================
// Case 3: relative replyToHref resolves under c.baseURL
// =============================================================================

func TestPostResponse_RelativeHrefResolvesAgainstBaseURL(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var seenPath atomic.Value
	seenPath.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, r *http.Request) {
		seenPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse()); err != nil {
		t.Fatalf("PostResponse: %v", err)
	}
	if got := seenPath.Load().(string); got != "/rsps/1/rsp" {
		t.Errorf("server saw path %q, want /rsps/1/rsp", got)
	}
}

// =============================================================================
// Case 4: absolute replyToHref does NOT double-prefix c.baseURL
// =============================================================================

func TestPostResponse_AbsoluteHrefBypassesBaseURL(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	// Stand up two distinct servers. The "primary" server is what c.baseURL
	// points at; the "other" server hosts the absolute replyTo. If the
	// production path string-concatenated the absolute href onto baseURL we
	// would see the primary server hit and the other server idle.
	primaryMux := http.NewServeMux()
	var primaryHits atomic.Int32
	primaryMux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
	})
	primaryURL, stopPrimary := startIdleListener(t, env, primaryMux)
	defer stopPrimary()

	var otherHits atomic.Int32
	var otherPath atomic.Value
	otherPath.Store("")
	otherMux := http.NewServeMux()
	otherMux.HandleFunc("/rsps/9/rsp", func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		otherPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	otherURL, stopOther := startIdleListener(t, env, otherMux)
	defer stopOther()

	client := newCSIPClient(t, env, primaryURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	absolute := otherURL + "/rsps/9/rsp"
	if err := client.PostResponse(ctx, absolute, sampleDERControlResponse()); err != nil {
		t.Fatalf("PostResponse(absolute): %v", err)
	}
	if got := otherHits.Load(); got != 1 {
		t.Errorf("absolute server hits = %d, want exactly 1", got)
	}
	if got := otherPath.Load().(string); got != "/rsps/9/rsp" {
		t.Errorf("absolute path = %q, want /rsps/9/rsp (no double-prefix)", got)
	}
	if got := primaryHits.Load(); got != 0 {
		t.Errorf("primary server received %d hits; absolute href should have bypassed baseURL", got)
	}
}

// =============================================================================
// Case 5: 400 Bad Request -> wrapped error, body NOT logged verbatim
// =============================================================================

func TestPostResponse_400BadRequestRedactsBody(t *testing.T) {
	// captureLogs callers must be serial : log.Default() is
	// process-global. See response_post_test.go captureLogs godoc.
	env := newCCMTestEnv(t)

	const secretBody = "<Error><Detail>do-not-leak-this-string</Detail></Error>"
	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/sep+xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, secretBody)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	logs := captureLogs(t, func() {
		err = client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse())
	})
	if err == nil {
		t.Fatal("PostResponse(400) returned nil; want error")
	}
	// Non-transient : must NOT match the sentinel.
	if errors.Is(err, inverter.ErrResponseTransient) {
		t.Errorf("4xx error must not match ErrResponseTransient; got %v", err)
	}
	// Status surfaces in the error message; body must NOT appear verbatim.
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q should mention status 400", err)
	}
	if strings.Contains(err.Error(), "do-not-leak-this-string") {
		t.Errorf("error message leaked response body content: %v", err)
	}
	if strings.Contains(logs, "do-not-leak-this-string") {
		t.Errorf("log output leaked response body content: %s", logs)
	}
}

// =============================================================================
// Case 6: transient 500 then 201 -> success after one retry
// =============================================================================

func TestPostResponse_500ThenSuccessUsesRetry(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse()); err != nil {
		t.Fatalf("PostResponse: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want exactly 2 (initial + 1 retry)", got)
	}
}

// =============================================================================
// Case 7: persistent 5xx -> ErrResponseTransient surfaces
// =============================================================================

func TestPostResponse_500PersistentReturnsTransient(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse())
	if err == nil {
		t.Fatal("PostResponse(persistent 500) returned nil; want error")
	}
	if !errors.Is(err, inverter.ErrResponseTransient) {
		t.Errorf("err = %v; want errors.Is(err, ErrResponseTransient) to succeed", err)
	}
	// Exactly one initial + one retry; the in-call retry is intentionally
	// surgical (full retry/dead-letter policy is out of scope here).
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want exactly 2 (no extra retries)", got)
	}
}

// =============================================================================
// Case 8: cancelled context surfaces context.Canceled
// =============================================================================

func TestPostResponse_ContextCancelledPropagates(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, r *http.Request) {
		// Block until the client cancels : gives the test deterministic
		// control over when the in-flight POST observes the cancellation.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel from the outside while the request is in-flight.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse())
	if err == nil {
		t.Fatal("PostResponse(cancelled) returned nil; want context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want errors.Is(err, context.Canceled) to succeed", err)
	}
	// Cancellation MUST NOT mask as a transient : the retry policy is
	// keyed on ErrResponseTransient and ctx errors are the caller's
	// stop-the-world signal, not retry signal.
	if errors.Is(err, inverter.ErrResponseTransient) {
		t.Errorf("cancelled error must not match ErrResponseTransient; got %v", err)
	}
}

// =============================================================================
// Case 9: Content-Type header is application/sep+xml on the wire
// (already partially covered by case 1; this is the explicit assertion +
// a hostile-server check to make sure the production path does not drop
// the header on retry)
// =============================================================================

func TestPostResponse_ContentTypeHeaderIsSEPXML(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)

	var attempts atomic.Int32
	var seenCT [2]atomic.Value
	for i := range seenCT {
		seenCT[i].Store("")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rsps/1/rsp", func(w http.ResponseWriter, r *http.Request) {
		i := attempts.Add(1) - 1
		if int(i) < len(seenCT) {
			seenCT[i].Store(r.Header.Get("Content-Type"))
		}
		if i == 0 {
			// Force a retry so we observe the header on attempt 2 too.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	client := newCSIPClient(t, env, serverURL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.PostResponse(ctx, "/rsps/1/rsp", sampleDERControlResponse()); err != nil {
		t.Fatalf("PostResponse: %v", err)
	}
	for i := 0; i < 2; i++ {
		if got := seenCT[i].Load().(string); got != "application/sep+xml" {
			t.Errorf("attempt %d Content-Type = %q, want application/sep+xml", i+1, got)
		}
	}
}

// =============================================================================
// Bonus: empty replyToHref returns the documented guard error without a
// network call. Mirrors the empty-href guards on every other client method
// and lifts PostResponse over ≥80%.
// =============================================================================

func TestPostResponse_EmptyHrefErrors(t *testing.T) {
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

	err := client.PostResponse(ctx, "", sampleDERControlResponse())
	if err == nil {
		t.Fatal("PostResponse(\"\") returned nil; want error")
	}
	if !strings.Contains(err.Error(), "replyTo href required") {
		t.Errorf("err = %v, want one containing %q", err, "replyTo href required")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("HTTP hits = %d, want 0 (early return)", got)
	}
}

// =============================================================================
// Bonus: a malformed href that url.Parse rejects produces a wrapped error
// without a network call. Defensive : confirms the resolution helper
// surfaces parse failures rather than turning them into transient retries.
// =============================================================================

func TestPostResponse_MalformedHrefSurfacesParseError(t *testing.T) {
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

	// url.Parse rejects a control character in the host portion. Use the
	// %-quoted bell to construct a deterministic parse failure without
	// relying on URL grammar quirks.
	bad := "http://\x7f/bad"
	if _, perr := url.Parse(bad); perr == nil {
		t.Skip("url.Parse unexpectedly accepted the malformed input; nothing to assert")
	}

	err := client.PostResponse(ctx, bad, sampleDERControlResponse())
	if err == nil {
		t.Fatal("PostResponse(malformed href) returned nil; want error")
	}
	// Must NOT be classified as transient : retry would be pointless.
	if errors.Is(err, inverter.ErrResponseTransient) {
		t.Errorf("malformed-href error must not match ErrResponseTransient; got %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("HTTP hits = %d, want 0 (parse failed before send)", got)
	}
}
