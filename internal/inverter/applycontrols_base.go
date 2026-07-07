package inverter

import "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

// ActiveControlBase resolves which DERControlBase the simulation tick loop
// should pass to ApplyControls. Closes the IEEE-041 defect at
// cmd/inverterclient/main.go where the base was hard-coded nil : the inverter
// never observed server-sent DERControls.
//
// Decision tree (in order : first match wins):
//
//  1. State machine reports EVENT_STARTED AND the active DERControl has a
//     non-nil DERControlBase → return the event's base. This is the
//     CSIP V1.2 CORE-012 step 6 "apply the active event" path.
//  2. defaultCtl is non-nil AND its DERControlBase is non-nil → return that.
//     This is the IEEE 2030.5 §10.7 "DefaultDERControl fallback" path :
//     when no event is active, the device applies the program's default.
//  3. Otherwise → nil. Preserves the pre-IEEE-041 behavior when no CSIP
//     server is reachable, no default has been provisioned, or the only
//     event in flight has a nil DERControlBase. ApplyControls handles a
//     nil base as a no-op (see internal/inverter/controller.go).
//
// Transient states (EVENT_RECEIVED, EVENT_COMPLETED, EVENT_CANCELLED) are
// intentionally NOT treated as "active event." During EVENT_RECEIVED the
// event has not started yet : the device must continue applying the
// DefaultDERControl until the state machine pops the event at fire-time.
// During EVENT_COMPLETED / EVENT_CANCELLED the state machine auto-reverts
// to DEFAULT in the same Tick (see internal/inverter/statemachine.go
// step 4), so these states are short-lived; this function returns the
// default base for consistency.
//
// The function is pure: no I/O, no logging, no goroutines, no mutation of
// inputs. The returned pointer aliases either snap.ActiveDERControl's or
// defaultCtl's field : callers MUST treat it as read-only. ApplyControls
// already treats *DERControlBase as read-only.
func ActiveControlBase(snap EventStateSnapshot, defaultCtl *sep2.DefaultDERControl) *sep2.DERControlBase {
	if snap.State == StateEventStarted &&
		snap.ActiveDERControl != nil &&
		snap.ActiveDERControl.DERControlBase != nil {
		return snap.ActiveDERControl.DERControlBase
	}
	if defaultCtl != nil && defaultCtl.DERControlBase != nil {
		return defaultCtl.DERControlBase
	}
	return nil
}
