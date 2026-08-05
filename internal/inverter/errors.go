package inverter

import (
	"errors"
	"fmt"
	"net/http"
)

// HTTP response-code router typed errors (IEEE-046, plan-1 Phase 7 entry).
//
// classifyResponse maps non-2xx HTTP responses returned by (*SEP2Client).Get /
// Post / Put to these sentinels (or to a *MovedError on a 3xx redirect) so
// callers can pattern-match with errors.Is / errors.As instead of re-parsing
// status codes. CSIP V1.2 GEN.037..GEN.049 require a DER Client to *process*
// each of these codes per spec semantics; this is the typed-error half of
// that work : caller behavior changes (graceful bypass replacing log.Fatalf,
// 301 follow with cached-href update) ship in IEEE-048 and IEEE-047.
//
// 5xx maps to the pre-existing ErrResponseTransient, so callers
// already pattern-matching that sentinel continue to work unchanged.
//
// Semantic vs HTTP not-found: the pre-existing ErrEndDeviceNotFound
// signals that a server's EndDeviceList does not contain the
// client's LFDI : an application-level absence on a 200-OK response, not a
// 404. It is deliberately *not* unified with ErrNotFound below; callers that
// idle-poll for provisioning must continue to distinguish "list returned but
// I am not in it" from "endpoint returned 404."
var (
	// ErrBadRequest is returned when the server responds with 400 Bad Request.
	// Per CSIP V1.2 §3.2.1 GEN.043 the request is malformed; retrying with
	// the same payload will not help. Callers should log and bypass.
	ErrBadRequest = errors.New("bad request (400)")

	// ErrNotFound is returned when the server responds with 404 Not Found.
	// For optional function sets (LogEventList, MirrorUsagePoint, subscription)
	// callers should bypass with a warning; for essential function sets
	// (RegistrationLink in --csip strict mode) callers should bail. Wiring is
	// IEEE-048's scope; this ticket only surfaces the typed error.
	ErrNotFound = errors.New("not found (404)")

	// ErrMethodNotAllowed is returned when the server responds with 405
	// Method Not Allowed. Per CSIP V1.2 §3.2.1 GEN.045 the response should
	// also carry an Allow header listing the supported verbs. Callers should
	// log a clear spec-mismatch warning and bypass the affected function set.
	ErrMethodNotAllowed = errors.New("method not allowed (405)")

	// ErrNotImplemented is returned when the server responds with 501 Not
	// Implemented. Per CSIP V1.2 §3.2.1 GEN.048 the server does not support
	// the requested resource. Callers should log and bypass.
	ErrNotImplemented = errors.New("not implemented (501)")

	// ErrLogEventLinkAbsent is returned by PostLogEvent when the caller
	// passes an empty logEventListHref. CSIP V1.2 BASIC-027 declares the
	// LogEvent function set OPTIONAL : when an EndDevice does not advertise
	// a LogEventListLink the alarm-class emitter must bypass with a warning,
	// not abort. Callers branch on errors.Is(err, ErrLogEventLinkAbsent) to
	// distinguish the "server gap" case from real POST failures. See
	// IEEE-053 (Plan-1 Phase 9 entry) and IEEE-054 (wiring).
	ErrLogEventLinkAbsent = errors.New("LogEventListLink absent")

	// ErrRateLimited is returned by PostLogEvent when the configured
	// LogEventRateLimiter denies the supplied logEventCode (e.g. the same
	// trip fired twice within the rate-limit window). The emitter performs
	// no HTTP traffic on a deny. Callers branch on errors.Is(err,
	// ErrRateLimited) to silently drop the duplicate without escalating.
	// The default (nil-limiter) policy is allow-all; the concrete limiter
	// implementation is owned by IEEE-054.
	ErrRateLimited = errors.New("LogEvent rate limited")
)

// MovedError signals a 3xx redirect (301/302/307/308). Carries the Location
// header so a caller can re-issue the request against the new URL and, in
// the case of 301 Moved Permanently, update its cached href so subsequent
// traversals use the new path.
//
// Only 301 handling is in IEEE-047's scope. The struct covers the broader
// redirect family because the stdlib http.Client otherwise treats them
// alike : disabling auto-follow on the client surfaces every redirect class
// through this type, and callers can pattern-match via the Status field
// when 302/307/308 needs distinct behavior.
type MovedError struct {
	// Location is the value of the Location header on the redirect
	// response. Empty when the server returned a redirect without a
	// Location header (a spec violation, but observed in the wild).
	Location string

	// Status is the redirect status code (301, 302, 307, 308). Callers
	// that need to discriminate (e.g. cache-update-on-301 vs single-hop-on-307)
	// switch on this field.
	Status int
}

func (e *MovedError) Error() string {
	if e.Location == "" {
		return fmt.Sprintf("moved (%d) with no Location header", e.Status)
	}
	return fmt.Sprintf("moved (%d) to %s", e.Status, e.Location)
}

// classifyResponse maps an *http.Response status code to the appropriate
// typed error, or returns nil for the success codes (200, 201, 204) that
// (*SEP2Client).Get / Post / Put treat as happy paths.
//
// The function does NOT close resp.Body or read from it : callers retain
// full ownership of the response. The returned error is always wrapped with
// the method-and-URL context at the call site (so callers reading the error
// message see "GET /edev: not found (404)" rather than just "not found").
//
// Status code mapping (CSIP V1.2 §3.2.1 GEN.037..GEN.049):
//
//	200 OK              → nil (GET happy path)
//	201 Created         → nil (POST/PUT created resource; caller reads Location)
//	204 No Content      → nil (POST/PUT succeeded, no body)
//	301 Moved Permanently → *MovedError{Status: 301, Location: <header>}
//	302/307/308          → *MovedError (general redirect; only 301 has follow
//	                       logic in IEEE-047, but the type surfaces all four)
//	400 Bad Request     → ErrBadRequest
//	404 Not Found       → ErrNotFound
//	405 Method Not Allowed → ErrMethodNotAllowed
//	501 Not Implemented → ErrNotImplemented
//	5xx (other)         → ErrResponseTransient (IEEE-043 sentinel reused)
//	other 4xx           → bare "unexpected client error %d"
//	other 1xx/3xx       → bare "unexpected status %d"
//
// resp must be non-nil; passing nil panics (caller bug).
func classifyResponse(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return &MovedError{
			Location: resp.Header.Get("Location"),
			Status:   resp.StatusCode,
		}
	case http.StatusBadRequest:
		return ErrBadRequest
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusMethodNotAllowed:
		return ErrMethodNotAllowed
	case http.StatusNotImplemented:
		return ErrNotImplemented
	}
	if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		return fmt.Errorf("status %d: %w", resp.StatusCode, ErrResponseTransient)
	}
	if resp.StatusCode >= 400 && resp.StatusCode <= 499 {
		return fmt.Errorf("unexpected client error %d", resp.StatusCode)
	}
	return fmt.Errorf("unexpected status %d", resp.StatusCode)
}
