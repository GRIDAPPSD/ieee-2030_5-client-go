package inverter

import (
	"log"
	"sync"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// DERControl event state machine (Phase 5, GRIDAPPSD/ieee-2030_5-server-go#92) ===========
//
// This file ties the polling cache (DERControlCache.Snapshot/Diff) and the
// randomization-aware scheduler (OnEventsAdded/OnEventsCancelled/
// PopExpired/Next) together into a state machine that
// owns the inverter's DefaultDERControl <-> active DERControl lifecycle.
//
// Lifecycle (single-event scope):
//
//	DEFAULT
//	   │
//	   │ first added event observed
//	   ▼
//	EVENT_RECEIVED  ◄─── server cancels before fire ─── ─┐
//	   │                                                 │
//	   │ now >= fireAt  (sched.PopExpired)               │
//	   ▼                                                 │
//	EVENT_STARTED  ◄─── server cancels mid-event ────────┤
//	   │                                                 │
//	   │ now >= activeExpireAt                           │
//	   ▼                                                 ▼
//	EVENT_COMPLETED ─────────────►  DEFAULT  ◄───── EVENT_CANCELLED
//	   (terminal, same-tick auto-revert)        (terminal, same-tick auto-revert)
//
// Out of scope for this state machine:
//   - Replacing ApplyControls(nil, ...) at cmd/inverterclient/main.go:699.
//     This file exposes Current() so the caller can read
//     ActiveDERControl when State == EVENT_STARTED; the actual swap lives in
//     main.go.
//   - DERCurve retrieval.
//   - Response Function Set / replyTo POSTs : Phase 6. The OnTransition hook
//     is the wiring point; Phase 6 registers it.
//   - Multi-event Primacy arbitration (Phase 5 doc test case 10). Single-
//     event scope by design: if two events pop at the same Tick the first
//     (FireAt-ascending) wins; the second sits in the queue until the first
//     terminates. Flagged TODO below.

// EventState enumerates the state-machine vertices. Values are stable for
// log-correlation; do not reorder. DEFAULT is the zero value so the
// zero-value StateMachine starts in the right state (NewStateMachine still
// preferred : guards against future field additions).
type EventState int

// EventState values. DEFAULT is intentionally zero so a zero-value
// StateMachine is observably in DEFAULT.
const (
	StateDefault EventState = iota
	StateEventReceived
	StateEventStarted
	StateEventCompleted
	StateEventCancelled
)

// String returns the canonical UPPER_SNAKE_CASE state name used in logs and
// test assertions. Implements fmt.Stringer.
func (s EventState) String() string {
	switch s {
	case StateDefault:
		return "DEFAULT"
	case StateEventReceived:
		return "EVENT_RECEIVED"
	case StateEventStarted:
		return "EVENT_STARTED"
	case StateEventCompleted:
		return "EVENT_COMPLETED"
	case StateEventCancelled:
		return "EVENT_CANCELLED"
	default:
		return "UNKNOWN"
	}
}

// EventStateSnapshot is a value-typed read-only view of the state machine.
// Callers MUST treat ActiveDERControl as read-only; the state machine returns
// a pointer to a private copy and reuses the slot across transitions.
//
// cmd/inverterclient/main.go's simulation
// loop consumes this: when State == EVENT_STARTED, ApplyControls is fed
// ActiveDERControl.DERControlBase; otherwise the DefaultDERControl base
// resolved in Phase 4.
type EventStateSnapshot struct {
	State            EventState
	ActiveMRID       string
	ActiveDERControl *sep2.DERControl
}

// TransitionHook is the wiring point Phase 6 (Response Function Set) plumbs
// to emit `status=1/2/3/6` Responses on every state change. The hook
// receives the (prev, next) state pair and the active DERControl pointer.
// `evt` may be nil for transitions that don't carry an event payload
// (notably the synthetic EVENT_COMPLETED → DEFAULT and
// EVENT_CANCELLED → DEFAULT auto-reverts).
//
// Hooks are invoked OUTSIDE the state-machine mutex. A hook that calls back
// into the state machine cannot deadlock; a hook that blocks holds up the
// caller's Tick loop. Phase 6 should fire the Response POST asynchronously.
type TransitionHook func(prev, next EventState, evt *sep2.DERControl)

// StateMachine owns the inverter's DEFAULT ↔ active DERControl lifecycle.
// The zero value is functional (state == DEFAULT) but NewStateMachine is the
// preferred entry point : it sets the mutex up explicitly and guards against
// future field additions.
//
// Concurrency: a single sync.Mutex serializes Tick / OnTransition /
// AddTransitionHook / Current. The expected workload is one Tick per
// pollRate (30s-5min); the extra read-parallelism an RWMutex would buy is
// not worth its complexity for a single-event-scope state machine. Hooks
// are invoked OUTSIDE the lock (locked-callback antipattern avoided :
// Pike rule 3 is satisfied even though no goroutine lives inside the
// state machine).
//
// Hook surface (Phase 6):
//   - OnTransition(hook) : replace-only. The single-hook slot; passing nil
//     clears it. Test code swaps implementations in/out via this method.
//   - AddTransitionHook(hook) : append. Each call adds a hook to a slice;
//     every transition fires every appended hook in registration order
//     PLUS the OnTransition slot (if set). Production code that wants to
//     compose multiple concerns (e.g. curve refresh + Response POST)
//     registers each through AddTransitionHook.
type StateMachine struct {
	mu             sync.Mutex
	state          EventState
	activeMRID     string
	activeDERCtrl  *sep2.DERControl
	activeExpireAt time.Time
	hook           TransitionHook
	hooks          []TransitionHook
}

// NewStateMachine constructs a state machine in DEFAULT. No required
// dependencies : the scheduler and now-source are passed to Tick so the
// state machine never holds a clock reference (parity with the scheduler's
// pure-function design).
func NewStateMachine() *StateMachine {
	return &StateMachine{state: StateDefault}
}

// OnTransition installs the single replace-only hook slot. Pass nil to
// clear. Subsequent transitions invoke the hook OUTSIDE the state-machine
// mutex. The replace-only semantics make this the preferred surface for
// test code that needs to swap implementations during a run.
//
// Production code with multiple independent concerns (e.g. curve
// refresh AND Response POST) should use AddTransitionHook instead,
// which appends rather than replaces.
func (sm *StateMachine) OnTransition(hook TransitionHook) {
	sm.mu.Lock()
	sm.hook = hook
	sm.mu.Unlock()
}

// AddTransitionHook appends a hook to the multi-hook fan-out. Every
// transition fires every appended hook in registration order, AFTER the
// (optional) OnTransition slot fires. Hooks are invoked OUTSIDE the
// state-machine mutex (same contract as OnTransition); a hook that blocks
// holds up the caller's Tick loop and a hook that calls back into the
// state machine cannot deadlock.
//
// There is no public removal API : hooks are expected to outlive the
// state machine they're attached to (Phase 6 wires them once at startup
// and never deregisters). A nil hook is a no-op append-attempt and is
// silently ignored to keep callers' guarded "register if non-nil"
// patterns clean.
func (sm *StateMachine) AddTransitionHook(hook TransitionHook) {
	if hook == nil {
		return
	}
	sm.mu.Lock()
	sm.hooks = append(sm.hooks, hook)
	sm.mu.Unlock()
}

// Current returns a read-only snapshot of the state machine. The
// ActiveDERControl pointer (if non-nil) is owned by the state machine and
// MUST be treated as read-only; the state machine may swap it on the next
// transition.
//
// main.go's simulation loop consumes this.
func (sm *StateMachine) Current() EventStateSnapshot {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return EventStateSnapshot{
		State:            sm.state,
		ActiveMRID:       sm.activeMRID,
		ActiveDERControl: sm.activeDERCtrl,
	}
}

// Tick is the single-step driver. The caller (cmd/inverterclient/main.go's
// state-machine goroutine, or a test driver) is responsible for:
//
//  1. Holding the previous DERControlCache snapshot (closure-local).
//  2. Taking a fresh snapshot, calling cache.Diff to compute the added /
//     updated / cancelled buckets.
//  3. Calling Tick(now, added, cancelled, sched) : Tick forwards added/
//     cancelled to the scheduler, calls sched.PopExpired(now), and walks
//     state transitions in a single atomic pass.
//
// Tick is safe under the state-machine mutex; the mutex is held across
// scheduler calls (sched has its own mutex : no risk of deadlock because the
// state machine never calls back into itself, and the scheduler never calls
// into the state machine). Hook invocations happen AFTER the mutex is
// released for each transition.
//
// `now` is the caller's clock reading (production: client.Now(), the
// server-offset clock; tests: a fixed-time closure). The state machine never reads
// real time.Now() : consistent with the scheduler's discipline.
//
// `sched` is required (panics on nil : programmer error, not a runtime
// failure mode, same as NewScheduler's nil-arg panics).
func (sm *StateMachine) Tick(
	now time.Time,
	added []sep2.DERControl,
	cancelled []sep2.DERControl,
	sched *Scheduler,
) {
	if sched == nil {
		panic("inverter.StateMachine.Tick: sched required")
	}

	// Collect transitions while holding the lock; fire hooks afterward so a
	// blocking or panicking hook can't strand the state-machine mutex.
	sm.mu.Lock()
	pending := sm.computeTransitionsLocked(now, added, cancelled, sched)
	sm.mu.Unlock()
	for _, t := range pending {
		log.Printf("statemachine: %s → %s mRID=%q now=%s",
			t.prev, t.next, t.mRID, now.UTC().Format(time.RFC3339Nano))
		if t.hook != nil {
			t.hook(t.prev, t.next, t.evt)
		}
		for _, h := range t.hooks {
			h(t.prev, t.next, t.evt)
		}
	}
}

// transition records a single state change for post-lock hook firing.
// hook and hooks are captured at transition time so a mid-Tick
// OnTransition / AddTransitionHook call is harmless : already-recorded
// transitions fire against the pre-call snapshot.
type transition struct {
	prev  EventState
	next  EventState
	mRID  string
	evt   *sep2.DERControl
	hook  TransitionHook
	hooks []TransitionHook
}

// tickLocked performs the scheduler forwarding and walks the transition
// graph under the state-machine mutex. Returns the ordered list of
// transitions for the caller to log + hook-fire OUTSIDE the lock.
//
// All transitions are determined in this order (matching the lifecycle
// diagram in the file header):
//
//  1. Forward `added` events to sched.OnEventsAdded.
//  2. Forward `cancelled` mRIDs to sched.OnEventsCancelled AND determine
//     whether the active mRID (or the next-pending head) was cancelled,
//     triggering an EVENT_RECEIVED→EVENT_CANCELLED or
//     EVENT_STARTED→EVENT_CANCELLED transition + auto-revert to DEFAULT.
//  3. If state is EVENT_STARTED and now >= activeExpireAt, transition
//     EVENT_STARTED→EVENT_COMPLETED→DEFAULT (same-tick auto-revert).
//  4. Drain sched.PopExpired(now): each popped event triggers
//     DEFAULT→EVENT_RECEIVED→EVENT_STARTED if it matches the active mRID,
//     or first-popped wins when state is DEFAULT.
//  5. If state is DEFAULT and sched.Next() returns a pending event, raise
//     to EVENT_RECEIVED so the caller's Current() sees the upcoming event.
//
// Multi-event arbitration TODO: when PopExpired returns >1 event in one
// call (FireAt-ascending), only the first becomes active. The remainder are
// dropped on the floor : single-event scope by design. Multi-event ranking
// by Primacy is a follow-up (Phase 5 doc case 10).
func (sm *StateMachine) computeTransitionsLocked(
	now time.Time,
	added []sep2.DERControl,
	cancelled []sep2.DERControl,
	sched *Scheduler,
) []transition {
	var out []transition

	// 1. Forward added events to the scheduler. The scheduler dedupes by
	// mRID (re-add replaces); the state machine doesn't need to filter.
	if len(added) > 0 {
		sched.OnEventsAdded(added)
	}

	// 2. Forward cancellations. Cancelled may touch the active event (raise
	// EVENT_*→EVENT_CANCELLED→DEFAULT) or a pending event in the queue (no
	// state-machine transition unless it WAS our pending-active head).
	if len(cancelled) > 0 {
		mRIDs := make([]string, 0, len(cancelled))
		cancelledSet := make(map[string]*sep2.DERControl, len(cancelled))
		for i := range cancelled {
			m := cancelled[i].MRID
			if m == "" {
				continue
			}
			mRIDs = append(mRIDs, m)
			cancelledSet[m] = &cancelled[i]
		}
		sched.OnEventsCancelled(mRIDs)

		if sm.activeMRID != "" {
			if cancelledEvt, hit := cancelledSet[sm.activeMRID]; hit &&
				(sm.state == StateEventReceived || sm.state == StateEventStarted) {
				// EVENT_RECEIVED→EVENT_CANCELLED→DEFAULT or
				// EVENT_STARTED→EVENT_CANCELLED→DEFAULT (same-tick auto-revert).
				out = append(out, sm.transitionLocked(StateEventCancelled, sm.activeMRID, cancelledEvt))
				out = append(out, sm.transitionLocked(StateDefault, "", nil))
			}
		}
	}

	// 3. Natural completion: EVENT_STARTED past its expiry.
	if sm.state == StateEventStarted && !sm.activeExpireAt.IsZero() && !now.Before(sm.activeExpireAt) {
		completed := sm.activeDERCtrl
		out = append(out, sm.transitionLocked(StateEventCompleted, sm.activeMRID, completed))
		out = append(out, sm.transitionLocked(StateDefault, "", nil))
	}

	// 4. Drain expired (fire-time-reached) events. First-popped wins under
	// single-event scope. Subsequent popped events are dropped : multi-event
	// arbitration is out of scope (flagged TODO above).
	expired := sched.PopExpired(now)
	if len(expired) > 1 {
		log.Printf("statemachine: TODO multi-event Primacy arbitration : %d events popped this tick; first wins, %d dropped",
			len(expired), len(expired)-1)
	}
	if len(expired) > 0 {
		head := expired[0]
		// If the state is DEFAULT, first observe EVENT_RECEIVED then advance
		// to EVENT_STARTED : even though both happen in the same Tick. This
		// keeps the transition trail honest for hook consumers.
		if sm.state == StateDefault {
			// Materialize a copy so the snapshot pointer outlives any future
			// scheduler-side mutation.
			evtCopy := head.Source.Copy()
			sm.activeMRID = head.MRID
			sm.activeDERCtrl = &evtCopy
			sm.activeExpireAt = head.ExpireAt
			// Synthesize the RECEIVED step the caller never observed : Tick
			// "saw" the event for the first time at PopExpired, but the
			// lifecycle requires RECEIVED before STARTED.
			out = append(out, sm.recordTransitionLocked(StateDefault, StateEventReceived, head.MRID, &evtCopy))
		}
		if sm.state == StateEventReceived && sm.activeMRID == head.MRID {
			// EVENT_RECEIVED → EVENT_STARTED.
			// activeDERCtrl is already set (either from prior RECEIVED tick
			// or the just-synthesized one above); refresh from the popped
			// source so we hold the most recent Copy.
			evtCopy := head.Source.Copy()
			sm.activeDERCtrl = &evtCopy
			sm.activeExpireAt = head.ExpireAt
			out = append(out, sm.transitionLocked(StateEventStarted, head.MRID, &evtCopy))

			// Same-tick natural-completion: if now is already past expiry
			// (e.g. past-due event whose interval already elapsed) advance
			// straight to COMPLETED→DEFAULT this Tick. Mirrors step 3 but
			// must run AFTER the EVENT_STARTED transition is recorded so
			// the hook trail is preserved.
			if !sm.activeExpireAt.IsZero() && !now.Before(sm.activeExpireAt) {
				out = append(out, sm.transitionLocked(StateEventCompleted, sm.activeMRID, sm.activeDERCtrl))
				out = append(out, sm.transitionLocked(StateDefault, "", nil))
			}
		}
	}

	// 5. Pending-head observation: if we're back in DEFAULT and the queue
	// has an upcoming event, raise to EVENT_RECEIVED so Current() reflects
	// the next-to-fire mRID. This is the path most tests hit on the first
	// Tick after an OnEventsAdded.
	if sm.state == StateDefault {
		if next, ok := sched.Next(); ok {
			evtCopy := next.Source.Copy()
			sm.activeMRID = next.MRID
			sm.activeDERCtrl = &evtCopy
			sm.activeExpireAt = next.ExpireAt
			out = append(out, sm.transitionLocked(StateEventReceived, next.MRID, &evtCopy))
		}
	}

	return out
}

// transitionLocked mutates the state machine to `next` and records a
// transition slip for post-lock hook firing. Caller MUST hold sm.mu.
//
// Side effects on the StateMachine:
//   - state := next
//   - active* fields cleared when next == DEFAULT.
func (sm *StateMachine) transitionLocked(next EventState, mRID string, evt *sep2.DERControl) transition {
	prev := sm.state
	t := transition{prev: prev, next: next, mRID: mRID, evt: evt, hook: sm.hook, hooks: snapshotHooks(sm.hooks)}
	sm.state = next
	if next == StateDefault {
		sm.activeMRID = ""
		sm.activeDERCtrl = nil
		sm.activeExpireAt = time.Time{}
	}
	return t
}

// recordTransitionLocked records a transition slip WITHOUT mutating the
// state : used when state mutation already happened (e.g. activeMRID set
// at PopExpired time) and we just need the hook event entry. Caller MUST
// hold sm.mu.
func (sm *StateMachine) recordTransitionLocked(prev, next EventState, mRID string, evt *sep2.DERControl) transition {
	sm.state = next
	return transition{prev: prev, next: next, mRID: mRID, evt: evt, hook: sm.hook, hooks: snapshotHooks(sm.hooks)}
}

// snapshotHooks returns an independent copy of the multi-hook slice so a
// concurrent AddTransitionHook call does not race with mid-Tick hook
// firing (post-lock). The slice is small (one or two entries in
// practice); the copy cost is negligible vs. the simpler reasoning it
// buys at the hook-firing boundary.
func snapshotHooks(src []TransitionHook) []TransitionHook {
	if len(src) == 0 {
		return nil
	}
	dst := make([]TransitionHook, len(src))
	copy(dst, src)
	return dst
}
