package main

// Package main covers two helper functions
// (GRIDAPPSD/ieee-2030_5-server-go#44):
//
// - redactPIN: masks all but the last 2 digits of a PIN
//     producing "***NN". IEEE 2030.5 §8.2.1 makes the last digit a check
//     digit; trailing 2 is conventional in security UIs for redaction that
//     preserves the check-digit signature without exposing the secret.
// - pinPollInterval: converts a SEP2
//     pollRate (seconds, uint32) to a time.Duration with the project's
//     floor (60s) and default-on-zero (30min) policy. Used by the Phase 2b
//     idle loops (server-PIN-not-provisioned and missing-RegistrationLink-
//     in-CSIP-strict) and the Phase 2c FSAList / DERProgram-walk idle loops.
//
// Cases covered:
//
//  1. TestRedactPIN_MatchesRedactionRegex   : every output
//                                              matches ^\*\*\*\d{2}$.
//                                              Asserted with a compiled
//                                              regex, not substring.
//  2. TestRedactPIN_Boundaries              : boundary
//                                              table-driven coverage of
//                                              p=0, p=5, p=99, p=100,
//                                              p=111115 (the CSIP V1.2
//                                              BASIC-001 step 5 reference
//                                              value), p=222222.
//  3. TestPinPollInterval_PassThrough       : pollRate=120
//                                              -> 120 seconds.
//  4. TestPinPollInterval_FloorApplied      : pollRate=10
//                                              -> 60 seconds (floor).
//  5. TestPinPollInterval_ZeroDefaults      : pollRate=0 -> 30
//                                              minutes (default-on-zero).
//
// The Phase 2b integration cases (PIN-match branches, missing-Registration
// Link branches, idle-loop policy) live in phase2b_test.go, which drives
// runPhase2bRegistration directly. This file covers only the two pure
// helper functions above.

import (
	"regexp"
	"testing"
	"time"
)

// pinRedactionRegex is the contract: every log line that
// surfaces a PIN goes through redactPIN, and redactPIN must always emit
// exactly three '*' followed by exactly two digits. Anything else is a
// regression that re-introduces cleartext PIN disclosure (low-severity
// but real per IEEE 2030.5 §8.2.1).
var pinRedactionRegex = regexp.MustCompile(`^\*\*\*\d{2}$`)

// TestRedactPIN_MatchesRedactionRegex verifies the contract above.
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

// TestRedactPIN_Boundaries : boundary table.
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

// TestPinPollInterval_PassThrough : pollRate above the floor.
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

// TestPinPollInterval_FloorApplied : pollRate below the floor.
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

// TestPinPollInterval_ZeroDefaults : pollRate=0 default-on-zero behavior.
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
