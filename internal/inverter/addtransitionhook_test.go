// Tests for IEEE-044 : StateMachine.AddTransitionHook multiplex (Phase 6
// ticket 2 of 3).
//
// AddTransitionHook is the append-only hook surface added in IEEE-044 so
// multiple independent concerns (IEEE-042 curve refresh + IEEE-044
// Response POST) can compose on the same state machine without one
// stomping the other (which the replace-only OnTransition would do).
//
// Coverage:
//   - Two appended hooks both fire on every transition, in registration order.
//   - Nil hook is silently ignored : no panic, no spurious slot.
//   - OnTransition slot + AddTransitionHook entries compose (OnTransition
//     fires first, then appended hooks in order).
package inverter_test

import (
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// TestStateMachine_AddTransitionHookMultiplex registers two hooks via
// AddTransitionHook and asserts both fire on a single transition, in the
// order they were registered. This is the IEEE-044 contract that lets
// IEEE-042 curve refresh and IEEE-044 Response POST coexist on the same
// state machine.
func TestStateMachine_AddTransitionHookMultiplex(t *testing.T) {
	t.Parallel()
	now, _ := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()

	var (
		mu    sync.Mutex
		order []string
	)
	hookA := func(prev, next inverter.EventState, evt *sep2.DERControl) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "A")
	}
	hookB := func(prev, next inverter.EventState, evt *sep2.DERControl) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "B")
	}

	sm.AddTransitionHook(hookA)
	sm.AddTransitionHook(hookB)

	// Single DEFAULT→EVENT_RECEIVED transition.
	added := []sep2.DERControl{makeControl("X", 3*time.Minute, 60, nil, nil)}
	sm.Tick(now(), added, nil, sched)

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	want := []string{"A", "B"}
	if len(got) != len(want) {
		t.Fatalf("hook fire count = %d, want %d (sequence=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hook[%d] = %q, want %q (full sequence=%v)", i, got[i], want[i], got)
		}
	}
}

// TestStateMachine_AddTransitionHookNilIgnored confirms AddTransitionHook
// silently drops a nil hook : callers can guard with "register if
// non-nil" knowing AddTransitionHook tolerates the nil path too.
func TestStateMachine_AddTransitionHookNilIgnored(t *testing.T) {
	t.Parallel()
	now, _ := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()

	sm.AddTransitionHook(nil) // must not panic, must not register a nil slot.

	var fired int
	sm.AddTransitionHook(func(prev, next inverter.EventState, evt *sep2.DERControl) {
		fired++
	})

	added := []sep2.DERControl{makeControl("X", 3*time.Minute, 60, nil, nil)}
	sm.Tick(now(), added, nil, sched)

	if fired != 1 {
		t.Errorf("hook fired %d times, want 1 (nil-append must not occupy a slot)", fired)
	}
}

// TestStateMachine_OnTransitionAndAddTransitionHookCompose verifies that
// the OnTransition slot AND AddTransitionHook entries fire on the same
// transition in the documented order (OnTransition first, then appended
// hooks in registration order). This is the IEEE-044 wire-up contract:
// IEEE-042 (curve refresh) lives in AddTransitionHook and IEEE-044
// (Response POST) lives in AddTransitionHook; OnTransition stays as the
// test-swap slot.
func TestStateMachine_OnTransitionAndAddTransitionHookCompose(t *testing.T) {
	t.Parallel()
	now, _ := newClock(fixedNow)
	sched := inverter.NewSchedulerWithClockForTesting(now)
	sm := inverter.NewStateMachine()

	var (
		mu    sync.Mutex
		order []string
	)
	sm.OnTransition(func(prev, next inverter.EventState, evt *sep2.DERControl) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "slot")
	})
	sm.AddTransitionHook(func(prev, next inverter.EventState, evt *sep2.DERControl) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "append1")
	})
	sm.AddTransitionHook(func(prev, next inverter.EventState, evt *sep2.DERControl) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "append2")
	})

	added := []sep2.DERControl{makeControl("X", 3*time.Minute, 60, nil, nil)}
	sm.Tick(now(), added, nil, sched)

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	want := []string{"slot", "append1", "append2"}
	if len(got) != len(want) {
		t.Fatalf("hook fire count = %d, want %d (sequence=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hook[%d] = %q, want %q (full sequence=%v)", i, got[i], want[i], got)
		}
	}
}
