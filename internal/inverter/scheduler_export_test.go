package inverter

import (
	"math/rand/v2"
	"time"
)

// Test-binary-only constructor and accessors for the scheduler. Linked into
// the _test binary only (file ends in _test.go). Pattern mirrors
// dercontrol_poll_export_test.go and poll_export_test.go.
//
// The production NewScheduler signature accepts a *rand.Rand directly, so
// no separate "production" vs "test" seam is strictly required to drive
// determinism. These helpers exist to:
//
//  1. Construct a seeded *rand.Rand with a documented seed so tests do not
//     repeat the rand.NewPCG boilerplate.
//  2. Inspect the queue's internal state (FireAt / ExpireAt / MRID) without
//     exporting scheduledEvent on the production API surface.

// NewSchedulerForTesting builds a Scheduler with a fixed-time nowFunc and a
// deterministically-seeded *rand.Rand. The seed pair (1, 2) is documented
// in tests as the canonical seed; tests that need a different seed can call
// SeededRandForTesting directly.
func NewSchedulerForTesting(now time.Time) *Scheduler {
	return NewScheduler(
		func() time.Time { return now },
		SeededRandForTesting(1, 2),
	)
}

// NewSchedulerWithClockForTesting builds a Scheduler with a caller-supplied
// nowFunc and the canonical seed. Used by tests that need to advance the
// clock between operations.
func NewSchedulerWithClockForTesting(nowFunc func() time.Time) *Scheduler {
	return NewScheduler(nowFunc, SeededRandForTesting(1, 2))
}

// SeededRandForTesting returns a *rand.Rand seeded with the given pair.
// Exposed so tests assert deterministic output of applyRandomizeStart /
// applyRandomizeDuration by replaying the same seed pair.
func SeededRandForTesting(seed1, seed2 uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed1, seed2))
}

// QueueSnapshotForTesting returns a copy of the scheduler's queue. The
// returned slice is independent : mutating it does not affect the
// scheduler. Each scheduledEvent's Source DERControl is the same value the
// scheduler stored (already a Copy() at OnEventsAdded time).
func QueueSnapshotForTesting(s *Scheduler) []scheduledEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]scheduledEvent, len(s.queue))
	copy(out, s.queue)
	return out
}

// EventFireAtForTesting returns the FireAt for the queue head, or the zero
// time when the queue is empty. Convenience for table-driven assertions
// that don't care about the full scheduledEvent payload.
func EventFireAtForTesting(s *Scheduler) time.Time {
	ev, ok := s.Next()
	if !ok {
		return time.Time{}
	}
	return ev.FireAt
}

// EventExpireAtForTesting returns the ExpireAt for the queue head, or the
// zero time when the queue is empty.
func EventExpireAtForTesting(s *Scheduler) time.Time {
	ev, ok := s.Next()
	if !ok {
		return time.Time{}
	}
	return ev.ExpireAt
}

// EventMRIDForTesting returns the head event's mRID, or "" when empty.
func EventMRIDForTesting(s *Scheduler) string {
	ev, ok := s.Next()
	if !ok {
		return ""
	}
	return ev.MRID
}

// QueueMRIDsForTesting returns every queued event's mRID in queue order
// (sorted by FireAt ascending). Convenience for the sort-order test case.
func QueueMRIDsForTesting(s *Scheduler) []string {
	snap := QueueSnapshotForTesting(s)
	out := make([]string, len(snap))
	for i, ev := range snap {
		out[i] = ev.MRID
	}
	return out
}

// PopExpiredMRIDsForTesting drains PopExpired(now) into a []string of
// mRIDs in pop order. Convenience for past-due / empty-queue tests that
// only care about which events came out.
func PopExpiredMRIDsForTesting(s *Scheduler, now time.Time) []string {
	expired := s.PopExpired(now)
	out := make([]string, len(expired))
	for i, ev := range expired {
		out[i] = ev.MRID
	}
	return out
}
