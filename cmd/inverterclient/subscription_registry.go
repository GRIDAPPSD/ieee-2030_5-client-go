// IEEE-052 Phase 8 ticket 4 of 4 (final): mutex-guarded registry wrapping
// the subscriptionsByResource map produced by IEEE-050's registerSubscriptions.
//
// Lives in its own file so the test file (subscription_registry_test.go)
// has a clear unit-test target and so main.go stays close to its previous
// size. The registry is the seam IEEE-052 needs to remove subscription
// entries from the inverter's local tracking when the server cancels a
// subscription via Notification status=1 (CORE-019 step 13).
//
// Concurrency: notifications arrive on independent net/http goroutines
// (the IEEE-049 NotifyReceiver fires Dispatch from its handler). The
// underlying map MUST therefore be guarded — a bare Go map raced from
// the dispatcher would crash the inverter.

package main

import (
	"sort"
	"sync"
)

// subscriptionRegistry is the IEEE-052 mutex-guarded wrapper around
// the subscribedResource→serverSubscriptionHref map produced by
// IEEE-050's registerSubscriptions. Methods are safe for concurrent
// use; the dispatcher's CancelHook calls Cancel from a net/http
// goroutine while main() reads via Snapshot.
//
// The zero value is unusable; use newSubscriptionRegistry.
type subscriptionRegistry struct {
	mu sync.RWMutex
	// entries maps subscribedResource href (what the inverter subscribed
	// TO, e.g. the FSAList or DERList href) to the server-assigned
	// subscription href (what the server returns from POST /sub).
	entries map[string]string
}

// newSubscriptionRegistry returns an empty registry seeded with the
// supplied entries. Pass nil for an empty registry. The supplied map is
// copied; callers may reuse or discard their original reference.
func newSubscriptionRegistry(seed map[string]string) *subscriptionRegistry {
	r := &subscriptionRegistry{entries: make(map[string]string, len(seed))}
	for k, v := range seed {
		r.entries[k] = v
	}
	return r
}

// Add records a subscription. Idempotent: re-adding the same subscribed
// href replaces the server-side subscription href (matches the natural
// "server reassigned this on resubscribe" behavior).
func (r *subscriptionRegistry) Add(subscribedHref, serverSubHref string) {
	if subscribedHref == "" {
		return
	}
	r.mu.Lock()
	r.entries[subscribedHref] = serverSubHref
	r.mu.Unlock()
}

// Cancel removes the entry for the given subscribed href. Idempotent:
// cancelling an unknown href is a no-op. Returns the server-side
// subscription href that was removed (or "" if nothing was tracked) so
// callers can log it.
//
// Bound as the IEEE-052 cancel hook on PhaseStateDispatcher via the
// CancelMethod adapter (registry.Cancel has signature
// `func(string) string`; CancelHook expects `func(string)`).
func (r *subscriptionRegistry) Cancel(subscribedHref string) string {
	if subscribedHref == "" {
		return ""
	}
	r.mu.Lock()
	prev, ok := r.entries[subscribedHref]
	if ok {
		delete(r.entries, subscribedHref)
	}
	r.mu.Unlock()
	if !ok {
		return ""
	}
	return prev
}

// Lookup returns the server-assigned subscription href for the given
// subscribed resource href, and true when found. Read-only; safe for
// concurrent use.
func (r *subscriptionRegistry) Lookup(subscribedHref string) (string, bool) {
	r.mu.RLock()
	v, ok := r.entries[subscribedHref]
	r.mu.RUnlock()
	return v, ok
}

// Len returns the current count of tracked subscriptions. Useful for
// tests and log lines.
func (r *subscriptionRegistry) Len() int {
	r.mu.RLock()
	n := len(r.entries)
	r.mu.RUnlock()
	return n
}

// Snapshot returns a sorted slice of (subscribedHref, serverSubHref)
// pairs for stable iteration / logging. Map order is non-deterministic;
// callers that need stable output use this. Tests rely on the sort to
// make assertions deterministic.
func (r *subscriptionRegistry) Snapshot() [][2]string {
	r.mu.RLock()
	out := make([][2]string, 0, len(r.entries))
	for k, v := range r.entries {
		out = append(out, [2]string{k, v})
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// CancelHookFunc adapts (*subscriptionRegistry).Cancel — which returns
// the removed server-side href so main() can log it — to the
// inverter.CancelHook signature (`func(string)`). The returned closure
// invokes Cancel and discards the return value; the registry-internal
// Cancel still produces it for direct callers (tests).
func (r *subscriptionRegistry) CancelHookFunc() func(subscribedHref string) {
	return func(subscribedHref string) {
		_ = r.Cancel(subscribedHref)
	}
}
