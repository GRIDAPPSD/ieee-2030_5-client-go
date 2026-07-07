// Tests for IEEE-038 : periodic DERControlList polling honoring pollRate.
//
// Two layers:
//
//  1. Pure DERControlCache tests (Snapshot / Diff / Refresh). No server,
//     no goroutines, fast. Cover the cache contract end to end.
//  2. End-to-end PollDERControlList tests against a gotls-backed HTTPS
//     listener (the same ccmTestEnv pattern IEEE-028 and IEEE-069 use).
//     These drive the goroutine loop, the tight-cadence test seam, and the
//     ctx-cancel exit.
//
// Test seam: SetDERControlPollDurationForTesting swaps the package-level
// derControlPollDuration mapper so tests run in tens of ms instead of
// minutes. Mirrors SetPollDurationForTesting from IEEE-028.
package inverter_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// =============================================================================
// Cache contract : pure tests, no HTTP, no goroutines.
// =============================================================================

// makeEventStatus returns a pointer to an EventStatus with the supplied
// currentStatus. Used in cache-diff fixtures.
func makeEventStatus(currentStatus uint8) *sep2.EventStatus {
	return &sep2.EventStatus{CurrentStatus: currentStatus}
}

// makeDERControl returns a DERControl with the supplied mRID and
// currentStatus. status==255 sentinel means "no EventStatus pointer at all"
// : exercises the omit-equals-scheduled branch.
func makeDERControl(mrid string, status uint8) sep2.DERControl {
	dc := sep2.DERControl{}
	dc.MRID = mrid
	if status != 255 {
		dc.EventStatus = makeEventStatus(status)
	}
	return dc
}

func TestDERControlCache_SnapshotIsIndependent(t *testing.T) {
	t.Parallel()
	cache := inverter.NewDERControlCache()
	cache.Refresh([]sep2.DERControl{
		makeDERControl("A", sep2.EventStatusScheduled),
		makeDERControl("B", sep2.EventStatusActive),
	})

	snap := cache.Snapshot()
	if got, want := len(snap), 2; got != want {
		t.Fatalf("Snapshot len = %d, want %d", got, want)
	}
	// Mutate the snapshot : cache must not be affected.
	delete(snap, "A")
	snap["C"] = makeDERControl("C", sep2.EventStatusActive)

	if got := cache.Len(); got != 2 {
		t.Errorf("cache.Len after snapshot mutation = %d, want 2", got)
	}
	again := cache.Snapshot()
	if _, ok := again["A"]; !ok {
		t.Error("cache lost A after caller mutated snapshot")
	}
	if _, ok := again["C"]; ok {
		t.Error("cache picked up C from caller mutation")
	}
}

func TestDERControlCache_RefreshSkipsEmptyMRID(t *testing.T) {
	t.Parallel()
	cache := inverter.NewDERControlCache()
	n := cache.Refresh([]sep2.DERControl{
		makeDERControl("A", sep2.EventStatusScheduled),
		makeDERControl("", sep2.EventStatusActive), // skipped
		makeDERControl("B", sep2.EventStatusScheduled),
	})
	if n != 2 {
		t.Errorf("Refresh return = %d, want 2 (empty mRID dropped)", n)
	}
	snap := cache.Snapshot()
	if _, ok := snap[""]; ok {
		t.Error("empty-mRID entry leaked into cache")
	}
}

// TestDERControlCache_Diff exercises the four meaningful diff transitions
// the IEEE-039+ scheduler / state machine needs:
//
//   - added: mRID newly seen.
//   - updated: known mRID whose currentStatus changed (and is not cancelled).
//   - cancelled: any mRID whose next currentStatus == 2.
//   - no-change: identical EventStatus omitted from every bucket.
func TestDERControlCache_Diff(t *testing.T) {
	t.Parallel()

	type wantCounts struct{ added, updated, cancelled int }
	tests := []struct {
		name    string
		seeded  []sep2.DERControl
		next    []sep2.DERControl
		expect  wantCounts
		details map[string]string // mrid -> bucket ("added"/"updated"/"cancelled")
	}{
		{
			name:   "added when cache empty",
			seeded: nil,
			next: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
				makeDERControl("B", sep2.EventStatusScheduled),
			},
			expect:  wantCounts{added: 2},
			details: map[string]string{"A": "added", "B": "added"},
		},
		{
			name: "added : third event appears",
			seeded: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
				makeDERControl("B", sep2.EventStatusScheduled),
			},
			next: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
				makeDERControl("B", sep2.EventStatusScheduled),
				makeDERControl("C", sep2.EventStatusScheduled),
			},
			expect:  wantCounts{added: 1},
			details: map[string]string{"C": "added"},
		},
		{
			name: "cancelled : B flips to status=2",
			seeded: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
				makeDERControl("B", sep2.EventStatusScheduled),
			},
			next: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
				makeDERControl("B", sep2.EventStatusCancelled),
			},
			expect:  wantCounts{cancelled: 1},
			details: map[string]string{"B": "cancelled"},
		},
		{
			name: "updated : A goes scheduled→active",
			seeded: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
			},
			next: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusActive),
			},
			expect:  wantCounts{updated: 1},
			details: map[string]string{"A": "updated"},
		},
		{
			name: "no change : identical status omitted",
			seeded: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusActive),
				makeDERControl("B", sep2.EventStatusScheduled),
			},
			next: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusActive),
				makeDERControl("B", sep2.EventStatusScheduled),
			},
			expect: wantCounts{}, // all zero
		},
		{
			name:   "empty mRID dropped from diff",
			seeded: nil,
			next: []sep2.DERControl{
				makeDERControl("", sep2.EventStatusActive),
				makeDERControl("A", sep2.EventStatusScheduled),
			},
			expect:  wantCounts{added: 1},
			details: map[string]string{"A": "added"},
		},
		{
			name: "cancelled wins over also-new",
			// Cache empty, next contains a cancelled event. The current
			// implementation reports it in the cancelled bucket so the IEEE-039+
			// state machine sees the cancellation as the dominant transition.
			seeded: nil,
			next: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusCancelled),
			},
			expect:  wantCounts{cancelled: 1},
			details: map[string]string{"A": "cancelled"},
		},
		{
			name: "nil EventStatus treated as scheduled",
			seeded: []sep2.DERControl{
				makeDERControl("A", 255), // nil EventStatus → status 0
			},
			next: []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled), // same effective status
			},
			expect: wantCounts{}, // no change
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := inverter.NewDERControlCache()
			if len(tc.seeded) > 0 {
				cache.Refresh(tc.seeded)
			}
			added, updated, cancelled := cache.Diff(tc.next)
			if got := len(added); got != tc.expect.added {
				t.Errorf("added = %d, want %d", got, tc.expect.added)
			}
			if got := len(updated); got != tc.expect.updated {
				t.Errorf("updated = %d, want %d", got, tc.expect.updated)
			}
			if got := len(cancelled); got != tc.expect.cancelled {
				t.Errorf("cancelled = %d, want %d", got, tc.expect.cancelled)
			}
			for mrid, wantBucket := range tc.details {
				gotBucket := findBucket(mrid, added, updated, cancelled)
				if gotBucket != wantBucket {
					t.Errorf("mRID %s: bucket = %q, want %q", mrid, gotBucket, wantBucket)
				}
			}
			// Diff must be pure : cache state unchanged after Diff.
			if got := cache.Len(); got != len(tc.seeded) {
				t.Errorf("cache.Len after Diff = %d, want %d (Diff is read-only)", got, len(tc.seeded))
			}
		})
	}
}

func findBucket(mrid string, added, updated, cancelled []sep2.DERControl) string {
	for _, c := range added {
		if c.MRID == mrid {
			return "added"
		}
	}
	for _, c := range updated {
		if c.MRID == mrid {
			return "updated"
		}
	}
	for _, c := range cancelled {
		if c.MRID == mrid {
			return "cancelled"
		}
	}
	return ""
}

// =============================================================================
// End-to-end PollDERControlList : gotls listener + tight cadence test seam.
// =============================================================================

// derControlListXML encodes a DERControlList payload exactly the way the
// production server emits it (sep+xml MIME, urn:ieee:std:2030.5:ns namespace).
func writeDERControlList(t *testing.T, w http.ResponseWriter, controls []sep2.DERControl) {
	t.Helper()
	w.Header().Set("Content-Type", "application/sep+xml")
	list := sep2.DERControlList{
		DERControl: controls,
	}
	list.All = uint32(len(controls))
	list.Results = uint32(len(controls))
	if err := xml.NewEncoder(w).Encode(&list); err != nil {
		t.Errorf("encode DERControlList: %v", err)
	}
}

// TestPollDERControlList_HappyPath case 1: a 3-event list, one tick, cache
// populated with 3 entries keyed by mRID.
func TestPollDERControlList_HappyPath(t *testing.T) {
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeDERControlList(t, w, []sep2.DERControl{
			makeDERControl("A", sep2.EventStatusScheduled),
			makeDERControl("B", sep2.EventStatusActive),
			makeDERControl("C", sep2.EventStatusScheduled),
		})
	})

	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	// Tight cadence so the test runs in ms.
	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return 10 * time.Millisecond
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()

	// Wait for the initial tick to populate the cache.
	if err := waitFor(func() bool { return cache.Len() == 3 }, 300*time.Millisecond); err != nil {
		t.Fatalf("cache never reached len=3 (hits=%d): %v", hits.Load(), err)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PollDERControlList returned %v, want context.Canceled/DeadlineExceeded", err)
	}

	snap := cache.Snapshot()
	for _, mrid := range []string{"A", "B", "C"} {
		if _, ok := snap[mrid]; !ok {
			t.Errorf("snapshot missing mRID %s; have %v", mrid, snap)
		}
	}
}

// TestPollDERControlList_PollRateHonored case 2: back-to-back polls throttle
// at >= the configured interval. We feed a 50ms interval, run for ~250ms,
// and assert hit count is in the expected band (1 initial + roughly N
// ticker fires).
func TestPollDERControlList_PollRateHonored(t *testing.T) {
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"
	const interval = 50 * time.Millisecond

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeDERControlList(t, w, nil)
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return interval
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()

	// Let it run for ~5 intervals worth.
	time.Sleep(5 * interval)
	cancel()
	<-done

	// Initial-tick + ~4-5 ticker fires; allow a generous band so this is
	// not flaky on slow hardware. Hard upper bound rules out free-spinning.
	got := hits.Load()
	if got < 2 {
		t.Errorf("hits = %d, want >=2 (polling never ticked)", got)
	}
	if got > 12 {
		t.Errorf("hits = %d in ~%s, want <=12 (interval not honored)", got, 5*interval)
	}
}

// TestPollDERControlList_ContextCancel case 3: cancelling ctx mid-loop
// returns the goroutine within a tight bound and propagates ctx.Err.
// "No goroutine leak" is asserted by waiting on the done channel within a
// bounded deadline : if the closure leaked, the receive would time out.
func TestPollDERControlList_ContextCancel(t *testing.T) {
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"

	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		writeDERControlList(t, w, nil)
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	// 1s interval is fine : we cancel before the first ticker tick fires.
	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return 1 * time.Second
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()

	// Give the initial fetch time to complete.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("PollDERControlList did not return within 500ms of ctx cancel : goroutine leak")
	}
}

// TestPollDERControlList_DiffNewAndCancelled cases 4 + 5: across two ticks
// the cache surfaces an added event (C) and a cancelled event (B) through
// the public Diff method that the IEEE-039+ scheduler will consume.
func TestPollDERControlList_DiffNewAndCancelled(t *testing.T) {
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"

	var phase atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		switch phase.Load() {
		case 0:
			writeDERControlList(t, w, []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
				makeDERControl("B", sep2.EventStatusScheduled),
			})
		default:
			writeDERControlList(t, w, []sep2.DERControl{
				makeDERControl("A", sep2.EventStatusScheduled),
				makeDERControl("B", sep2.EventStatusCancelled),
				makeDERControl("C", sep2.EventStatusScheduled),
			})
		}
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return 20 * time.Millisecond
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()

	// Wait for the first tick to populate the cache.
	if err := waitFor(func() bool { return cache.Len() == 2 }, 300*time.Millisecond); err != nil {
		t.Fatalf("first poll didn't populate cache: %v", err)
	}

	// Capture diff against the *current* state, then flip the server and
	// wait for the second tick. We assert the diff via the public Diff
	// surface, which is what IEEE-039+ will consume.
	phase.Store(1)
	next := []sep2.DERControl{
		makeDERControl("A", sep2.EventStatusScheduled),
		makeDERControl("B", sep2.EventStatusCancelled),
		makeDERControl("C", sep2.EventStatusScheduled),
	}
	added, _, cancelled := cache.Diff(next)
	if len(added) != 1 || added[0].MRID != "C" {
		t.Errorf("Diff.added = %+v, want [C]", mridsOf(added))
	}
	if len(cancelled) != 1 || cancelled[0].MRID != "B" {
		t.Errorf("Diff.cancelled = %+v, want [B]", mridsOf(cancelled))
	}

	// Wait for the poll goroutine to commit the second snapshot so the
	// "live" cache reflects the new state. This drives the actual loop
	// integration, not just Diff in isolation.
	if err := waitFor(func() bool { return cache.Len() == 3 }, 300*time.Millisecond); err != nil {
		t.Fatalf("second poll didn't refresh cache to 3 entries: %v", err)
	}
	cancel()
	<-done
}

// TestPollDERControlList_TransientHTTPFailure case 6: a server that fails
// the first request with 500 and serves the second with 200 must not stop
// the polling loop. After two ticks the cache is populated.
func TestPollDERControlList_TransientHTTPFailure(t *testing.T) {
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeDERControlList(t, w, []sep2.DERControl{
			makeDERControl("A", sep2.EventStatusScheduled),
		})
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return 20 * time.Millisecond
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()

	if err := waitFor(func() bool { return cache.Len() == 1 }, 400*time.Millisecond); err != nil {
		t.Fatalf("cache never populated after 500 retry (hits=%d): %v", hits.Load(), err)
	}
	if hits.Load() < 2 {
		t.Errorf("hits = %d, want >=2 (loop did not retry past the 500)", hits.Load())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestPollDERControlList_EmptyList case 7: server returns an empty
// DERControlList. Cache must stay empty; no panic.
func TestPollDERControlList_EmptyList(t *testing.T) {
	env := newCCMTestEnv(t)
	const href = "/derp/0/derc"

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(href, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeDERControlList(t, w, nil)
	})
	serverURL, stop := startIdleListener(t, env, mux)
	defer stop()

	restore := inverter.SetDERControlPollDurationForTesting(func(_ uint32) time.Duration {
		return 20 * time.Millisecond
	})
	defer restore()

	client := newCSIPClient(t, env, serverURL, true)
	cache := inverter.NewDERControlCache()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.PollDERControlList(ctx, href, 30, cache) }()

	if err := waitFor(func() bool { return hits.Load() >= 1 }, 150*time.Millisecond); err != nil {
		t.Fatalf("server never hit: %v", err)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("cache.Len = %d, want 0 on empty list", got)
	}
	cancel()
	<-done
}

// TestPollDERControlList_RejectsEmptyHref guards the programmer-error path :
// the goroutine must refuse to start without an href so callers don't end
// up with a silent no-op log loop.
func TestPollDERControlList_RejectsEmptyHref(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	client := newCSIPClient(t, env, "https://example.invalid", true)
	cache := inverter.NewDERControlCache()
	err := client.PollDERControlList(context.Background(), "", 30, cache)
	if err == nil {
		t.Fatal("PollDERControlList with empty href returned nil, want error")
	}
}

func TestPollDERControlList_RejectsNilCache(t *testing.T) {
	t.Parallel()
	env := newCCMTestEnv(t)
	client := newCSIPClient(t, env, "https://example.invalid", true)
	err := client.PollDERControlList(context.Background(), "/derp/0/derc", 30, nil)
	if err == nil {
		t.Fatal("PollDERControlList with nil cache returned nil, want error")
	}
}

// =============================================================================
// Helpers
// =============================================================================

// waitFor polls `cond` every 5ms until it returns true or `bound` elapses.
// Returns nil on success, an error on timeout. Used to assert async
// progress in the polling-loop tests without sprinkling time.Sleep
// guesses.
func waitFor(cond func() bool, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		if cond() {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("waitFor: condition not met within bound")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mridsOf collects the MRID of each DERControl in a slice. Convenient for
// table-driven assertion error messages.
func mridsOf(controls []sep2.DERControl) []string {
	out := make([]string, 0, len(controls))
	for _, c := range controls {
		out = append(out, c.MRID)
	}
	return out
}
