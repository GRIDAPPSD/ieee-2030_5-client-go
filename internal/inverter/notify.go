// Package inverter — IEEE-049 inbound HTTPS Notification receiver.
//
// Until Phase 8, the inverter was outbound-only: it polled the SEP2 server
// for DERControlList changes (IEEE-038). CSIP V1.2 CORE-018 recommends a
// subscription/notification flow to optimize network traffic — the inverter
// subscribes to a resource (typically FSAList) and the server POSTs a
// Notification to the inverter every time the resource changes.
//
// IEEE-049 is the inbound side only: a TLS listener using the project's
// vendored gotls fork (TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8) running an
// http.Server that accepts POST /notify, parses the IEEE 2030.5 Notification
// XML body, and hands it to a no-op dispatch hook. Subscription POST
// (IEEE-050), Phase 5 state-machine dispatch (IEEE-051), and cancellation
// handling on status=1 (IEEE-052) are out of scope for this ticket.
//
// mTLS posture mirrors the server-side ccmserver: ClientAuth =
// RequireAnyClientCert + a VerifyPeerCertificate that walks the chain via
// VerifyPeerCertWithHardwareModuleSAN. This handles IEEE 2030.5 device
// certs carrying the critical HardwareModuleName SAN (OID 1.3.6.1.5.5.7.8.4)
// which stdlib x509 cannot parse.

package inverter

import (
	"context"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	sepTLS "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2tls"
	gotls "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2tls/gotls"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// notifyMaxBodyBytes caps the Notification POST body so a buggy or hostile
// server can't OOM the inverter. IEEE 2030.5 Notification resources are
// small (typically <2 KiB); 64 KiB is generous headroom for any future
// growth and matches typical XML payload caps elsewhere in the project.
const notifyMaxBodyBytes = 64 * 1024

// notifyReadHeaderTimeout caps slowloris-style attacks where a peer dribbles
// header bytes. Matches the conservative posture used by the project's
// other internal servers (see internal/server).
const notifyReadHeaderTimeout = 5 * time.Second

// notifyShutdownTimeout bounds graceful shutdown — connections still being
// served get this long to finish before the server is force-closed.
const notifyShutdownTimeout = 5 * time.Second

// NotificationDispatcher is the callback the receiver invokes for every
// well-formed Notification POST. IEEE-049 ships a no-op default so the
// listener path is exercised end-to-end without forcing IEEE-051's
// Phase-5-state-machine integration to land first.
//
// The dispatcher runs synchronously inside the /notify handler — keep it
// cheap. Heavy work (HTTP GETs to re-fetch a changed resource, state-machine
// ticks) belongs in a separate goroutine the dispatcher spawns itself, with
// a context tied to the receiver's lifetime.
//
// ctx is the per-request context; cancellation propagates to any goroutines
// spawned by the dispatcher implementation.
type NotificationDispatcher func(ctx context.Context, n sep2.Notification)

// NoopNotificationDispatcher is the IEEE-049 default — logs and returns.
// IEEE-051 will replace this with the real Phase-5 dispatcher.
func NoopNotificationDispatcher(_ context.Context, n sep2.Notification) {
	log.Printf("Notification receiver: subscribed=%q new=%q status=%d (no-op dispatch — IEEE-051 pending)",
		n.SubscribedResource, n.NewResourceURI, n.Status)
}

// NotifyReceiverConfig carries the inputs NewNotifyReceiver needs.
//
// CertFile / KeyFile / CAFile point at the same PEM files the outbound
// SEP2 client (NewSEP2Client) loads. The inverter's device cert acts as
// the listener's server cert; the CA pool is the trust anchor for
// validating the server-as-client cert.
//
// ListenAddr is the bind address (default 127.0.0.1:0 → random port). Empty
// string disables the receiver entirely; the caller should treat a
// nil-returned receiver as "subscription/notification disabled, fall back
// to polling."
//
// Dispatcher is the per-notification callback. Nil substitutes
// NoopNotificationDispatcher so callers can wire the listener and defer
// dispatcher selection.
type NotifyReceiverConfig struct {
	CertFile   string
	KeyFile    string
	CAFile     string
	ListenAddr string
	Dispatcher NotificationDispatcher
}

// NotifyReceiver owns the inbound HTTPS listener and the /notify handler.
//
// Lifecycle: NewNotifyReceiver -> Start -> Stop. Start binds the listener
// (so Addr() returns the bound address before any goroutine spawns), then
// runs the http.Server in a goroutine the caller's Stop() can join.
type NotifyReceiver struct {
	tlsCfg     *gotls.Config
	listenAddr string
	dispatcher NotificationDispatcher

	mu     sync.Mutex
	tlsL   net.Listener
	srv    *http.Server
	doneCh chan error
}

// NewNotifyReceiver assembles a receiver but does NOT bind the listener.
// Call Start to bind and begin serving. Bind-on-Start lets the caller
// distinguish config errors (returned from NewNotifyReceiver) from runtime
// network errors (returned from Start).
//
// Returns (nil, nil) when cfg.ListenAddr is empty — the caller's contract
// is "no listener, no subscription flow" and a nil-without-error result
// makes that branch easy to detect.
func NewNotifyReceiver(cfg NotifyReceiverConfig) (*NotifyReceiver, error) {
	if cfg.ListenAddr == "" {
		return nil, nil
	}

	tlsCfg, err := buildNotifyTLSConfig(cfg.CertFile, cfg.KeyFile, cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("build notify TLS config: %w", err)
	}

	dispatcher := cfg.Dispatcher
	if dispatcher == nil {
		dispatcher = NoopNotificationDispatcher
	}

	return &NotifyReceiver{
		tlsCfg:     tlsCfg,
		listenAddr: cfg.ListenAddr,
		dispatcher: dispatcher,
	}, nil
}

// buildNotifyTLSConfig produces a gotls.Config that mirrors the server-side
// NewCCMServerConfig: CCM-8 primary cipher, GCM fallback, mTLS via
// RequireAnyClientCert + HardwareModuleName-SAN-tolerant verify. The
// inverter's device cert is the listener's server cert; the CA pool is
// loaded from a single PEM file (the inverter doesn't have the
// "extra-client-CAs" use case the server has).
func buildNotifyTLSConfig(certFile, keyFile, caFile string) (*gotls.Config, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read cert %q: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read key %q: %w", keyFile, err)
	}
	cert, err := gotls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse cert %q: %w", certFile, err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA %q: %w", caFile, err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA %q: no PEM data", caFile)
	}

	return &gotls.Config{
		Certificates: []gotls.Certificate{cert},
		ClientCAs:    caPool,
		// IEEE 2030.5 / CSIP §6.11 device certs carry a critical
		// HardwareModuleName SAN that stdlib x509 cannot parse.
		// RequireAnyClientCert + manual verify in the hook matches
		// internal/tls/ccmserver.go.
		ClientAuth: gotls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return sepTLS.VerifyPeerCertWithHardwareModuleSAN(rawCerts, caPool)
		},
		MinVersion: gotls.VersionTLS12,
		MaxVersion: gotls.VersionTLS12,
		CipherSuites: []uint16{
			gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8,
			gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
		CurvePreferences: []gotls.CurveID{gotls.CurveP256},
	}, nil
}

// Start binds the listener and begins serving in a goroutine. The bound
// address is available via Addr() immediately after Start returns nil.
//
// Start is idempotent in the sense that a second call returns
// ErrAlreadyStarted rather than re-binding. Restart-after-Stop is not
// supported (the http.Server is single-use).
func (r *NotifyReceiver) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.srv != nil {
		return ErrNotifyReceiverAlreadyStarted
	}

	tcpL, err := net.Listen("tcp", r.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", r.listenAddr, err)
	}
	tlsL := gotls.NewListener(tcpL, r.tlsCfg)

	mux := http.NewServeMux()
	mux.Handle("/notify", r.notifyHandler())

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: notifyReadHeaderTimeout,
		// IEEE-049 doesn't need ConnContext / CCMIdentityMiddleware —
		// the handler doesn't inspect the peer cert (the SEP2 server
		// is presumed trusted once the chain validates). Subsequent
		// IEEE-051 dispatchers MAY need peer identity; if so they can
		// add the same SetupCCMServer / CCMIdentityMiddleware pair the
		// server-side router uses.
	}

	doneCh := make(chan error, 1)
	r.tlsL = tlsL
	r.srv = srv
	r.doneCh = doneCh

	// Capture doneCh in the goroutine's closure so Stop can clear
	// r.doneCh without racing with the serve goroutine's send.
	go func() {
		err := srv.Serve(tlsL)
		if errors.Is(err, http.ErrServerClosed) {
			doneCh <- nil
			return
		}
		doneCh <- err
	}()

	return nil
}

// ErrNotifyReceiverAlreadyStarted is returned by Start when the receiver
// is already running. Calling Start twice is a programmer error; the
// sentinel is exported so tests can match against it precisely.
var ErrNotifyReceiverAlreadyStarted = errors.New("notify receiver already started")

// ErrNotifyReceiverNotStarted is returned by Addr and Stop when the
// receiver has never been started, or has been stopped already. Same
// rationale as ErrNotifyReceiverAlreadyStarted.
var ErrNotifyReceiverNotStarted = errors.New("notify receiver not started")

// Addr returns the bound listener address, including the random-port
// resolution when the configured address used :0. Returns
// ErrNotifyReceiverNotStarted if Start has not been called (or returned
// an error).
func (r *NotifyReceiver) Addr() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tlsL == nil {
		return "", ErrNotifyReceiverNotStarted
	}
	return r.tlsL.Addr().String(), nil
}

// Stop initiates a graceful shutdown of the http.Server and waits for the
// serve goroutine to exit. The supplied ctx bounds the graceful phase;
// expiring it triggers an immediate Close.
//
// Stop is safe to call multiple times: the first call drains the serve
// goroutine; subsequent calls return ErrNotifyReceiverNotStarted because
// the receiver's state has been reset.
func (r *NotifyReceiver) Stop(ctx context.Context) error {
	r.mu.Lock()
	srv := r.srv
	doneCh := r.doneCh
	r.srv = nil
	r.doneCh = nil
	r.tlsL = nil
	r.mu.Unlock()

	if srv == nil {
		return ErrNotifyReceiverNotStarted
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, notifyShutdownTimeout)
	defer cancel()

	// Shutdown closes the underlying listener as part of its work, so we
	// don't need a separate r.tlsL.Close().
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Force-close as a last resort. http.Server.Close is safe to
		// call after a failed Shutdown.
		_ = srv.Close()
	}

	// Wait for the Serve goroutine to drain. doneCh receives exactly once.
	serveErr := <-doneCh
	return serveErr
}

// notifyHandler returns the http.HandlerFunc bound to POST /notify.
//
// Status code policy:
//   - 405 Method Not Allowed for any non-POST method.
//   - 415 Unsupported Media Type when Content-Type is not application/sep+xml.
//   - 400 Bad Request on read failure, body-too-large, malformed XML, or
//     XML that doesn't unmarshal into a Notification.
//   - 204 No Content on accept. (IEEE 2030.5 §10.13 allows 200 or 204;
//     204 with no body is the lightest response and matches what most
//     server-side fixtures expect from subscribers.)
//
// The dispatcher is invoked synchronously before responding so that any
// observable side effects (in tests) have completed before the client
// sees the 204. Heavy work belongs in a goroutine the dispatcher spawns
// itself.
func (r *NotifyReceiver) notifyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// IEEE 2030.5 §10 / CSIP §6.6 mandates application/sep+xml as
		// the canonical content type for the EXI-or-XML resource
		// payload. Accept the bare value; ignore trailing params like
		// charset since the spec doesn't require us to police them.
		ct := req.Header.Get("Content-Type")
		if ct != contentTypeSEPXML &&
			ct != contentTypeSEPXML+"; charset=utf-8" &&
			ct != contentTypeSEPXML+";charset=utf-8" {
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}

		body, err := io.ReadAll(io.LimitReader(req.Body, notifyMaxBodyBytes+1))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if len(body) > notifyMaxBodyBytes {
			http.Error(w, "body too large", http.StatusBadRequest)
			return
		}
		if len(body) == 0 {
			http.Error(w, "empty body", http.StatusBadRequest)
			return
		}

		var n sep2.Notification
		if err := xml.Unmarshal(body, &n); err != nil {
			http.Error(w, "malformed XML", http.StatusBadRequest)
			return
		}
		// Sanity check: an empty Notification (all default fields) is
		// almost certainly a unmarshal-into-wrong-type situation. The
		// spec doesn't strictly require subscribedResource to be set,
		// but in practice every CORE-018 notification carries one.
		if n.SubscribedResource == "" && n.NewResourceURI == "" && n.Status == 0 {
			http.Error(w, "notification missing subscribedResource and newResourceURI", http.StatusBadRequest)
			return
		}

		r.dispatcher(req.Context(), n)

		w.WriteHeader(http.StatusNoContent)
	}
}
