package inverter

// Test-binary-only accessor for the state machine's internal expiry slot.
// Linked into the _test binary only (file ends in _test.go). Pattern mirrors
// IEEE-038's dercontrol_poll_export_test.go and IEEE-039's
// scheduler_export_test.go.
//
// The public Current() snapshot intentionally does NOT expose activeExpireAt
// — it's an internal scheduling concern not relevant to IEEE-041's
// ApplyControls consumer. Tests need to verify expiry handling without
// driving real wall-clock, so we expose a read-only helper here.

import "time"

// ActiveExpireAtForTesting returns the state machine's tracked expiry for
// the active event. Returns the zero time when no event is active.
func ActiveExpireAtForTesting(sm *StateMachine) time.Time {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.activeExpireAt
}
