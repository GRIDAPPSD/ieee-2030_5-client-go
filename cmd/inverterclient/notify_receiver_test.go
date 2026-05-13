package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/certs"
	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
)

// writeCertEnv writes a fresh CA + device cert under t.TempDir() and
// returns the SimConfig fields the helper expects. The CA + device cert
// pair is the minimum the IEEE-049 receiver needs to bind.
func writeCertEnv(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	caCertPEM, caKeyPEM, err := certs.GenerateCA(certs.CAOptions{
		CommonName: "Notify Test CA",
		ValidYears: 1,
	})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	caCert, err := certs.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM CA: %v", err)
	}
	caKey, err := certs.ParseKeyPEM(caKeyPEM)
	if err != nil {
		t.Fatalf("ParseKeyPEM CA: %v", err)
	}
	devCertPEM, devKeyPEM, err := certs.GenerateDeviceCert(caCert, caKey, certs.DeviceCertOptions{
		DeviceType:  certs.DeviceTypeGeneric,
		HWSerialNum: "NOTIFY-CMD-001",
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	tmp := t.TempDir()
	certFile = filepath.Join(tmp, "device.crt")
	keyFile = filepath.Join(tmp, "device.key")
	caFile = filepath.Join(tmp, "ca.crt")
	for _, w := range []struct {
		path string
		data []byte
	}{
		{certFile, devCertPEM},
		{keyFile, devKeyPEM},
		{caFile, caCertPEM},
	} {
		if err := os.WriteFile(w.path, w.data, 0o600); err != nil {
			t.Fatalf("write %s: %v", w.path, err)
		}
	}
	return certFile, keyFile, caFile
}

// TestStartNotifyReceiver_DisabledOnEmptyAddr asserts the disabled-on-empty
// contract: empty --notify-listen returns nil receiver without error and
// without touching cert files. The atomic op for IEEE-049 against an
// inverter that wants polling-only is "set SEP2_NOTIFY_LISTEN= (empty)
// and proceed."
func TestStartNotifyReceiver_DisabledOnEmptyAddr(t *testing.T) {
	t.Parallel()

	got := startNotifyReceiver(inverter.SimConfig{
		CertFile: filepath.Join(t.TempDir(), "missing.crt"),
		KeyFile:  filepath.Join(t.TempDir(), "missing.key"),
		CAFile:   filepath.Join(t.TempDir(), "missing.ca"),
	}, "", nil, nil)
	if got != nil {
		t.Fatalf("expected nil receiver for empty listen addr, got %+v", got)
	}
}

// TestStartNotifyReceiver_GracefulBypassOnBadCerts asserts the IEEE-048-
// style graceful-bypass posture: when cert load fails, the helper logs
// and returns nil. main() then proceeds with polling-only. This is the
// invariant the IEEE-049 ticket protects: an optional function set that
// can't initialize must NOT crash the inverter.
func TestStartNotifyReceiver_GracefulBypassOnBadCerts(t *testing.T) {
	t.Parallel()

	got := startNotifyReceiver(inverter.SimConfig{
		CertFile: filepath.Join(t.TempDir(), "missing.crt"),
		KeyFile:  filepath.Join(t.TempDir(), "missing.key"),
		CAFile:   filepath.Join(t.TempDir(), "missing.ca"),
	}, "127.0.0.1:0", nil, nil)
	if got != nil {
		// Defensive cleanup in case a future regression returns a real
		// receiver — don't leak a listener.
		_ = got.Stop(context.Background())
		t.Fatalf("expected nil receiver for bad cert paths, got %+v", got)
	}
}

// TestStartNotifyReceiver_HappyPath asserts the receiver actually starts
// and publishes its bound address to the HMI when cert env + listen addr
// are valid. Exercises the full wire: NewNotifyReceiver -> Start -> Addr
// -> HMI.SetNotifyAddr.
func TestStartNotifyReceiver_HappyPath(t *testing.T) {
	t.Parallel()

	certFile, keyFile, caFile := writeCertEnv(t)
	hmi := inverter.NewHMI()

	rcv := startNotifyReceiver(inverter.SimConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   caFile,
	}, "127.0.0.1:0", hmi, nil)
	if rcv == nil {
		t.Fatal("expected non-nil receiver for valid cert env")
	}
	defer func() { _ = rcv.Stop(context.Background()) }()

	if got := hmi.NotifyAddr(); got == "" {
		t.Error("HMI.NotifyAddr is empty; helper failed to publish bound address")
	}
}

// TestStartNotifyReceiver_AcceptsCustomDispatcher asserts the IEEE-051
// dispatcher seam: startNotifyReceiver forwards its dispatcher arg to
// NewNotifyReceiver instead of forcing the NoopNotificationDispatcher.
// This is the wiring point main() uses to install the real
// *PhaseStateDispatcher (which is no-op until Register* is called).
func TestStartNotifyReceiver_AcceptsCustomDispatcher(t *testing.T) {
	t.Parallel()

	certFile, keyFile, caFile := writeCertEnv(t)
	d := inverter.NewPhaseStateDispatcher()

	rcv := startNotifyReceiver(inverter.SimConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   caFile,
	}, "127.0.0.1:0", nil, d.Dispatch)
	if rcv == nil {
		t.Fatal("expected non-nil receiver")
	}
	defer func() { _ = rcv.Stop(context.Background()) }()
	// The receiver came up; that's the contract. Behavioral coverage of
	// dispatcher routing lives in internal/inverter/notification_dispatcher_test.go.
}

// TestStartNotifyReceiver_BindFailureBypass asserts that a bind failure
// (port already in use, malformed address) returns nil rather than
// crashing. The receiver-start helper is the only place in main()'s boot
// path that intentionally swallows TCP-bind errors — verify it.
func TestStartNotifyReceiver_BindFailureBypass(t *testing.T) {
	t.Parallel()

	certFile, keyFile, caFile := writeCertEnv(t)

	got := startNotifyReceiver(inverter.SimConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   caFile,
	}, "this-is-not-a-valid-address", nil, nil)
	if got != nil {
		_ = got.Stop(context.Background())
		t.Fatalf("expected nil receiver for malformed listen addr, got %+v", got)
	}
}
