package inverter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// DERControl polling (Phase 5 entry) ===============================
//
// CSIP V1.2 CORE-012 procedure step 6 requires the DER Client to periodically
// poll its associated DERControl resource and observe Status changes. This
// file ships the polling SKELETON only:
//
//   - PollDERControlList runs as a foreground call wrapped by `go` at the
//     caller. It ticks at the advertised pollRate, GETs the DERControlList
//     at the supplied href via the existing SEP2Client.GetDERControlList
//     (paged with ?l=255 already), and refreshes the supplied cache.
//   - DERControlCache stores the latest snapshot keyed by mRID and exposes a
//     pure Diff method so later Phase 5 consumers (the scheduler,
//     the state machine) can react to added / updated / cancelled
//     events without touching the cache internals.
//
// Out of scope for this file:
// - Event scheduling and randomization.
// - DEFAULT/EVENT_RECEIVED/EVENT_STARTED state machine.
// - Replacing ApplyControls(nil,...) at main.go:667.
// - DERCurve retrieval.
//   - Response POSTs on status transitions (Phase 6).

// defaultDERControlPollDuration maps an IEEE 2030.5 pollRate (seconds,
// uint32) to a time.Duration with the project's standard floor (60s) and
// default-on-zero (30min) policy. Mirrors pinPollInterval in
// cmd/inverterclient/main.go but kept package-local in internal/inverter
// so the polling loop can be unit-tested without crossing the cmd
// boundary.
func defaultDERControlPollDuration(pollRateSec uint32) time.Duration {
	d := time.Duration(pollRateSec) * time.Second
	if d <= 0 {
		d = 30 * time.Minute
	}
	if d < 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// derControlPollDurationPtr holds the current mapper. Stored in an
// atomic.Pointer so tests can swap in a tight cadence
// (SetDERControlPollDurationForTesting) while
// (*SEP2Client).PollDERControlList reads on its own goroutine : no race.
// Mirrors the pollDuration testability seam pattern used elsewhere in
// this package.
var derControlPollDurationPtr atomic.Pointer[pollDurationFunc]

func init() {
	fn := pollDurationFunc(defaultDERControlPollDuration)
	derControlPollDurationPtr.Store(&fn)
}

// derControlPollDuration returns the wait duration for a DERControlList
// pollRate (seconds). Race-safe against test seam swaps.
func derControlPollDuration(pollRateSec uint32) time.Duration {
	return (*derControlPollDurationPtr.Load())(pollRateSec)
}

// DERControlCache holds the most recent DERControlList snapshot keyed by
// mRID. The zero value is NOT ready for use : callers must construct via
// NewDERControlCache so the underlying map is non-nil.
//
// Concurrency: an RWMutex guards the map. The scheduler reads frequently
// (via Snapshot) and writes only on each pollRate tick, so RW is the right
// shape from day one. The mutex is held only briefly: never across an HTTP
// call, only while swapping or copying map entries.
type DERControlCache struct {
	mu       sync.RWMutex
	controls map[string]sep2.DERControl
	rejected []sep2.RejectedDERControl
}

// NewDERControlCache constructs an empty cache ready for concurrent use.
func NewDERControlCache() *DERControlCache {
	return &DERControlCache{controls: make(map[string]sep2.DERControl)}
}

// Snapshot returns an independent copy of the cache keyed by mRID. Mutating
// the returned map does not affect the cache. Each value is a Copy() of the
// stored DERControl so the caller cannot mutate pointer fields either.
func (c *DERControlCache) Snapshot() map[string]sep2.DERControl {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]sep2.DERControl, len(c.controls))
	for k, v := range c.controls {
		out[k] = v.Copy()
	}
	return out
}

// Rejected returns a copy of the latest list's items whose DERControlBase
// failed to decode. A slice, not a map: the mRID is optional on a rejected
// item, so two items can share the empty key.
func (c *DERControlCache) Rejected() []sep2.RejectedDERControl {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return copyRejected(c.rejected)
}

// copyRejected deep-copies each item's event fields through DERControl.Copy,
// the one owner of those pointer copies.
func copyRejected(items []sep2.RejectedDERControl) []sep2.RejectedDERControl {
	if len(items) == 0 {
		return nil
	}
	out := make([]sep2.RejectedDERControl, len(items))
	for i, r := range items {
		out[i] = sep2.RejectedDERControl{
			RandomizableEvent: sep2.DERControl{RandomizableEvent: r.RandomizableEvent}.Copy().RandomizableEvent,
			Err:               r.Err,
		}
	}
	return out
}

// Len returns the number of entries currently cached. Exposed for tests and
// operator logging; the canonical inspection path is Snapshot.
func (c *DERControlCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.controls)
}

// acceptedControls is the list's controls less any whose mRID is also
// rejected: the rejection wins, so a control sharing a rejected mRID is
// never scheduled.
func acceptedControls(list sep2.DERControlListResult) []sep2.DERControl {
	if len(list.Rejected) == 0 {
		return list.DERControl
	}
	rejected := make(map[string]bool, len(list.Rejected))
	for _, r := range list.Rejected {
		if r.MRID != "" {
			rejected[r.MRID] = true
		}
	}
	out := make([]sep2.DERControl, 0, len(list.DERControl))
	for _, c := range list.DERControl {
		if !rejected[c.MRID] {
			out = append(out, c)
		}
	}
	return out
}

// Diff compares `next` (a freshly polled DERControlList contents) against the
// current cache state and returns the changes a consumer (the scheduler
// / state machine) needs to act on:
//
//   - added:     mRIDs present in `next` but not in the cache.
//   - updated:   mRIDs present in both, where the cached EventStatus.CurrentStatus
//     differs from the next EventStatus.CurrentStatus and the next is
//     NOT EventStatusCancelled (cancelled wins its own bucket).
//   - cancelled: mRIDs whose `next` EventStatus.CurrentStatus == EventStatusCancelled
//     (== 2) regardless of cached state : newly seen cancellations
//     surface here too, not in added.
//
// Diff is a pure read against the current snapshot : it does NOT mutate the
// cache. Refresh applies a new state. The order matters: callers Diff first
// to learn the deltas, then Refresh to commit the new state.
//
// Slices are returned as fresh copies; the caller may sort or filter them
// freely. Entries returned in `next`-order (range-iteration order) so callers
// that care about determinism should sort by mRID downstream.
func (c *DERControlCache) Diff(next sep2.DERControlListResult) (added, updated, cancelled []sep2.DERControl) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, nc := range acceptedControls(next) {
		if nc.MRID == "" {
			// Spec note: DERControl mRID is "shall be present" per IEEE 2030.5
			// §10.7. A blank one is server-side garbage; skip it rather than
			// poisoning the cache with empty-string-keyed entries.
			continue
		}
		nextStatus := derControlCurrentStatus(nc)
		if nextStatus == sep2.EventStatusCancelled {
			cancelled = append(cancelled, nc.Copy())
			continue
		}
		prev, exists := c.controls[nc.MRID]
		if !exists {
			added = append(added, nc.Copy())
			continue
		}
		if derControlCurrentStatus(prev) != nextStatus {
			updated = append(updated, nc.Copy())
		}
	}
	return added, updated, cancelled
}

// Refresh replaces the cache contents, controls and rejected items, with the
// supplied list. Like Diff it skips controls with empty mRID. Holds the write
// lock only across the swap; never across I/O. Returns the count of controls
// committed for operator logging.
func (c *DERControlCache) Refresh(next sep2.DERControlListResult) int {
	controls := acceptedControls(next)
	fresh := make(map[string]sep2.DERControl, len(controls))
	for _, nc := range controls {
		if nc.MRID == "" {
			continue
		}
		fresh[nc.MRID] = nc.Copy()
	}
	rejected := copyRejected(next.Rejected)
	c.mu.Lock()
	c.controls = fresh
	c.rejected = rejected
	c.mu.Unlock()
	return len(fresh)
}

// Update diffs next against the cache, commits it with Refresh, and logs the
// change counts prefixed by source when anything changed.
func (c *DERControlCache) Update(next sep2.DERControlListResult, source string) {
	added, updated, cancelled := c.Diff(next)
	total := c.Refresh(next)
	if len(added)+len(updated)+len(cancelled) > 0 {
		log.Printf("%s: %d total (+%d new, ~%d updated, !%d cancelled)",
			source, total, len(added), len(updated), len(cancelled))
	}
}

// derControlCurrentStatus returns the EventStatus.CurrentStatus for an event,
// or EventStatusScheduled (0) when EventStatus is absent. Server omits-equals-
// scheduled is the IEEE 2030.5 §10.1.3 default semantic for unset status.
func derControlCurrentStatus(c sep2.DERControl) uint8 {
	if c.EventStatus == nil {
		return sep2.EventStatusScheduled
	}
	return c.EventStatus.CurrentStatus
}

// PollDERControlList runs the periodic DERControlList polling loop. It does
// NOT spawn its own goroutine : the caller invokes it as
//
//	go client.PollDERControlList(ctx, href, dcap.PollRate, cache)
//
// matching the precedent set by RunTimeSync.
//
// On each tick the loop GETs the list (reusing GetDERControlList's paging),
// calls cache.Refresh, and logs the delta counts derived from cache.Diff
// (computed before Refresh so the diff reflects new vs prior state). The
// diff is logged at INFO; downstream consumers (the scheduler, the state
// machine) consume the diff via their own Diff calls against a shared cache
// snapshot.
//
// pollRate is clamped to a 60s floor and a 30min default-on-zero by
// derControlPollDuration : same convention pinPollInterval applies in
// cmd/inverterclient/main.go.
//
// Error handling:
//   - Empty href is a programmer error and returns immediately with a wrapped
//     error. Callers gate on selectedDERProgram.DERControlListLink != nil
//     before spawning the goroutine.
//   - Transient HTTP / parse failures are logged and the loop continues at
//     the next tick. A flaky server should not silently halt control polling.
//   - ctx-cancel exits the loop cleanly and returns ctx.Err(). The wrapped
//     error chain is errors.Is-compatible with context.Canceled /
//     context.DeadlineExceeded.
func (c *SEP2Client) PollDERControlList(
	ctx context.Context,
	href string,
	pollRate uint32,
	cache *DERControlCache,
) error {
	if href == "" {
		return fmt.Errorf("PollDERControlList: href required")
	}
	if cache == nil {
		return fmt.Errorf("PollDERControlList: cache required")
	}
	interval := derControlPollDuration(pollRate)
	log.Printf("DERControlList poll: starting href=%s interval=%s", href, interval)

	// Initial tick fires immediately so the cache populates without waiting
	// a full interval : the scheduler needs a snapshot at startup.
	if newHref, err := c.pollDERControlListOnce(ctx, href, cache); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.Printf("DERControlList poll: initial fetch failed (continuing): %v", err)
	} else if newHref != "" {
		log.Printf("DERControlList poll: 301 follow : cached href %s → %s", href, newHref)
		href = newHref
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		newHref, err := c.pollDERControlListOnce(ctx, href, cache)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Recoverable : log and try again next tick.
			log.Printf("DERControlList poll: fetch failed (continuing): %v", err)
			continue
		}
		if newHref != "" {
			log.Printf("DERControlList poll: 301 follow : cached href %s → %s", href, newHref)
			href = newHref
		}
	}
}

// pollDERControlListOnce performs a single GET + cache refresh. Extracted so
// the loop body stays small and tests can drive the I/O path directly. The
// mutex is held only inside Diff/Refresh, never across the GET.
//
// When the GET follows a 301, the new href (with paging query
// stripped) is returned so the calling loop can update its local cached
// href and stop paying a redirect on every subsequent tick. Empty newHref
// means no follow happened.
func (c *SEP2Client) pollDERControlListOnce(
	ctx context.Context,
	href string,
	cache *DERControlCache,
) (newHref string, err error) {
	list, newHref, err := c.GetDERControlList(ctx, href)
	if err != nil {
		return "", fmt.Errorf("poll DERControlList %s: %w", href, err)
	}
	cache.Update(list, "DERControlList poll")
	return newHref, nil
}
