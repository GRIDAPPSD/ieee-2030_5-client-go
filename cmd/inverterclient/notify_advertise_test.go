package main

import "testing"

func TestValidateNotifyAdvertiseHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty: valid, falls back to bound address", "", false},
		{"bare host: valid, port comes from the listener", "example.org", false},
		{"host:port: valid, full override", "example.org:8444", false},
		{"IP:port: valid", "203.0.113.7:8444", false},
		{"bracketed IPv6, no port: valid, bare host", "[fe80::1]", false},
		{"bracketed IPv6:port: valid, full override", "[fe80::1]:8444", false},
		{"unbracketed IPv6, no port: valid, bare host", "fe80::1", false},
		{"scheme prefix rejected: not a URL", "https://example.org:8444", true},
		{"scheme prefix rejected, no port", "http://example.org", true},
		{"malformed host:port rejected", "example.org:not-a-port", true},
		// Fix-round-3 finding 6: a path or query character makes this not
		// a host at all, whatever SplitHostPort's own rules say.
		{"path suffix rejected", "evil.example/x", true},
		{"query suffix rejected", "evil.example/x?y=", true},
		{"userinfo prefix rejected", "evil@host", true},
		{"backslash rejected", `evil.example\x`, true},
		{"embedded space rejected", "evil example", true},
	}
	for _, tt := range tests {
		err := validateNotifyAdvertiseHost(tt.in)
		if tt.wantErr && err == nil {
			t.Errorf("%s: validateNotifyAdvertiseHost(%q): want error, got nil", tt.name, tt.in)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("%s: validateNotifyAdvertiseHost(%q): want nil, got %v", tt.name, tt.in, err)
		}
	}
}

func TestEffectiveNotifyAdvertiseHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		advertiseHost string
		boundAddr     string
		want          string
	}{
		{"empty advertise host: bound address unchanged", "", "127.0.0.1:54321", "127.0.0.1:54321"},
		{"bare host: combined with the bound port", "example.org", "127.0.0.1:54321", "example.org:54321"},
		{"host:port: full override, bound port ignored", "example.org:8444", "127.0.0.1:54321", "example.org:8444"},
		// Fix-round-3 finding 6: a bracketed IPv6 literal must not be
		// double-bracketed when combined with the bound port.
		{"bracketed IPv6, no port: single-bracketed result", "[fe80::1]", "127.0.0.1:54321", "[fe80::1]:54321"},
		{"unbracketed IPv6, no port: bracketed result", "fe80::1", "127.0.0.1:54321", "[fe80::1]:54321"},
		{"bracketed IPv6:port: full override, verbatim", "[fe80::1]:8444", "127.0.0.1:54321", "[fe80::1]:8444"},
	}
	for _, tt := range tests {
		got := effectiveNotifyAdvertiseHost(tt.advertiseHost, tt.boundAddr)
		if got != tt.want {
			t.Errorf("%s: effectiveNotifyAdvertiseHost(%q, %q) = %q, want %q",
				tt.name, tt.advertiseHost, tt.boundAddr, got, tt.want)
		}
	}
}

func TestIsUnreachableAdvertiseHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		hostport string
		want     bool
	}{
		{"127.0.0.1:54321", true},
		{"127.0.0.1", true},
		{"localhost:54321", true},
		{"::1", true},
		{"[::1]:54321", true},
		{"example.org:54321", false},
		{"203.0.113.7:54321", false},
		// Fix-round-3 finding 6: the unspecified / any-interface address
		// is exactly as unreachable as loopback, as a destination.
		{"0.0.0.0:54321", true},
		{"0.0.0.0", true},
		{"[::]:54321", true},
		{"::", true},
	}
	for _, tt := range tests {
		if got := isUnreachableAdvertiseHost(tt.hostport); got != tt.want {
			t.Errorf("isUnreachableAdvertiseHost(%q) = %v, want %v", tt.hostport, got, tt.want)
		}
	}
}

// TestEffectiveNotifyAdvertiseHost_DefaultListenIsLoopback is fix-round-2
// finding 3's exact reported scenario: --notify-listen at 127.0.0.1:0 (the
// default) with no --notify-advertise-host must resolve to a loopback
// address, so the warning fires.
func TestEffectiveNotifyAdvertiseHost_DefaultListenIsLoopback(t *testing.T) {
	t.Parallel()
	got := effectiveNotifyAdvertiseHost("", "127.0.0.1:54321")
	if !isUnreachableAdvertiseHost(got) {
		t.Errorf("default --notify-listen bound address %q: want isUnreachableAdvertiseHost true, got false", got)
	}
}
