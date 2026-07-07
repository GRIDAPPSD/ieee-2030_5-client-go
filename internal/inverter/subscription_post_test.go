// IEEE-050 PostSubscription unit tests.
//
// Covers the 9-case matrix from the backlog ticket: 201 happy path,
// 405 polling-fallback errors.Is contract, 500/400 wrapped-not-fatal,
// empty-arg validation, body shape verification, 301 follow (reusing
// IEEE-047), and context cancellation.

package inverter

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// newTestSEP2Client builds a minimal SEP2Client suitable for httptest
// servers. The test server uses stdlib TLS (or plaintext on httptest.Server
// when callers want it), so this helper installs a permissive RootCAs +
// InsecureSkipVerify combo. The HardwareModuleName-SAN verify hook is
// bypassed here : IEEE-050 cares about HTTP status flow, not TLS posture
// (which IEEE-019 / IEEE-027 already exercise). For plaintext httptest
// servers we use a stdlib http.Client wired into the SEP2Client struct
// directly.
func newTestSEP2Client(t *testing.T, baseURL string, transport http.RoundTripper) *SEP2Client {
	t.Helper()
	return &SEP2Client{
		httpClient: &http.Client{Transport: transport, Timeout: 5 * time.Second},
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		sfdi:       "1",
		lfdi:       "0000000000000000000000000000000000000001",
	}
}

// testServerWithStatus returns an httptest.Server that records the captured
// POST path + body and responds with the configured status. When status is
// 201 the Location header is also set so the helper has something to
// return.
func testServerWithStatus(t *testing.T, status int, location string) (srv *httptest.Server, gotPath *string, gotBody *[]byte) {
	t.Helper()
	pathHolder := new(string)
	bodyHolder := new([]byte)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*pathHolder = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		*bodyHolder = body
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, pathHolder, bodyHolder
}

func TestPostSubscription_HappyPath201(t *testing.T) {
	t.Parallel()
	srv, gotPath, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/sub/sub-XYZ")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	href, err := c.PostSubscription(
		context.Background(),
		"/edev/1/sub",
		"/edev/1/fsa",
		"https://inverter.example/notify",
	)
	if err != nil {
		t.Fatalf("PostSubscription err = %v, want nil", err)
	}
	if href != "/edev/1/sub/sub-XYZ" {
		t.Errorf("returned href = %q, want %q", href, "/edev/1/sub/sub-XYZ")
	}
	if *gotPath != "/edev/1/sub" {
		t.Errorf("server saw path = %q, want /edev/1/sub", *gotPath)
	}
	// Body shape: subscribedResource, notificationURI, encoding=0 (XML).
	body := string(*gotBody)
	if !strings.Contains(body, "<subscribedResource>/edev/1/fsa</subscribedResource>") {
		t.Errorf("body missing subscribedResource:\n%s", body)
	}
	if !strings.Contains(body, "<notificationURI>https://inverter.example/notify</notificationURI>") {
		t.Errorf("body missing notificationURI:\n%s", body)
	}
	if !strings.Contains(body, "<encoding>0</encoding>") {
		t.Errorf("body missing <encoding>0</encoding>:\n%s", body)
	}
}

func TestPostSubscription_405FallbackErrorsIs(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusMethodNotAllowed, "")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	href, err := c.PostSubscription(
		context.Background(),
		"/edev/1/sub",
		"/edev/1/fsa",
		"https://inverter.example/notify",
	)
	if err == nil {
		t.Fatalf("PostSubscription err = nil, want ErrMethodNotAllowed")
	}
	if !errors.Is(err, ErrMethodNotAllowed) {
		t.Errorf("err not errors.Is ErrMethodNotAllowed: %v", err)
	}
	if href != "" {
		t.Errorf("href = %q, want \"\" on 405", href)
	}
}

func TestPostSubscription_500WrappedNotFatal(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusInternalServerError, "")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	href, err := c.PostSubscription(
		context.Background(),
		"/edev/1/sub",
		"/edev/1/fsa",
		"https://inverter.example/notify",
	)
	if err == nil {
		t.Fatalf("PostSubscription err = nil, want wrapped 5xx error")
	}
	if !errors.Is(err, ErrResponseTransient) {
		t.Errorf("err not errors.Is ErrResponseTransient: %v", err)
	}
	if href != "" {
		t.Errorf("href = %q, want \"\" on 5xx", href)
	}
}

func TestPostSubscription_400Wrapped(t *testing.T) {
	t.Parallel()
	srv, _, _ := testServerWithStatus(t, http.StatusBadRequest, "")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	_, err := c.PostSubscription(
		context.Background(),
		"/edev/1/sub",
		"/edev/1/fsa",
		"https://inverter.example/notify",
	)
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("err not errors.Is ErrBadRequest: %v", err)
	}
}

func TestPostSubscription_EmptyArgs(t *testing.T) {
	t.Parallel()
	c := newTestSEP2Client(t, "http://unused", http.DefaultTransport)
	cases := []struct {
		name                                          string
		listHref, subscribedHref, notifyURL, wantSnip string
	}{
		{"emptyListHref", "", "/edev/1/fsa", "https://x/notify", "subscriptionListHref required"},
		{"emptySubscribedHref", "/edev/1/sub", "", "https://x/notify", "subscribedHref required"},
		{"emptyNotifyURL", "/edev/1/sub", "/edev/1/fsa", "", "notifyURL required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.PostSubscription(context.Background(), tc.listHref, tc.subscribedHref, tc.notifyURL)
			if err == nil {
				t.Fatalf("err = nil, want %q", tc.wantSnip)
			}
			if !strings.Contains(err.Error(), tc.wantSnip) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSnip)
			}
		})
	}
}

func TestPostSubscription_BodyParsesAsSubscription(t *testing.T) {
	t.Parallel()
	srv, _, gotBody := testServerWithStatus(t, http.StatusCreated, "/edev/1/sub/sub-A")
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	_, err := c.PostSubscription(
		context.Background(),
		"/edev/1/sub",
		"/edev/1/derp/1/derc",
		"https://inverter.example/notify",
	)
	if err != nil {
		t.Fatalf("PostSubscription err = %v", err)
	}

	var parsed sep2.Subscription
	if err := xml.Unmarshal(*gotBody, &parsed); err != nil {
		t.Fatalf("server-captured body did not unmarshal as Subscription: %v\nbody: %s", err, string(*gotBody))
	}
	if parsed.SubscribedResource != "/edev/1/derp/1/derc" {
		t.Errorf("SubscribedResource = %q, want %q", parsed.SubscribedResource, "/edev/1/derp/1/derc")
	}
	if parsed.NotificationURI != "https://inverter.example/notify" {
		t.Errorf("NotificationURI = %q, want %q", parsed.NotificationURI, "https://inverter.example/notify")
	}
	if parsed.Encoding != sep2.EncodingXML {
		t.Errorf("Encoding = %d, want %d (EncodingXML)", parsed.Encoding, sep2.EncodingXML)
	}
}

// TestPostSubscription_301Follow verifies the IEEE-047 single-hop follow
// path is inherited from (*SEP2Client).Post: a 301 on the subscription-list
// href is followed exactly once, and the eventual 201's Location is
// surfaced to the caller.
func TestPostSubscription_301Follow(t *testing.T) {
	t.Parallel()

	// Two-routed server: /edev/1/sub returns 301 → /v2/edev/1/sub which
	// returns 201 with Location /v2/edev/1/sub/sub-Z.
	mux := http.NewServeMux()
	var postsToV2 atomic.Int32
	mux.HandleFunc("/edev/1/sub", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/v2/edev/1/sub")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/v2/edev/1/sub", func(w http.ResponseWriter, r *http.Request) {
		postsToV2.Add(1)
		w.Header().Set("Location", "/v2/edev/1/sub/sub-Z")
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	href, err := c.PostSubscription(
		context.Background(),
		"/edev/1/sub",
		"/edev/1/fsa",
		"https://inverter.example/notify",
	)
	if err != nil {
		t.Fatalf("PostSubscription err = %v, want nil", err)
	}
	if href != "/v2/edev/1/sub/sub-Z" {
		t.Errorf("returned href = %q, want post-redirect Location %q", href, "/v2/edev/1/sub/sub-Z")
	}
	if got := postsToV2.Load(); got != 1 {
		t.Errorf("/v2/edev/1/sub POST count = %d, want 1 (single follow)", got)
	}
}

func TestPostSubscription_ContextCancel(t *testing.T) {
	t.Parallel()
	// Server blocks long enough for ctx to fire.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestSEP2Client(t, srv.URL, srv.Client().Transport)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.PostSubscription(ctx, "/edev/1/sub", "/edev/1/fsa", "https://x/notify")
	if err == nil {
		t.Fatalf("PostSubscription err = nil, want context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context cancellation", err)
	}
}

// Compile-time guard: keep the SubscriptionListLink field on EndDevice
// present so IEEE-050 main.go wiring doesn't rot if someone refactors
// pkg/sep2/enddevice.go. Compile-time only; no runtime cost.
var _ = func() *sep2.ListLink {
	var e sep2.EndDevice
	return e.SubscriptionListLink
}
