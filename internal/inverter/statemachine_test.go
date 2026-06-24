// Tests for IEEE-040 : DERControl event state machine (Phase 5 ticket 3 of 5).
//
// The state machine is a pure data structure: no goroutine, no I/O, no real
// time.Now(). Every test injects a fixed-time clock via the helpers and
// drives Tick directly. Coverage hits the 10 mandatory cases plus the
// hook-and-edge-case ladder documented in the ticket.
//
//   1. Initial state is DEFAULT                    : TestStateMachine_InitialStateDefault
//   2. Add → EVENT_RECEIVED                        : TestStateMachine_AddTransitionsToReceived
//   3. Fire → EVENT_STARTED                        : TestStateMachine_FireTransitionsToStarted
//   4. Natural completion → COMPLETED → DEFAULT    : TestStateMachine_NaturalCompletionRevertsToDefault
//   5. Server cancel mid-event                     : TestStateMachine_ServerCancelMidEvent
//   6. Server cancel before fire                   : TestStateMachine_ServerCancelBeforeFire
//   7. Hook fired on each transition               : TestStateMachine_HookFiredOnEachTransition
//   8. Hook is optional (nil → no panic)           : TestStateMachine_NilHookDoesNotPanic
//   9. Idempotent re-adds                          : TestStateMachine_IdempotentReAdds
//  10. Multi-event overlap minimum                 : TestStateMachine_MultiEventOverlapFirstWins
//
// Supporting coverage:
//   - EventState.String() returns canonical names  : TestEventState_String
//   - Past-due event collapses Tick path           : TestStateMachine_PastDueEventCollapsesInOneTick
//   - Tick on nil scheduler panics                 : TestStateMachine_TickPanicsOnNilScheduler
//   - Hook can be replaced mid-life                : TestStateMachine_HookCanBeReplaced

package inverter_test

import (
	"sync"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// =============================================================================
// Helpers.
// =============================================================================

// newClock returns a closure-backed clock whose value can be advanced from
// the test. The returned `advance` func is goroutine-safe (mutex inside)
// because some tests run subtests in parallel and would otherwise race on
// the captured variable.
func newClock(start time.Time) (now func() time.Time, advance func(time.Time)) {
	var mu sync.Mutex
	t := start
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return t
		}, func(next time.Time) {
			mu.Lock()
			defer mu.Unlock()
			t = next
		}
}

// transitionRecord captures a (prev, next, mRID) tuple from the hook. Tests
// inspect a slice of these to assert the expected transition trail.
type transitionRecord struct {
	Prev EventState
	Next EventState
	MRID string
	// Nil OK : synthetic terminal-revert transitions pass nil evt.
	EvtPresent bool
}

// EventState alias for the test package : keeps subtest assertions
// readable without an inverter. prefix on every constant.
type EventState = inverter.EventState

const (
	stateDefault        = inverter.StateDefault
	stateEventReceived  = inverter.StateEventReceived
	stateEventStarted   = inverter.StateEventStarted
	stateEventCompleted = inverter.StateEventCompleted
	stateEventCancelled = inverter.StateEventCancelled
)

// recordingHook returns a hook plus a func that returns the captured slice
// (under its own mutex : hooks can fire from different Tick goroutines in
// theory, though our tests are single-threaded).
func recordingHook() (hook inverter.TransitionHook, drain func() []transitionRecord) {
	var (
		mu   sync.Mutex
		seen []transitionRecord
	)
	hook = func(prev, next inverter.EventState, evt *sep2.DERControl) {
		mu.Lock()
		defer mu.Unlock()
		rec := transitionRecord{Prev: prev, Next: next, EvtPresent: evt != nil}
		if evt != nil {
			rec.MRID = evt.MRID
		}
		seen = append(seen, rec)
	}
	drain = func() []transitionRecord {
		mu.Lock()
		defer mu.Unlock()
		out := make([]transitionRecord, len(seen))
		copy(out, seen)
		return out
	}
	return hook, drain
}

// cancelledControl returns a DERControl with EventStatus.CurrentStatus=2
// matching IEEE-038's cancellation surface. The caller's mRID and start
// fields are copied so the cancelled slice shape mirrors what
// DERControlCache.Diff produces.
func cancelledControl(src sep2.DERControl) sep2.DERControl {
	c := src.Copy()
	c.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusCancelled}
	return c
}

// =============================================================================
// EventState.String : supporting.
// =============================================================================

func TestEventState_String(t *testing.T) {
	t.Parallel()
	cases := map[inverter.EventState]string{
		inverter.StateDefault:         "DEFAULT",
		inverter.StateEventReceived:   "EVENT_RECEIVED",
		inverter.StateEventStarted:    "EVENT_STARTED",
		inverter.StateEventCompleted:  "EVENT_COMPLETED",
		inverter.StateEventCancelled:  "EVENT_CANCELLED",
		inverter.EventState(0xFFFFFF): "UNKNOWN",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("EventState(%d).String() = %q, want %q", s, got, want)
		}
	}
}

// =============================================================================
// Case 1: initial state is DEFAULT.
// =============================================================================

func TestStateMachine_InitialStateDefault(t *testing.T) {
	t.Parallel()
	sm := inverter.NewStateMachine()
	snap := sm.Current()
	if snap.State != stateDefault {
		t.Errorf("initial State = %s, want %s", snap.State, stateDefault)
	}
	if snap.ActiveMRID != "" {
		t.Errorf("initial ActiveMRID = %q, want empty", snap.ActiveMRID)
	}
	if snap.ActiveDERControl != nil {
		t.Errorf("initial ActiveDERControl = %+v, want nil", snap.ActiveDERControl)
	}
}

// =============================================================================
// Case 2: add → EVENT_RECEIVED.
// =============================================================================

func TestStateMachine_AddTransitionsToReceived(t *testing.T) {
	t.Parallel()
	now, _ := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()

	added := []sep2.DERControl{makeControl("A", 3*time.Minute, 120, nil, nil)}
	sm.Tick(now(), added, nil, sched)

	snap := sm.Current()
	if snap.State != stateEventReceived {
		t.Errorf("State = %s, want %s", snap.State, stateEventReceived)
	}
	if snap.ActiveMRID != "A" {
		t.Errorf("ActiveMRID = %q, want %q", snap.ActiveMRID, "A")
	}
	if snap.ActiveDERControl == nil || snap.ActiveDERControl.MRID != "A" {
		t.Errorf("ActiveDERControl = %+v, want non-nil with mRID=A", snap.ActiveDERControl)
	}
	// activeExpireAt should be set so subsequent natural-completion fires.
	if got := inverter.ActiveExpireAtForTesting(sm); got.IsZero() {
		t.Errorf("activeExpireAt = zero, want set after EVENT_RECEIVED")
	}
}

// =============================================================================
// Case 3: fire → EVENT_STARTED.
// =============================================================================

func TestStateMachine_FireTransitionsToStarted(t *testing.T) {
	t.Parallel()
	now, advance := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()

	start := fixedNow.Add(3 * time.Minute)
	added := []sep2.DERControl{makeControl("A", 3*time.Minute, 120, nil, nil)}
	sm.Tick(now(), added, nil, sched)
	if got := sm.Current().State; got != stateEventReceived {
		t.Fatalf("pre-fire State = %s, want %s", got, stateEventReceived)
	}

	// Advance the clock past start. Next Tick must transition to STARTED.
	advance(start.Add(1 * time.Second))
	sm.Tick(now(), nil, nil, sched)

	snap := sm.Current()
	if snap.State != stateEventStarted {
		t.Errorf("post-fire State = %s, want %s", snap.State, stateEventStarted)
	}
	if snap.ActiveMRID != "A" {
		t.Errorf("ActiveMRID = %q, want A", snap.ActiveMRID)
	}
	if snap.ActiveDERControl == nil {
		t.Errorf("ActiveDERControl = nil, want set")
	}
}

// =============================================================================
// Case 4: natural completion → EVENT_COMPLETED → DEFAULT.
// =============================================================================

func TestStateMachine_NaturalCompletionRevertsToDefault(t *testing.T) {
	t.Parallel()
	now, advance := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()
	hook, drain := recordingHook()
	sm.OnTransition(hook)

	added := []sep2.DERControl{makeControl("A", 1*time.Minute, 60, nil, nil)}
	sm.Tick(now(), added, nil, sched)
	// Advance past fire AND expire so this single Tick collapses
	// RECEIVED → STARTED → COMPLETED → DEFAULT.
	advance(fixedNow.Add(3 * time.Minute))
	sm.Tick(now(), nil, nil, sched)

	snap := sm.Current()
	if snap.State != stateDefault {
		t.Errorf("post-completion State = %s, want DEFAULT", snap.State)
	}
	if snap.ActiveMRID != "" {
		t.Errorf("post-completion ActiveMRID = %q, want empty", snap.ActiveMRID)
	}
	if snap.ActiveDERControl != nil {
		t.Errorf("post-completion ActiveDERControl = %+v, want nil", snap.ActiveDERControl)
	}

	// Hook trail: DEFAULT→RECEIVED, RECEIVED→STARTED, STARTED→COMPLETED, COMPLETED→DEFAULT.
	recs := drain()
	want := []transitionRecord{
		{Prev: stateDefault, Next: stateEventReceived, MRID: "A", EvtPresent: true},
		{Prev: stateEventReceived, Next: stateEventStarted, MRID: "A", EvtPresent: true},
		{Prev: stateEventStarted, Next: stateEventCompleted, MRID: "A", EvtPresent: true},
		{Prev: stateEventCompleted, Next: stateDefault, MRID: "", EvtPresent: false},
	}
	assertTransitionTrail(t, recs, want)
}

// =============================================================================
// Case 5: server cancel mid-event → EVENT_CANCELLED → DEFAULT.
// =============================================================================

func TestStateMachine_ServerCancelMidEvent(t *testing.T) {
	t.Parallel()
	now, advance := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()
	hook, drain := recordingHook()
	sm.OnTransition(hook)

	src := makeControl("A", 1*time.Minute, 600, nil, nil) // long duration so cancel happens mid-flight
	sm.Tick(now(), []sep2.DERControl{src}, nil, sched)
	// Advance past fire → STARTED.
	advance(fixedNow.Add(2 * time.Minute))
	sm.Tick(now(), nil, nil, sched)
	if got := sm.Current().State; got != stateEventStarted {
		t.Fatalf("pre-cancel State = %s, want EVENT_STARTED", got)
	}

	// Server cancels A mid-event.
	sm.Tick(now(), nil, []sep2.DERControl{cancelledControl(src)}, sched)

	snap := sm.Current()
	if snap.State != stateDefault {
		t.Errorf("post-cancel State = %s, want DEFAULT", snap.State)
	}
	if snap.ActiveMRID != "" {
		t.Errorf("post-cancel ActiveMRID = %q, want empty", snap.ActiveMRID)
	}

	recs := drain()
	want := []transitionRecord{
		{Prev: stateDefault, Next: stateEventReceived, MRID: "A", EvtPresent: true},
		{Prev: stateEventReceived, Next: stateEventStarted, MRID: "A", EvtPresent: true},
		{Prev: stateEventStarted, Next: stateEventCancelled, MRID: "A", EvtPresent: true},
		{Prev: stateEventCancelled, Next: stateDefault, MRID: "", EvtPresent: false},
	}
	assertTransitionTrail(t, recs, want)
}

// =============================================================================
// Case 6: server cancel before fire → EVENT_CANCELLED → DEFAULT.
// =============================================================================

func TestStateMachine_ServerCancelBeforeFire(t *testing.T) {
	t.Parallel()
	now, _ := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()
	hook, drain := recordingHook()
	sm.OnTransition(hook)

	src := makeControl("A", 5*time.Minute, 60, nil, nil)
	sm.Tick(now(), []sep2.DERControl{src}, nil, sched)
	if got := sm.Current().State; got != stateEventReceived {
		t.Fatalf("pre-cancel State = %s, want EVENT_RECEIVED", got)
	}

	// Server cancels A before fire-time. Clock has NOT advanced; we expect
	// RECEIVED→CANCELLED→DEFAULT without ever passing through STARTED.
	sm.Tick(now(), nil, []sep2.DERControl{cancelledControl(src)}, sched)

	snap := sm.Current()
	if snap.State != stateDefault {
		t.Errorf("post-cancel State = %s, want DEFAULT", snap.State)
	}

	recs := drain()
	want := []transitionRecord{
		{Prev: stateDefault, Next: stateEventReceived, MRID: "A", EvtPresent: true},
		{Prev: stateEventReceived, Next: stateEventCancelled, MRID: "A", EvtPresent: true},
		{Prev: stateEventCancelled, Next: stateDefault, MRID: "", EvtPresent: false},
	}
	assertTransitionTrail(t, recs, want)
}

// =============================================================================
// Case 7: hook fired on each transition with correct (prev, next, evt).
// =============================================================================

func TestStateMachine_HookFiredOnEachTransition(t *testing.T) {
	t.Parallel()
	now, advance := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()
	hook, drain := recordingHook()
	sm.OnTransition(hook)

	src := makeControl("A", 1*time.Minute, 60, nil, nil)
	sm.Tick(now(), []sep2.DERControl{src}, nil, sched)
	advance(fixedNow.Add(90 * time.Second)) // past fireAt
	sm.Tick(now(), nil, nil, sched)
	advance(fixedNow.Add(5 * time.Minute)) // past expireAt
	sm.Tick(now(), nil, nil, sched)

	recs := drain()
	if len(recs) != 4 {
		t.Fatalf("hook fired %d times, want 4 (full lifecycle)", len(recs))
	}
	want := []transitionRecord{
		{Prev: stateDefault, Next: stateEventReceived, MRID: "A", EvtPresent: true},
		{Prev: stateEventReceived, Next: stateEventStarted, MRID: "A", EvtPresent: true},
		{Prev: stateEventStarted, Next: stateEventCompleted, MRID: "A", EvtPresent: true},
		{Prev: stateEventCompleted, Next: stateDefault, MRID: "", EvtPresent: false},
	}
	assertTransitionTrail(t, recs, want)
}

// =============================================================================
// Case 8: hook is optional : nil hook must not panic.
// =============================================================================

func TestStateMachine_NilHookDoesNotPanic(t *testing.T) {
	t.Parallel()
	now, advance := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine() // no OnTransition call : hook stays nil.

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic with nil hook: %v", r)
		}
	}()

	sm.Tick(now(), []sep2.DERControl{makeControl("A", 1*time.Minute, 60, nil, nil)}, nil, sched)
	advance(fixedNow.Add(5 * time.Minute))
	sm.Tick(now(), nil, nil, sched)

	// Cycle still completed correctly.
	if got := sm.Current().State; got != stateDefault {
		t.Errorf("post-lifecycle State = %s, want DEFAULT", got)
	}
}

// =============================================================================
// Case 9: idempotent re-adds.
// =============================================================================

func TestStateMachine_IdempotentReAdds(t *testing.T) {
	t.Parallel()
	now, _ := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()
	hook, drain := recordingHook()
	sm.OnTransition(hook)

	add := []sep2.DERControl{makeControl("A", 3*time.Minute, 60, nil, nil)}
	sm.Tick(now(), add, nil, sched)
	// Re-add the same mRID. Scheduler dedupes; state machine MUST NOT
	// double-fire EVENT_RECEIVED.
	sm.Tick(now(), add, nil, sched)
	sm.Tick(now(), add, nil, sched)

	snap := sm.Current()
	if snap.State != stateEventReceived {
		t.Errorf("State = %s, want EVENT_RECEIVED", snap.State)
	}
	if snap.ActiveMRID != "A" {
		t.Errorf("ActiveMRID = %q, want A", snap.ActiveMRID)
	}

	// Hook trail must show ONE DEFAULT→RECEIVED, no duplicates.
	recs := drain()
	if len(recs) != 1 {
		t.Errorf("hook fired %d times, want 1 (idempotent re-add)", len(recs))
	}
	if len(recs) > 0 && (recs[0].Prev != stateDefault || recs[0].Next != stateEventReceived) {
		t.Errorf("trail[0] = %+v, want DEFAULT→EVENT_RECEIVED", recs[0])
	}
}

// =============================================================================
// Case 10: multi-event overlap : first popped (FireAt-ascending) wins.
// =============================================================================

func TestStateMachine_MultiEventOverlapFirstWins(t *testing.T) {
	t.Parallel()
	now, advance := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()
	hook, drain := recordingHook()
	sm.OnTransition(hook)

	// Two events fire at the same Tick boundary (different fireAt within
	// the same clock window). The first popped (FireAt-ascending) becomes
	// active; the second is dropped on the floor per single-event scope.
	first := makeControl("first", 1*time.Minute, 600, nil, nil)
	second := makeControl("second", 2*time.Minute, 600, nil, nil)
	sm.Tick(now(), []sep2.DERControl{first, second}, nil, sched)

	// EVENT_RECEIVED with the first as active (earliest FireAt).
	if got := sm.Current().ActiveMRID; got != "first" {
		t.Errorf("pre-fire ActiveMRID = %q, want first", got)
	}

	// Advance past BOTH fire times in a single tick : both events pop.
	advance(fixedNow.Add(5 * time.Minute))
	sm.Tick(now(), nil, nil, sched)

	snap := sm.Current()
	if snap.State != stateEventStarted {
		t.Errorf("State = %s, want EVENT_STARTED (first wins)", snap.State)
	}
	if snap.ActiveMRID != "first" {
		t.Errorf("ActiveMRID = %q, want first (FireAt-ascending arbitration)", snap.ActiveMRID)
	}

	// Sanity: hook saw RECEIVED→STARTED for `first`. The second event was
	// dropped at the multi-event TODO line : no hook fires for it.
	recs := drain()
	startTransitions := 0
	for _, r := range recs {
		if r.Next == stateEventStarted {
			startTransitions++
			if r.MRID != "first" {
				t.Errorf("EVENT_STARTED transition for mRID=%q, want first", r.MRID)
			}
		}
	}
	if startTransitions != 1 {
		t.Errorf("EVENT_STARTED fired %d times, want 1 (single-event scope)", startTransitions)
	}
}

// =============================================================================
// Supporting: past-due event collapses lifecycle in a single Tick.
// =============================================================================

func TestStateMachine_PastDueEventCollapsesInOneTick(t *testing.T) {
	t.Parallel()
	now, _ := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()
	hook, drain := recordingHook()
	sm.OnTransition(hook)

	// start = now-2min, duration = 1min : server-side already completed.
	pastDue := makeControl("past", -2*time.Minute, 60, nil, nil)
	sm.Tick(now(), []sep2.DERControl{pastDue}, nil, sched)

	// One Tick collapses DEFAULT→RECEIVED→STARTED→COMPLETED→DEFAULT.
	snap := sm.Current()
	if snap.State != stateDefault {
		t.Errorf("post-tick State = %s, want DEFAULT (past-due collapse)", snap.State)
	}

	recs := drain()
	want := []transitionRecord{
		{Prev: stateDefault, Next: stateEventReceived, MRID: "past", EvtPresent: true},
		{Prev: stateEventReceived, Next: stateEventStarted, MRID: "past", EvtPresent: true},
		{Prev: stateEventStarted, Next: stateEventCompleted, MRID: "past", EvtPresent: true},
		{Prev: stateEventCompleted, Next: stateDefault, MRID: "", EvtPresent: false},
	}
	assertTransitionTrail(t, recs, want)
}

// =============================================================================
// Supporting: Tick on nil scheduler panics.
// =============================================================================

func TestStateMachine_TickPanicsOnNilScheduler(t *testing.T) {
	t.Parallel()
	sm := inverter.NewStateMachine()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Tick with nil scheduler did not panic")
		}
	}()
	sm.Tick(fixedNow, nil, nil, nil)
}

// =============================================================================
// Supporting: hook can be replaced mid-lifecycle.
// =============================================================================

func TestStateMachine_HookCanBeReplaced(t *testing.T) {
	t.Parallel()
	now, advance := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()

	first, drainFirst := recordingHook()
	second, drainSecond := recordingHook()
	sm.OnTransition(first)

	sm.Tick(now(), []sep2.DERControl{makeControl("A", 1*time.Minute, 60, nil, nil)}, nil, sched)

	// Swap hook AFTER EVENT_RECEIVED fires.
	sm.OnTransition(second)
	advance(fixedNow.Add(5 * time.Minute))
	sm.Tick(now(), nil, nil, sched)

	firstRecs := drainFirst()
	if len(firstRecs) != 1 || firstRecs[0].Next != stateEventReceived {
		t.Errorf("first hook recorded %v, want exactly DEFAULT→EVENT_RECEIVED", firstRecs)
	}
	secondRecs := drainSecond()
	if len(secondRecs) != 3 {
		t.Errorf("second hook fired %d times, want 3 (RECEIVED→STARTED, STARTED→COMPLETED, COMPLETED→DEFAULT)",
			len(secondRecs))
	}
}

// =============================================================================
// Helpers.
// =============================================================================

// assertTransitionTrail compares an observed hook trail against the wanted
// trail. Logs the full sequence on mismatch.
func assertTransitionTrail(t *testing.T, got, want []transitionRecord) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("trail length = %d, want %d (got=%+v, want=%+v)", len(got), len(want), got, want)
		return
	}
	for i, w := range want {
		g := got[i]
		if g.Prev != w.Prev || g.Next != w.Next || g.MRID != w.MRID || g.EvtPresent != w.EvtPresent {
			t.Errorf("trail[%d] = %+v, want %+v", i, g, w)
		}
	}
}
