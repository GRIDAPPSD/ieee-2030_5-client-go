// --notify-advertise-host validation and resolution. Fix-round-2 finding
// 3: the flag was accepted unvalidated, never combined with the port the
// listener actually bound (so a host-only override silently discarded a
// real port), and nothing warned when the address a remote server would
// call back on is loopback-only, as it is under the default
// --notify-listen=127.0.0.1:0. Fix-round-3 finding 6 tightened it further:
// a value carrying a path or query character was accepted as a "bare
// host", a bracketed IPv6 literal was double-bracketed when combined with
// the bound port, and an unspecified bind address (0.0.0.0, ::) drew no
// warning even though it is exactly as unreachable as loopback.

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

// notifyHostDelimiters are characters with no place in a bare host or a
// host:port value: URL path, query, fragment and userinfo delimiters, a
// backslash (some URL parsers treat it as a path separator), and a space.
// Their presence means v is not a host at all, whatever SplitHostPort
// makes of it: "evil.example/x?y=" and "evil@host/path" both split
// "cleanly" as a bare host under net.SplitHostPort's rules, which only
// look for a port after the last colon.
const notifyHostDelimiters = `/?#@\ `

// bareHostAndPort classifies v as either a full "host:numeric-port"
// (hasPort=true) or a bare host with no port (hasPort=false), and returns
// the host with any pre-existing IPv6 brackets stripped, so a caller
// combining it with a port via net.JoinHostPort does not double-bracket
// an operator-supplied literal such as "[fe80::1]". err is non-nil for a
// URL, a value carrying a path/query/userinfo character, or an invalid
// port; host and hasPort are meaningless when err is non-nil.
func bareHostAndPort(v string) (host string, hasPort bool, err error) {
	if strings.Contains(v, "://") {
		return "", false, fmt.Errorf("must be host[:port], not a URL: %q", v)
	}
	if strings.ContainsAny(v, notifyHostDelimiters) {
		return "", false, fmt.Errorf("must be a bare host or host:port: %q", v)
	}
	if h, _, serr := splitNumericHostPort(v); serr == nil {
		return h, true, nil
	}
	_, _, serr := net.SplitHostPort(v)
	var addrErr *net.AddrError
	if errors.As(serr, &addrErr) && (addrErr.Err == "missing port in address" || addrErr.Err == "too many colons in address") {
		// Bare host, no port: either SplitHostPort found no colon at all
		// (or only a bracketed host with nothing after it), or v is an
		// unbracketed IPv6 literal with more than one colon. Strip any
		// brackets the operator added so the caller re-adds them exactly
		// once via net.JoinHostPort.
		return strings.TrimSuffix(strings.TrimPrefix(v, "["), "]"), false, nil
	}
	return "", false, fmt.Errorf("%w", serr)
}

// validateNotifyAdvertiseHost validates the raw --notify-advertise-host
// flag value at startup. Empty is valid (falls back to the bound
// listener's address).
func validateNotifyAdvertiseHost(v string) error {
	if v == "" {
		return nil
	}
	if _, _, err := bareHostAndPort(v); err != nil {
		return fmt.Errorf("--notify-advertise-host %w", err)
	}
	return nil
}

// effectiveNotifyAdvertiseHost computes the host:port used in the notify
// URL. advertiseHost is the (already-validated) flag value; boundAddr is
// the listener's own Addr(). An advertiseHost naming its own numeric port
// is used verbatim (a full override). A bare-host advertiseHost is
// combined with boundAddr's port via net.JoinHostPort, which brackets an
// IPv6 literal itself: bareHostAndPort already stripped any brackets the
// operator supplied, so the result is bracketed exactly once. Empty
// advertiseHost returns boundAddr unchanged (today's default behavior).
func effectiveNotifyAdvertiseHost(advertiseHost, boundAddr string) string {
	if advertiseHost == "" {
		return boundAddr
	}
	host, hasPort, err := bareHostAndPort(advertiseHost)
	if err != nil {
		return advertiseHost // already validated at startup; defensive fallback.
	}
	if hasPort {
		return advertiseHost
	}
	_, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return advertiseHost // bound port unavailable; best effort.
	}
	return net.JoinHostPort(host, port)
}

// isUnreachableAdvertiseHost reports whether hostport (a host, or a
// host:port) names an address a remote server cannot use to reach this
// process: a loopback address (127.0.0.0/8, ::1, the literal
// "localhost"), or an unspecified / any-interface address (0.0.0.0, ::)
// that is meaningful only as a bind address, never as a destination one.
// Used to warn when the effective notify-advertise address cannot be
// reached by a server on another host, the exact footgun the default
// --notify-listen=127.0.0.1:0 sets up silently.
func isUnreachableAdvertiseHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}
