package inverter

import "time"

// ReadingMRIDForTesting exposes readingMRID to the external inverter_test
// package, so the per-device, per-reading mRID format can be tested
// directly against a fixed time and sequence without depending on
// wall-clock timing.
//
// Only available via _test.go suffix; never linked into the production
// binary.
func ReadingMRIDForTesting(deviceLFDI string, at time.Time, seq uint64) string {
	return readingMRID(deviceLFDI, at, seq)
}
