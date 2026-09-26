package main

// Tests: ratingInt16, the nameplate-to-wire-int16 guard used when building
// the DERCapability/DERSettings PUT payloads (Phase 3 DER setup).

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRatingInt16_InRange pins the pass-through cast for every rating this
// client's package-level Rating var and its fields exercise today.
func TestRatingInt16_InRange(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want int16
	}{
		{"zero", 0, 0},
		{"current 10kW nameplate", 10000, 10000},
		{"current RatedVAr", 4400, 4400},
		{"max int16", math.MaxInt16, math.MaxInt16},
		{"min int16", math.MinInt16, math.MinInt16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ratingInt16(tc.in); got != tc.want {
				t.Errorf("ratingInt16(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestRatingInt16_OutOfRangeFatal proves the guard fires rather than
// silently wrapping: a rating past int16 range must exit loud. log.Fatalf
// calls os.Exit, so this runs the guard in a subprocess (the standard Go
// pattern for testing an os.Exit path) rather than taking down the test
// binary itself.
func TestRatingInt16_OutOfRangeFatal(t *testing.T) {
	if os.Getenv("RATING_INT16_HELPER_WANT_FATAL") == "1" {
		ratingInt16(math.MaxInt16 + 1)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRatingInt16_OutOfRangeFatal")
	cmd.Env = append(os.Environ(), "RATING_INT16_HELPER_WANT_FATAL=1")
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("ratingInt16(MaxInt16+1) subprocess: err=%v (want a non-zero-exit *exec.ExitError); output=%s", err, out)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("ratingInt16(MaxInt16+1) subprocess exit code = %d, want 1; output=%s", exitErr.ExitCode(), out)
	}
	const wantSubstr = "exceeds sep2 ActivePower/ReactivePower int16 range"
	if !strings.Contains(string(out), wantSubstr) {
		t.Fatalf("subprocess output = %q, want it to contain %q", out, wantSubstr)
	}
}
