package inverter

import (
	"log"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// DERControl event scheduler (Phase 5, GRIDAPPSD/ieee-2030_5-server-go#88) ===============
//
// The polling cache (DERControlCache.Snapshot / Diff) produces deltas; this
// file consumes them to build a randomization-aware event scheduler:
//
//   - For each polled DERControl, parse Interval.Start + Interval.Duration,
//     apply RandomizeStart and RandomizeDuration per IEEE 2030.5 §10.1.4,
//     queue a scheduledEvent keyed by mRID, sorted ascending by FireAt.
//   - Expose Next() (peek) and PopExpired(now) so the state machine
//     can drive EVENT_RECEIVED -> EVENT_STARTED transitions without coupling
//     to the queue internals.
//   - All scheduling math uses an injected nowFunc : production passes
//     client.Now (the server-offset wall clock); tests inject
//     a fixed-time clock. The scheduler NEVER reads real time.Now() inline.
//
// Out of scope for this scheduler:
//   - Firing events / mutating inverter mode/output : the state machine owns
//     that; this scheduler just produces the queue.
//   - Replacing ApplyControls(nil, ...) at cmd/inverterclient/main.go.
//   - DERCurve retrieval.
//   - Response Function Set / replyTo POSTs : Phase 6.
//
// IEEE 2030.5 §10.1.4 randomization interpretation (Phase 5 doc exit criterion 3):
//
//   - randomizeStart jitters the start time within [start, start+|rs|].
//     Phase 5 doc states: "start time is jittered uniformly within
//     [start, start+randomizeStart]". Negative values use the symmetric
//     backward window [start-|rs|, start].
//   - randomizeDuration is symmetric about the nominal duration:
//     [duration-|rd|/2, duration+|rd|/2]. Negative randomizeDuration is
//     treated by absolute value (the window is still symmetric).
//   - Both windows clamp so ExpireAt >= FireAt : pathological inputs
//     cannot produce a negative-duration scheduled event.

// scheduledEvent is the unit the scheduler queues. Source carries the full
// DERControl so the state machine can inspect EventStatus, primacy,
// and the DERControlBase fields when it actually applies the event. The
// scheduler itself never reads Source's body : it only routes by mRID and
// fire window.
//
// Exported so the state machine can construct test fixtures without going
// through the package boundary.
type scheduledEvent struct {
	MRID     string
	FireAt   time.Time
	ExpireAt time.Time
	Source   sep2.DERControl
}

// Scheduler holds the sorted in-memory queue of pending DERControl events.
// The zero value is NOT ready for use : callers MUST construct via
// NewScheduler so nowFunc and rng are non-nil.
//
// Concurrency: a single sync.Mutex guards the queue. The expected workload
// is a single DER with a handful of pending events per polling tick; the
// extra read parallelism an RWMutex would buy is not worth its complexity
// here. The mutex is held only across in-memory queue mutation; never
// across I/O. Pike rule 3 (no goroutine leaks) is not relevant : Scheduler
// owns no goroutine; the state machine will own its own loop.
type Scheduler struct {
	mu      sync.Mutex
	queue   []scheduledEvent
	nowFunc func() time.Time
	rng     *rand.Rand
}

// NewScheduler constructs a Scheduler. nowFunc supplies the wall clock for
// expiry checks: production callers pass (*SEP2Client).Now ;
// tests pass a fixed-time closure for determinism. rng supplies the
// randomization source : production seeds rand.NewPCG from current nanos;
// tests inject a deterministic seed.
//
// Both parameters are required. NewScheduler panics on nil arguments :
// a nil nowFunc or rng is a programmer error caught at startup, not a
// runtime failure mode.
func NewScheduler(nowFunc func() time.Time, rng *rand.Rand) *Scheduler {
	if nowFunc == nil {
		panic("inverter.NewScheduler: nowFunc required")
	}
	if rng == nil {
		panic("inverter.NewScheduler: rng required")
	}
	return &Scheduler{
		nowFunc: nowFunc,
		rng:     rng,
	}
}

// OnEventsAdded inserts each event into the queue, sorted by FireAt
// ascending. Caller obtains `events` from DERControlCache.Diff's `added`
// bucket; the state machine routes updated/cancelled to the matching method.
//
// Events with a nil Interval are skipped : IEEE 2030.5 §10.7 requires
// DERControl to carry an interval, but server-side garbage shouldn't crash
// the scheduler. Skip count is logged.
//
// Events whose Interval.Start is 0 (Unix epoch) are still queued. Per the
// spec a Start of 0 is unusual but not invalid; the state machine detects
// "fire time has already passed by hours" and treats it as past-due during
// the state-machine transition. The scheduler stays pure.
//
// Repeated calls with the same mRID are tolerated: the older entry is
// removed before the new one is inserted. This is the path that handles
// "server adjusted Interval.Start mid-poll" without the state machine
// having to route through OnEventsCancelled first.
func (s *Scheduler) OnEventsAdded(events []sep2.DERControl) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range events {
		if ev.Interval == nil {
			log.Printf("scheduler: skipping event mRID=%q with nil Interval", ev.MRID)
			continue
		}
		fireAt, expireAt := s.computeWindow(ev)
		// Drop any existing entry for this mRID : replace-not-duplicate.
		s.removeByMRIDLocked(ev.MRID)
		s.queue = append(s.queue, scheduledEvent{
			MRID:     ev.MRID,
			FireAt:   fireAt,
			ExpireAt: expireAt,
			Source:   ev.Copy(),
		})
	}
	sort.Slice(s.queue, func(i, j int) bool {
		return s.queue[i].FireAt.Before(s.queue[j].FireAt)
	})
}

// OnEventsCancelled removes entries whose mRID appears in `mRIDs`. Unknown
// mRIDs are tolerated and logged at info : server may cancel an event the
// client never saw (poll missed it), which is a benign race, not an error.
func (s *Scheduler) OnEventsCancelled(mRIDs []string) {
	if len(mRIDs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range mRIDs {
		if !s.removeByMRIDLocked(m) {
			log.Printf("scheduler: cancel for unknown mRID=%q (benign : no-op)", m)
		}
	}
}

// OnEventsUpdated is an intentional no-op. EventStatus.currentStatus
// transitions need the state-machine context to distinguish
// "transitioning to Active mid-window" (no re-queue) from
// "server pushed Interval forward" (re-queue with new fireAt). Implementing
// it here without that context would commit to a semantic the state machine
// may need to undo. Documented intentional no-op, not forgotten.
func (s *Scheduler) OnEventsUpdated(events []sep2.DERControl) {
	if len(events) == 0 {
		return
	}
	log.Printf("scheduler: OnEventsUpdated called with %d events : no-op, handled by the state machine",
		len(events))
}

// Next returns the head of the queue (earliest FireAt) without removing it.
// Returns ok=false when the queue is empty. Used by the state machine to
// compute the next wake-up duration for its loop.
func (s *Scheduler) Next() (scheduledEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return scheduledEvent{}, false
	}
	return s.queue[0], true
}

// PopExpired removes and returns every queued event whose FireAt <= now.
// Returned slice is ordered ascending by FireAt (matches queue order).
// An empty queue returns nil. The state machine calls this each loop
// iteration to drive EVENT_RECEIVED -> EVENT_STARTED.
//
// PopExpired does NOT consult ExpireAt : pop-by-fire-time is the
// state-machine's entry point; expiry handling is the state machine's
// concern after it transitions to EVENT_STARTED.
func (s *Scheduler) PopExpired(now time.Time) []scheduledEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil
	}
	cut := 0
	for cut < len(s.queue) && !s.queue[cut].FireAt.After(now) {
		cut++
	}
	if cut == 0 {
		return nil
	}
	expired := make([]scheduledEvent, cut)
	copy(expired, s.queue[:cut])
	// Compact the queue in-place : preserve ascending sort.
	s.queue = append(s.queue[:0], s.queue[cut:]...)
	return expired
}

// Len returns the current queue size. Operator / test helper; not on the
// hot path. Mirrors DERControlCache.Len.
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// Now returns the scheduler's clock reading via the injected nowFunc.
// Exposed so the state machine (or any caller that already holds a
// *Scheduler) can read the same clock the scheduler uses without
// re-injecting nowFunc.
func (s *Scheduler) Now() time.Time {
	return s.nowFunc()
}

// removeByMRIDLocked removes the first queue entry matching mRID. Returns
// true if a removal happened, false if no match. Caller MUST hold s.mu.
// Order is preserved by slicing : no swap-with-last shortcut, because the
// queue is sorted by FireAt and a swap would break the invariant.
func (s *Scheduler) removeByMRIDLocked(mRID string) bool {
	for i, ev := range s.queue {
		if ev.MRID == mRID {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return true
		}
	}
	return false
}

// computeWindow turns a parsed DERControl into a (FireAt, ExpireAt) pair,
// applying §10.1.4 randomization. Caller MUST hold s.mu (because we touch
// s.rng : math/rand/v2 *rand.Rand is NOT safe for concurrent use without
// external synchronization).
//
// ev.Interval is guaranteed non-nil by OnEventsAdded; passing a nil-Interval
// event here would crash. The check stays at the caller boundary so the
// helper signature is simple.
func (s *Scheduler) computeWindow(ev sep2.DERControl) (fireAt, expireAt time.Time) {
	start := time.Unix(ev.Interval.Start, 0)
	duration := time.Duration(ev.Interval.Duration) * time.Second

	fireAt = applyRandomizeStart(start, ev.RandomizeStart, s.rng)
	jitteredDuration := applyRandomizeDuration(duration, ev.RandomizeDuration, s.rng)
	if jitteredDuration < 0 {
		jitteredDuration = 0
	}
	expireAt = fireAt.Add(jitteredDuration)
	return fireAt, expireAt
}

// applyRandomizeStart returns a uniformly-random time in:
//
//   - [start, start+|rs|]      when randomizeStart > 0 (positive-window)
//   - [start-|rs|, start]      when randomizeStart < 0 (backward-window)
//   - start                    when randomizeStart is nil or zero
//
// Documented in the package header. Positive-window is the Phase 5 doc
// exit-criterion-3 default. We use rng.Int64N for an inclusive upper bound
// in seconds; the per-second granularity matches the spec's int32-seconds
// representation of randomizeStart.
//
// Caller (computeWindow) holds the scheduler mutex; rng calls are safe.
func applyRandomizeStart(start time.Time, randomizeStart *int32, rng *rand.Rand) time.Time {
	if randomizeStart == nil || *randomizeStart == 0 {
		return start
	}
	rs := int64(*randomizeStart)
	if rs > 0 {
		// jitter in [0, rs] seconds, inclusive.
		offset := rng.Int64N(rs + 1)
		return start.Add(time.Duration(offset) * time.Second)
	}
	// negative : backward window [start-|rs|, start].
	abs := -rs
	offset := rng.Int64N(abs + 1)
	return start.Add(time.Duration(-offset) * time.Second)
}

// applyRandomizeDuration returns a uniformly-random duration in:
//
//   - [duration-|rd|/2, duration+|rd|/2]   when randomizeDuration != 0
//   - duration                              when randomizeDuration is nil or zero
//
// The window is centered on `duration` per Phase 5 doc exit criterion 3.
// Negative randomizeDuration is treated by absolute value : the spec allows
// signed values but the window shape is symmetric regardless of sign.
//
// rng.Int64N gives [0, |rd|+1) so we shift by |rd|/2 to center the window.
// The resulting duration may be negative if duration is small and |rd| is
// large; the caller clamps to zero. Documented for state-machine reviewers.
func applyRandomizeDuration(duration time.Duration, randomizeDuration *int32, rng *rand.Rand) time.Duration {
	if randomizeDuration == nil || *randomizeDuration == 0 {
		return duration
	}
	rd := int64(*randomizeDuration)
	if rd < 0 {
		rd = -rd
	}
	// jitter in [0, rd] seconds, then shift left by rd/2 to center.
	jitterSec := rng.Int64N(rd + 1)
	shift := time.Duration(jitterSec-rd/2) * time.Second
	return duration + shift
}
