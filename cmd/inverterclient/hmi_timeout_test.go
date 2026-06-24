package main

import (
	"testing"
	"time"
)

// TestHMIServerTimeoutsSet asserts that the HMI http.Server has all four
// timeout fields set to non-zero values. Zero means "no limit" : a
// Slowloris / slow-body attack surface even on a local dashboard port.
func TestHMIServerTimeoutsSet(t *testing.T) {
	srv := newHMIServer(8888)
	if srv.ReadHeaderTimeout == 0 {
		t.Error("hmiServer: ReadHeaderTimeout is 0 (no limit)")
	}
	if srv.ReadTimeout == 0 {
		t.Error("hmiServer: ReadTimeout is 0 (no limit)")
	}
	if srv.WriteTimeout == 0 {
		t.Error("hmiServer: WriteTimeout is 0 (no limit)")
	}
	if srv.IdleTimeout == 0 {
		t.Error("hmiServer: IdleTimeout is 0 (no limit)")
	}
}

// TestHMIServerTimeoutConstants asserts the named constants are positive.
func TestHMIServerTimeoutConstants(t *testing.T) {
	for name, d := range map[string]time.Duration{
		"hmiReadHeaderTimeout": hmiReadHeaderTimeout,
		"hmiReadTimeout":       hmiReadTimeout,
		"hmiWriteTimeout":      hmiWriteTimeout,
		"hmiIdleTimeout":       hmiIdleTimeout,
	} {
		if d <= 0 {
			t.Errorf("%s must be positive, got %v", name, d)
		}
		if d < time.Second {
			t.Errorf("%s (%v) is below the 1-second sanity floor", name, d)
		}
	}
}
