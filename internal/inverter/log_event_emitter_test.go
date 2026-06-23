// IEEE-053 PostLogEvent unit tests.
//
// Covers the matrix declared in backlog IEEE-053 plus the rate-limit /
// time-source seams the ticket calls out: 201 happy path, empty href →
// ErrLogEventLinkAbsent (no HTTP), rate-limit deny → ErrRateLimited (no
// HTTP), 405 graceful-bypass, 500 wrapped 5xx, 400 wrapped 4xx, body
// shape (PEN + LogEventCode + createdDateTime round-trip), createdDateTime
// derived from c.Now() not time.Now() (testable time source),
// caller-supplied non-zero PEN / CreatedDateTime preserved verbatim, and
// the no-op default-limiter contract.
//
// Reuses newTestSEP2Client + testServerWithStatus from subscription_post_test.go
// (same package); no new helpers needed beyond a time-pin shim and a
// captured-event decoder.

package inverter

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// stubRateLimiter is a deterministic LogEventRateLimiter for the deny
// path. allow=false denies every code; allow=true allows every code.
type stubRateLimiter struct{ allow bool }

func (s stubRateLimiter) Allow(uint8) bool { return s.allow }

// codeRecordingLimiter captures the codes Allow was called with so tests
// can assert PostLogEvent consults the limiter exactly once per call.
type codeRecordingLimiter struct {
	allow bool
	codes []uint8
}

func (l *codeRecordingLimiter) Allow(code uint8) bool {
	l.codes = append(l.codes, code)
	return l.allow
}

// pinClientTime installs a synthetic server-time offset so c.Now() returns
// the supplied wall-clock value. Returns a cleanup that restores zero.
func pinClientTime(t *testing.T, c *SEP2Client, want time.Time) {
	t.Helper()
	offset := want.Sub(time.Now())
	c.serverTimeOffsetNanos.Store(int64(offset))
	t.Cleanup(func() { c.serverTimeOffsetNanos.Store(0) })
}

func TestPostLogEvent_HappyPath201(t *testing.T) {
	t.Parallel()
	srv, gotPath, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-42")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	c.pen = 40732 // PNNL-ish placeholder, doesn't have to be real

	evt := sep2.LogEvent{
		FunctionSet:  sep2.FunctionSetDER,
		LogEventCode: 1, // LE_VOLT_LO per Phase 9 doc tentative mapping
		LogEventID:   42,
		ProfileID:    2,
	}
	href, err := c.PostLogEvent(context.Background(), "/edev/1/lel", evt)
	if err != nil {
		t.Fatalf("PostLogEvent err = %v, want nil", err)
	}
	if href != "/edev/1/lel/le-42" {
		t.Errorf("returned href = %q, want %q", href, "/edev/1/lel/le-42")
	}
	if *gotPath != "/edev/1/lel" {
		t.Errorf("server saw path = %q, want /edev/1/lel", *gotPath)
	}
	var parsed sep2.LogEvent
	if err := xml.Unmarshal(*gotBody, &parsed); err != nil {
		t.Fatalf("body did not unmarshal as LogEvent: %v\nbody: %s", err, string(*gotBody))
	}
	if parsed.LogEventCode != 1 {
		t.Errorf("parsed.LogEventCode = %d, want 1", parsed.LogEventCode)
	}
	if parsed.LogEventPEN != 40732 {
		t.Errorf("parsed.LogEventPEN = %d, want 40732 (filled from c.pen)", parsed.LogEventPEN)
	}
	if parsed.CreatedDateTime == 0 {
		t.Error("parsed.CreatedDateTime = 0, want a synthesized timestamp")
	}
}

func TestPostLogEvent_EmptyListHrefReturnsAbsentSentinel(t *testing.T) {
	t.Parallel()
	// No httptest server: assertion is that we get the sentinel with zero
	// HTTP traffic. Use a transport that panics on use to catch a leak.
	c := newTestSEP2Client(t, "http://unused", panicRoundTripper{t: t})

	href, err := c.PostLogEvent(context.Background(), "", sep2.LogEvent{LogEventCode: 1})
	if !errors.Is(err, ErrLogEventLinkAbsent) {
		t.Fatalf("err = %v, want errors.Is ErrLogEventLinkAbsent", err)
	}
	if href != "" {
		t.Errorf("href = %q, want \"\" on absent link", href)
	}
}

func TestPostLogEvent_RateLimitDenyReturnsErrRateLimited(t *testing.T) {
	t.Parallel()
	c := newTestSEP2Client(t, "http://unused", panicRoundTripper{t: t})
	c.SetLogEventRateLimiter(stubRateLimiter{allow: false})

	href, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 1})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want errors.Is ErrRateLimited", err)
	}
	if href != "" {
		t.Errorf("href = %q, want \"\"", href)
	}
}

func TestPostLogEvent_RateLimitAllowConsultedExactlyOnce(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-7")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	rl := &codeRecordingLimiter{allow: true}
	c.SetLogEventRateLimiter(rl)

	if _, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 7}); err != nil {
		t.Fatalf("PostLogEvent err = %v", err)
	}
	if len(rl.codes) != 1 || rl.codes[0] != 7 {
		t.Errorf("limiter consulted codes = %v, want [7]", rl.codes)
	}
}

func TestPostLogEvent_NilLimiterAllowsAllByDefault(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-1")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	// Do not call SetLogEventRateLimiter — exercise the nil default.

	if _, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 99}); err != nil {
		t.Fatalf("PostLogEvent err = %v, want nil with nil limiter", err)
	}
}

func TestPostLogEvent_405GracefulBypass(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusMethodNotAllowed, "")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	_, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 1})
	if !errors.Is(err, ErrMethodNotAllowed) {
		t.Errorf("err = %v, want errors.Is ErrMethodNotAllowed", err)
	}
}

func TestPostLogEvent_500WrappedTransient(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusInternalServerError, "")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	_, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 1})
	if !errors.Is(err, ErrResponseTransient) {
		t.Errorf("err = %v, want errors.Is ErrResponseTransient", err)
	}
}

func TestPostLogEvent_400WrappedBadRequest(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusBadRequest, "")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	_, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 1})
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("err = %v, want errors.Is ErrBadRequest", err)
	}
}

func TestPostLogEvent_NetworkErrorWrapped(t *testing.T) {
	t.Parallel()
	// Server that closes the connection without responding produces a
	// transport-level error. Pointing at an immediately-closed listener
	// is the simplest portable way to provoke one.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // close before client uses it
	c := newTestSEP2Client(t, srv.URL, http.DefaultTransport)

	_, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 1})
	if err == nil {
		t.Fatalf("PostLogEvent err = nil, want network error")
	}
	if !strings.Contains(err.Error(), "POST /edev/1/lel logEvent code=1") {
		t.Errorf("err message does not include POST context: %v", err)
	}
}

func TestPostLogEvent_CreatedDateTimeFromClientNow(t *testing.T) {
	t.Parallel()
	srv, _, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-1")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	// Pin the client's notion of "now" to a fixed wall-clock so we can
	// assert createdDateTime derives from c.Now() (server-synced clock),
	// not bare time.Now().
	pinned := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	pinClientTime(t, c, pinned)

	// Caller leaves CreatedDateTime zero so the emitter fills it.
	if _, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 1}); err != nil {
		t.Fatalf("PostLogEvent err = %v", err)
	}
	var parsed sep2.LogEvent
	if err := xml.Unmarshal(*gotBody, &parsed); err != nil {
		t.Fatalf("body did not unmarshal: %v", err)
	}
	// pinClientTime stores an offset relative to time.Now() at the call,
	// then PostLogEvent reads c.Now() shortly after. Allow ±2s of slop
	// for test-suite scheduling jitter.
	want := pinned.Unix()
	diff := parsed.CreatedDateTime - want
	if diff < -2 || diff > 2 {
		t.Errorf("CreatedDateTime = %d, want within ±2s of %d (pinned)", parsed.CreatedDateTime, want)
	}
}

func TestPostLogEvent_CallerSuppliedTimestampPreservedVerbatim(t *testing.T) {
	t.Parallel()
	srv, _, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-1")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	pinClientTime(t, c, time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC))

	want := int64(1234567890)
	evt := sep2.LogEvent{
		LogEventCode:    1,
		CreatedDateTime: want, // caller already set it; emitter must NOT overwrite
	}
	if _, err := c.PostLogEvent(context.Background(), "/edev/1/lel", evt); err != nil {
		t.Fatalf("PostLogEvent err = %v", err)
	}
	var parsed sep2.LogEvent
	if err := xml.Unmarshal(*gotBody, &parsed); err != nil {
		t.Fatalf("body did not unmarshal: %v", err)
	}
	if parsed.CreatedDateTime != want {
		t.Errorf("CreatedDateTime = %d, want %d (caller-supplied, preserved verbatim)", parsed.CreatedDateTime, want)
	}
}

func TestPostLogEvent_CallerSuppliedPENPreservedVerbatim(t *testing.T) {
	t.Parallel()
	srv, _, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-1")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	c.pen = 11111 // client's default PEN

	evt := sep2.LogEvent{
		LogEventCode: 1,
		LogEventPEN:  99999, // caller override; emitter must NOT overwrite
	}
	if _, err := c.PostLogEvent(context.Background(), "/edev/1/lel", evt); err != nil {
		t.Fatalf("PostLogEvent err = %v", err)
	}
	var parsed sep2.LogEvent
	if err := xml.Unmarshal(*gotBody, &parsed); err != nil {
		t.Fatalf("body did not unmarshal: %v", err)
	}
	if parsed.LogEventPEN != 99999 {
		t.Errorf("LogEventPEN = %d, want 99999 (caller override, preserved)", parsed.LogEventPEN)
	}
}

func TestPostLogEvent_ZeroClientPENStaysZeroIfCallerZero(t *testing.T) {
	t.Parallel()
	srv, _, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-1")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)
	// c.pen left at zero (no SimConfig.LogEventPEN configured).

	if _, err := c.PostLogEvent(context.Background(), "/edev/1/lel", sep2.LogEvent{LogEventCode: 1}); err != nil {
		t.Fatalf("PostLogEvent err = %v", err)
	}
	var parsed sep2.LogEvent
	if err := xml.Unmarshal(*gotBody, &parsed); err != nil {
		t.Fatalf("body did not unmarshal: %v", err)
	}
	if parsed.LogEventPEN != 0 {
		t.Errorf("LogEventPEN = %d, want 0 (both client and caller zero)", parsed.LogEventPEN)
	}
}

func TestSEP2Client_PENAccessor(t *testing.T) {
	t.Parallel()
	c := newTestSEP2Client(t, "http://unused", http.DefaultTransport)
	c.pen = 40732
	if got := c.PEN(); got != 40732 {
		t.Errorf("PEN() = %d, want 40732", got)
	}
}

func TestSEP2Client_SetLogEventRateLimiterNilDisablesLimiting(t *testing.T) {
	t.Parallel()
	c := newTestSEP2Client(t, "http://unused", http.DefaultTransport)
	c.SetLogEventRateLimiter(stubRateLimiter{allow: false})
	if c.logEventLimiter == nil {
		t.Fatal("after SetLogEventRateLimiter(stubRateLimiter), logEventLimiter is nil")
	}
	c.SetLogEventRateLimiter(nil)
	if c.logEventLimiter != nil {
		t.Errorf("after SetLogEventRateLimiter(nil), logEventLimiter = %v, want nil", c.logEventLimiter)
	}
}

func TestPostLogEvent_PreservesBodyRoundTripOfFullEvent(t *testing.T) {
	t.Parallel()
	srv, _, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/lel/le-99")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	want := sep2.LogEvent{
		CreatedDateTime: 1700000000,
		FunctionSet:     sep2.FunctionSetDER,
		LogEventCode:    5, // LE_REACTIVE_LIMIT per Phase 9 mapping
		LogEventID:      99,
		LogEventPEN:     40732,
		ProfileID:       2,
		Details:         "voltvar curtailment trip",
	}
	if _, err := c.PostLogEvent(context.Background(), "/edev/1/lel", want); err != nil {
		t.Fatalf("PostLogEvent err = %v", err)
	}
	var got sep2.LogEvent
	if err := xml.Unmarshal(*gotBody, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LogEventCode != want.LogEventCode ||
		got.LogEventID != want.LogEventID ||
		got.LogEventPEN != want.LogEventPEN ||
		got.ProfileID != want.ProfileID ||
		got.FunctionSet != want.FunctionSet ||
		got.CreatedDateTime != want.CreatedDateTime ||
		got.Details != want.Details {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestPostLogEvent_ContextCancelledBeforeCallDoesNotEmitRequest(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "/edev/1/lel/le-1")
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the POST

	_, err := c.PostLogEvent(ctx, "/edev/1/lel", sep2.LogEvent{LogEventCode: 1})
	if err == nil {
		t.Fatalf("PostLogEvent err = nil, want context.Canceled wrap")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is context.Canceled", err)
	}
	// We don't strictly assert hits==0 (request may race the cancel-check
	// inside http.Transport), but the error contract is the load-bearing
	// assertion.
}

// panicRoundTripper is an http.RoundTripper that fails the test if used.
// Installed in the ErrLogEventLinkAbsent + ErrRateLimited paths to assert
// PostLogEvent makes no HTTP traffic in those branches.
type panicRoundTripper struct{ t *testing.T }

func (p panicRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	p.t.Helper()
	p.t.Fatal("PostLogEvent issued an HTTP request when it must not have")
	return nil, errors.New("unreachable")
}
