package inverter_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"encoding/xml"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	certs "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2cert"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	gotls "gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2tls/gotls"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// TestInverterAcceptsServerWithCriticalHardwareModuleSAN reproduces the
// IEEE-027 failure mode: a strict CSIP server presents a cert that carries
// the IEEE 2030.5 §6.11 device-profile critical HardwareModuleName SAN
// (otherName OID 1.3.6.1.5.5.7.8.4).
//
// Without a peer-cert verify hook, stdlib's x509.Verify lists the SAN OID in
// UnhandledCriticalExtensions and the handshake fails with
// `x509: unhandled critical extension`. This is exactly what Craig hit
// against the strict-CSIP server at 192.168.150.213:8888 right after IEEE-019
// landed; the IEEE-019 fixture used SAN-less server certs and missed it.
//
// RED before scope items 1-3 of IEEE-027: client errors with
// `unhandled critical extension`. GREEN once VerifyPeerCertificate is wired
// to the existing internal/tls verify helper: handshake completes and /dcap
// returns.
func TestInverterAcceptsServerWithCriticalHardwareModuleSAN(t *testing.T) {
	env := newHMNServerEnv(t)

	serverURL, stop := startGotlsListenerWithCert(t, env, []uint16{
		gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8,
		gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
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
		t.Fatalf("Discover against server with critical HMN SAN: %v", err)
	}
	if dcap.PollRate != 30 {
		t.Errorf("PollRate = %d, want 30", dcap.PollRate)
	}
}

// hmnServerEnv is the test fixture for a gotls server whose cert carries the
// critical HardwareModuleName SAN that stdlib does not parse. Device cert is
// a vanilla CSIP device cert from internal/certs; the server cert is the
// hand-built one in serverCertWithHMNSAN.
type hmnServerEnv struct {
	serverCertPath string
	serverKeyPath  string
	deviceCertPath string
	deviceKeyPath  string
	caCertPath     string
}

func newHMNServerEnv(t *testing.T) *hmnServerEnv {
	t.Helper()

	caCertPEM, caKeyPEM, err := certs.GenerateCA(certs.CAOptions{
		CommonName: "IEEE-027 Test CA",
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

	serverCertPEM, serverKeyPEM, err := serverCertWithHMNSAN(caCert, caKey, "IEEE-027-SRV-001")
	if err != nil {
		t.Fatalf("build server cert with HMN SAN: %v", err)
	}

	deviceCertPEM, deviceKeyPEM, err := certs.GenerateDeviceCert(caCert, caKey, certs.DeviceCertOptions{
		DeviceType:  certs.DeviceTypeGeneric,
		HWSerialNum: "IEEE-027-DEV-001",
	})
	if err != nil {
		t.Fatalf("generate device cert: %v", err)
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

	return &hmnServerEnv{
		serverCertPath: srvCert,
		serverKeyPath:  srvKey,
		deviceCertPath: devCert,
		deviceKeyPath:  devKey,
		caCertPath:     caPath,
	}
}

// serverCertWithHMNSAN builds a server certificate that mirrors the wire-level
// profile of an IEEE 2030.5 §6.11 / CSIP §6.2 device cert: empty Subject and
// a critical SubjectAlternativeName whose only entry is an RFC 4108 §5
// HardwareModuleName otherName. ExtKeyUsage is set to ServerAuth (and
// ClientAuth, matching the production server cert) so the gotls server can
// present it. The SAN is deliberately otherName-only — stdlib's SAN parser
// only adds the SAN OID to UnhandledCriticalExtensions when it parsed
// zero of {DNSNames, EmailAddresses, IPAddresses, URIs} (parser.go ~L728), so
// mixing a known form (DNS/IP) with the otherName would mask the failure.
//
// This re-implements the inner ASN.1 marshaling that internal/certs hides
// behind unexported helpers; the IEEE-027 scope explicitly rules out exporting
// those helpers, and the test only needs to produce a wire-equivalent cert.
func serverCertWithHMNSAN(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, hwSerial string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, nil, err
	}
	if serial.Sign() == 0 {
		// rand.Int can return 0 with vanishing probability; sidestep
		// the zero-serial-not-RFC-5280-compliant edge.
		serial = big.NewInt(1)
	}

	sanExt, err := buildHardwareModuleNameOnlySAN(certs.OIDIeee20305, hwSerial)
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyAgreement,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		ExtraExtensions:       []pkix.Extension{sanExt},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// buildHardwareModuleNameOnlySAN constructs a critical SubjectAlternativeName
// extension whose only entry is an RFC 4108 §5 HardwareModuleName otherName
// carrying (hwType, hwSerial). Mirrors internal/certs/generate.go
// buildHardwareModuleNameSAN — if that production marshaler's struct shape
// changes, this test should be updated in lockstep.
func buildHardwareModuleNameOnlySAN(hwType asn1.ObjectIdentifier, hwSerial string) (pkix.Extension, error) {
	type hardwareModuleName struct {
		HWType      asn1.ObjectIdentifier
		HWSerialNum asn1.RawValue
	}
	hmn := hardwareModuleName{
		HWType: hwType,
		HWSerialNum: asn1.RawValue{
			Class: asn1.ClassUniversal,
			Tag:   asn1.TagOctetString,
			Bytes: []byte(hwSerial),
		},
	}
	hmnBytes, err := asn1.Marshal(hmn)
	if err != nil {
		return pkix.Extension{}, err
	}
	otherName := struct {
		TypeID asn1.ObjectIdentifier
		Value  asn1.RawValue
	}{
		TypeID: certs.OIDHardwareModuleName,
		Value: asn1.RawValue{
			Class:      asn1.ClassContextSpecific,
			Tag:        0,
			IsCompound: true,
			Bytes:      hmnBytes,
		},
	}
	otherNameBytes, err := asn1.MarshalWithParams(otherName, "tag:0")
	if err != nil {
		return pkix.Extension{}, err
	}

	val, err := asn1.Marshal([]asn1.RawValue{{FullBytes: otherNameBytes}})
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{
		Id:       certs.OIDSubjectAltName,
		Critical: true,
		Value:    val,
	}, nil
}

// startGotlsListenerWithCert boots a gotls-backed HTTPS server using the
// fixture's hand-built server cert (critical HMN SAN). No client-verify hook
// is installed because IEEE-027 is about the client's view of the server
// cert — peer client auth is not the test surface here.
func startGotlsListenerWithCert(t *testing.T, env *hmnServerEnv, cipherSuites []uint16) (serverURL string, stop func()) {
	t.Helper()

	certPEM, err := os.ReadFile(env.serverCertPath)
	if err != nil {
		t.Fatalf("read server cert: %v", err)
	}
	keyPEM, err := os.ReadFile(env.serverKeyPath)
	if err != nil {
		t.Fatalf("read server key: %v", err)
	}
	gotlsCert, err := gotls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	cfg := &gotls.Config{
		Certificates:     []gotls.Certificate{gotlsCert},
		MinVersion:       gotls.VersionTLS12,
		MaxVersion:       gotls.VersionTLS12,
		CipherSuites:     cipherSuites,
		CurvePreferences: []gotls.CurveID{gotls.CurveP256},
		ClientAuth:       gotls.NoClientCert,
	}

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	tlsL := gotls.NewListener(tcpL, cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/dcap", func(w http.ResponseWriter, _ *http.Request) {
		dcap := sep2.DeviceCapability{PollRate: 30}
		w.Header().Set("Content-Type", "application/sep+xml")
		_ = xml.NewEncoder(w).Encode(&dcap)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})

	return "https://" + tlsL.Addr().String(), func() {
		_ = srv.Close()
		_ = tlsL.Close()
	}
}
