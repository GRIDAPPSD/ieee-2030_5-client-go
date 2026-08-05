package inverter

import "time"

// SetDERControlPollDurationForTesting swaps the package-level
// derControlPollDuration mapper so tests can drive PollDERControlList at
// sub-second cadence without faking time. Returns a restore func that puts
// the production mapper back.
//
// The mapper is held in an atomic.Pointer so this write is
// race-safe against the concurrent read on PollDERControlList's
// goroutine. Test seams that swap a function value behind a
// goroutine-spawning production loop MUST go through the atomic accessor.
//
// Only linked into the _test binary (file ends in _test.go). Mirrors
// SetPollDurationForTesting.
func SetDERControlPollDurationForTesting(fn func(pollRateSec uint32) time.Duration) (restore func()) {
	prev := derControlPollDurationPtr.Load()
	next := pollDurationFunc(fn)
	derControlPollDurationPtr.Store(&next)
	return func() { derControlPollDurationPtr.Store(prev) }
}
