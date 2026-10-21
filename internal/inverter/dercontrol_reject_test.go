package inverter_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/invertertest"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// badControl is a control whose base fails core's decode on the wire.
func badControl(mrid, replyTo string, mask sep2.HexBinary8) sep2.DERControl {
	dc := makeDERControl(mrid, sep2.EventStatusScheduled)
	dc.ReplyTo = replyTo
	dc.ResponseRequired = &mask
	fv := invertertest.BadFixedVar
	dc.DERControlBase = &sep2.DERControlBase{OpModFixedVar: &fv}
	return dc
}

// A list holding one undecodable control still reaches the cache: the valid
// control is cached, the bad one is kept aside as rejected with the identity
// a 253 needs, and no poll logs a fetch failure.
func TestPollDERControlList_UndecodableItemKeepsTheList(t *testing.T) {
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		invertertest.WriteListWithBadItems(t, w, sep2.DERControlList{DERControl: []sep2.DERControl{
			makeDERControl("A", sep2.EventStatusScheduled),
			badControl("B", "/rsp/B", 0x02),
		}}, 1)
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()
	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return 10 * time.Millisecond
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()
	logs := captureLogs(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()
		if err := waitFor(func() bool { return hits.Load() >= 3 }, time.Second); err != nil {
			t.Errorf("fewer than 3 polls: %v", err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("PollDERControlList returned %v, want context.Canceled", err)
		}
	})

	snap := cache.Snapshot()
	if _, ok := snap["A"]; !ok || len(snap) != 1 {
		t.Errorf("cached mRIDs %v, want only A", snap)
	}
	rej := cache.Rejected()
	if len(rej) != 1 || rej[0].MRID != "B" || rej[0].ReplyTo != "/rsp/B" ||
		rej[0].ResponseRequired == nil || *rej[0].ResponseRequired != 0x02 || rej[0].Err == nil {
		t.Errorf("rejected = %+v, want B with replyTo /rsp/B, responseRequired 0x02 and an error", rej)
	}
	if strings.Contains(logs, "fetch failed") {
		t.Errorf("a poll logged a fetch failure:\n%s", logs)
	}
}

// The rejection wins over a control with the same mRID: the cache leaves
// the control out, so it is never added, and keeps the rejected item.
// Rejected items with an empty mRID are kept side by side, not merged.
func TestDERControlCache_RejectionWinsAndEmptyMRIDsDoNotCollide(t *testing.T) {
	cache := inverter.NewDERControlCache()
	res := invertertest.ListResult(
		[]sep2.DERControl{makeDERControl("A", 0), makeDERControl("X", 0)},
		invertertest.Rejected("X", "", 0), invertertest.Rejected("", "", 0), invertertest.Rejected("", "", 0),
	)
	added, _, _ := cache.Diff(res)
	if got := mridsOf(added); len(got) != 1 || got[0] != "A" {
		t.Errorf("Diff added %v, want [A]", got)
	}
	if n := cache.Refresh(res); n != 1 {
		t.Errorf("Refresh committed %d controls, want 1", n)
	}
	if _, ok := cache.Snapshot()["X"]; ok {
		t.Error("control X cached although X is rejected")
	}
	rej := cache.Rejected()
	if len(rej) != 3 || rej[0].MRID != "X" || rej[1].MRID != "" || rej[2].MRID != "" {
		t.Fatalf("rejected = %+v, want X and two empty-mRID items", rej)
	}
	rej[0].MRID = "mutated"
	if cache.Rejected()[0].MRID != "X" {
		t.Error("Rejected returned the cache's own slice")
	}
	cache.Refresh(invertertest.ListResult([]sep2.DERControl{makeDERControl("A", 0)}))
	if got := cache.Rejected(); len(got) != 0 {
		t.Errorf("rejected after a clean list = %+v, want none", got)
	}
}

// Reject answers each mRID once, keeps the state in force, and wins over a
// control with the same mRID even when no validator is installed.
func TestStateMachine_RejectWithoutValidator(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	sched := inverter.NewScheduler(clock, inverter.SeededRandForTesting(1, 2))
	sm := inverter.NewStateMachine()
	var trail []string
	sm.AddTransitionHook(func(_, next inverter.EventState, evt *sep2.DERControl) {
		if evt != nil {
			trail = append(trail, evt.MRID+" "+next.String())
		}
	})

	items := []sep2.RejectedDERControl{
		invertertest.Rejected("X", "/rsp/X", 0x03), invertertest.Rejected("", "", 0x03), invertertest.Rejected("", "", 0x03),
	}
	logs := captureLogs(t, func() {
		sm.Reject(now, items, sched)
		sm.Reject(now, items, sched)
	})
	sm.Tick(now, []sep2.DERControl{controlAt("X", now.Add(-time.Minute), 3600, sep2.DERControlBase{})}, nil, sched)

	if len(trail) != 1 || trail[0] != "X EVENT_REJECTED" {
		t.Errorf("trail %v, want exactly [X EVENT_REJECTED]", trail)
	}
	if snap := sm.Current(); snap.State != inverter.StateDefault || snap.ActiveMRID != "" {
		t.Errorf("state %s mRID %q, want DEFAULT with none active", snap.State, snap.ActiveMRID)
	}
	if n := strings.Count(logs, "no mRID, no response possible: bad base \n"); n != 4 {
		t.Errorf("empty-mRID items logged %d times over 2 calls, want 4; log:\n%s", n, logs)
	}
}

// The cache's rejected items are deep copies: changing a pointer field of
// the list it was given, or of a copy it handed out, does not reach it.
func TestDERControlCache_RejectedIsADeepCopy(t *testing.T) {
	cache := inverter.NewDERControlCache()
	src := invertertest.Rejected("X", "/rsp/X", 0x03)
	src.Interval = &sep2.DateTimeInterval{Start: 100, Duration: 60}
	src.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusScheduled}
	cache.Refresh(invertertest.ListResult(nil, src))

	*src.ResponseRequired = 0x01
	src.Interval.Start = 999
	src.EventStatus.CurrentStatus = sep2.EventStatusCancelled
	got := cache.Rejected()
	if *got[0].ResponseRequired != 0x03 || got[0].Interval.Start != 100 || got[0].EventStatus.CurrentStatus != sep2.EventStatusScheduled {
		t.Errorf("source change reached the cache: responseRequired 0x%02x, start %d, status %d",
			uint8(*got[0].ResponseRequired), got[0].Interval.Start, got[0].EventStatus.CurrentStatus)
	}

	*got[0].ResponseRequired = 0x01
	got[0].Interval.Start = 999
	again := cache.Rejected()
	if *again[0].ResponseRequired != 0x03 || again[0].Interval.Start != 100 {
		t.Errorf("a handed-out copy's change reached the cache: responseRequired 0x%02x, start %d",
			uint8(*again[0].ResponseRequired), again[0].Interval.Start)
	}
}

// Update commits a list and logs its change counts under the caller's
// prefix. A control the server cancelled counts as cancelled on every list.
func TestDERControlCache_UpdateCommitsAndLogsChanges(t *testing.T) {
	cache := inverter.NewDERControlCache()
	list := invertertest.ListResult(
		[]sep2.DERControl{makeDERControl("A", 0), makeDERControl("B", sep2.EventStatusCancelled)},
		invertertest.Rejected("C", "", 0),
	)
	logs := captureLogs(t, func() {
		cache.Update(list, "src")
		cache.Update(list, "src")
	})
	first := "src: 2 total (+1 new, ~0 updated, !1 cancelled)\n"
	second := "src: 2 total (+0 new, ~0 updated, !1 cancelled)\n"
	if strings.Count(logs, first) != 1 || strings.Count(logs, second) != 1 || strings.Count(logs, "\n") != 2 {
		t.Errorf("log %q, want one %q and one %q", logs, first, second)
	}
	if snap := cache.Snapshot(); len(snap) != 2 || len(cache.Rejected()) != 1 {
		t.Errorf("cache holds %d controls and %d rejected, want 2 and 1", len(snap), len(cache.Rejected()))
	}
}

// Reject withdraws a pending event of the same mRID from the scheduler, so
// it never starts even when no later Tick reports it cancelled.
func TestStateMachine_RejectWithdrawsAPendingEvent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	sched := inverter.NewScheduler(clock, inverter.SeededRandForTesting(1, 2))
	sm := inverter.NewStateMachine()
	var trail []string
	sm.AddTransitionHook(func(_, next inverter.EventState, evt *sep2.DERControl) {
		m := ""
		if evt != nil {
			m = evt.MRID
		}
		trail = append(trail, m+" "+next.String())
	})
	sm.Tick(now, []sep2.DERControl{controlAt("X", now.Add(time.Minute), 3600, sep2.DERControlBase{})}, nil, sched)
	sm.Reject(now, []sep2.RejectedDERControl{invertertest.Rejected("X", "", 0x03)}, sched)
	sm.Tick(now.Add(2*time.Minute), nil, nil, sched)

	if got, want := strings.Join(trail, "|"), "X EVENT_RECEIVED|X EVENT_REJECTED| DEFAULT"; got != want {
		t.Errorf("trail %q, want X received, X rejected, then DEFAULT and nothing more", got)
	}
	if snap := sm.Current(); snap.State != inverter.StateDefault {
		t.Errorf("state %s, want DEFAULT", snap.State)
	}
}
