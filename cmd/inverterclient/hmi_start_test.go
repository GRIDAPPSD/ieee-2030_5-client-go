package main

import (
	"net"
	"testing"
)

// TestListenHMI_ReportsRealBindAddress is fix-round-1 finding 6: the
// dashboard must log the address the listener actually bound to, not a
// reconstruction of the configured port. Binding to ":0" (a free port
// chosen by the OS) proves the returned listener's Addr() carries the real
// resolved port, which a guessed "http://localhost:<configured-port>"
// string could not for a port the caller left to the OS.
func TestListenHMI_ReportsRealBindAddress(t *testing.T) {
	t.Parallel()
	ln, err := listenHMI(":0")
	if err != nil {
		t.Fatalf("listenHMI(:0): %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("Addr() = %q: not a host:port: %v", addr, err)
	}
	if port == "0" || port == "" {
		t.Errorf("Addr() = %q: want a real resolved port, not the placeholder", addr)
	}
}

// TestListenHMI_OccupiedPortFails proves listenHMI fails loud (an error,
// not a panic or a silent no-op) rather than the caller believing an
// unbound listener came up. It is the control for the graceful-bypass
// caller in main(): a failure here is what main() must detect and log
// instead of claiming a bind address it does not have.
func TestListenHMI_OccupiedPortFails(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup: listen 127.0.0.1:0: %v", err)
	}
	defer taken.Close()

	if _, err := listenHMI(taken.Addr().String()); err == nil {
		t.Errorf("listenHMI(%s) on an already-bound address: want error, got nil", taken.Addr().String())
	}
}
