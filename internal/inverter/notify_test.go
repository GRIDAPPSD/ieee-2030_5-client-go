package inverter_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	certs "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2cert"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	gotls "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2tls/gotls"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// httpServerForHMI wraps the HMI handler in an httptest.Server so the
// /notify-addr endpoint can be exercised over real HTTP. The HMI itself is
// a plain net/http handler; no TLS is needed for these tests.
func httpServerForHMI(h *inverter.HMI) *httptest.Server {
	return httptest.NewServer(h.Handler())
}

// getBody issues a GET against the supplied URL and returns the body as a
// string. Fails the test on any transport or read error so callers don't
// repeat the boilerplate.
func getBody(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(body)
}

// newReceiverFromEnv wires a NotifyReceiver against the shared ccmTestEnv
// fixture. It returns a started receiver and a teardown helper; tests should
// call the teardown via t.Cleanup or defer.
//
// listenAddr defaults to 127.0.0.1:0 (random port). Callers that want a
// disabled receiver should call NewNotifyReceiver directly with "" — the
// disabled path doesn't reach this helper.
func newReceiverFromEnv(t *testing.T, env *ccmTestEnv, dispatcher inverter.NotificationDispatcher) (*inverter.NotifyReceiver, func()) {
	t.Helper()

	rcv, err := inverter.NewNotifyReceiver(inverter.NotifyReceiverConfig{
		CertFile:   env.deviceCertPath,
		KeyFile:    env.deviceKeyPath,
		CAFile:     env.caCertPath,
		ListenAddr: "127.0.0.1:0",
		Dispatcher: dispatcher,
	})
	if err != nil {
		t.Fatalf("NewNotifyReceiver: %v", err)
	}
	if rcv == nil {
		t.Fatalf("NewNotifyReceiver returned nil receiver for non-empty addr")
	}
	if err := rcv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	teardown := func() {
		_ = rcv.Stop(context.Background())
	}
	t.Cleanup(teardown)
	return rcv, teardown
}

// notifyClientForEnv builds an HTTP client that mimics what a SEP2 server
// would do when POSTing a Notification: gotls with CCM-8, mTLS using the
// shared CA-signed cert env.
func notifyClientForEnv(t *testing.T, env *ccmTestEnv) *http.Client {
	t.Helper()

	certPEM, err := os.ReadFile(env.deviceCertPath)
	if err != nil {
		t.Fatalf("read device cert: %v", err)
	}
	keyPEM, err := os.ReadFile(env.deviceKeyPath)
	if err != nil {
		t.Fatalf("read device key: %v", err)
	}
	cert, err := gotls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	caPEM, err := os.ReadFile(env.caCertPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("append CA")
	}

	tlsCfg := &gotls.Config{
		Certificates: []gotls.Certificate{cert},
		RootCAs:      rootPool,
		MinVersion:   gotls.VersionTLS12,
		MaxVersion:   gotls.VersionTLS12,
		CipherSuites: []uint16{
			gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8,
			gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
		// Receiver's server cert is the inverter's device cert, which
		// carries an otherName-only SAN (no DNS / IP entry) so stdlib
		// hostname verification cannot succeed. Bypass and rely on
		// chain validity — the CA pool is locked down.
		InsecureSkipVerify: true, //nolint:gosec // see comment
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
	}

	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&gotls.Dialer{Config: tlsCfg}).DialContext(ctx, network, addr)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// notifyURL composes a https URL to the receiver's bound address with the
// given path. Tests that need a different path can supply it directly.
func notifyURL(t *testing.T, rcv *inverter.NotifyReceiver, path string) string {
	t.Helper()
	addr, err := rcv.Addr()
	if err != nil {
		t.Fatalf("rcv.Addr: %v", err)
	}
	return "https://" + addr + path
}

// sampleNotificationXML returns a minimal well-formed Notification body. The
// CORE-018 step-4 payload carries subscribedResource (the resource the
// inverter subscribed to), newResourceURI (the actual changed item — usually
// the same), and status. mRID is required on Resource per IEEE 2030.5 §10.
func sampleNotificationXML(t *testing.T, status uint8, newURI string) []byte {
	t.Helper()
	n := sep2.Notification{
		Resource: sep2.Resource{
			Href: "/notify",
		},
		SubscribedResource: "/edev/0/fsa",
		SubscriptionURI:    "/edev/0/sub/1",
		Status:             status,
		NewResourceURI:     newURI,
	}
	body, err := xml.Marshal(&n)
	if err != nil {
		t.Fatalf("marshal Notification: %v", err)
	}
	return body
}

// --- Unit tests --------------------------------------------------------

// TestNewNotifyReceiver_EmptyAddrDisabled asserts the "no listener"
// contract: an empty ListenAddr returns (nil, nil) so callers can detect
// the disabled state without an error path.
func TestNewNotifyReceiver_EmptyAddrDisabled(t *testing.T) {
	t.Parallel()

	rcv, err := inverter.NewNotifyReceiver(inverter.NotifyReceiverConfig{
		ListenAddr: "",
	})
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if rcv != nil {
		t.Fatalf("expected nil receiver, got %+v", rcv)
	}
}

// TestNewNotifyReceiver_BadCertPath asserts that a missing cert file
// surfaces as a wrapped error rather than panicking at Start time.
func TestNewNotifyReceiver_BadCertPath(t *testing.T) {
	t.Parallel()

	_, err := inverter.NewNotifyReceiver(inverter.NotifyReceiverConfig{
		CertFile:   filepath.Join(t.TempDir(), "nope.crt"),
		KeyFile:    filepath.Join(t.TempDir(), "nope.key"),
		CAFile:     filepath.Join(t.TempDir(), "nope.ca"),
		ListenAddr: "127.0.0.1:0",
	})
	if err == nil {
		t.Fatalf("expected error from missing cert")
	}
}

// TestNotifyReceiver_StartIdempotent asserts that a double-Start returns
// ErrNotifyReceiverAlreadyStarted rather than re-binding (which would
// either deadlock or leak the original listener).
func TestNotifyReceiver_StartIdempotent(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)
	rcv, _ := newReceiverFromEnv(t, env, nil)

	if err := rcv.Start(); !errors.Is(err, inverter.ErrNotifyReceiverAlreadyStarted) {
		t.Fatalf("second Start: want %v, got %v", inverter.ErrNotifyReceiverAlreadyStarted, err)
	}
}

// TestNotifyReceiver_AddrBeforeStart asserts Addr returns
// ErrNotifyReceiverNotStarted when the receiver was never started.
func TestNotifyReceiver_AddrBeforeStart(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)
	rcv, err := inverter.NewNotifyReceiver(inverter.NotifyReceiverConfig{
		CertFile:   env.deviceCertPath,
		KeyFile:    env.deviceKeyPath,
		CAFile:     env.caCertPath,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("NewNotifyReceiver: %v", err)
	}
	if _, err := rcv.Addr(); !errors.Is(err, inverter.ErrNotifyReceiverNotStarted) {
		t.Fatalf("Addr before Start: want %v, got %v", inverter.ErrNotifyReceiverNotStarted, err)
	}
}

// TestNotifyReceiver_StopWithoutStart asserts Stop is symmetric with Addr.
func TestNotifyReceiver_StopWithoutStart(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)
	rcv, err := inverter.NewNotifyReceiver(inverter.NotifyReceiverConfig{
		CertFile:   env.deviceCertPath,
		KeyFile:    env.deviceKeyPath,
		CAFile:     env.caCertPath,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("NewNotifyReceiver: %v", err)
	}
	if err := rcv.Stop(context.Background()); !errors.Is(err, inverter.ErrNotifyReceiverNotStarted) {
		t.Fatalf("Stop before Start: want %v, got %v", inverter.ErrNotifyReceiverNotStarted, err)
	}
}

// --- Handler-level table tests -----------------------------------------

// TestNotifyHandler_StatusCodes covers the policy matrix from notify.go:
// method, content-type, body shape.
func TestNotifyHandler_StatusCodes(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)

	cases := []struct {
		name        string
		method      string
		contentType string
		body        []byte
		wantStatus  int
	}{
		{
			name:        "happy_204_xml",
			method:      http.MethodPost,
			contentType: "application/sep+xml",
			body:        sampleNotificationXML(t, 0, "/edev/0/fsa"),
			wantStatus:  http.StatusNoContent,
		},
		{
			name:        "happy_204_xml_with_charset",
			method:      http.MethodPost,
			contentType: "application/sep+xml; charset=utf-8",
			body:        sampleNotificationXML(t, 1, "/edev/0/fsa"),
			wantStatus:  http.StatusNoContent,
		},
		{
			name:        "wrong_method_get",
			method:      http.MethodGet,
			contentType: "application/sep+xml",
			body:        nil,
			wantStatus:  http.StatusMethodNotAllowed,
		},
		{
			name:        "wrong_method_put",
			method:      http.MethodPut,
			contentType: "application/sep+xml",
			body:        sampleNotificationXML(t, 0, "/x"),
			wantStatus:  http.StatusMethodNotAllowed,
		},
		{
			name:        "wrong_content_type",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        []byte(`{"foo":"bar"}`),
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "empty_body",
			method:      http.MethodPost,
			contentType: "application/sep+xml",
			body:        []byte{},
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "malformed_xml",
			method:      http.MethodPost,
			contentType: "application/sep+xml",
			body:        []byte(`<<<not-xml>>>`),
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "well_formed_but_wrong_root",
			method:      http.MethodPost,
			contentType: "application/sep+xml",
			body:        []byte(`<DeviceCapability xmlns="urn:ieee:std:2030.5:ns"></DeviceCapability>`),
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "body_too_large",
			method:      http.MethodPost,
			contentType: "application/sep+xml",
			body:        bytes.Repeat([]byte("x"), 65*1024),
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rcv, _ := newReceiverFromEnv(t, env, nil)
			client := notifyClientForEnv(t, env)

			req, err := http.NewRequestWithContext(context.Background(),
				tc.method, notifyURL(t, rcv, "/notify"), bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", tc.contentType)

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("client.Do: %v", err)
			}
			defer func() {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status: want %d, got %d; body=%q", tc.wantStatus, resp.StatusCode, string(body))
			}
		})
	}
}

// TestNotifyHandler_DispatcherInvoked asserts the dispatcher receives a
// faithful copy of the parsed Notification on the happy path. This is the
// IEEE-049 -> IEEE-051 seam: IEEE-051 will register the real dispatcher
// against this same hook.
func TestNotifyHandler_DispatcherInvoked(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)

	var captured atomic.Value // sep2.Notification
	var fired atomic.Int32

	dispatcher := func(_ context.Context, n sep2.Notification) {
		captured.Store(n)
		fired.Add(1)
	}

	rcv, _ := newReceiverFromEnv(t, env, dispatcher)
	client := notifyClientForEnv(t, env)

	body := sampleNotificationXML(t, 1, "/edev/0/fsa/3")
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, notifyURL(t, rcv, "/notify"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/sep+xml")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", resp.StatusCode)
	}

	if got := fired.Load(); got != 1 {
		t.Fatalf("dispatcher fired %d times, want 1", got)
	}
	gotN, ok := captured.Load().(sep2.Notification)
	if !ok {
		t.Fatal("dispatcher did not capture a Notification")
	}
	if gotN.SubscribedResource != "/edev/0/fsa" {
		t.Errorf("SubscribedResource = %q", gotN.SubscribedResource)
	}
	if gotN.NewResourceURI != "/edev/0/fsa/3" {
		t.Errorf("NewResourceURI = %q", gotN.NewResourceURI)
	}
	if gotN.Status != 1 {
		t.Errorf("Status = %d", gotN.Status)
	}
}

// TestNotifyHandler_DispatcherNotInvokedOnError asserts the dispatcher is
// NOT called for malformed bodies — important so a buggy server can't
// trigger Phase 5 state machine churn by spamming garbage.
func TestNotifyHandler_DispatcherNotInvokedOnError(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)

	var fired atomic.Int32
	dispatcher := func(_ context.Context, _ sep2.Notification) {
		fired.Add(1)
	}

	rcv, _ := newReceiverFromEnv(t, env, dispatcher)
	client := notifyClientForEnv(t, env)

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, notifyURL(t, rcv, "/notify"), bytes.NewReader([]byte("<broken")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/sep+xml")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
	if got := fired.Load(); got != 0 {
		t.Fatalf("dispatcher fired %d times on bad body, want 0", got)
	}
}

// --- Integration tests -------------------------------------------------

// TestNotifyReceiver_AddrBound asserts the bound address is observable
// immediately after Start and that the port is non-zero (random-port
// resolution worked).
func TestNotifyReceiver_AddrBound(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)
	rcv, _ := newReceiverFromEnv(t, env, nil)

	addr, err := rcv.Addr()
	if err != nil {
		t.Fatalf("Addr: %v", err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("Addr = %q, want 127.0.0.1:N", addr)
	}
	// The :0 placeholder must have resolved to a real port number.
	if strings.HasSuffix(addr, ":0") {
		t.Errorf("Addr = %q still has zero port", addr)
	}
}

// TestNotifyReceiver_RejectsUnsignedClient asserts mTLS posture: a client
// presenting a cert signed by a foreign CA fails the handshake against
// the receiver. This is the security invariant — without it any host
// could pose as the SEP2 server and inject Notifications.
func TestNotifyReceiver_RejectsUnsignedClient(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)
	rcv, _ := newReceiverFromEnv(t, env, nil)

	// Build a second, foreign CA and issue a device cert under it. The
	// receiver's CA pool only trusts env's CA — this cert's chain will
	// not validate.
	foreignCAPEM, foreignCAKeyPEM, err := certs.GenerateCA(certs.CAOptions{
		CommonName: "Foreign CA",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatalf("foreign CA: %v", err)
	}
	foreignCACert, err := certs.ParseCertificatePEM(foreignCAPEM)
	if err != nil {
		t.Fatalf("parse foreign CA cert: %v", err)
	}
	foreignCAKey, err := certs.ParseKeyPEM(foreignCAKeyPEM)
	if err != nil {
		t.Fatalf("parse foreign CA key: %v", err)
	}
	foreignDevCertPEM, foreignDevKeyPEM, err := certs.GenerateDeviceCert(foreignCACert, foreignCAKey, certs.DeviceCertOptions{
		DeviceType:  certs.DeviceTypeGeneric,
		HWSerialNum: "FOREIGN-001",
	})
	if err != nil {
		t.Fatalf("foreign device cert: %v", err)
	}
	cert, err := gotls.X509KeyPair(foreignDevCertPEM, foreignDevKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	// Trust the receiver's CA on the dial side so the server's leaf
	// chain validates from this client's perspective — we want the
	// handshake to fail because of OUR cert, not the server's.
	rootPool := x509.NewCertPool()
	caPEM, err := os.ReadFile(env.caCertPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	if !rootPool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("append CA")
	}

	tlsCfg := &gotls.Config{
		Certificates: []gotls.Certificate{cert},
		RootCAs:      rootPool,
		MinVersion:   gotls.VersionTLS12,
		MaxVersion:   gotls.VersionTLS12,
		CipherSuites: []uint16{
			gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8,
			gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
		InsecureSkipVerify: true, //nolint:gosec // see notifyClientForEnv
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&gotls.Dialer{Config: tlsCfg}).DialContext(ctx, network, addr)
			},
		},
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, notifyURL(t, rcv, "/notify"),
		bytes.NewReader(sampleNotificationXML(t, 0, "/edev/0/fsa")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/sep+xml")

	if _, err := client.Do(req); err == nil {
		t.Fatalf("expected handshake/connection error for foreign-CA client cert, got nil")
	}
}

// TestHMI_NotifyAddrRoundTrip asserts Set/Get symmetry on the HMI's new
// notifyAddr field. The HMI is the operator-visible surface that exposes
// the IEEE-049 listener bind address; correctness here is what lets the
// operator point a server-side subscription at the right port.
func TestHMI_NotifyAddrRoundTrip(t *testing.T) {
	t.Parallel()

	h := inverter.NewHMI()
	if got := h.NotifyAddr(); got != "" {
		t.Errorf("zero value: NotifyAddr = %q, want \"\"", got)
	}
	h.SetNotifyAddr("127.0.0.1:54321")
	if got := h.NotifyAddr(); got != "127.0.0.1:54321" {
		t.Errorf("after Set: NotifyAddr = %q, want 127.0.0.1:54321", got)
	}
	h.SetNotifyAddr("")
	if got := h.NotifyAddr(); got != "" {
		t.Errorf("after clear: NotifyAddr = %q, want \"\"", got)
	}
}

// TestHMI_NotifyAddrHandler asserts the /notify-addr HTTP endpoint
// renders the recorded address (or the "disabled" sentinel when empty).
func TestHMI_NotifyAddrHandler(t *testing.T) {
	t.Parallel()

	h := inverter.NewHMI()
	srv := httpServerForHMI(h)
	defer srv.Close()

	// Empty → "disabled\n"
	body := getBody(t, srv.URL+"/notify-addr")
	if body != "disabled\n" {
		t.Errorf("empty: body = %q, want \"disabled\\n\"", body)
	}

	h.SetNotifyAddr("127.0.0.1:9876")
	body = getBody(t, srv.URL+"/notify-addr")
	if body != "https://127.0.0.1:9876/notify\n" {
		t.Errorf("set: body = %q, want https URL", body)
	}
}

// TestNotifyReceiver_StopGracefulIsRepeatable asserts Stop drains the
// serve goroutine cleanly. A leak here would surface as a goroutine
// hang under -race.
func TestNotifyReceiver_StopGraceful(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)
	rcv, _ := newReceiverFromEnv(t, env, nil)

	// First Stop is via t.Cleanup; we additionally trigger it manually
	// to verify it returns nil (graceful shutdown).
	if err := rcv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Double-Stop returns ErrNotifyReceiverNotStarted because the first
	// Stop reset the receiver's state. Calling it twice is not a panic.
	if err := rcv.Stop(context.Background()); !errors.Is(err, inverter.ErrNotifyReceiverNotStarted) {
		t.Fatalf("second Stop: want %v, got %v", inverter.ErrNotifyReceiverNotStarted, err)
	}
}
