package main

import (
	"log"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// refusalLog logs a line when its message changes, so a refusal that holds
// for every tick is logged once, and its end is logged too.
type refusalLog struct{ last string }

func (l *refusalLog) observe(who, msg string) {
	if msg == l.last {
		return
	}
	prev := l.last
	l.last = msg
	if msg != "" {
		log.Printf("%s: %s", who, msg)
		return
	}
	if prev != "" {
		log.Printf("%s: control no longer refused", who)
	}
}

// controlApplier applies a control base. A base refused whole falls back to
// the DefaultDERControl's base, and to the nil-base output when the default
// is absent or refused too.
type controlApplier struct {
	refusals refusalLog
}

func (a *controlApplier) apply(who string, base *sep2.DERControlBase, defaultCtl *sep2.DefaultDERControl, grid inverter.GridState, maxPW float64, ratings inverter.Ratings, curves *inverter.DERCurveCache) inverter.ControlOutputs {
	out := inverter.ApplyControlsWithCurves(base, grid, maxPW, ratings, curves)
	if out.Refused == nil {
		a.refusals.observe(who, "")
		return out
	}
	if defaultCtl != nil && defaultCtl.DERControlBase != nil && defaultCtl.DERControlBase != base {
		def := inverter.ApplyControlsWithCurves(defaultCtl.DERControlBase, grid, maxPW, ratings, curves)
		if def.Refused == nil {
			a.refusals.observe(who, "control refused: "+out.Refused.Error()+"; applying the default control")
			return def
		}
	}
	a.refusals.observe(who, "control refused: "+out.Refused.Error()+"; applying no control")
	return inverter.ApplyControlsWithCurves(nil, grid, maxPW, ratings, curves)
}

// controlValidator is the state machine's receipt check: an event the
// device cannot honor under ratings() is rejected whole.
func controlValidator(ratings func() inverter.Ratings) func(*sep2.DERControl) error {
	return func(evt *sep2.DERControl) error {
		return inverter.CheckControl(evt.DERControlBase, ratings())
	}
}

// newEventStateMachine is the single-device state machine, rejecting at
// receipt an event this device cannot honor.
func newEventStateMachine() *inverter.StateMachine {
	sm := inverter.NewStateMachine()
	sm.SetValidator(controlValidator(ownRatings))
	return sm
}
