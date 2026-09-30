package main

// Regression scan: fatal call-site inventory in main.go.
//
// Every call in cmd/inverterclient/main.go that ends the process on an
// error has been audited: kept where the function set is essential to
// operating, replaced with a warning-and-bypass where the function set is
// optional. Two call forms are both fatal and both scanned: log.Fatalf
// (only before any fleet has started: ratingInt16's boot-time guard) and
// fatalf (item 5's helper, for every site reached after fleet start,
// since it runs shutdown's steps 2 to 4 before exiting, which os.Exit
// from a bare log.Fatalf would skip). This scan reads
// cmd/inverterclient/main.go and asserts:
//
//   1. The two graceful-bypass call sites (Phase 1b time sync, Phase 3 DER
//      list GET) stay gone, in either call form. The patterns that used to
//      trigger a fatal exit there are unmistakable; if either resurfaces,
//      this test fails.
//
//   2. The set of REMAINING fatal call lines in main.go matches an explicit
//      allow-list of essential/by-design sites (boot path, phase2b/2c/2c
//      exit-code owners, item 4's fleet-refusal path). Any new fatal call
//      line outside this allow-list fails the test and forces explicit
//      audit: nobody adds a new fatal call site without updating this scan.
//
// Counts the literal `log.Fatalf(` and `fatalf(` tokens to filter out
// comment references (which mention either in prose but do not call it).

import (
	"os"
	"strings"
	"testing"
)

// expectedLogFatalfSitePrefixes is every literal source line (trimmed of
// surrounding whitespace) on which `log.Fatalf(` may legitimately appear
// in cmd/inverterclient/main.go: only the boot-time guard reached before
// any fleet has started, where there is nothing yet for a shutdown helper
// to stop.
var expectedLogFatalfSitePrefixes = []string{
	// ratingInt16 guards the nameplate-to-wire-int16 cast at DER
	// capability/settings PUT time : a startup-config problem, not a
	// transient network error, so it is fatal rather than "continuing".
	// Reached before ctx, and so before any fleet Supervisor exists
	// (main.go: called at line 297, fleets start after ctx at line 300),
	// so it is the one fatal exit fatalf's shutdown has nothing to do for.
	`log.Fatalf("nameplate rating %.0f exceeds sep2 ActivePower/ReactivePower int16 range [%d, %d]", w, math.MinInt16, math.MaxInt16)`,
}

// expectedFatalfSitePrefixes is every literal source line on which the
// `fatalf(` helper (item 5) may legitimately appear: every essential exit
// reached after fleets have started, where shutdown's steps 2 to 4 (stop
// the notify receiver, then every fleet supervisor) must run before the
// process exits, which only fatalf (not a bare log.Fatalf) guarantees.
//
// If a fatal call site is removed (e.g. extracted for graceful bypass, as
// Phase 1b / Phase 3 were), drop its entry here. If a new one is added,
// add its prefix here AND justify it in code comments at the call site.
var expectedFatalfSitePrefixes = []string{
	`fatalf("create client: %v", err)`,
	`fatalf("discover: %v", err)`,
	`fatalf("wait for advertised links: %v", err)`,
	// #71: the lookup branch now also runs in the aggregator role, not only
	// under --csip, so the message no longer names --csip specifically.
	`fatalf("EndDevice lookup required but DeviceCapability has no EndDeviceListLink")`,
	`fatalf("lookup own EndDevice: %v", err)`,
	`fatalf("DeviceCapability has no EndDeviceListLink; registration impossible")`,
	`fatalf("register: %v", err)`,
	// The three phase2b/2c/2c-derprogram extracted exit-code owners. They
	// share the same literal call expression so the slice has it once and
	// the count is enforced separately below.
	`fatalf("%s", fe.Error())`,
	// #71: the aggregator role builds its own no-op Reporter inline
	// (runPhase4Metering is skipped) rather than a startup-config problem,
	// matching phase4_metering.go's own identical fatal call for the same
	// operation: client.LFDI() is never empty once the client exists, so
	// this is unreachable in practice; fail loudly rather than silently
	// disable metering if that ever changes.
	`fatalf("build reporter: %v", err)`,
}

// expectedFatalfPercentSCount is the number of `fatalf("%s", fe.Error())`
// occurrences in main.go : one per extracted phase (phase2b, phase2c-fsalist,
// phase2c-derprogram). Pinned so a fourth extraction (or a fourth call site
// sneaking in) trips this test.
const expectedFatalfPercentSCount = 3

// TestLogFatalfCallSites scans cmd/inverterclient/main.go and enforces the
// audited fatal-exit inventory, in both call forms.
func TestLogFatalfCallSites(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	var logFatalfLines, fatalfLines []string
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		// Filter out comments : `// log.Fatalf ...` / `// fatalf ...` is
		// prose, not a call.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "log.Fatalf(") {
			logFatalfLines = append(logFatalfLines, trimmed)
		}
		if strings.Contains(trimmed, "fatalf(") && !strings.Contains(trimmed, "log.Fatalf(") {
			fatalfLines = append(fatalfLines, trimmed)
		}
	}

	checkAllowList := func(lines []string, allowed []string, kind string) {
		for _, line := range lines {
			matched := false
			for _, want := range allowed {
				if strings.HasPrefix(line, want) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("unexpected %s call site in main.go: %s\nadd it to the allow-list with a justification comment, or replace with graceful bypass", kind, line)
			}
		}
	}
	checkAllowList(logFatalfLines, expectedLogFatalfSitePrefixes, "log.Fatalf")
	checkAllowList(fatalfLines, expectedFatalfSitePrefixes, "fatalf")

	percentSSeen := 0
	for _, line := range fatalfLines {
		if strings.HasPrefix(line, `fatalf("%s", fe.Error())`) {
			percentSSeen++
		}
	}
	if percentSSeen != expectedFatalfPercentSCount {
		t.Errorf("fatalf(%%s, fe.Error()) count = %d, want %d (one per extracted phase 2b/2c/2c-derprogram)", percentSSeen, expectedFatalfPercentSCount)
	}

	// Graceful-bypass forbidden patterns: the two retired call sites must
	// not return, in either call form.
	forbidden := []struct {
		needle string
		reason string
	}{
		{
			needle: `("initial server time sync `,
			reason: "Phase 1b time-sync was extracted into runPhase1bTimeSync with graceful bypass",
		},
		{
			needle: `("GET DER list `,
			reason: "Phase 3 DER list GET was extracted into fetchDERListForSetup with graceful bypass",
		},
	}
	allLines := append(append([]string{}, logFatalfLines...), fatalfLines...)
	for _, f := range forbidden {
		for _, line := range allLines {
			if strings.Contains(line, f.needle) {
				t.Errorf("regression: forbidden fatal call returned in main.go: %s\nreason it was removed: %s", line, f.reason)
			}
		}
	}
}
