package inverter

import "time"

// SetPollDurationForTesting swaps the package-level pollDuration mapper so
// tests can drive WaitForAdvertisedLinks at sub-second cadence without
// faking time. The returned func restores the previous mapper.
//
// The mapper is held in an atomic.Pointer so this write is
// race-safe against the concurrent read on WaitForAdvertisedLinks's
// goroutine. Test seams that swap a function value behind a
// goroutine-spawning production loop MUST go through the atomic accessor.
//
// Only available via _test.go suffix; never linked into the production
// binary.
func SetPollDurationForTesting(fn func(pollRateSec uint32) time.Duration) (restore func()) {
	prev := pollDurationPtr.Load()
	next := pollDurationFunc(fn)
	pollDurationPtr.Store(&next)
	return func() { pollDurationPtr.Store(prev) }
}
