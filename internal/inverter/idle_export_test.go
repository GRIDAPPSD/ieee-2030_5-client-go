package inverter

import "time"

// SetPollDurationForTesting swaps the package-level pollDuration mapper so
// tests can drive WaitForAdvertisedLinks at sub-second cadence without
// faking time. The returned func restores the previous mapper.
//
// Only available via _test.go suffix; never linked into the production
// binary. See IEEE-028.
func SetPollDurationForTesting(fn func(pollRateSec uint32) time.Duration) (restore func()) {
	prev := pollDuration
	pollDuration = fn
	return func() { pollDuration = prev }
}
