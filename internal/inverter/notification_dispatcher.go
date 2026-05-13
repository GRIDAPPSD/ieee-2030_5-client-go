// Package inverter — IEEE-051 Notification dispatcher.
//
// IEEE-049 stood up the `/notify` listener with a no-op default dispatcher.
// IEEE-050 made the SEP2 server actually POST Notifications by registering
// Subscriptions on the EndDevice.SubscriptionListLink. This file is the
// missing middle: the real dispatcher that takes each parsed Notification,
// figures out which resource changed, GETs the latest value via the
// existing typed SEP2 client helpers, and feeds the result into Phase 5's
// state machine (indirectly, via the DERControlCache the state-machine
// tick goroutine snapshots).
//
// CSIP V1.2 CORE-018 procedure step 5: "Inverter responds 201 or 204, then
// performs a GET on the resource href included in the Notification body."
// IEEE-049 already handled the 204 response; this ticket adds the GET +
// state-machine feed.
//
// Architecture: the dispatcher does NOT call into the state machine
// directly. Instead it refreshes the same DERControlCache the polling
// loop refreshes (`(*SEP2Client).pollDERControlListOnce`). The state-
// machine tick goroutine in cmd/inverterclient/main.go reads `cache.
// Snapshot()` on every tick and computes its own diff against its
// previous snapshot — so a notification-driven cache refresh becomes a
// state-machine transition on the next tick (≤ tickInterval latency).
// Polling stays active; notifications just lower the floor.
//
// Out of scope (IEEE-052 territory):
//   - status=1 (subscription cancelled) → fallback to polling-only.
//     This dispatcher logs and drops; IEEE-052 will replace the drop
//     with real fallback logic.

package inverter

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"

	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// derControlListFetcher is the narrow consumer-side interface the
// dispatcher needs from a SEP2Client. Defined at the consumer per Pike
// rule 6 (small interfaces) so tests can stub the HTTP path without
// spinning a real client. SEP2Client satisfies this implicitly.
type derControlListFetcher interface {
	GetDERControlList(ctx context.Context, href string) (sep2.DERControlList, string, error)
}

// PhaseStateDispatcher is the IEEE-051 NotificationDispatcher implementation.
//
// Wiring: cmd/inverterclient/main.go constructs an empty dispatcher BEFORE
// the SEP2 client + Phase 5 cache exist (so the IEEE-049 NotifyReceiver
// can be brought up early and publish its bound address to the IEEE-050
// subscription POST), then calls RegisterDERControlList(...) once Phase 5
// wiring is complete. The Dispatch method handles the transition: until
// Register* is called, every notification logs and drops (effectively the
// IEEE-049 no-op behavior); after Register*, notifications drive real GETs.
//
// Concurrency: a single RWMutex serializes Register* against Dispatch.
// HTTP requests arrive on independent goroutines from net/http; the
// dispatcher's HTTP-call path itself is reentrant (no per-dispatch state
// in the struct), so the RWMutex is held only across the configuration
// read. The actual GET + cache update do not block other dispatches.
//
// IEEE-052 will add Status=1 cancellation tracking; for now status=1 is
// logged + dropped.
type PhaseStateDispatcher struct {
	mu                 sync.RWMutex
	client             derControlListFetcher
	cache              *DERControlCache
	derControlListHref string
}

// NewPhaseStateDispatcher returns a dispatcher with no Phase 5 wiring.
// Callers MUST call RegisterDERControlList before notifications are
// expected to drive state-machine changes; until then, every Dispatch
// call logs the notification and drops it (same behavior as
// NoopNotificationDispatcher).
//
// This shape exists because cmd/inverterclient/main.go starts the
// IEEE-049 NotifyReceiver well before it knows the DERControlList href
// (which is discovered during Phase 4 DERProgram walk). Late-bind via
// RegisterDERControlList lets the receiver come up early and the
// dispatcher light up later in the same process lifetime.
func NewPhaseStateDispatcher() *PhaseStateDispatcher {
	return &PhaseStateDispatcher{}
}

// RegisterDERControlList wires the Phase 5 cache + client + href into
// the dispatcher. Idempotent: subsequent calls replace the previous
// configuration. Passing href=="" or cache==nil or client==nil is a
// programmer error — the dispatcher panics rather than silently
// dropping every notification.
//
// Returns an error rather than panicking on bad inputs so production
// callers can degrade gracefully (log + continue in polling-only mode)
// when a misconfiguration is discovered at wire-up time. The error type
// is a sentinel so callers can errors.Is-match without inspecting strings.
func (d *PhaseStateDispatcher) RegisterDERControlList(
	client derControlListFetcher,
	cache *DERControlCache,
	href string,
) error {
	if client == nil {
		return errors.New("RegisterDERControlList: client required")
	}
	if cache == nil {
		return errors.New("RegisterDERControlList: cache required")
	}
	if href == "" {
		return errors.New("RegisterDERControlList: href required")
	}
	d.mu.Lock()
	d.client = client
	d.cache = cache
	d.derControlListHref = href
	d.mu.Unlock()
	return nil
}

// Dispatch is the NotificationDispatcher entry point. Bound as a method
// value (`dispatcher.Dispatch`) when passed to NotifyReceiverConfig.
// Matches the func(ctx, sep2.Notification) signature exactly.
//
// Behavior:
//
//  1. status==1 (Subscription cancelled, CORE-019 step 13) → log + drop.
//     IEEE-052 territory.
//  2. Not yet registered → log + drop (IEEE-049 no-op semantics).
//  3. Changed-resource href identification (in order of preference):
//     n.Href (Resource.Href in the body) → n.NewResourceURI → n.SubscribedResource.
//     If none are populated → log + drop.
//  4. Href matches the registered DERControlListHref (exact OR prefix —
//     a per-mRID DERControl href like .../derp/1/derc/abc still lives
//     under .../derp/1/derc) → fetch + cache.
//  5. Unknown href → log + drop. Never crash.
//
// The GET + cache update path mirrors pollDERControlListOnce so polling
// and notification-driven refresh are observably equivalent from the
// state machine's perspective.
func (d *PhaseStateDispatcher) Dispatch(ctx context.Context, n sep2.Notification) {
	if n.Status == sep2.NotificationStatusSubscripted {
		// Per CORE-019 step 13, status=1 means "Subscription cancelled by
		// server." IEEE-051 logs + drops; IEEE-052 will flip the inverter
		// back to polling-only for the affected resource.
		log.Printf("Notification dispatcher: status=1 (subscription cancelled) for subscribedResource=%q — log+drop (IEEE-052 will handle fallback)",
			n.SubscribedResource)
		return
	}

	d.mu.RLock()
	client := d.client
	cache := d.cache
	dercListHref := d.derControlListHref
	d.mu.RUnlock()

	if client == nil || cache == nil || dercListHref == "" {
		log.Printf("Notification dispatcher: not yet registered — log+drop notification subscribed=%q new=%q",
			n.SubscribedResource, n.NewResourceURI)
		return
	}

	changed := changedResourceHref(n)
	if changed == "" {
		log.Printf("Notification dispatcher: notification carries no resource href (subscribedResource=%q newResourceURI=%q href=%q) — log+drop",
			n.SubscribedResource, n.NewResourceURI, n.Href)
		return
	}

	switch resourceKindFor(changed, dercListHref) {
	case resourceKindDERControlList:
		if err := d.refreshDERControlList(ctx, client, cache, dercListHref); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Notification dispatcher: DERControlList refresh cancelled for %s: %v", dercListHref, err)
				return
			}
			log.Printf("Notification dispatcher: DERControlList refresh failed for %s: %v (continuing — polling loop will recover)", dercListHref, err)
			return
		}
	default:
		log.Printf("Notification dispatcher: notification for unknown resource %q (registered DERControlListHref=%q) — log+drop",
			changed, dercListHref)
	}
}

// changedResourceHref extracts the "what changed" href from a Notification.
// Preference order:
//
//  1. n.Href — the spec puts the changed resource's href in the Resource
//     base (the Notification itself carries it as the canonical
//     identifier).
//  2. n.NewResourceURI — present on add/remove events per IEEE 2030.5
//     §10.13.
//  3. n.SubscribedResource — fallback to the subscribed-resource href
//     itself (correct for "list resource changed" notifications where no
//     more-specific href is included).
//
// Returns "" only when all three are empty — caller logs + drops.
func changedResourceHref(n sep2.Notification) string {
	if n.Href != "" {
		return n.Href
	}
	if n.NewResourceURI != "" {
		return n.NewResourceURI
	}
	return n.SubscribedResource
}

// resourceKind enumerates the resource families the dispatcher can route to.
// IEEE-051 lights up DERControlList only; the enum exists so future tickets
// (DefaultDERControl notifications, DERStatus push, FSAList re-walk) can
// add cases without rewriting the routing logic.
type resourceKind int

const (
	resourceKindUnknown resourceKind = iota
	resourceKindDERControlList
)

// resourceKindFor classifies a changed-resource href against the registered
// DERControlListHref. Returns resourceKindDERControlList for an exact match
// or a per-mRID DERControl href that lives under the list. Everything else
// is resourceKindUnknown (log + drop at the caller).
//
// Prefix tolerance is important: when a server publishes a new DERControl
// at .../derp/1/derc/abc, the notification may carry that specific href in
// n.Href even though we subscribed at the FSAList or DERList level. The
// list-prefix walk catches that.
func resourceKindFor(changed, derControlListHref string) resourceKind {
	if changed == "" || derControlListHref == "" {
		return resourceKindUnknown
	}
	if changed == derControlListHref {
		return resourceKindDERControlList
	}
	// Per-mRID child href: .../derc/<id> lives under the list href .../derc
	// The server may add a trailing slash on either side; tolerate both.
	listWithSlash := strings.TrimRight(derControlListHref, "/") + "/"
	if strings.HasPrefix(changed, listWithSlash) {
		return resourceKindDERControlList
	}
	return resourceKindUnknown
}

// refreshDERControlList fetches the latest DERControlList and refreshes
// the cache. The implementation mirrors pollDERControlListOnce; the only
// reason it lives here instead of being called directly is so the
// dispatcher can present a `derControlListFetcher` interface seam to
// tests without depending on the full *SEP2Client surface.
//
// Errors propagate up. The caller logs and continues — a failed
// notification-driven refresh is non-fatal because the periodic poll
// loop is still running and will recover on its next tick.
func (d *PhaseStateDispatcher) refreshDERControlList(
	ctx context.Context,
	client derControlListFetcher,
	cache *DERControlCache,
	href string,
) error {
	list, _, err := client.GetDERControlList(ctx, href)
	if err != nil {
		return err
	}
	added, updated, cancelled := cache.Diff(list.DERControl)
	total := cache.Refresh(list.DERControl)
	if len(added)+len(updated)+len(cancelled) > 0 {
		log.Printf("Notification dispatcher: DERControlList refreshed via notification — %d total (+%d new, ~%d updated, !%d cancelled)",
			total, len(added), len(updated), len(cancelled))
	}
	return nil
}
