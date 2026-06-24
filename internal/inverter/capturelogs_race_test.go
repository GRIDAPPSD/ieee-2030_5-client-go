// IEEE-081: race regression test for captureLogs.
//
// The captureLogs helper in response_post_test.go swaps log.Default()'s
// global writer to capture log.Printf output for assertion. Two t.Parallel()
// tests calling captureLogs concurrently would otherwise race on the global
// writer slot : the previous workaround was to strip t.Parallel() from
// TestPostResponseWithRetry_DeadLetterLog (Pike DD).
//
// IEEE-081 makes captureLogs race-safe by serializing the swap behind a
// package-level sync.Mutex. This test pins that contract: two goroutines
// hammer captureLogs with deterministic, distinguishable content, and each
// must see only its own log output. If a future change removes the mutex
// (or "fixes" captureLogs in a way that drops the serialization), this
// test trips under `go test -race` long before it gets to main.

package inverter_test

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
)

// TestCaptureLogs_ConcurrentCallsAreIsolated runs N goroutines inside a
// single test that each call captureLogs with a unique tag and assert
// their buf contains their own tag and nothing else. Race-detector trips
// would surface here before the bigger response_post / response_retry
// suites cross-talk.
//
// Deliberately NOT t.Parallel() : captureLogs callers MUST be serial in
// this package (see captureLogs godoc in response_post_test.go).
func TestCaptureLogs_ConcurrentCallsAreIsolated(t *testing.T) {
	const goroutines = 8
	const writesPerGoroutine = 25

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			tag := fmt.Sprintf("worker-%d", idx)
			logs := captureLogs(t, func() {
				for j := 0; j < writesPerGoroutine; j++ {
					log.Printf("%s line=%d", tag, j)
				}
			})
			// Must see all of this goroutine's lines.
			for j := 0; j < writesPerGoroutine; j++ {
				want := fmt.Sprintf("%s line=%d", tag, j)
				if !strings.Contains(logs, want) {
					errs[idx] = fmt.Errorf("missing own line %q in:\n%s", want, logs)
					return
				}
			}
			// Must NOT see any other goroutine's tag : that would mean
			// captureLogs leaked another goroutine's writes into this buf.
			for k := 0; k < goroutines; k++ {
				if k == idx {
					continue
				}
				other := fmt.Sprintf("worker-%d ", k)
				if strings.Contains(logs, other) {
					errs[idx] = fmt.Errorf("leaked cross-goroutine tag %q in:\n%s", other, logs)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}
