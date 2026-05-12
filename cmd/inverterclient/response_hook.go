// Package main — IEEE-044 hook: wire IEEE-040 StateMachine transitions to
// IEEE-043 (*SEP2Client).PostResponse. Phase 6 ticket 2 of 3.
//
// On every state-machine transition the hook produced by responsePOSTHook:
//
//  1. Skips when the event has no replyTo, or is the synthetic
//     EVENT_COMPLETED → DEFAULT / EVENT_CANCELLED → DEFAULT auto-revert
//     (which carry evt == nil per the IEEE-040 contract).
//  2. Maps (prev, next) to an IEEE 2030.5-2023 §10.10 Table 31 Response
//     status (1/2/3/6) via mapTransitionToStatus. A return of 0 means "no
//     wire status applies to this edge" → skip.
//  3. Honors the event's responseRequired bitmap (Table 32) via
//     responseRequiredOn. A nil mask means "no opt-in" → skip.
//  4. Calls (*SEP2Client).PostResponse with a 30s context. PostResponse
//     already wraps a one-shot transient retry; IEEE-045 will replace
//     that with proper retry / backoff / dead-letter.
//
// The hook never blocks the state-machine Tick — Phase 6 entry's
// PostResponse is synchronous, but its 30s context bounds the call. If
// IEEE-045 needs async dispatch the wrapping happens there, not here.
//
// Pike rules satisfied:
//   - Errors are values: every PostResponse failure is wrapped with %w
//     into a log entry; no err is silently dropped.
//   - No goroutine leaks: this file spawns no goroutines.
//   - Scope: this file does NOT modify the StateMachine, the SEP2Client,
//     or PostResponse semantics. It is a leaf consumer.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// responsePOSTTimeout caps every PostResponse call from the hook. Sized
// well above IEEE-043's HTTP request timeout (currently ~10s) to allow the
// one-shot retry to complete without truncation, but bounded so a
// pathological server does not stall the hook fan-out forever.
const responsePOSTTimeout = 30 * time.Second

// responsePoster is the minimal SEP2Client surface the hook depends on.
// Defined at the consumer per Pike's interface-at-the-consumer rule;
// keeps the hook trivially testable with a httptest stub through
// inverter.NewSEP2Client OR a hand-rolled fake that satisfies this
// method set.
type responsePoster interface {
	PostResponse(ctx context.Context, replyToHref string, resp sep2.DERControlResponse) error
}

// nowFunc is the test seam for CreatedDateTime. Production passes
// time.Now (or the SEP2Client's Now() for time-sync alignment); tests
// inject a fixed clock so DERControlResponse.CreatedDateTime is
// deterministic without race-y comparisons.
type nowFunc func() time.Time

// responsePOSTHook returns a TransitionHook that emits a Response POST on
// every state-machine transition that maps to a Table 31 wire status AND
// is selected by the event's responseRequired bitmap.
//
// lfdi is the inverter's LFDI hex string (40 chars). It is embedded in
// each emitted DERControlResponse so the server can identify the
// originating EndDevice without a TLS-cert lookup.
//
// now is the clock source for CreatedDateTime. Pass time.Now (or the
// SEP2Client's Now method) in production; tests inject a fixed-time
// closure.
func responsePOSTHook(client responsePoster, lfdi string, now nowFunc) inverter.TransitionHook {
	return func(prev, next inverter.EventState, evt *sep2.DERControl) {
		// Auto-revert transitions (EVENT_COMPLETED → DEFAULT, EVENT_CANCELLED
		// → DEFAULT) carry evt == nil per the IEEE-040 contract. There is no
		// event to acknowledge on those edges.
		if evt == nil {
			return
		}
		// No replyTo → server did not invite a Response on this event.
		// Spec-compliant servers may still set responseRequired bits in this
		// case, but without a target URI the client cannot deliver.
		if evt.ReplyTo == "" {
			return
		}

		status := mapTransitionToStatus(prev, next)
		if status == 0 {
			return
		}

		var mask uint8
		if evt.ResponseRequired == nil {
			// No opt-in. Nil means the server did not request any Response
			// at all; spec default is "no Response required" (Table 32).
			return
		}
		mask = *evt.ResponseRequired
		if !responseRequiredOn(mask, status) {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), responsePOSTTimeout)
		defer cancel()

		statusVal := status
		resp := sep2.DERControlResponse{
			Response: sep2.Response{
				Resource: sep2.Resource{
					Href: deriveResponseHref(evt.MRID, status),
				},
				CreatedDateTime: now().Unix(),
				EndDeviceLFDI:   lfdi,
				Status:          &statusVal,
				Subject:         evt.MRID,
			},
		}

		if err := client.PostResponse(ctx, evt.ReplyTo, resp); err != nil {
			log.Printf("IEEE-044: response POST failed event=%q status=%d replyTo=%q: %v",
				evt.MRID, status, evt.ReplyTo, err)
			return
		}
		log.Printf("IEEE-044: response POST ok event=%q status=%d replyTo=%q", evt.MRID, status, evt.ReplyTo)
	}
}

// mapTransitionToStatus reduces a state-machine edge to its IEEE 2030.5-2023
// §10.10 Table 31 wire-value Response status. Returns 0 ("no wire status
// applies") for edges the spec does not require an acknowledgement on —
// notably the auto-revert from EVENT_COMPLETED → DEFAULT and
// EVENT_CANCELLED → DEFAULT (those acknowledgements fire on the
// COMPLETED / CANCELLED record itself, one transition earlier).
//
// Edges that DO produce a wire status:
//
//	*                 → EVENT_RECEIVED   ⇒ ResponseStatusEventReceived  (1)
//	EVENT_RECEIVED    → EVENT_STARTED    ⇒ ResponseStatusEventStarted   (2)
//	EVENT_STARTED     → EVENT_COMPLETED  ⇒ ResponseStatusEventCompleted (3)
//	*                 → EVENT_CANCELLED  ⇒ ResponseStatusEventCancelled (6)
//
// Pure function. No state, no side effects.
func mapTransitionToStatus(prev, next inverter.EventState) uint8 {
	switch next {
	case inverter.StateEventReceived:
		return sep2.ResponseStatusEventReceived
	case inverter.StateEventStarted:
		if prev == inverter.StateEventReceived {
			return sep2.ResponseStatusEventStarted
		}
	case inverter.StateEventCompleted:
		if prev == inverter.StateEventStarted {
			return sep2.ResponseStatusEventCompleted
		}
	case inverter.StateEventCancelled:
		return sep2.ResponseStatusEventCancelled
	}
	return 0
}

// responseRequiredOn tests whether the responseRequired bitmap selects a
// given Table 31 wire status. IEEE 2030.5-2023 §10.10 Table 32 defines
// the bit layout (HexBinary8 — one octet, eight bits):
//
//	bit 0 (0x01) — Response required on receipt        (status 1)
//	bit 1 (0x02) — Response required on event start    (status 2)
//	bit 2 (0x04) — Response required on event complete (status 3)
//	bit 3 (0x08) — Response required on user opt-out   (status 4)
//	bit 4 (0x10) — Response required on user opt-in    (status 5)
//	bit 5 (0x20) — Response required on cancellation   (status 6)
//	bits 6..7    — reserved
//
// Returns false for any status the hook never emits (4/5/7+); those are
// out-of-scope for IEEE-044 (opt-in/opt-out and superseded paths land
// later). Returns false for status 0 (the "no wire status" sentinel).
func responseRequiredOn(mask uint8, status uint8) bool {
	switch status {
	case sep2.ResponseStatusEventReceived:
		return mask&0x01 != 0
	case sep2.ResponseStatusEventStarted:
		return mask&0x02 != 0
	case sep2.ResponseStatusEventCompleted:
		return mask&0x04 != 0
	case sep2.ResponseStatusEventCancelled:
		return mask&0x20 != 0
	}
	return false
}

// deriveResponseHref returns a deterministic href for the
// DERControlResponse resource, derived from the event mRID and the wire
// status. The format is the first 32 hex characters of
// SHA256(eventMRID + ":" + statusName) prefixed with "/response/" so the
// server can route the POST without an additional lookup table.
//
// Idempotency: the same (eventMRID, status) pair always yields the same
// href, so a transient retry that re-POSTs against a server already
// holding the resource sees a 200 (or 201 with the same Location) rather
// than allocating a duplicate Response. Servers that ignore client-set
// hrefs allocate their own — IEEE 2030.5 §10.1.1 leaves the choice to
// the server. The client-suggested href is a hint, never a contract.
//
// Non-cryptographic uses of SHA-256 are idiomatic in Go for opaque
// identifiers; this is not a security boundary.
func deriveResponseHref(eventMRID string, status uint8) string {
	sum := sha256.Sum256([]byte(eventMRID + ":" + responseStatusName(status)))
	return "/response/" + hex.EncodeToString(sum[:16]) // 16 bytes → 32 hex chars
}

// responseStatusName maps Table 31 wire values to their spec names for
// use inside deriveResponseHref. Unknown values fall through to a
// hex-formatted suffix so collisions are still avoided.
func responseStatusName(status uint8) string {
	switch status {
	case sep2.ResponseStatusEventReceived:
		return "received"
	case sep2.ResponseStatusEventStarted:
		return "started"
	case sep2.ResponseStatusEventCompleted:
		return "completed"
	case sep2.ResponseStatusEventCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("0x%02x", status)
	}
}
