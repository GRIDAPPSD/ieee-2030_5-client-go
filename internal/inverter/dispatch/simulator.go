// Package dispatch : simulator dispatcher (relocated from package inverter).
//
// IEEE-051 Notification dispatcher for the simulator role.
//
// IEEE-049 stood up the /notify listener with a no-op default dispatcher.
// IEEE-050 made the SEP2 server actually POST Notifications by registering
// Subscriptions on the EndDevice.SubscriptionListLink. The SimulatorDispatcher
// is the consumer policy: it takes each parsed Notification, identifies which
// resource changed, GETs the latest value via the existing typed SEP2 client
// helpers, and feeds the result into Phase 5's state machine (indirectly, via
// the DERControlCache the state-machine tick goroutine snapshots).
//
// CSIP V1.2 CORE-018 procedure step 5: "Inverter responds 201 or 204, then
// performs a GET on the resource href included in the Notification body."
// IEEE-049 already handled the 204 response; IEEE-051 added the GET and
// state-machine feed.
//
// IEEESIM-004: moved from package inverter into package dispatch so both
// dispatcher implementations cohabit the dispatch package and the dependency
// direction matches the device-seam topology. Behavior is unchanged.

package dispatch

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// SimulatorDispatcher is the simulator consumer-policy implementation of
// Dispatcher. It refreshes the DERControlCache so the Phase 5 state machine
// applies the change on its next tick.
//
// Wiring: cmd/inverterclient/main.go constructs an empty dispatcher BEFORE
// the SEP2 client and Phase 5 cache exist (so the IEEE-049 NotifyReceiver
// can be brought up early and publish its bound address to the IEEE-050
// subscription POST), then calls RegisterDERControlList once Phase 5
// wiring is complete. Until then, every Dispatch call logs and drops.
//
// Concurrency: a single RWMutex serializes Register* against Dispatch.
// HTTP requests arrive on independent goroutines from net/http; the
// dispatcher's HTTP-call path itself is reentrant (no per-dispatch state
// in the struct), so the RWMutex is held only across the configuration
// read. The actual GET and cache update do not block other dispatches.
type SimulatorDispatcher struct {
	mu                 sync.RWMutex
	client             DERControlListFetcher
	cache              *inverter.DERControlCache
	derControlListHref string
	// cancelHook is the optional seam invoked on status=1 notifications
	// (CORE-019 step 13, subscription cancelled by server). Nil-safe: a nil
	// hook means log and drop. Defined as a func so the dispatcher does not
	// import the cmd-side registry type.
	cancelHook inverter.CancelHook
}

// NewSimulatorDispatcher returns a dispatcher with no Phase 5 wiring.
// Callers MUST call RegisterDERControlList before notifications are
// expected to drive state-machine changes; until then, every Dispatch
// call logs the notification and drops it.
func NewSimulatorDispatcher() *SimulatorDispatcher {
	return &SimulatorDispatcher{}
}

// RegisterDERControlList wires the Phase 5 cache, client, and href into
// the dispatcher. Idempotent: subsequent calls replace the previous
// configuration. Passing href=="", cache==nil, or client==nil returns an
// error so callers can degrade gracefully on misconfiguration.
func (d *SimulatorDispatcher) RegisterDERControlList(
	client DERControlListFetcher,
	cache *inverter.DERControlCache,
	href string,
) error {
	return registerDERControlList(&d.mu, &d.client, &d.cache, &d.derControlListHref, client, cache, href)
}

// RegisterCancelHook installs the cancel hook invoked on every status=1
// (subscription cancelled by server) notification. Idempotent: subsequent
// calls replace the previous hook. Passing nil clears the hook.
func (d *SimulatorDispatcher) RegisterCancelHook(hook inverter.CancelHook) {
	d.mu.Lock()
	d.cancelHook = hook
	d.mu.Unlock()
}

// Dispatch is the NotificationDispatcher entry point. Bound as a method
// value (dispatcher.Dispatch) when passed to NotifyReceiverConfig.
func (d *SimulatorDispatcher) Dispatch(ctx context.Context, n sep2.Notification) {
	if n.Status == sep2.NotificationStatusSubscripted {
		d.mu.RLock()
		hook := d.cancelHook
		d.mu.RUnlock()
		if hook != nil {
			hook(n.SubscribedResource)
			log.Printf("Notification dispatcher (simulator): status=1 (subscription cancelled by server) for subscribedResource=%q, cancel hook invoked, polling continues",
				n.SubscribedResource)
			return
		}
		log.Printf("Notification dispatcher (simulator): status=1 (subscription cancelled by server) for subscribedResource=%q, no cancel hook registered, log+drop",
			n.SubscribedResource)
		return
	}

	d.mu.RLock()
	client := d.client
	cache := d.cache
	dercListHref := d.derControlListHref
	d.mu.RUnlock()

	if client == nil || cache == nil || dercListHref == "" {
		log.Printf("Notification dispatcher (simulator): not yet registered, log+drop notification subscribed=%q new=%q",
			n.SubscribedResource, n.NewResourceURI)
		return
	}

	changed := changedResourceHref(n)
	if changed == "" {
		log.Printf("Notification dispatcher (simulator): notification carries no resource href (subscribedResource=%q newResourceURI=%q href=%q), log+drop",
			n.SubscribedResource, n.NewResourceURI, n.Href)
		return
	}

	switch resourceKindFor(changed, dercListHref) {
	case resourceKindDERControlList:
		if err := refreshDERControlList(ctx, client, cache, dercListHref); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Notification dispatcher (simulator): DERControlList refresh cancelled for %s: %v", dercListHref, err)
				return
			}
			log.Printf("Notification dispatcher (simulator): DERControlList refresh failed for %s: %v (continuing, polling loop will recover)", dercListHref, err)
			return
		}
	default:
		log.Printf("Notification dispatcher (simulator): notification for unknown resource %q (registered DERControlListHref=%q), log+drop",
			changed, dercListHref)
	}
}

// changedResourceHref extracts the "what changed" href from a Notification.
// Preference order:
//
//  1. n.Href: the spec puts the changed resource's href in the Resource base.
//  2. n.NewResourceURI: present on add/remove events per IEEE 2030.5 §10.13.
//  3. n.SubscribedResource: fallback for "list resource changed" notifications
//     where no more-specific href is included.
//
// Returns "" only when all three are empty; caller logs and drops.
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
type resourceKind int

const (
	resourceKindUnknown resourceKind = iota
	resourceKindDERControlList
)

// resourceKindFor classifies a changed-resource href against the registered
// DERControlListHref. Returns resourceKindDERControlList for an exact match
// or a per-mRID DERControl href that lives under the list.
func resourceKindFor(changed, derControlListHref string) resourceKind {
	if changed == "" || derControlListHref == "" {
		return resourceKindUnknown
	}
	if changed == derControlListHref {
		return resourceKindDERControlList
	}
	// Per-mRID child href: .../derc/<id> lives under the list href .../derc.
	// The server may add a trailing slash on either side; tolerate both.
	listWithSlash := strings.TrimRight(derControlListHref, "/") + "/"
	if strings.HasPrefix(changed, listWithSlash) {
		return resourceKindDERControlList
	}
	return resourceKindUnknown
}

// refreshDERControlList fetches the latest DERControlList and refreshes the
// cache. Mirrors the original notification_dispatcher.go implementation.
func refreshDERControlList(
	ctx context.Context,
	client DERControlListFetcher,
	cache *inverter.DERControlCache,
	href string,
) error {
	list, _, err := client.GetDERControlList(ctx, href)
	if err != nil {
		return err
	}
	added, updated, cancelled := cache.Diff(list.DERControl)
	total := cache.Refresh(list.DERControl)
	if len(added)+len(updated)+len(cancelled) > 0 {
		log.Printf("Notification dispatcher: DERControlList refreshed via notification, %d total (+%d new, ~%d updated, !%d cancelled)",
			total, len(added), len(updated), len(cancelled))
	}
	return nil
}

// registerDERControlList is a shared helper that both SimulatorDispatcher and
// ProductionDispatcher use for the RegisterDERControlList method body.
// Pointer indirection lets both structs share this logic without embedding.
func registerDERControlList(
	mu *sync.RWMutex,
	clientPtr *DERControlListFetcher,
	cachePtr **inverter.DERControlCache,
	hrefPtr *string,
	client DERControlListFetcher,
	cache *inverter.DERControlCache,
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
	mu.Lock()
	*clientPtr = client
	*cachePtr = cache
	*hrefPtr = href
	mu.Unlock()
	return nil
}
