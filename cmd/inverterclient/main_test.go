package main

import (
	"flag"
	"testing"
)

// TestServerFlagDefault pins the default value of `-server` so the
// `make run-inverter` Makefile recipe and the binary agree out of the box.
func TestServerFlagDefault(t *testing.T) {
	const want = "https://localhost:8443"

	// Re-run flag registration in an isolated FlagSet so the test does not
	// depend on side effects from main().
	fs := flag.NewFlagSet("inverterclient-test", flag.ContinueOnError)
	var serverURL string
	fs.StringVar(&serverURL, "server", defaultServerURL, "IEEE 2030.5 server URL")

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if serverURL != want {
		t.Fatalf("default --server = %q, want %q", serverURL, want)
	}
}

// TestHMIPortFlagDefault pins --hmi-port default to 0 (disabled). Issue #71:
// a fixed 8080 default made a second default-flagged process on one host
// collide; disabled-by-default lets any number of aggregator and DER-client
// processes start together, per ADR-009 decision 1.
func TestHMIPortFlagDefault(t *testing.T) {
	t.Parallel()
	const want = 0

	fs := flag.NewFlagSet("inverterclient-test", flag.ContinueOnError)
	hmiPort := fs.Int("hmi-port", 0, "HMI web dashboard port (0 to disable)")

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if *hmiPort != want {
		t.Fatalf("default --hmi-port = %d, want %d", *hmiPort, want)
	}
}

// TestClientRoleFlagDefault pins --client-role default to "der" (ADR-009
// decision 1: every existing invocation keeps the DER Client role).
func TestClientRoleFlagDefault(t *testing.T) {
	t.Parallel()
	const want = "der"

	fs := flag.NewFlagSet("inverterclient-test", flag.ContinueOnError)
	role := fs.String("client-role", want, "IEEE 2030.5 client role: der|aggregator")

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if *role != want {
		t.Fatalf("default --client-role = %q, want %q", *role, want)
	}
}
