package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

func TestRefusalLogLogsEachChangeOnce(t *testing.T) {
	logs := captureSessionLog(t)
	var l refusalLog
	refused := fmt.Errorf("%w: refType 3 (%%statVarAvail) but the device has no valid statVarAvail", inverter.ErrFixedVarRefused)

	l.observe("inverter", nil)
	l.observe("inverter", refused)
	l.observe("inverter", refused)
	l.observe("inverter", nil)
	l.observe("inverter", nil)

	got := logs.String()
	if n := strings.Count(got, "inverter: control refused: opModFixedVar refused: refType 3"); n != 1 {
		t.Errorf("refusal logged %d times, want 1; log:\n%s", n, got)
	}
	if n := strings.Count(got, "inverter: control no longer refused"); n != 1 {
		t.Errorf("clearing logged %d times, want 1; log:\n%s", n, got)
	}
	if lines := strings.Count(got, "\n"); lines != 2 {
		t.Errorf("log has %d lines, want 2 (no line before the first refusal); log:\n%s", lines, got)
	}
}

// ownRatings must follow the nameplate a sim config sets, and must leave
// statVarAvail unknown rather than zero, which would scale a %statVarAvail
// fixed var to 0 VAr instead of refusing it.
func TestOwnRatingsFollowTheNameplate(t *testing.T) {
	prev := inverter.Rating
	t.Cleanup(func() { inverter.Rating = prev })
	applyNameplate(5000)

	r := ownRatings()
	if r.SetMaxW != 5000 || r.SetMaxVar != 2200 {
		t.Errorf("ownRatings = SetMaxW %g, SetMaxVar %g; want 5000 and 2200", float64(r.SetMaxW), r.SetMaxVar)
	}
	if r.StatVarAvail != nil {
		t.Errorf("StatVarAvail = %g, want nil (not tracked)", *r.StatVarAvail)
	}
}
