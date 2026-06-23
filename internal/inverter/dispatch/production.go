// Package dispatch — production dispatcher (IEEESIM-004 scaffold).
//
// ProductionDispatcher is the production consumer-policy implementation of
// Dispatcher. At landing it is behaviorally equivalent to SimulatorDispatcher
// (spec-correct CSIP CORE-018 cache-refresh routing) with three clearly-marked
// TBD extension points where real deployments will diverge.
//
// The production difference at landing time lives in the device backend
// (realdevice writes real registers through safety guards), NOT in this
// dispatcher. The dispatcher and the device backend are two independent axes
// of the same production posture, selected once at construction.
//
// See docs/adr/ADR-004-production-as-mode.md for the measurable revisit
// trigger that governs when to extract a separate hardened-gateway repo.

package dispatch

import (
	"context"
	"errors"
	"log"
	"sync"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// ProductionDispatcher is the production consumer-policy implementation of
// Dispatcher. It satisfies RegisterableDispatcher so main() can late-bind
// Phase 5 wiring identically to SimulatorDispatcher.
//
// Three TBD extension points are marked below. Each returns the safe default
// (same behavior as SimulatorDispatcher) until a real deployment defines the
// production policy:
//
//  1. Audit logging: production emits a structured audit record per applied
//     control. Starting: same log.Printf as simulator. TBD: structured audit sink.
//  2. Urgent-path notifications: production may treat certain notifications
//     (emergency curtailment) as fire-now rather than wait-for-next-tick.
//     Starting: same next-tick cache-refresh latency as simulator.
//     TBD: urgent-path hook.
//  3. Cancellation side effects: production cancellation may need to revert a
//     physical setpoint to safe-default immediately (tied to the fail-safe
//     guard on the real-device backend). Starting: same CancelHook pass-through
//     as simulator. TBD: revert-to-safe-default on cancel.
type ProductionDispatcher struct {
	mu                 sync.RWMutex
	client             DERControlListFetcher
	cache              *inverter.DERControlCache
	derControlListHref string
	cancelHook         inverter.CancelHook
}

// NewProductionDispatcher returns a production dispatcher with no Phase 5
// wiring. Callers MUST call RegisterDERControlList before notifications are
// expected to drive behavior; until then, every Dispatch call logs and drops.
func NewProductionDispatcher() *ProductionDispatcher {
	return &ProductionDispatcher{}
}

// RegisterDERControlList wires the Phase 5 cache, client, and href into
// the dispatcher. Idempotent: subsequent calls replace the previous
// configuration. Returns an error on nil/empty inputs.
func (d *ProductionDispatcher) RegisterDERControlList(
	client DERControlListFetcher,
	cache *inverter.DERControlCache,
	href string,
) error {
	return registerDERControlList(&d.mu, &d.client, &d.cache, &d.derControlListHref, client, cache, href)
}

// RegisterCancelHook installs the cancel hook invoked on every status=1
// (subscription cancelled by server) notification. Idempotent. Passing nil
// clears the hook.
func (d *ProductionDispatcher) RegisterCancelHook(hook inverter.CancelHook) {
	d.mu.Lock()
	d.cancelHook = hook
	d.mu.Unlock()
}

// Dispatch handles one decoded Notification.
//
// Current behavior is identical to SimulatorDispatcher: it refreshes the
// DERControlCache so the Phase 5 state machine applies the change on its next
// tick. The three TBD production extension points are called out inline.
func (d *ProductionDispatcher) Dispatch(ctx context.Context, n sep2.Notification) {
	if n.Status == sep2.NotificationStatusSubscripted {
		d.mu.RLock()
		hook := d.cancelHook
		d.mu.RUnlock()
		if hook != nil {
			// TBD(production): revert-to-safe-default on cancel.
			// A production deployment may need to revert a physical setpoint
			// to safe-default immediately on subscription cancellation, tied
			// to the fail-safe guard on the real-device backend (see
			// device.WithSafetyGuards). Starting: same CancelHook invocation
			// as simulator, no immediate setpoint revert.
			hook(n.SubscribedResource)
			log.Printf("Notification dispatcher (production): status=1 (subscription cancelled by server) for subscribedResource=%q, cancel hook invoked, polling continues",
				n.SubscribedResource)
			return
		}
		log.Printf("Notification dispatcher (production): status=1 (subscription cancelled by server) for subscribedResource=%q, no cancel hook registered, log+drop",
			n.SubscribedResource)
		return
	}

	d.mu.RLock()
	client := d.client
	cache := d.cache
	dercListHref := d.derControlListHref
	d.mu.RUnlock()

	if client == nil || cache == nil || dercListHref == "" {
		log.Printf("Notification dispatcher (production): not yet registered, log+drop notification subscribed=%q new=%q",
			n.SubscribedResource, n.NewResourceURI)
		return
	}

	changed := changedResourceHref(n)
	if changed == "" {
		log.Printf("Notification dispatcher (production): notification carries no resource href (subscribedResource=%q newResourceURI=%q href=%q), log+drop",
			n.SubscribedResource, n.NewResourceURI, n.Href)
		return
	}

	switch resourceKindFor(changed, dercListHref) {
	case resourceKindDERControlList:
		// TBD(production): audit logging.
		// A production deployment emits a structured audit record per applied
		// control (who commanded what, when) beyond this log.Printf. Starting:
		// same logging as simulator. TBD: structured audit sink.
		if err := refreshDERControlList(ctx, client, cache, dercListHref); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Notification dispatcher (production): DERControlList refresh cancelled for %s: %v", dercListHref, err)
				return
			}
			log.Printf("Notification dispatcher (production): DERControlList refresh failed for %s: %v (continuing, polling loop will recover)", dercListHref, err)
			return
		}
		// TBD(production): urgent-path notifications.
		// A production deployment may treat certain notifications (emergency
		// curtailment DERControl) as fire-now rather than wait-for-next-tick.
		// Starting: same next-tick cache-refresh latency as simulator; the
		// state machine observes the refreshed cache on its next tick.
		// TBD: urgent-path hook that signals the state machine to tick
		// immediately on high-priority controls.
	default:
		log.Printf("Notification dispatcher (production): notification for unknown resource %q (registered DERControlListHref=%q), log+drop",
			changed, dercListHref)
	}
}
