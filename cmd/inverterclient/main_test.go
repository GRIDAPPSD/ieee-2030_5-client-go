package main

import (
	"flag"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// newTestFlagSet registers every inverterclient flag through the real
// registerFlags production uses, into a fresh FlagSet and SimConfig, and
// parses no args (so every value is its default). Fix-round-1 finding 4:
// a test that hand-copies flag defaults into its own FlagSet cannot fail
// when the real default changes; a test that calls registerFlags can.
func newTestFlagSet(t *testing.T) (*inverter.SimConfig, *cliFlags) {
	t.Helper()
	fs := flag.NewFlagSet("inverterclient-test", flag.ContinueOnError)
	cfg := &inverter.SimConfig{}
	cf := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	return cfg, cf
}

// TestServerFlagDefault pins the default value of `-server` so the
// `make run-inverter` Makefile recipe and the binary agree out of the box.
func TestServerFlagDefault(t *testing.T) {
	t.Parallel()
	const want = "https://localhost:8443"
	cfg, _ := newTestFlagSet(t)
	if cfg.ServerURL != want {
		t.Fatalf("default --server = %q, want %q", cfg.ServerURL, want)
	}
}

// TestHMIPortFlagDefault pins --hmi-port default to 0 (disabled). Issue #71:
// a fixed 8080 default made a second default-flagged process on one host
// collide; disabled-by-default lets any number of aggregator and DER-client
// processes start together. Reading the real registerFlags means a mutant
// that reverts the default to 8080 fails this test; a hand-copied FlagSet
// could not have caught it.
func TestHMIPortFlagDefault(t *testing.T) {
	t.Parallel()
	const want = 0
	_, cf := newTestFlagSet(t)
	if *cf.HMIPort != want {
		t.Fatalf("default --hmi-port = %d, want %d", *cf.HMIPort, want)
	}
}

// TestClientRoleFlagDefault pins --client-role default to "der": every
// existing invocation keeps the DER Client role. Reading the real
// registerFlags means a mutant that flips the default to "aggregator"
// fails this test.
func TestClientRoleFlagDefault(t *testing.T) {
	t.Parallel()
	const want = "der"
	cfg, _ := newTestFlagSet(t)
	if cfg.ClientRole != want {
		t.Fatalf("default --client-role = %q, want %q", cfg.ClientRole, want)
	}
}
