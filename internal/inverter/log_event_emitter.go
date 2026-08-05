// Package inverter : LogEvent emitter primitive (plan-1 Phase 9 entry).
// See GRIDAPPSD/ieee-2030_5-server-go#189.
//
// CSIP V1.2 BASIC-027 (Alarms, pp 139-140) requires the DER Client to POST
// LogEvent resources to the server when alarm-class state transitions
// occur (LVRT trip, HVRT trip, freq-watt curtailment, inverter offline,
// manufacturer-specific faults). The server-side handler already exists
// (internal/handler/log_event.go) and is unit-tested; this file adds the
// outbound primitive so the alarm callers can fire-and-document.
//
// This file ships:
//  1. PostLogEvent : the HTTP primitive. Returns the new resource href on
//     201 Created, or a wrapped error (typed sentinels where useful).
//  2. PEN plumbing : SimConfig.LogEventPEN -> SEP2Client.pen -> stamped into
//     the outgoing LogEvent if the caller left logEventPEN zero.
//  3. logEventLimiter seam : a concrete throttle plugs in here; the
//     default nil limiter allows every POST through.
//  4. Reuse of (*SEP2Client).Now() (the server-synced clock) : the
//     emitter does NOT synthesize createdDateTime; it only fills in zero
//     values left by the caller as a convenience so the alarm sites can
//     stay terse.
//
// Out of scope:
//   - Wiring trip / curtailment detection sites to call PostLogEvent.
//   - The rate-limiter implementation itself; this file only defines the
//     interface.
//   - The logEventCode → trip-type mapping.

package inverter

import (
	"context"
	"errors"
	"fmt"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// LogEventRateLimiter is the rate-limit seam consulted by PostLogEvent.
//
// Allow returns true when the caller is permitted to POST a LogEvent
// carrying the supplied logEventCode, false to deny. The emitter performs
// no HTTP traffic on a deny and returns ErrRateLimited.
//
// The interface is intentionally tiny : one method, no setup, no
// teardown : the concrete implementation can ship a simple in-memory ring buffer keyed by
// code (1 LogEvent per code per minute, the BASIC-027 trip-flap mitigation)
// without dragging context, cancellation, or persistence into the seam.
// Per the Pike rule "accept interfaces, return concrete types," this
// interface is declared at the consumer (inverter package, where
// PostLogEvent calls it) : the concrete struct just satisfies it implicitly.
type LogEventRateLimiter interface {
	Allow(logEventCode uint8) bool
}

// PostLogEvent submits a LogEvent to the server's LogEvent list and
// returns the new resource's href (Location header on 201 Created).
//
// logEventListHref is the path published by the server in
// EndDevice.LogEventListLink (typically "/edev/{id}/lel"). The caller
// sources it via link-derivation. An empty string returns
// ErrLogEventLinkAbsent without making any HTTP request : LogEvent is an
// OPTIONAL function set, and a missing link is a server gap to be logged
// and bypassed, not a fatal.
//
// The caller-supplied event is mutated for two zero-value conveniences:
//
//  1. evt.CreatedDateTime == 0 -> filled in from c.Now() (the
//     server-synced clock, falling back to time.Now() before first sync).
//     Callers that need a specific timestamp set it explicitly.
//
//  2. evt.LogEventPEN == 0 → filled in from c.PEN() (SimConfig.LogEventPEN
//     captured at construction). When both are zero the LogEvent is
//     published with PEN=0 (no manufacturer namespace) which is acceptable
//     for test / interop but not for production; operators must register
//     a PEN with IANA and pass --pen at startup.
//
// FunctionSet, LogEventCode, LogEventID, ProfileID, Details, and
// ExtendedData are caller-controlled : the emitter does not synthesize
// them. The mutation is intentional so the alarm callers can pass
// a partially-filled event and rely on the emitter for the time + PEN.
//
// On 201 Created the server returns the new LogEvent href in the Location
// header; PostLogEvent returns that value. Callers may persist or log it.
//
// On 405 Method Not Allowed the server doesn't support LogEvent submission;
// PostLogEvent returns the wrapped ErrMethodNotAllowed (via classifyResponse).
// Callers match with errors.Is(err, ErrMethodNotAllowed) and continue :
// alarm reporting is graceful-bypass, not fatal.
//
// On a rate-limit deny (configured via SetLogEventRateLimiter) PostLogEvent
// returns ErrRateLimited and makes no HTTP request. Default (nil limiter)
// allows every POST through.
//
// On any other status the wrapped error from the underlying Post helper
// is returned. CSIP V1.2 BASIC-027 graceful-degradation: a failed POST
// must NOT crash the inverter.
//
// 301 follow is inherited from (*SEP2Client).Post : a redirect on the
// LogEventList path is followed once.
func (c *SEP2Client) PostLogEvent(
	ctx context.Context,
	logEventListHref string,
	evt sep2.LogEvent,
) (newLogEventHref string, err error) {
	if logEventListHref == "" {
		return "", ErrLogEventLinkAbsent
	}

	if c.logEventLimiter != nil && !c.logEventLimiter.Allow(evt.LogEventCode) {
		return "", fmt.Errorf("PostLogEvent code=%d: %w", evt.LogEventCode, ErrRateLimited)
	}

	// Zero-value conveniences: caller can leave CreatedDateTime / LogEventPEN
	// unset and the emitter will fill them in. This keeps the alarm-site
	// call sites terse while preserving the ability to set an
	// explicit timestamp (e.g. tests pinning time.Time) or override the PEN.
	if evt.CreatedDateTime == 0 {
		evt.CreatedDateTime = c.Now().Unix()
	}
	if evt.LogEventPEN == 0 {
		evt.LogEventPEN = c.pen
	}

	location, _, postErr := c.Post(ctx, logEventListHref, &evt)
	if postErr != nil {
		// errors.Is preserves the typed sentinel through the wrapper so
		// callers can pattern-match ErrMethodNotAllowed / ErrBadRequest /
		// ErrResponseTransient (5xx).
		return "", fmt.Errorf("POST %s logEvent code=%d: %w", logEventListHref, evt.LogEventCode, postErr)
	}
	return location, nil
}

// Compile-time check that ErrLogEventLinkAbsent / ErrRateLimited are
// distinct sentinels (not aliases). Catches an accidental "var Err... =
// Err..." regression on review.
var _ = func() bool {
	return !errors.Is(ErrLogEventLinkAbsent, ErrRateLimited) &&
		!errors.Is(ErrRateLimited, ErrLogEventLinkAbsent)
}
