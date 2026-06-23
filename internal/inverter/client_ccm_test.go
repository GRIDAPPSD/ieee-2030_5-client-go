package inverter_test

import (
	"context"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	certs "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2cert"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	sepTLS "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2tls"
	gotls "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2tls/gotls"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// ccmTestEnv is the shared TLS fixture for the CCM negotiation tests. It
// generates a self-signed CA + server cert + device cert into t.TempDir and
// writes both sides of the pair to disk so the listener and the inverter
// client can each load through the production paths (NewCCMServerConfig and
// NewSEP2Client respectively).
type ccmTestEnv struct {
	caPool *x509.CertPool

	serverCertPath string
	serverKeyPath  string
	deviceCertPath string
	deviceKeyPath  string
	caCertPath     string
}

func newCCMTestEnv(t *testing.T) *ccmTestEnv {
	t.Helper()

	caCertPEM, caKeyPEM, err := certs.GenerateCA(certs.CAOptions{
		CommonName: "CCM Test CA",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	caCert, err := certs.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caKey, err := certs.ParseKeyPEM(caKeyPEM)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}

	serverCertPEM, serverKeyPEM, err := certs.GenerateServerCert(caCert, caKey, certs.ServerCertOptions{
		Hosts:      []string{"127.0.0.1", "localhost"},
		CommonName: "CCM Test Server",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatalf("generate server cert: %v", err)
	}

	deviceCertPEM, deviceKeyPEM, err := certs.GenerateDeviceCert(caCert, caKey, certs.DeviceCertOptions{
		DeviceType:  certs.DeviceTypeGeneric,
		HWSerialNum: "CCM-TEST-001",
	})
	if err != nil {
		t.Fatalf("generate device cert: %v", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("append CA cert to pool")
	}

	tmpDir := t.TempDir()
	srvCert := filepath.Join(tmpDir, "server.crt")
	srvKey := filepath.Join(tmpDir, "server.key")
	devCert := filepath.Join(tmpDir, "device.crt")
	devKey := filepath.Join(tmpDir, "device.key")
	caPath := filepath.Join(tmpDir, "ca.crt")
	for _, w := range []struct {
		path string
		data []byte
	}{
		{srvCert, serverCertPEM},
		{srvKey, serverKeyPEM},
		{devCert, deviceCertPEM},
		{devKey, deviceKeyPEM},
		{caPath, caCertPEM},
	} {
		if err := os.WriteFile(w.path, w.data, 0o600); err != nil {
			t.Fatalf("write %s: %v", w.path, err)
		}
	}

	return &ccmTestEnv{
		caPool:         caPool,
		serverCertPath: srvCert,
		serverKeyPath:  srvKey,
		deviceCertPath: devCert,
		deviceKeyPath:  devKey,
		caCertPath:     caPath,
	}
}

// startGotlsListener boots a tiny gotls-backed HTTPS server with the supplied
// cipher list. It reuses the production NewCCMServerConfig so the verify hook
// that tolerates IEEE 2030.5 device certs' critical HardwareModuleName SAN is
// in play, then overrides the cipher suite list to control negotiation. It
// returns the listening URL and captures the cipher suite the most recent
// connection actually negotiated.
func startGotlsListener(t *testing.T, env *ccmTestEnv, cipherSuites []uint16) (serverURL string, negotiated *atomic.Uint32, stop func()) {
	t.Helper()

	cfg, err := sepTLS.NewCCMServerConfig(env.serverCertPath, env.serverKeyPath, env.caCertPath)
	if err != nil {
		t.Fatalf("NewCCMServerConfig: %v", err)
	}
	cfg.CipherSuites = cipherSuites

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	tlsL := gotls.NewListener(tcpL, cfg)

	negotiated = new(atomic.Uint32)

	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		dcap := sep2.DeviceCapability{PollRate: 30}
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&dcap)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if gc, ok := c.(*gotls.Conn); ok {
				// Force the handshake so the negotiated cipher is observable
				// before any request handlers run.
				if err := gc.Handshake(); err == nil {
					negotiated.Store(uint32(gc.ConnectionState().CipherSuite))
				}
			}
			return ctx
		},
	}

	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})

	return "https://" + tlsL.Addr().String(), negotiated, func() {
		_ = srv.Close()
		_ = tlsL.Close()
	}
}

// TestInverterNegotiatesCCM8 asserts the inverter client successfully
// completes a TLS handshake against a CCM-8-only gotls server and that the
// negotiated cipher suite is TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 (0xC0AE).
//
// RED: with the stdlib crypto/tls client this fails at handshake — stdlib
// does not implement CCM-8. GREEN: once the inverter client is on the
// vendored gotls stack, handshake completes and the cipher matches.
func TestInverterNegotiatesCCM8(t *testing.T) {
	env := newCCMTestEnv(t)

	serverURL, negotiated, stop := startGotlsListener(t, env, []uint16{
		gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8,
	})
	defer stop()

	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL: serverURL,
		CertFile:  env.deviceCertPath,
		KeyFile:   env.deviceKeyPath,
		CAFile:    env.caCertPath,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dcap, err := client.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if dcap.PollRate != 30 {
		t.Errorf("PollRate = %d, want 30", dcap.PollRate)
	}

	got := uint16(negotiated.Load())
	if got != gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 {
		t.Errorf("negotiated cipher = 0x%04X, want 0xC0AE (TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8)", got)
	}
}

// TestInverterStrictRejectsGCMOnlyServer asserts that when CSIPStrict is set
// the inverter client refuses to fall back to GCM and the handshake fails
// cleanly (an error from Do, not a panic or a hang).
func TestInverterStrictRejectsGCMOnlyServer(t *testing.T) {
	env := newCCMTestEnv(t)

	serverURL, _, stop := startGotlsListener(t, env, []uint16{
		// GCM only — no CCM-8 on offer. A strict CSIP client must NOT
		// negotiate.
		0xC02B, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
	})
	defer stop()

	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL:  serverURL,
		CertFile:   env.deviceCertPath,
		KeyFile:    env.deviceKeyPath,
		CAFile:     env.caCertPath,
		CSIPStrict: true,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.Discover(ctx)
	if err == nil {
		t.Fatal("Discover succeeded against GCM-only server in strict mode; want handshake failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Discover hung until context deadline: %v", err)
	}
}
