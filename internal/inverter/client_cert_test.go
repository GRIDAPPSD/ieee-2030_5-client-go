package inverter_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/certs"
	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
)

// TestNewSEP2Client_CertLoadErrors covers the cert-load failure modes for
// NewSEP2Client (IEEE-008). Prior to the fix, a missing or malformed cert
// file caused a nil-pointer panic because pem.Decode's nil return was
// dereferenced without a guard, and os.ReadFile's error was discarded.
//
// Each subtest mutates only the client cert file (CertFile) — the CA file
// and key file are always valid so we isolate failures to the cert read
// + PEM decode path under test.
func TestNewSEP2Client_CertLoadErrors(t *testing.T) {
	caCertPEM, caKeyPEM, err := certs.GenerateCA(certs.CAOptions{
		CommonName: "IEEE-008 Test CA",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}

	caCert, err := certs.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("parse CA cert PEM: %v", err)
	}
	caKey, err := certs.ParseKeyPEM(caKeyPEM)
	if err != nil {
		t.Fatalf("parse CA key PEM: %v", err)
	}

	deviceCertPEM, deviceKeyPEM, err := certs.GenerateDeviceCert(caCert, caKey, certs.DeviceCertOptions{
		DeviceType:  certs.DeviceTypeGeneric,
		HWSerialNum: "IEEE-008-TEST",
	})
	if err != nil {
		t.Fatalf("generate device cert: %v", err)
	}

	tmpDir := t.TempDir()
	caPath := filepath.Join(tmpDir, "ca.crt")
	keyPath := filepath.Join(tmpDir, "device.key")
	validCertPath := filepath.Join(tmpDir, "device.crt")
	missingCertPath := filepath.Join(tmpDir, "missing.crt")
	noBeginCertPath := filepath.Join(tmpDir, "no-begin.crt")
	truncatedCertPath := filepath.Join(tmpDir, "truncated.crt")

	mustWrite(t, caPath, caCertPEM)
	mustWrite(t, keyPath, deviceKeyPEM)
	mustWrite(t, validCertPath, deviceCertPEM)
	mustWrite(t, noBeginCertPath, []byte("this is not a PEM-encoded certificate at all\n"))
	// Truncated PEM: keep the BEGIN line so tls.LoadX509KeyPair sees a PEM
	// block boundary, then chop off the body so pem.Decode of the second
	// read returns nil. (LoadX509KeyPair is exercised first and may already
	// fail here; either way the cert path must not panic.)
	if i := strings.Index(string(deviceCertPEM), "\n"); i > 0 {
		mustWrite(t, truncatedCertPath, deviceCertPEM[:i+1])
	} else {
		mustWrite(t, truncatedCertPath, []byte("-----BEGIN CERTIFICATE-----\n"))
	}

	tests := []struct {
		name     string
		certFile string
		wantErr  bool
		wantSub  string // substring required in err.Error() when wantErr
	}{
		{
			name:     "missing cert file",
			certFile: missingCertPath,
			wantErr:  true,
			wantSub:  "missing.crt",
		},
		{
			name:     "malformed PEM (no BEGIN line)",
			certFile: noBeginCertPath,
			wantErr:  true,
			wantSub:  "no-begin.crt",
		},
		{
			name:     "truncated PEM",
			certFile: truncatedCertPath,
			wantErr:  true,
			wantSub:  "truncated.crt",
		},
		{
			name:     "valid PEM",
			certFile: validCertPath,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewSEP2Client panicked: %v", r)
				}
			}()

			client, err := inverter.NewSEP2Client(inverter.SimConfig{
				ServerURL: "https://localhost:0",
				CertFile:  tt.certFile,
				KeyFile:   keyPath,
				CAFile:    caPath,
			})

			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantSub)
				}
				if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("err = %q, want substring %q", err.Error(), tt.wantSub)
				}
				if client != nil {
					t.Error("want nil client on error, got non-nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if client == nil {
				t.Fatal("want non-nil client on success")
			}
			if client.SFDI() == "" || client.LFDI() == "" {
				t.Errorf("expected SFDI/LFDI to be derived, got SFDI=%q LFDI=%q", client.SFDI(), client.LFDI())
			}
		})
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
