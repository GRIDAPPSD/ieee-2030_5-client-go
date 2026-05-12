package inverter

import "time"

// SetMinTimeSyncPollRateForTesting swaps the package-level
// minTimeSyncPollRate floor so tests can drive RunTimeSync's loop body
// at sub-second cadence without faking time. The returned func restores
// the previous floor.
//
// Only available via _test.go suffix; never linked into the production
// binary. Mirrors SetPollDurationForTesting (IEEE-028). See IEEE-070.
func SetMinTimeSyncPollRateForTesting(d time.Duration) (restore func()) {
	prev := minTimeSyncPollRate
	minTimeSyncPollRate = d
	return func() { minTimeSyncPollRate = prev }
}
