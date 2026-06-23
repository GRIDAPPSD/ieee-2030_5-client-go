// Test-binary-only seam for IEEE-045's `PostResponseWithRetry`.
//
// The production helper relies on the unexported `retryClock` interface
// for its sleep-vs-cancel seam. Tests need to substitute a deterministic
// fake clock without exposing that interface on the public API surface.
// Pattern mirrors scheduler_export_test.go and idle_export_test.go.

package inverter

import (
	"context"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// TestRetryClock is the test-only adapter that satisfies the unexported
// `retryClock` interface. Test files implement Wait; this adapter forwards
// to it.
type TestRetryClock interface {
	Wait(ctx context.Context, d time.Duration) error
}

// testRetryClockAdapter bridges the exported TestRetryClock surface to
// the unexported `retryClock` interface used by
// `postResponseWithRetryClock`. Adapter pattern keeps the production
// interface unexported while tests still drive the helper deterministically.
type testRetryClockAdapter struct {
	inner TestRetryClock
}

func (a testRetryClockAdapter) wait(ctx context.Context, d time.Duration) error {
	return a.inner.Wait(ctx, d)
}

// PostResponseWithRetryForTest is the test-binary-only variant of
// `PostResponseWithRetry` that accepts an injected clock. Production code
// uses `PostResponseWithRetry`, which hardcodes `realRetryClock`.
func PostResponseWithRetryForTest(
	ctx context.Context,
	client ResponsePoster,
	replyToHref string,
	resp sep2.DERControlResponse,
	cfg ResponseRetryConfig,
	clock TestRetryClock,
) error {
	return postResponseWithRetryClock(ctx, client, replyToHref, resp, cfg, testRetryClockAdapter{inner: clock})
}
