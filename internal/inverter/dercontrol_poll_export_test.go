package inverter

import "time"

// SetDERControlPollDurationForTesting swaps the package-level
// derControlPollDuration mapper so tests can drive PollDERControlList at
// sub-second cadence without faking time. Returns a restore func that puts
// the production mapper back.
//
// Only linked into the _test binary (file ends in _test.go). Mirrors
// SetPollDurationForTesting (IEEE-028).
func SetDERControlPollDurationForTesting(fn func(pollRateSec uint32) time.Duration) (restore func()) {
	prev := derControlPollDuration
	derControlPollDuration = fn
	return func() { derControlPollDuration = prev }
}
