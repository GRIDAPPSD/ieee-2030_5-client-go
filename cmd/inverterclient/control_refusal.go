package main

import (
	"log"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// refusalLog logs a refused control when the reason changes, so a refusal
// that holds for every tick of an event is logged once, and its end is
// logged too.
type refusalLog struct{ last string }

func (l *refusalLog) observe(who string, refused error) {
	reason := ""
	if refused != nil {
		reason = refused.Error()
	}
	if reason == l.last {
		return
	}
	l.last = reason
	if refused != nil {
		log.Printf("%s: control refused: %v", who, refused)
		return
	}
	log.Printf("%s: control no longer refused", who)
}

// controlApplier applies a control base and, when the base is refused
// whole, keeps applying the base in force before it.
type controlApplier struct {
	inForce  *sep2.DERControlBase
	refusals refusalLog
}

func (a *controlApplier) apply(who string, base *sep2.DERControlBase, grid inverter.GridState, maxPW float64, ratings inverter.Ratings, curves *inverter.DERCurveCache) inverter.ControlOutputs {
	out := inverter.ApplyControlsWithCurves(base, grid, maxPW, ratings, curves)
	a.refusals.observe(who, out.Refused)
	if out.Refused == nil {
		if base == nil {
			a.inForce = nil
		} else {
			c := base.Copy()
			a.inForce = &c
		}
		return out
	}
	kept := inverter.ApplyControlsWithCurves(a.inForce, grid, maxPW, ratings, curves)
	if kept.Refused != nil {
		// The ratings changed under the base in force; never hand the
		// device an empty output, which would disconnect it.
		a.inForce = nil
		kept = inverter.ApplyControlsWithCurves(nil, grid, maxPW, ratings, curves)
	}
	return kept
}

// controlValidator is the state machine's receipt check: an event the
// device cannot honor under ratings() is rejected whole.
func controlValidator(ratings func() inverter.Ratings) func(*sep2.DERControl) error {
	return func(evt *sep2.DERControl) error {
		return inverter.CheckControl(evt.DERControlBase, ratings())
	}
}
