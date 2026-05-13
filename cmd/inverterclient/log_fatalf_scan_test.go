package main

// IEEE-048 regression scan: log.Fatalf call-site inventory in main.go.
//
// Phase 7 Exit Criterion 4
// (`plans/plan-1-csip-client-conformance/phase-7-http-semantics.md`):
//
//	"Every log.Fatalf call in cmd/inverterclient/main.go that fires on HTTP
//	 error has been audited: kept where the function set is essential to
//	 operating, replaced with a warning-and-bypass where the function set
//	 is optional."
//
// This scan reads cmd/inverterclient/main.go and asserts:
//
//   1. The two graceful-bypass call sites previously at lines 251 (Phase 1b
//      time sync) and 635 (Phase 3 DER list GET) are gone. The patterns
//      that triggered Fatalf there are unmistakable; if either resurfaces,
//      this test fails.
//
//   2. The set of REMAINING `log.Fatalf(` call lines in main.go matches an
//      explicit allow-list of "essential / by-design" sites carried over
//      from prior tickets (IEEE-028/029/031/034 boot path,
//      IEEE-074/075/076 phase2b/2c/2c exit-code owners). Any new
//      `log.Fatalf(` line outside this allow-list fails the test and
//      forces explicit audit. The intent: nobody adds a new fatal call
//      site without updating this scan.
//
// Counts the literal `log.Fatalf(` token to filter out comment references
// (which mention `log.Fatalf` in prose but do not call it).

import (
	"os"
	"strings"
	"testing"
)

// expectedFatalfSitePrefixes is the set of literal source lines (trimmed of
// surrounding whitespace) on which `log.Fatalf(` may legitimately appear in
// cmd/inverterclient/main.go after IEEE-048.
//
// Each entry is the unique prefix of the source line — the literal call
// site, not its line number, so the test does not need to re-baseline on
// every comment-only edit. If a fatal call site is removed (e.g. extracted
// like IEEE-048 just did for Phase 1b / Phase 3), drop its entry here.
// If a new fatal call site is added, add its prefix here AND justify it
// in code comments at the call site.
var expectedFatalfSitePrefixes = []string{
	`log.Fatalf("create client: %v", err)`,
	`log.Fatalf("discover: %v", err)`,
	`log.Fatalf("wait for advertised links: %v", err)`,
	`log.Fatalf("--csip set but DeviceCapability has no EndDeviceListLink")`,
	`log.Fatalf("lookup own EndDevice: %v", err)`,
	`log.Fatalf("DeviceCapability has no EndDeviceListLink; registration impossible")`,
	`log.Fatalf("register: %v", err)`,
	// The three phase2b/2c/2c-derprogram extracted exit-code owners. They
	// share the same literal call expression so the slice has it once and
	// the count is enforced separately below.
	`log.Fatalf("%s", fe.Error())`,
}

// expectedFatalfPercentSCount is the number of `log.Fatalf("%s", fe.Error())`
// occurrences in main.go — one per extracted phase (IEEE-074 phase2b,
// IEEE-075 phase2c-fsalist, IEEE-076 phase2c-derprogram). Pinned so a fourth
// extraction (or a fourth call site sneaking in) trips this test.
const expectedFatalfPercentSCount = 3

// TestLogFatalfCallSites scans cmd/inverterclient/main.go and enforces the
// IEEE-048 audited inventory.
func TestLogFatalfCallSites(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	var callLines []string
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		// Filter out comments — `// log.Fatalf ...` is prose, not a call.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "log.Fatalf(") {
			callLines = append(callLines, trimmed)
		}
	}

	// Allow-list match: every call line must appear (as a prefix) in the
	// expected set, AND the multi-occurrence `%s` form must appear exactly
	// the expected number of times.
	percentSSeen := 0
	for _, line := range callLines {
		matched := false
		for _, want := range expectedFatalfSitePrefixes {
			if strings.HasPrefix(line, want) {
				matched = true
				if want == `log.Fatalf("%s", fe.Error())` {
					percentSSeen++
				}
				break
			}
		}
		if !matched {
			t.Errorf("unexpected log.Fatalf call site in main.go: %s\nadd it to expectedFatalfSitePrefixes with a justification comment, or replace with graceful bypass per IEEE-048", line)
		}
	}
	if percentSSeen != expectedFatalfPercentSCount {
		t.Errorf("log.Fatalf(%%s, fe.Error()) count = %d, want %d (one per extracted phase 2b/2c/2c-derprogram)", percentSSeen, expectedFatalfPercentSCount)
	}

	// Graceful-bypass forbidden patterns: the two IEEE-048 retired call
	// sites must not return. Match on a distinctive substring of each.
	forbidden := []struct {
		needle string
		reason string
	}{
		{
			needle: `log.Fatalf("initial server time sync `,
			reason: "Phase 1b time-sync was extracted into runPhase1bTimeSync with graceful bypass (IEEE-048)",
		},
		{
			needle: `log.Fatalf("GET DER list `,
			reason: "Phase 3 DER list GET was extracted into fetchDERListForSetup with graceful bypass (IEEE-048)",
		},
	}
	for _, f := range forbidden {
		for _, line := range callLines {
			if strings.Contains(line, f.needle) {
				t.Errorf("regression: forbidden log.Fatalf returned in main.go: %s\nreason it was removed: %s", line, f.reason)
			}
		}
	}
}
