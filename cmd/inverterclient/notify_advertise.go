// --notify-advertise-host validation and resolution. Fix-round-2 finding
// 3: the flag was accepted unvalidated, never combined with the port the
// listener actually bound (so a host-only override silently discarded a
// real port), and nothing warned when the address a remote server would
// call back on is loopback-only, as it is under the default
// --notify-listen=127.0.0.1:0.

package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// splitNumericHostPort wraps net.SplitHostPort with a numeric-port check:
// SplitHostPort itself accepts any non-empty port string ("example.org:abc"
// splits cleanly with port "abc"), so a caller that wants an actual
// listenable address needs this stricter form.
func splitNumericHostPort(hostport string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(hostport)
	if err != nil {
		return "", "", err
	}
	if n, perr := strconv.Atoi(port); perr != nil || n < 0 || n > 65535 {
		return "", "", fmt.Errorf("invalid port %q in %q", port, hostport)
	}
	return host, port, nil
}

// validateNotifyAdvertiseHost validates the raw --notify-advertise-host
// flag value at startup. Empty is valid (falls back to the bound
// listener's address). A value carrying a URL scheme (http://, https://)
// is rejected: the flag takes a bare host or host:port, not a URL. A
// value with a colon must parse as host:numeric-port; a value with no
// colon is accepted as a bare host (effectiveNotifyAdvertiseHost supplies
// the port from the bound listener).
func validateNotifyAdvertiseHost(v string) error {
	if v == "" {
		return nil
	}
	if strings.Contains(v, "://") {
		return fmt.Errorf("--notify-advertise-host must be host[:port], not a URL: %q", v)
	}
	_, _, err := splitNumericHostPort(v)
	if err == nil {
		return nil
	}
	var addrErr *net.AddrError
	if errors.As(err, &addrErr) && addrErr.Err == "missing port in address" {
		return nil // bare host: the bound listener's port is used.
	}
	return fmt.Errorf("--notify-advertise-host %q: %w", v, err)
}

// effectiveNotifyAdvertiseHost computes the host:port used in the notify
// URL. advertiseHost is the (already-validated) flag value; boundAddr is
// the listener's own Addr(). An advertiseHost naming its own numeric port
// is used verbatim (a full override). A bare-host advertiseHost is
// combined with boundAddr's port, so the operator overrides only the host
// a NAT'd or multi-homed deployment needs, without having to also track
// and repeat the ephemeral port a ":0" bind chose. Empty advertiseHost
// returns boundAddr unchanged (today's default behavior).
func effectiveNotifyAdvertiseHost(advertiseHost, boundAddr string) string {
	if advertiseHost == "" {
		return boundAddr
	}
	if _, _, err := splitNumericHostPort(advertiseHost); err == nil {
		return advertiseHost
	}
	_, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return advertiseHost // bound port unavailable; best effort.
	}
	return net.JoinHostPort(advertiseHost, port)
}

// isLoopbackHost reports whether hostport (a host, or a host:port) names a
// loopback address: 127.0.0.0/8, ::1, or the literal "localhost". Used to
// warn when the effective notify-advertise address cannot be reached by a
// server running on another host, the exact footgun the default
// --notify-listen=127.0.0.1:0 sets up silently.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
