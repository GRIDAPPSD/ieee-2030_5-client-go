package inverter

// AvoidReservedAllFForTesting exposes avoidReservedAllF to the external
// inverter_test package, so the reserved-value guard can be proven
// directly against a crafted all-0xFF input instead of hoping a SHA-256
// output collides with it.
//
// Only available via _test.go suffix; never linked into the production
// binary.
func AvoidReservedAllFForTesting(b []byte) []byte {
	return avoidReservedAllF(b)
}
