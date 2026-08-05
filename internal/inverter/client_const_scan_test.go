// Package inverter_test ships an optional endpoint
// constant scan: a build-tag-less unit test that fails on reintroduction
// of the old hardcoded endpoint path literals in internal/inverter/client.go.
//
// Why this exists: a prior change replaced four method signatures (Register,
// PutDERCapability and the DER PUT family, CreateMirrorUsagePoint,
// PostMeterReading) that previously baked in `/edev`, `/edev/.../dercap`,
// `/mup`, and `/mup/.../mr` strings. Per IEEE 2030.5 §10.3 / CSIP §6.6
// the client MUST derive every path from advertised links. A regression
// : someone "simplifying" the API by hardcoding the path again : is
// silent under the existing tests because the integration test happens
// to mount the server at those same literals. This scan catches it.
package inverter_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenLiterals are path strings that MUST NOT appear as Go string
// literals inside internal/inverter/client.go. They were removed
// and any reintroduction means a callsite is hardcoding a path
// the spec requires to come from the link graph.
//
// Care: these are exact-match substrings. Literals like "/edev" alone
// would false-positive against legitimate log messages or comments. The
// chosen forms are unambiguous URL literals : `"/edev/`" (quoted, with
// trailing slash) catches `c.Post(ctx, "/edev/...", ...)` shapes
// without colliding with the doc comments that just say `/edev`.
var forbiddenLiterals = []string{
	`"/edev/" +`, // string concatenation onto a hardcoded /edev path
	`"/mup/" +`,  // string concatenation onto a hardcoded /mup path
	`"/edev"`,    // direct literal in a method body (not in a comment)
	`"/mup"`,     // direct literal in a method body (not in a comment)
}

// TestNoHardcodedEndpointConstants scans internal/inverter/client.go for
// the removed path literals. The check is line-based and skips
// lines that are entirely inside Go block/line comments : comments
// referencing the old paths are documentation, not regressions.
//
// Optional case.
func TestNoHardcodedEndpointConstants(t *testing.T) {
	t.Parallel()

	// Locate client.go relative to this test file. runtime.Caller lets
	// the test run from any working directory (`go test ./...`,
	// `cd internal/inverter && go test`, etc.) without depending on os.Getwd.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate test source")
	}
	clientGo := filepath.Join(filepath.Dir(thisFile), "client.go")

	src, err := os.ReadFile(clientGo)
	if err != nil {
		t.Fatalf("read %s: %v", clientGo, err)
	}

	lines := strings.Split(string(src), "\n")
	inBlockComment := false
	for i, line := range lines {
		stripped := strings.TrimSpace(line)

		// Track /* ... */ block comments. A line that opens AND closes
		// is treated as fully comment; one that only opens flips the
		// flag for subsequent lines.
		if inBlockComment {
			if strings.Contains(stripped, "*/") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(stripped, "/*") {
			if !strings.Contains(stripped, "*/") {
				inBlockComment = true
			}
			continue
		}
		// Skip pure line comments. Lines with a trailing // comment
		// are still checked on the code portion to the left.
		if strings.HasPrefix(stripped, "//") {
			continue
		}
		// Trim any trailing `// ...` so a code line with an explanatory
		// comment doesn't trip on a literal mentioned in the comment.
		codePart := line
		if idx := strings.Index(codePart, "//"); idx >= 0 {
			// Be conservative: only strip when the // sits outside a
			// string literal. A simple even-quote count is sufficient
			// for client.go's style : no raw strings cross-line, no
			// escaped quotes inside the patterns we look for.
			before := codePart[:idx]
			if strings.Count(before, `"`)%2 == 0 {
				codePart = before
			}
		}

		for _, bad := range forbiddenLiterals {
			if strings.Contains(codePart, bad) {
				t.Errorf("client.go:%d: forbidden hardcoded endpoint literal %q (hrefs must come from the link graph): %s",
					i+1, bad, strings.TrimSpace(line))
			}
		}
	}
}
