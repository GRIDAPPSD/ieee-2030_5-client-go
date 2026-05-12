package main

// Backfill of the IEEE-034 + IEEE-033 helper-function tests:
//
//   - redactPIN  (IEEE-034): masks all but the last 2 digits of a PIN
//     producing "***NN". IEEE 2030.5 §8.2.1 makes the last digit a check
//     digit; trailing 2 is conventional in security UIs for redaction that
//     preserves the check-digit signature without exposing the secret.
//   - pinPollInterval (IEEE-033 / IEEE-035 / IEEE-036): converts a SEP2
//     pollRate (seconds, uint32) to a time.Duration with the project's
//     floor (60s) and default-on-zero (30min) policy. Used by the Phase 2b
//     idle loops (server-PIN-not-provisioned and missing-RegistrationLink-
//     in-CSIP-strict) and the Phase 2c FSAList / DERProgram-walk idle loops.
//
// Origin tickets MERGED at 833ae73 (IEEE-033) and 4965474 (IEEE-034).
// Plan-3 csip-test-debt-sweep Phase 3, IEEE-071.
//
// Cases shipped:
//
//  1. TestRedactPIN_MatchesRedactionRegex   — IEEE-034 case 7: every output
//                                              matches ^\*\*\*\d{2}$.
//                                              Asserted with a compiled
//                                              regex, not substring.
//  2. TestRedactPIN_Boundaries              — IEEE-034 case 7 (boundary):
//                                              table-driven coverage of
//                                              p=0, p=5, p=99, p=100,
//                                              p=111115 (the CSIP V1.2
//                                              BASIC-001 step 5 reference
//                                              value), p=222222.
//  3. TestPinPollInterval_PassThrough       — IEEE-033 case 6: pollRate=120
//                                              -> 120 seconds.
//  4. TestPinPollInterval_FloorApplied      — IEEE-033 case 7: pollRate=10
//                                              -> 60 seconds (floor).
//  5. TestPinPollInterval_ZeroDefaults      — derived from IEEE-033 cases
//                                              6/7: pollRate=0 -> 30
//                                              minutes (default-on-zero).
//
// SUPERSESSION + Phase 2b DEFERRAL:
//
// IEEE-032 cases 5-6, IEEE-033 cases 1-5+8, and IEEE-034 cases 1-6+8 all
// live inside `cmd/inverterclient/main.go`'s `main()` body — the Phase 2b
// block straddling lines 281-376 of the production file. Two structural
// problems block direct integration tests:
//
//   1. log.Fatalf on the fatal-mismatch path (line 373) tears down the
//      test process and cannot be intercepted without subprocess-spawning
//      patterns (`os/exec` re-invocation), which the IEEE-028 / IEEE-031
//      / IEEE-038 / IEEE-070 sweep precedents have NOT used.
//   2. Phase 2b is plain straight-line code inside main(), not a function;
//      there is no seam through which a test can drive the
//      ExpectedPIN / rg.PIN / RegistrationLink / AllowUnregistered state
//      space.
//
// Both are fixable, but only by extracting Phase 2b into a function. Per
// Pike's hard rule (no production refactor in a tests-only ticket beyond
// a minimum-surgical seam) and the supplementary IEEE-071 brief, that
// extraction is deferred to a follow-up ticket. The IEEE-032 #1-#4
// unit cases (registration_test.go) plus the helper unit cases here cover
// the testable surface of the IEEE-032/033/034 frozen behavior; the
// Phase 2b cases blocked by the absent seam are catalogued in the PR
// body so the follow-up ticket inherits a complete list.

import (
	"regexp"
	"testing"
	"time"
)

// pinRedactionRegex is the IEEE-034 case 7 contract: every log line that
// surfaces a PIN goes through redactPIN, and redactPIN must always emit
// exactly three '*' followed by exactly two digits. Anything else is a
// regression that re-introduces cleartext PIN disclosure (low-severity
// but real per IEEE 2030.5 §8.2.1).
var pinRedactionRegex = regexp.MustCompile(`^\*\*\*\d{2}$`)

// TestRedactPIN_MatchesRedactionRegex — IEEE-034 case 7.
//
// Spans the realistic PIN range plus the CSIP V1.2 BASIC-001 step 5
// reference value (111115) and a worst-case mismatch sample (222222).
// The point of the strict regex is to catch a regression that drops the
// %02d zero-padding (which would produce "***5" for p=5) or that
// accidentally re-introduces cleartext (which would produce "111115").
func TestRedactPIN_MatchesRedactionRegex(t *testing.T) {
	t.Parallel()

	samples := []uint{0, 1, 5, 9, 10, 15, 42, 99, 100, 105, 1000, 111115, 222222}
	for _, p := range samples {
		got := redactPIN(p)
		if !pinRedactionRegex.MatchString(got) {
			t.Errorf("redactPIN(%d) = %q; does not match %s", p, got, pinRedactionRegex)
		}
	}
}

// TestRedactPIN_Boundaries — IEEE-034 case 7 (boundary table).
//
// Pins the exact masked output for representative inputs. The two-arm
// implementation (p < 100 vs p >= 100) creates a discontinuity at 100;
// the table covers both arms and the boundary.
func TestRedactPIN_Boundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   uint
		want string
	}{
		{"zero", 0, "***00"},
		{"single-digit", 5, "***05"},
		{"two-digit upper", 99, "***99"},
		{"boundary", 100, "***00"},
		{"three-digit", 105, "***05"},
		{"six-digit reference (BASIC-001 step 5)", 111115, "***15"},
		{"six-digit mismatch sample", 222222, "***22"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactPIN(tc.in)
			if got != tc.want {
				t.Errorf("redactPIN(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPinPollInterval_PassThrough — IEEE-033 case 6.
//
// pollRate of 120 seconds is above the 60s floor and a non-zero value, so
// pinPollInterval returns exactly 120s.
func TestPinPollInterval_PassThrough(t *testing.T) {
	t.Parallel()

	got := pinPollInterval(120)
	want := 120 * time.Second
	if got != want {
		t.Errorf("pinPollInterval(120) = %s, want %s", got, want)
	}
}

// TestPinPollInterval_FloorApplied — IEEE-033 case 7.
//
// pollRate of 10 seconds is below the 60s floor; pinPollInterval clamps
// to the floor, NOT to the 30-minute default.
func TestPinPollInterval_FloorApplied(t *testing.T) {
	t.Parallel()

	got := pinPollInterval(10)
	want := 60 * time.Second
	if got != want {
		t.Errorf("pinPollInterval(10) = %s, want %s (floor)", got, want)
	}
}

// TestPinPollInterval_ZeroDefaults — derived from IEEE-033 cases 6/7.
//
// pollRate of 0 hits the default-on-zero branch (30 minutes). The floor
// check runs AFTER the default branch, so the returned value is the full
// 30 minutes, not the 60s floor.
func TestPinPollInterval_ZeroDefaults(t *testing.T) {
	t.Parallel()

	got := pinPollInterval(0)
	want := 30 * time.Minute
	if got != want {
		t.Errorf("pinPollInterval(0) = %s, want %s (default-on-zero)", got, want)
	}
}
