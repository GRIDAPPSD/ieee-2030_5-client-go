package inverter_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// badFixedVar marks the control writeListWithBadItems turns into a refType 4
// FixedVar, which core refuses on decode and the encoder refuses to write.
var badFixedVar = sep2.FixedVar{RefType: 2, Value: 1234}

// writeListWithBadItems writes controls as a DERControlList, with every
// control carrying badFixedVar rewritten to refType 4 on the wire.
func writeListWithBadItems(t *testing.T, w http.ResponseWriter, controls []sep2.DERControl, bad int) {
	t.Helper()
	list := sep2.DERControlList{DERControl: controls}
	list.All = uint32(len(controls))
	list.Results = uint32(len(controls))
	raw, err := xml.Marshal(&list)
	if err != nil {
		t.Errorf("encode DERControlList: %v", err)
		return
	}
	good := []byte("<refType>2</refType><value>1234</value>")
	if n := bytes.Count(raw, good); n != bad {
		t.Errorf("setup: %d marked FixedVars in the list, want %d", n, bad)
	}
	w.Header().Set("Content-Type", "application/sep+xml")
	_, _ = w.Write(bytes.ReplaceAll(raw, good, []byte("<refType>4</refType><value>1234</value>")))
}

// badControl is a control whose base fails core's decode.
func badControl(mrid, replyTo string, mask sep2.HexBinary8) sep2.DERControl {
	dc := makeDERControl(mrid, sep2.EventStatusScheduled)
	dc.ReplyTo = replyTo
	dc.ResponseRequired = &mask
	fv := badFixedVar
	dc.DERControlBase = &sep2.DERControlBase{OpModFixedVar: &fv}
	return dc
}

type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(t *testing.T) *lockedLog {
	t.Helper()
	l := &lockedLog{}
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

// A list holding one undecodable control still reaches the cache: the valid
// control is cached, the bad one is kept aside as rejected with the identity
// a 253 needs, and no poll logs a fetch failure.
func TestPollDERControlList_UndecodableItemKeepsTheList(t *testing.T) {
	logs := captureLog(t)
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeListWithBadItems(t, w, []sep2.DERControl{
			makeDERControl("A", sep2.EventStatusScheduled),
			badControl("B", "/rsp/B", 0x02),
		}, 1)
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()
	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return 10 * time.Millisecond
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()

	if err := waitFor(func() bool { return hits.Load() >= 3 }, time.Second); err != nil {
		t.Fatalf("fewer than 3 polls: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("PollDERControlList returned %v, want context.Canceled", err)
	}

	snap := cache.Snapshot()
	if _, ok := snap["A"]; !ok || len(snap) != 1 {
		t.Errorf("cached mRIDs %v, want only A", snap)
	}
	rej := cache.Rejected()
	if len(rej) != 1 || rej[0].MRID != "B" || rej[0].ReplyTo != "/rsp/B" ||
		rej[0].ResponseRequired == nil || *rej[0].ResponseRequired != 0x02 || rej[0].Err == nil {
		t.Errorf("rejected = %+v, want B with replyTo /rsp/B, responseRequired 0x02 and an error", rej)
	}
	if out := logs.String(); strings.Contains(out, "fetch failed") {
		t.Errorf("a poll logged a fetch failure:\n%s", out)
	}
}

func listResult(controls []sep2.DERControl, rejected ...sep2.RejectedDERControl) sep2.DERControlListResult {
	return sep2.DERControlListResult{DERControlList: sep2.DERControlList{DERControl: controls}, Rejected: rejected}
}

func rejectedItem(mrid string) sep2.RejectedDERControl {
	var r sep2.RejectedDERControl
	r.MRID = mrid
	r.Err = errors.New("bad base " + mrid)
	return r
}

// The rejection wins over a control with the same mRID: the cache leaves
// the control out, so it is never added, and keeps the rejected item.
// Rejected items with an empty mRID are kept side by side, not merged.
func TestDERControlCache_RejectionWinsAndEmptyMRIDsDoNotCollide(t *testing.T) {
	cache := inverter.NewDERControlCache()
	res := listResult(
		[]sep2.DERControl{makeDERControl("A", 0), makeDERControl("X", 0)},
		rejectedItem("X"), rejectedItem(""), rejectedItem(""),
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
	cache.Refresh(listResult([]sep2.DERControl{makeDERControl("A", 0)}))
	if got := cache.Rejected(); len(got) != 0 {
		t.Errorf("rejected after a clean list = %+v, want none", got)
	}
}

// Reject answers each mRID once, keeps the state in force, and wins over a
// control with the same mRID even when no validator is installed.
func TestStateMachine_RejectWithoutValidator(t *testing.T) {
	logs := captureLog(t)
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

	x := rejectedItem("X")
	x.ReplyTo = "/rsp/X"
	items := []sep2.RejectedDERControl{x, rejectedItem(""), rejectedItem("")}
	sm.Reject(now, items)
	sm.Reject(now, items)
	sm.Tick(now, []sep2.DERControl{controlAt("X", now.Add(-time.Minute), 3600, sep2.DERControlBase{})}, nil, sched)

	if len(trail) != 1 || trail[0] != "X EVENT_REJECTED" {
		t.Errorf("trail %v, want exactly [X EVENT_REJECTED]", trail)
	}
	if snap := sm.Current(); snap.State != inverter.StateDefault || snap.ActiveMRID != "" {
		t.Errorf("state %s mRID %q, want DEFAULT with none active", snap.State, snap.ActiveMRID)
	}
	if n := strings.Count(logs.String(), "no mRID, no response possible: bad base \n"); n != 4 {
		t.Errorf("empty-mRID items logged %d times over 2 calls, want 4; log:\n%s", n, logs.String())
	}
}
