// Tests for the event scheduler with randomization (Phase 5 ticket 2 of 5).
//
// The scheduler is a pure data structure: no goroutine, no I/O, no real
// time.Now(). Every test injects a fixed-time nowFunc and a seeded *rand.Rand
// via the test-binary-only constructors in scheduler_export_test.go, so the
// suite is fully deterministic and race-clean by construction.
//
// Coverage of the 9 mandatory cases from the Phase 5 doc:
//
//   1. Parse happy path                        : TestScheduler_ParseHappyPath
//   2. Sort order                              : TestScheduler_SortOrderByFireAt
//   3. randomizeStart deterministic            : TestScheduler_RandomizeStartDeterministic
//   4. randomizeStart distribution             : TestScheduler_RandomizeStartDistribution
//   5. randomizeDuration                       : TestScheduler_RandomizeDuration
//   6. Cancellation removes from queue         : TestScheduler_CancellationRemovesFromQueue
//   7. Past-due event                          : TestScheduler_PastDueEvent
//   8. Empty queue                             : TestScheduler_EmptyQueue
//   9. nowFunc injection                       : TestScheduler_NowFuncInjection
//
// Supporting coverage:
//   - OnEventsAdded skips nil Interval         : TestScheduler_NilIntervalSkipped
//   - OnEventsAdded replaces by mRID           : TestScheduler_ReAddReplacesByMRID
//   - OnEventsUpdated is a documented no-op    : TestScheduler_UpdateIsNoOp
//   - NewScheduler panics on nil args          : TestScheduler_NewSchedulerPanicsOnNil
//   - applyRandomizeStart negative window      : TestApplyRandomizeStart_NegativeWindow
//   - applyRandomizeDuration clamp             : TestScheduler_RandomizeDurationClampsToZero

package inverter_test

import (
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// =============================================================================
// Helpers.
// =============================================================================

// fixedNow is the canonical wall-clock used by tests that don't need to
// advance the clock. UTC for determinism across machines.
var fixedNow = time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

// makeControl builds a DERControl with Interval and optional randomize
// pointers. startOffset is added to fixedNow; pass negative for past-due.
// duration is in seconds. randomizeStart/randomizeDuration are pointers so
// nil means "field absent" (omit-equals-zero per §10.1.4). 0-valued
// pointers are also accepted and tested.
func makeControl(mrid string, startOffset time.Duration, durationSec uint32, rs, rd *int32) sep2.DERControl {
	start := fixedNow.Add(startOffset).Unix()
	dc := sep2.DERControl{}
	dc.MRID = mrid
	dc.Interval = &sep2.DateTimeInterval{
		Start:    start,
		Duration: durationSec,
	}
	dc.RandomizeStart = rs
	dc.RandomizeDuration = rd
	return dc
}

// ptrInt32 returns a pointer to the int32 literal v. Convenience for
// table-driven tests where inlining &v on a literal is illegal.
func ptrInt32(v int32) *int32 {
	return &v
}

// =============================================================================
// Mandatory case 1: parse happy path.
// =============================================================================

func TestScheduler_ParseHappyPath(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	start := fixedNow.Add(3 * time.Minute)
	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 3*time.Minute, 120, nil, nil),
	})

	if got := s.Len(); got != 1 {
		t.Fatalf("queue Len = %d, want 1", got)
	}
	fireAt := inverter.EventFireAtForTesting(s)
	expireAt := inverter.EventExpireAtForTesting(s)
	if !fireAt.Equal(start) {
		t.Errorf("FireAt = %s, want %s", fireAt, start)
	}
	if want := start.Add(2 * time.Minute); !expireAt.Equal(want) {
		t.Errorf("ExpireAt = %s, want %s", expireAt, want)
	}
	if got := inverter.EventMRIDForTesting(s); got != "A" {
		t.Errorf("head mRID = %q, want %q", got, "A")
	}
}

// =============================================================================
// Mandatory case 2: sort order.
// =============================================================================

func TestScheduler_SortOrderByFireAt(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	// Insert in [3min, 1min, 2min] order : head must be the 1min event.
	s.OnEventsAdded([]sep2.DERControl{
		makeControl("three", 3*time.Minute, 60, nil, nil),
		makeControl("one", 1*time.Minute, 60, nil, nil),
		makeControl("two", 2*time.Minute, 60, nil, nil),
	})

	got := inverter.QueueMRIDsForTesting(s)
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("queue length = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("queue[%d] mRID = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}

	// PopExpired at +1min returns exactly "one".
	popped := inverter.PopExpiredMRIDsForTesting(s, fixedNow.Add(1*time.Minute))
	if len(popped) != 1 || popped[0] != "one" {
		t.Errorf("PopExpired(+1min) = %v, want [one]", popped)
	}
	if remaining := inverter.QueueMRIDsForTesting(s); len(remaining) != 2 ||
		remaining[0] != "two" || remaining[1] != "three" {
		t.Errorf("remaining queue = %v, want [two three]", remaining)
	}
}

// =============================================================================
// Mandatory case 3: randomizeStart deterministic under a seeded RNG.
// =============================================================================

func TestScheduler_RandomizeStartDeterministic(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	start := fixedNow.Add(3 * time.Minute)
	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 3*time.Minute, 60, ptrInt32(30), nil),
	})

	fireAt := inverter.EventFireAtForTesting(s)
	// Bounds check: must land in [start, start+30s] inclusive.
	if fireAt.Before(start) || fireAt.After(start.Add(30*time.Second)) {
		t.Errorf("FireAt = %s, want in [%s, %s]", fireAt, start, start.Add(30*time.Second))
	}
	// Determinism check: re-seed the same way and replay. We cannot easily
	// predict the exact rand value without replicating rand/v2's PCG, but
	// we CAN assert "two schedulers with the same seed produce the same
	// FireAt" : which proves determinism end-to-end.
	s2 := inverter.NewSchedulerForTesting(fixedNow)
	s2.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 3*time.Minute, 60, ptrInt32(30), nil),
	})
	if got, want := inverter.EventFireAtForTesting(s2), fireAt; !got.Equal(want) {
		t.Errorf("replayed FireAt = %s, want %s (RNG non-deterministic?)", got, want)
	}
}

// =============================================================================
// Mandatory case 4: randomizeStart uniform-ish distribution over 1000 events.
// =============================================================================

func TestScheduler_RandomizeStartDistribution(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	const n = 1000
	const rs int32 = 30

	events := make([]sep2.DERControl, n)
	start := fixedNow.Add(3 * time.Minute)
	for i := range events {
		// Unique mRIDs : duplicates would replace each other (test #11
		// covers that path). Use a deterministic format that won't collide.
		mrid := mridFromIndex(i)
		events[i] = makeControl(mrid, 3*time.Minute, 60, ptrInt32(rs), nil)
	}
	s.OnEventsAdded(events)

	if got := s.Len(); got != n {
		t.Fatalf("queue Len = %d, want %d", got, n)
	}

	// Bucket each FireAt into 1-second bins [0, 30].
	const buckets = 31 // inclusive upper bound : rng.Int64N(rs+1) produces [0, rs] inclusive.
	counts := make([]int, buckets)
	snap := inverter.QueueSnapshotForTesting(s)
	for _, ev := range snap {
		offsetSec := int(ev.FireAt.Sub(start).Round(time.Second) / time.Second)
		if offsetSec < 0 || offsetSec > int(rs) {
			t.Fatalf("FireAt out of window: offset=%ds, want [0, %d]", offsetSec, rs)
		}
		counts[offsetSec]++
	}
	// Sanity: every bucket non-empty. With 1000 draws over 31 buckets,
	// expected per-bucket count is ~32; a uniform distribution should
	// hit every bucket comfortably. A skewed RNG would leave gaps.
	for i, c := range counts {
		if c == 0 {
			t.Errorf("bucket %d (offset=%ds) is empty : RNG distribution may be skewed", i, i)
		}
	}
}

// mridFromIndex returns a stable mRID for the i-th synthetic event.
// Padded to 4 digits so lexical and numeric orderings agree.
func mridFromIndex(i int) string {
	// Avoid fmt.Sprintf to keep the helper allocation-free in benchmarks.
	const digits = "0123456789"
	out := []byte("ev-0000")
	for k := 6; k >= 3; k-- {
		out[k] = digits[i%10]
		i /= 10
	}
	return string(out)
}

// =============================================================================
// Mandatory case 5: randomizeDuration symmetric window under seeded RNG.
// =============================================================================

func TestScheduler_RandomizeDuration(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 3*time.Minute, 120, nil, ptrInt32(20)),
	})

	fireAt := inverter.EventFireAtForTesting(s)
	expireAt := inverter.EventExpireAtForTesting(s)
	got := expireAt.Sub(fireAt)
	// Symmetric window [duration-|rd|/2, duration+|rd|/2] = [110s, 130s].
	min := 110 * time.Second
	max := 130 * time.Second
	if got < min || got > max {
		t.Errorf("duration = %s, want in [%s, %s]", got, min, max)
	}

	// Determinism check: replay with the same seed.
	s2 := inverter.NewSchedulerForTesting(fixedNow)
	s2.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 3*time.Minute, 120, nil, ptrInt32(20)),
	})
	want := inverter.EventExpireAtForTesting(s2).Sub(inverter.EventFireAtForTesting(s2))
	if got != want {
		t.Errorf("replayed duration = %s, want %s (RNG non-deterministic?)", got, want)
	}
}

// =============================================================================
// Mandatory case 6: cancellation removes from queue; unknown mRID is a no-op.
// =============================================================================

func TestScheduler_CancellationRemovesFromQueue(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 1*time.Minute, 60, nil, nil),
		makeControl("B", 2*time.Minute, 60, nil, nil),
	})
	if got := s.Len(); got != 2 {
		t.Fatalf("queue Len pre-cancel = %d, want 2", got)
	}

	s.OnEventsCancelled([]string{"A"})
	if got := s.Len(); got != 1 {
		t.Errorf("queue Len after cancelling A = %d, want 1", got)
	}
	if got := inverter.EventMRIDForTesting(s); got != "B" {
		t.Errorf("head mRID = %q, want %q (A should be gone)", got, "B")
	}

	// Unknown mRID is a no-op.
	s.OnEventsCancelled([]string{"never-existed"})
	if got := s.Len(); got != 1 {
		t.Errorf("queue Len after cancelling unknown mRID = %d, want 1 (no-op)", got)
	}

	// Cancelling everything leaves an empty queue.
	s.OnEventsCancelled([]string{"B"})
	if got := s.Len(); got != 0 {
		t.Errorf("queue Len after cancelling B = %d, want 0", got)
	}

	// Empty mRID list is also a no-op (early return).
	s.OnEventsCancelled(nil)
	if got := s.Len(); got != 0 {
		t.Errorf("queue Len after cancelling nil = %d, want 0", got)
	}
}

// =============================================================================
// Mandatory case 7: past-due event.
// =============================================================================

func TestScheduler_PastDueEvent(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	// start = now - 1min, duration = 2min : server-side already running.
	pastStart := fixedNow.Add(-1 * time.Minute)
	s.OnEventsAdded([]sep2.DERControl{
		makeControl("past", -1*time.Minute, 120, nil, nil),
	})
	if got := inverter.EventFireAtForTesting(s); !got.Equal(pastStart) {
		t.Errorf("FireAt = %s, want %s (past)", got, pastStart)
	}

	popped := inverter.PopExpiredMRIDsForTesting(s, fixedNow)
	if len(popped) != 1 || popped[0] != "past" {
		t.Errorf("PopExpired(now) = %v, want [past]", popped)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("queue Len after pop = %d, want 0", got)
	}
}

// =============================================================================
// Mandatory case 8: empty queue.
// =============================================================================

func TestScheduler_EmptyQueue(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	if got, ok := s.Next(); ok {
		t.Errorf("Next on empty queue returned ok=true (event=%+v)", got)
	}
	if popped := s.PopExpired(fixedNow); popped != nil {
		t.Errorf("PopExpired on empty queue = %v, want nil", popped)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("Len on empty queue = %d, want 0", got)
	}

	// OnEventsAdded with nil/empty slice is a no-op.
	s.OnEventsAdded(nil)
	s.OnEventsAdded([]sep2.DERControl{})
	if got := s.Len(); got != 0 {
		t.Errorf("Len after no-op adds = %d, want 0", got)
	}
}

// =============================================================================
// Mandatory case 9: nowFunc injection : expiry uses the injected clock.
// =============================================================================

func TestScheduler_NowFuncInjection(t *testing.T) {
	t.Parallel()

	// virtualNow is mutable so the test advances the clock without sleeping.
	virtualNow := fixedNow
	s := inverter.NewSchedulerWithClockForTesting(func() time.Time { return virtualNow })

	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 30*time.Second, 60, nil, nil),
	})

	// At t=fixedNow, the event has not fired.
	if popped := s.PopExpired(s.Now()); len(popped) != 0 {
		t.Errorf("PopExpired at virtualNow=fixedNow returned %v, want empty", popped)
	}
	if got := s.Now(); !got.Equal(fixedNow) {
		t.Errorf("scheduler Now() = %s, want %s", got, fixedNow)
	}

	// Advance virtualNow past the event start.
	virtualNow = fixedNow.Add(31 * time.Second)
	if popped := inverter.PopExpiredMRIDsForTesting(s, s.Now()); len(popped) != 1 || popped[0] != "A" {
		t.Errorf("PopExpired at virtualNow=+31s = %v, want [A]", popped)
	}

	// Importantly, the scheduler MUST NOT consult real time.Now() : if it
	// did, the event at fixedNow+30s would already have fired by wall-clock
	// (since fixedNow is in the past). The previous assertion already proves
	// this: PopExpired at virtualNow=fixedNow returned empty, even though
	// real wall-clock is well past fixedNow.
}

// =============================================================================
// Supporting: nil Interval is skipped.
// =============================================================================

func TestScheduler_NilIntervalSkipped(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	bad := sep2.DERControl{}
	bad.MRID = "no-interval"
	// Interval intentionally nil.

	good := makeControl("good", 1*time.Minute, 60, nil, nil)

	s.OnEventsAdded([]sep2.DERControl{bad, good})
	if got := s.Len(); got != 1 {
		t.Errorf("queue Len = %d, want 1 (nil-Interval event must be skipped)", got)
	}
	if got := inverter.EventMRIDForTesting(s); got != "good" {
		t.Errorf("head mRID = %q, want %q", got, "good")
	}
}

// =============================================================================
// Supporting: re-adding the same mRID replaces (not duplicates) the entry.
// =============================================================================

func TestScheduler_ReAddReplacesByMRID(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 1*time.Minute, 60, nil, nil),
	})
	// Same mRID, server adjusted start to +5min.
	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 5*time.Minute, 60, nil, nil),
	})

	if got := s.Len(); got != 1 {
		t.Errorf("queue Len = %d, want 1 (re-add should replace)", got)
	}
	want := fixedNow.Add(5 * time.Minute)
	if got := inverter.EventFireAtForTesting(s); !got.Equal(want) {
		t.Errorf("FireAt = %s, want %s (re-added value)", got, want)
	}
}

// =============================================================================
// Supporting: OnEventsUpdated is a documented no-op.
// =============================================================================

func TestScheduler_UpdateIsNoOp(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 1*time.Minute, 60, nil, nil),
	})
	original := inverter.EventFireAtForTesting(s)

	// OnEventsUpdated with the same mRID but a different start MUST NOT
	// re-queue (out of scope here : the state machine owns that decision).
	s.OnEventsUpdated([]sep2.DERControl{
		makeControl("A", 5*time.Minute, 60, nil, nil),
	})
	if got := s.Len(); got != 1 {
		t.Errorf("queue Len after OnEventsUpdated = %d, want 1", got)
	}
	if got := inverter.EventFireAtForTesting(s); !got.Equal(original) {
		t.Errorf("FireAt after OnEventsUpdated = %s, want %s (unchanged)", got, original)
	}

	// Empty slice is also a no-op.
	s.OnEventsUpdated(nil)
	s.OnEventsUpdated([]sep2.DERControl{})
	if got := s.Len(); got != 1 {
		t.Errorf("queue Len after no-op updates = %d, want 1", got)
	}
}

// =============================================================================
// Supporting: NewScheduler panics on nil arguments.
// =============================================================================

func TestScheduler_NewSchedulerPanicsOnNil(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		nowFunc func() time.Time
		rng     bool // true = supply seeded; false = nil
	}{
		{"nil nowFunc", nil, true},
		{"nil rng", func() time.Time { return fixedNow }, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewScheduler(%s) did not panic", tc.name)
				}
			}()
			var rng = inverter.SeededRandForTesting(1, 2)
			if !tc.rng {
				rng = nil
			}
			_ = inverter.NewScheduler(tc.nowFunc, rng)
		})
	}
}

// =============================================================================
// Supporting: randomizeStart negative window jitters backward.
// =============================================================================

func TestScheduler_RandomizeStartNegativeWindow(t *testing.T) {
	t.Parallel()
	s := inverter.NewSchedulerForTesting(fixedNow)

	// randomizeStart = -10s : window [start-10s, start].
	start := fixedNow.Add(3 * time.Minute)
	s.OnEventsAdded([]sep2.DERControl{
		makeControl("A", 3*time.Minute, 60, ptrInt32(-10), nil),
	})

	got := inverter.EventFireAtForTesting(s)
	min := start.Add(-10 * time.Second)
	if got.Before(min) || got.After(start) {
		t.Errorf("FireAt = %s, want in [%s, %s] (negative window)", got, min, start)
	}
}

// =============================================================================
// Supporting: randomizeDuration negative value is treated by absolute value.
// =============================================================================

func TestScheduler_RandomizeDurationNegativeIsAbs(t *testing.T) {
	t.Parallel()

	// rd=-20 must produce the same window shape as rd=+20: [110s, 130s].
	// We assert across enough seeds that we cover the spread, not equality
	// of any single draw (the random offset differs by RNG state).
	for seed := uint64(1); seed <= 20; seed++ {
		s := inverter.NewScheduler(
			func() time.Time { return fixedNow },
			inverter.SeededRandForTesting(seed, seed+1),
		)
		s.OnEventsAdded([]sep2.DERControl{
			makeControl("A", 3*time.Minute, 120, nil, ptrInt32(-20)),
		})
		fireAt := inverter.EventFireAtForTesting(s)
		expireAt := inverter.EventExpireAtForTesting(s)
		got := expireAt.Sub(fireAt)
		min := 110 * time.Second
		max := 130 * time.Second
		if got < min || got > max {
			t.Errorf("seed %d: duration = %s, want in [%s, %s] (|rd|-treatment for negative rd)",
				seed, got, min, max)
		}
	}
}

// =============================================================================
// Supporting: randomizeDuration clamps non-negative when |rd|/2 > duration.
// =============================================================================

func TestScheduler_RandomizeDurationClampsToZero(t *testing.T) {
	t.Parallel()

	// Pathological: duration=2s, randomizeDuration=100s : symmetric window
	// would be [-48s, +52s] which crosses zero. Run many seeds to ensure
	// at least one ExpireAt-FireAt would be negative without the clamp.
	// We can't easily force a specific seed to produce a negative; instead,
	// assert across N seeds that every result is >= 0.
	const tries = 200
	negativeFound := false
	for seed := uint64(1); seed <= tries; seed++ {
		s := inverter.NewScheduler(
			func() time.Time { return fixedNow },
			inverter.SeededRandForTesting(seed, seed+1),
		)
		s.OnEventsAdded([]sep2.DERControl{
			makeControl("A", 0, 2, nil, ptrInt32(100)),
		})
		fireAt := inverter.EventFireAtForTesting(s)
		expireAt := inverter.EventExpireAtForTesting(s)
		got := expireAt.Sub(fireAt)
		if got < 0 {
			negativeFound = true
			t.Errorf("seed %d: duration = %s, must be clamped to >= 0", seed, got)
		}
	}
	if negativeFound {
		t.Fatal("clamp did not hold under at least one seed")
	}
}
