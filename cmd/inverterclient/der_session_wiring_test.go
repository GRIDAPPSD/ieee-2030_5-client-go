// Regression scan: pins that main.go's DER-only subsystems are actually
// wired through the role gate, the way log_fatalf_scan_test.go pins the
// log.Fatalf call-site inventory. runDERSessionForRole and
// skipDERPipelineForRole are unit-tested on their own (role_test.go); this
// file is the seam that proves main() still calls them at every site fix
// round 3 named: the control poll, the event engine (curve hook, response
// hook, state-machine tick), and the simulated-device tick loop that also
// gates the alarm detector.
//
// A mutant that reverts any one of these call sites back to the bare
// `selected` (or drops the tick-loop skip) compiles and passes every
// other test; this scan is what fails it.

package main

import (
	"os"
	"strings"
	"testing"
)

// wantRunDERSessionLines are literal source lines (trimmed) that must all
// appear in main.go: the five places runDERSession gates a DER-only
// subsystem, in the order they appear.
var wantRunDERSessionLines = []string{
	"runDERSession := runDERSessionForRole(cfg, selected)",
	"if runDERSession && selectedDERProgram.DERControlListLink != nil {",
	"if runDERSession && selectedDERProgram.DERCurveListLink != nil {",
	"} else if runDERSession {",
	"if runDERSession {",
}

// TestDERSessionWiring_RunDERSessionGatesEveryDEROnlySubsystem fails if any
// of wantRunDERSessionLines is missing, or if a bare `selected` (not
// `runDERSession`) reappears as a gate condition where one of these lines
// belongs. The exact count of "runDERSession" occurrences is pinned too:
// 1 declaration + 5 uses = 6. A mutant that drops one use but keeps the
// others changes the count without necessarily removing a whole line this
// scan already checks by prefix, so the count is the second, independent
// signal.
func TestDERSessionWiring_RunDERSessionGatesEveryDEROnlySubsystem(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)

	for _, want := range wantRunDERSessionLines {
		if !strings.Contains(text, want) {
			t.Errorf("main.go missing expected DER-session gate line: %s", want)
		}
	}

	// strings.Count on the substring "runDERSession" also matches inside
	// "runDERSessionForRole" (the declaration line names both), so the
	// total is 8, not 5: 1 comment mention + 2 on the declaration line
	// (the variable and the function-name prefix) + 5 gate uses. A
	// mutant that removes any one of the 5 gate uses, or the
	// declaration, changes this count even if it does not remove a full
	// line another assertion in this test already checks by prefix.
	got := strings.Count(text, "runDERSession")
	const want = 8
	if got != want {
		t.Errorf("main.go: %d occurrences of \"runDERSession\" (substring, including inside runDERSessionForRole), want %d", got, want)
	}
}

// TestDERSessionWiring_TickLoopSkipsForAggregatorRole pins the Phase 5
// tick-loop gate that stops the simulated device tick, the alarm
// detector, the HMI broadcast and the status/metering reports from
// running for the aggregator role. Rather than matching an exact
// whitespace block (fragile to a harmless reformat), it locates the
// `case <-ticker.C:` arm and the first `dev.ReadState(ctx)` call after it,
// and requires both `skipDERPipelineForRole(cfg)` and a `continue` to
// appear strictly between them: the gate must run before the simulated
// device is touched, inside the tick handler, not somewhere unrelated in
// the file.
func TestDERSessionWiring_TickLoopSkipsForAggregatorRole(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)

	const tickCase = "case <-ticker.C:"
	caseIdx := strings.Index(text, tickCase)
	if caseIdx < 0 {
		t.Fatal("main.go: `case <-ticker.C:` not found")
	}
	rest := text[caseIdx+len(tickCase):]

	const readState = "dev.ReadState(ctx)"
	readIdx := strings.Index(rest, readState)
	if readIdx < 0 {
		t.Fatal("main.go: `dev.ReadState(ctx)` not found after the tick case")
	}
	tickHandlerPrefix := rest[:readIdx]

	if !strings.Contains(tickHandlerPrefix, "skipDERPipelineForRole(cfg)") {
		t.Error("main.go: the tick handler does not check skipDERPipelineForRole before dev.ReadState (mutant: the role gate was removed)")
	}
	if !strings.Contains(tickHandlerPrefix, "continue") {
		t.Error("main.go: the tick handler's role gate does not `continue` before dev.ReadState (mutant: the skip no longer skips the tick)")
	}
}
