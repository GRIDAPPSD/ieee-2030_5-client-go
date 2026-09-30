package inverter

import "time"

// ReadingMRIDForTesting exposes readingMRID to the external inverter_test
// package, so the per-device MRID format can be tested directly against a
// fixed time without depending on wall-clock timing.
//
// Only available via _test.go suffix; never linked into the production
// binary.
func ReadingMRIDForTesting(deviceLFDI string, at time.Time) string {
	return readingMRID(deviceLFDI, at)
}
