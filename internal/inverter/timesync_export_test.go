package inverter

import (
	"time"
)

// SetMinTimeSyncPollRateForTesting swaps the package-level
// minTimeSyncPollRate floor so tests can drive RunTimeSync's loop body
// at sub-second cadence without faking time. The returned func restores
// the previous floor.
//
// IEEE-081: the floor is stored as an atomic.Int64 (nanoseconds) so this
// write is race-safe against a concurrent RunTimeSync read in another
// goroutine. Test seams that drive a goroutine-spawning production loop
// MUST go through the atomic accessor : see Pike's IEEE-081 audit.
//
// Only available via _test.go suffix; never linked into the production
// binary. Mirrors SetPollDurationForTesting (IEEE-028). See IEEE-070.
func SetMinTimeSyncPollRateForTesting(d time.Duration) (restore func()) {
	prev := minTimeSyncPollRateNanos.Swap(int64(d))
	return func() { minTimeSyncPollRateNanos.Store(prev) }
}
