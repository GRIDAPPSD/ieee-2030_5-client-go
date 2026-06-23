// IEEE-050 Phase 8 ticket 2 of 4: register Subscriptions with the SEP2
// server so it can POST Notifications to the IEEE-049 /notify listener.
//
// Extracted into its own file so the helper has a function seam for the
// unit tests in subscription_register_test.go, and so main.go stays close
// to its previous size. Pattern mirrors notify_receiver.go's graceful-
// bypass philosophy (IEEE-048): a failed subscription registration is a
// degrade-to-polling event, never a crash.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// notifyURLForReceiver returns the full https://<addr>/notify URL the SEP2
// server should POST Notifications to. Returns "" when the receiver is nil
// or its bound address is unavailable — the caller treats an empty URL as
// "subscription flow disabled, fall back to polling."
//
// Production callers pass *inverter.NotifyReceiver directly; the function
// adapts via notifyURLForAddrSource so unit tests can stub the Addr() seam
// without spinning a live TLS listener. The two-level shape (concrete
// wrapper + small interface helper) makes the nil-receiver branch
// unambiguous: the typed-nil *NotifyReceiver case is caught before any
// interface conversion happens.
func notifyURLForReceiver(rcv *inverter.NotifyReceiver) string {
	if rcv == nil {
		return ""
	}
	return notifyURLForAddrSource(rcv)
}

// addrSource is the narrow consumer-side seam notifyURLForAddrSource needs.
// Defined at the consumer per Pike rule 6 (small interfaces).
type addrSource interface {
	Addr() (string, error)
}

// notifyURLForAddrSource is the unit-testable inner helper. Assumes a
// non-nil source; the *inverter.NotifyReceiver wrapper above does the
// typed-nil guard.
func notifyURLForAddrSource(src addrSource) string {
	addr, err := src.Addr()
	if err != nil || addr == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/notify", addr)
}

// subscriptionPoster is the narrow consumer-side interface registerSubscriptions
// needs from a SEP2Client. Defined at the consumer per Pike rule 6 (small
// interfaces). The test in subscription_register_test.go stubs this directly
// without spinning a real SEP2Client.
type subscriptionPoster interface {
	PostSubscription(ctx context.Context, subscriptionListHref, subscribedHref, notifyURL string) (string, error)
}

// registerSubscriptions POSTs Subscriptions on behalf of the inverter for
// each resource it cares about. Called after Phase 2b registration completes
// and the IEEE-049 NotifyReceiver is up.
//
// Bypass policy (all log + continue, never fatal):
//   - notifyURL == "" (no receiver up) → log "disabled" and return empty map.
//   - edev.SubscriptionListLink == nil (server doesn't advertise the link)
//     → log "not supported (no SubscriptionListLink)" and return empty map.
//   - PostSubscription returns ErrMethodNotAllowed (405) on any resource →
//     log "subscriptions not supported (405); polling-only fallback" and
//     ABORT the rest of the per-resource loop. A 405 is a per-server
//     capability statement, not a per-resource one; trying the next resource
//     would yield the same 405 and just spam the log.
//   - PostSubscription returns any other error → log warning, continue with
//     the next resource. Future resources may still succeed.
//
// Returns a *subscriptionRegistry (IEEE-052) — a mutex-guarded wrapper
// around the subscribed-resource-href → server-assigned-subscription-href
// map. IEEE-052 wires registry.CancelHookFunc() as the dispatcher's
// CancelHook so status=1 notifications free the inverter-side entry.
// Always returns a non-nil registry so callers can wire it
// unconditionally; an empty registry is a no-op for the cancel hook.
func registerSubscriptions(
	ctx context.Context,
	client subscriptionPoster,
	edev sep2.EndDevice,
	notifyURL string,
) *subscriptionRegistry {
	out := newSubscriptionRegistry(nil)

	if notifyURL == "" {
		log.Println("Subscription register: no /notify receiver URL; subscription flow disabled, polling-only")
		return out
	}
	if edev.SubscriptionListLink == nil || edev.SubscriptionListLink.Href == "" {
		log.Println("Subscription register: server did not advertise EndDevice.SubscriptionListLink; subscriptions not supported, polling-only")
		return out
	}

	listHref := edev.SubscriptionListLink.Href

	// Resource hrefs the inverter wants change-notifications for. CSIP V1.2
	// CORE-018 step 2: subscribe to the FSAList (the canonical entry point
	// for DERControl change visibility) and the DERList (so DER-shape
	// changes propagate). Both are conditional on the server advertising
	// the relevant link on the EndDevice resource.
	type target struct {
		label string
		href  string
	}
	var targets []target
	if edev.FunctionSetAssignmentsListLink != nil && edev.FunctionSetAssignmentsListLink.Href != "" {
		targets = append(targets, target{"FSAList", edev.FunctionSetAssignmentsListLink.Href})
	}
	if edev.DERListLink != nil && edev.DERListLink.Href != "" {
		targets = append(targets, target{"DERList", edev.DERListLink.Href})
	}
	if len(targets) == 0 {
		log.Println("Subscription register: EndDevice advertises no subscribable child links; polling-only")
		return out
	}

	for _, t := range targets {
		subHref, err := client.PostSubscription(ctx, listHref, t.href, notifyURL)
		if err != nil {
			if errors.Is(err, inverter.ErrMethodNotAllowed) {
				log.Printf("Subscription register: server returned 405 on %s; subscriptions not supported, polling-only fallback (IEEE-050)", t.label)
				return out
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Subscription register: context cancelled while subscribing to %s; aborting", t.label)
				return out
			}
			log.Printf("Subscription register: POST %s subscribe %s failed (%v); continuing with polling for this resource", listHref, t.label, err)
			continue
		}
		log.Printf("Subscription register: subscribed to %s at %s (server href %s)", t.label, t.href, subHref)
		out.Add(t.href, subHref)
	}

	return out
}
