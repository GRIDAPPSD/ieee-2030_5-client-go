package inverter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// mRIDType (IEEE 2030.5-2018 sec 10.x object model, "mRIDType object
// (HexBinary128)") is at most 32 hex characters, with an IANA PEN in bits
// 0-31 when one is configured. This client has no PEN setting configured,
// so every mRID it mints is derived from identity and per-call entropy,
// then hashed into 128 bits so the result is always valid hex regardless
// of input length or content.
//
// 0xFFFFFFFF...FFFF (32 hex Fs) is reserved for an in-progress accumulator
// record; deriveMRID never returns it (see avoidReservedAllF).
func deriveMRID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	b := avoidReservedAllF(sum[:16])
	return strings.ToUpper(hex.EncodeToString(b))
}

// avoidReservedAllF flips the low bit of the last byte when b is all
// 0xFF, since 0xFFFFFFFFFFFFFFFFFFFFFFFF[PEN] is reserved (mRIDType) for
// an in-progress accumulator record. b must be 16 bytes (HexBinary128).
// Returns b unchanged when it is not all 0xFF.
func avoidReservedAllF(b []byte) []byte {
	for _, v := range b {
		if v != 0xFF {
			return b
		}
	}
	out := make([]byte, len(b))
	copy(out, b)
	out[len(out)-1] ^= 0x01
	return out
}

// MirrorUsagePointMRID derives a MirrorUsagePoint mRID for the device
// named by deviceLFDI. A mirror is created once per device, so deviceLFDI
// alone gives per-device uniqueness; the "mup" tag domain-separates it
// from reading mRIDs (readingMRID) so the two families never collide.
func MirrorUsagePointMRID(deviceLFDI string) string {
	return deriveMRID("mup", deviceLFDI)
}

// NewFlowReservationRequestMRID mints the mRID of one FlowReservationRequest
// from the aggregator's LFDI, the creation instant and 16 random bytes, so
// two requests made in the same second or by one LFDI never share an mRID:
// the server keys its answer on it and a repeat would be read as the same
// request. It fails if the system's random source does.
func NewFlowReservationRequestMRID(selfLFDI string, now time.Time) (string, error) {
	entropy := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		return "", fmt.Errorf("mint FlowReservationRequest mRID: %w", err)
	}
	return deriveMRID("frq", selfLFDI, now.UTC().Format(time.RFC3339Nano), hex.EncodeToString(entropy)), nil
}
