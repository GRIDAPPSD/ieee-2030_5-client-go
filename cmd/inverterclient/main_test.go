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
