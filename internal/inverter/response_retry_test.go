// Tests for IEEE-045 — `PostResponseWithRetry` (Phase 6 closer).
//
// Coverage (one assertion concept per test where the input space allows):
//
//  1. Happy path — first attempt succeeds, no retry, no sleep.
//  2. Transient then OK — retry fires once, exactly one sleep at
//     InitialDelay.
//  3. Persistent transient — MaxAttempts attempts, two sleeps
//     (InitialDelay, then InitialDelay × Multiplier), wrapped err carries
//     ErrResponseTransient.
//  4. 4xx (non-transient) — no retry, wrapped non-sentinel err returned
//     verbatim.
//  5. Context cancelled mid-retry wait — wrapped context.Canceled,
//     no further POSTs.
//  6. Backoff cap — MaxDelay clamps every wait so the schedule never
//     exceeds MaxDelay.
//  7. Dead-letter log — log line contains "DEAD-LETTER", event mRID, and
//     status after all attempts fail.
//  8. Defaults — DefaultResponseRetryConfig() yields the documented
//     constants; zero-valued fields fall through to defaults.
//
// The retry primitive is exercised through a deterministic fake clock so
// tests are race-clean and complete in well under a second regardless of
// the configured backoff schedule.

package inverter_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// =============================================================================
// Fixtures
// =============================================================================

// fakePoster is a thread-safe `inverter.ResponsePoster` that records every
// PostResponse invocation and pops queued return values off the front of
// `errs`. A nil `errs` entry means "return nil"; running out of entries
// keeps returning nil thereafter.
type fakePoster struct {
	mu    sync.Mutex
	calls int
	errs  []error
}

func (f *fakePoster) PostResponse(_ context.Context, _ string, _ sep2.DERControlResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakePoster) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeRetryClock is the deterministic `retryClock` used by the tests. It
// records every requested wait duration. If `cancelOn[i]` is set,
// the i-th wait returns ctx.Err() instead of "elapsed", simulating a
// context cancelled while the helper is sleeping. The clock honors a
// pre-cancelled ctx the same way `realRetryClock` does.
type fakeRetryClock struct {
	mu       sync.Mutex
	waits    []time.Duration
	cancelOn map[int]bool // 1-indexed: 1 = first wait, etc.
}

func (c *fakeRetryClock) Wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	idx := len(c.waits)
	cancel := c.cancelOn[idx]
	c.mu.Unlock()
	if cancel {
		// Surface ctx.Err() — by the time the test triggers a cancel
		// the underlying context has been Canceled.
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (c *fakeRetryClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.waits))
	copy(out, c.waits)
	return out
}

// sampleResp returns a DERControlResponse with a Subject (mRID) and a
// Status so dead-letter log assertions can verify the line carries both.
func sampleResp() sep2.DERControlResponse {
	s := sep2.ResponseStatusEventStarted
	return sep2.DERControlResponse{
		Response: sep2.Response{
			Subject: "EVT-RETRY-001",
			Status:  &s,
		},
	}
}

// transientErr returns a freshly-wrapped error that satisfies
// `errors.Is(err, ErrResponseTransient)` — mirrors the wire-up that
// PostResponse produces on a 503 or transport-level error.
func transientErr(detail string) error {
	return fmt.Errorf("POST Response %s: %w", detail, inverter.ErrResponseTransient)
}

// (captureLogs lives in response_post_test.go — same package, reused.)

// tightCfg returns a retry config with tight delays suitable for tests
// that walk the schedule. Even though waits run on a fake clock and
// never burn wall-clock time, keeping numeric values small makes the
// recorded `waits` slice trivial to assert against.
func tightCfg() inverter.ResponseRetryConfig {
	return inverter.ResponseRetryConfig{
		MaxAttempts:       3,
		InitialDelay:      10 * time.Millisecond,
		MaxDelay:          1 * time.Second,
		BackoffMultiplier: 2.0,
	}
}

// =============================================================================
// (1) Happy path — first attempt succeeds, no retry, no sleep.
// =============================================================================

func TestPostResponseWithRetry_HappyPath(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{}
	clock := &fakeRetryClock{}

	err := inverter.PostResponseWithRetryForTest(
		context.Background(), poster, "/rsps/1/rsp", sampleResp(), tightCfg(), clock,
	)
	if err != nil {
		t.Fatalf("PostResponseWithRetry: %v", err)
	}
	if got := poster.Calls(); got != 1 {
		t.Errorf("PostResponse calls = %d, want 1", got)
	}
	if waits := clock.Waits(); len(waits) != 0 {
		t.Errorf("waits = %v, want none on happy path", waits)
	}
}

// =============================================================================
// (2) Transient then OK — retry fires once.
// =============================================================================

func TestPostResponseWithRetry_TransientThenOK(t *testing.T) {
	t.Parallel()
	cfg := tightCfg()
	poster := &fakePoster{errs: []error{transientErr("/rsps/1/rsp")}}
	clock := &fakeRetryClock{}

	err := inverter.PostResponseWithRetryForTest(
		context.Background(), poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
	)
	if err != nil {
		t.Fatalf("PostResponseWithRetry: %v", err)
	}
	if got := poster.Calls(); got != 2 {
		t.Errorf("PostResponse calls = %d, want 2 (first transient, second OK)", got)
	}
	waits := clock.Waits()
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want exactly 1 (between attempts 1 and 2)", waits)
	}
	if waits[0] != cfg.InitialDelay {
		t.Errorf("first wait = %v, want InitialDelay=%v", waits[0], cfg.InitialDelay)
	}
}

// =============================================================================
// (3) Persistent transient — exhaust attempts.
// =============================================================================

func TestPostResponseWithRetry_PersistentTransient(t *testing.T) {
	t.Parallel()
	cfg := tightCfg()
	// 3 transients in a row → all 3 attempts fail.
	poster := &fakePoster{errs: []error{
		transientErr("attempt-1"),
		transientErr("attempt-2"),
		transientErr("attempt-3"),
	}}
	clock := &fakeRetryClock{}

	err := inverter.PostResponseWithRetryForTest(
		context.Background(), poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
	)
	if err == nil {
		t.Fatalf("PostResponseWithRetry: nil err, want wrapped ErrResponseTransient")
	}
	if !errors.Is(err, inverter.ErrResponseTransient) {
		t.Errorf("err = %v, want errors.Is ErrResponseTransient", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("err message = %q, want attempt count", err.Error())
	}
	if got := poster.Calls(); got != cfg.MaxAttempts {
		t.Errorf("PostResponse calls = %d, want %d", got, cfg.MaxAttempts)
	}

	waits := clock.Waits()
	if len(waits) != 2 {
		t.Fatalf("waits = %v, want 2 (between attempts 1→2 and 2→3)", waits)
	}
	if waits[0] != cfg.InitialDelay {
		t.Errorf("wait[0] = %v, want %v", waits[0], cfg.InitialDelay)
	}
	wantSecond := time.Duration(float64(cfg.InitialDelay) * cfg.BackoffMultiplier)
	if waits[1] != wantSecond {
		t.Errorf("wait[1] = %v, want %v", waits[1], wantSecond)
	}
}

// =============================================================================
// (4) 4xx (non-transient) — no retry.
// =============================================================================

func TestPostResponseWithRetry_NonTransientNoRetry(t *testing.T) {
	t.Parallel()
	cfg := tightCfg()
	clientErr := errors.New("POST Response /rsps/1/rsp: client error status 400")
	poster := &fakePoster{errs: []error{clientErr}}
	clock := &fakeRetryClock{}

	err := inverter.PostResponseWithRetryForTest(
		context.Background(), poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
	)
	if err == nil {
		t.Fatalf("PostResponseWithRetry: nil err, want non-transient")
	}
	if errors.Is(err, inverter.ErrResponseTransient) {
		t.Errorf("err satisfies errors.Is ErrResponseTransient; must not for 4xx: %v", err)
	}
	if !errors.Is(err, clientErr) {
		t.Errorf("err = %v, want to unwrap to clientErr", err)
	}
	if got := poster.Calls(); got != 1 {
		t.Errorf("PostResponse calls = %d, want exactly 1 (no retry on 4xx)", got)
	}
	if waits := clock.Waits(); len(waits) != 0 {
		t.Errorf("waits = %v, want none on non-transient err", waits)
	}
}

// =============================================================================
// (5) Context cancelled mid-retry wait.
// =============================================================================

func TestPostResponseWithRetry_ContextCancelledDuringBackoff(t *testing.T) {
	t.Parallel()
	cfg := tightCfg()
	poster := &fakePoster{errs: []error{transientErr("attempt-1")}}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel BEFORE Wait() is invoked so the first wait sees ctx.Err()
	// non-nil and returns context.Canceled. The clock's `cancelOn[1]`
	// machinery here cancels the parent ctx as the wait fires.
	clock := &fakeRetryClock{
		cancelOn: map[int]bool{1: true},
	}
	// Hand-wire the cancel: we need ctx.Err() to be set when Wait runs.
	// Pre-cancel — the helper has already done one attempt, so this
	// targets the backoff-wait branch specifically (the first attempt
	// runs against an as-yet-non-cancelled ctx).
	go func() { cancel() }()

	err := inverter.PostResponseWithRetryForTest(
		ctx, poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
	)
	if err == nil {
		t.Fatalf("PostResponseWithRetry: nil err, want context.Canceled wrap")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is context.Canceled", err)
	}
	if got := poster.Calls(); got != 1 {
		t.Errorf("PostResponse calls = %d, want exactly 1 (cancel aborts after first transient)", got)
	}
}

// TestPostResponseWithRetry_ContextAlreadyCancelled covers the pre-loop
// short-circuit — a ctx cancelled before the first attempt should NOT
// burn an HTTP request.
func TestPostResponseWithRetry_ContextAlreadyCancelled(t *testing.T) {
	t.Parallel()
	cfg := tightCfg()
	poster := &fakePoster{}
	clock := &fakeRetryClock{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := inverter.PostResponseWithRetryForTest(
		ctx, poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
	)
	if err == nil {
		t.Fatalf("PostResponseWithRetry: nil err, want context.Canceled wrap")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is context.Canceled", err)
	}
	if got := poster.Calls(); got != 0 {
		t.Errorf("PostResponse calls = %d, want 0 (pre-cancelled ctx must short-circuit)", got)
	}
}

// TestPostResponseWithRetry_ContextCancelledOnAttempt covers the case
// where PostResponse itself returns a context.Canceled-wrapped error
// (e.g. ctx fires while the HTTP request is in flight). Must NOT retry.
func TestPostResponseWithRetry_ContextCancelledOnAttempt(t *testing.T) {
	t.Parallel()
	cfg := tightCfg()
	ctxErr := fmt.Errorf("POST Response /rsps/1/rsp: %w", context.Canceled)
	poster := &fakePoster{errs: []error{ctxErr}}
	clock := &fakeRetryClock{}

	err := inverter.PostResponseWithRetryForTest(
		context.Background(), poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
	)
	if err == nil {
		t.Fatalf("PostResponseWithRetry: nil err, want context.Canceled wrap")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is context.Canceled", err)
	}
	if got := poster.Calls(); got != 1 {
		t.Errorf("PostResponse calls = %d, want exactly 1 (ctx err short-circuits)", got)
	}
	if waits := clock.Waits(); len(waits) != 0 {
		t.Errorf("waits = %v, want none when ctx err short-circuits", waits)
	}
}

// =============================================================================
// (6) Backoff cap — MaxDelay clamps every wait.
// =============================================================================

func TestPostResponseWithRetry_BackoffCappedByMaxDelay(t *testing.T) {
	t.Parallel()
	// Tight cap: every wait must be ≤ MaxDelay even with 4 attempts.
	cfg := inverter.ResponseRetryConfig{
		MaxAttempts:       4,
		InitialDelay:      1 * time.Second, // already at the cap
		MaxDelay:          2 * time.Second,
		BackoffMultiplier: 10.0, // would explode past the cap immediately
	}
	poster := &fakePoster{errs: []error{
		transientErr("a1"), transientErr("a2"), transientErr("a3"), transientErr("a4"),
	}}
	clock := &fakeRetryClock{}

	_ = inverter.PostResponseWithRetryForTest(
		context.Background(), poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
	)
	if got := poster.Calls(); got != cfg.MaxAttempts {
		t.Fatalf("PostResponse calls = %d, want %d", got, cfg.MaxAttempts)
	}
	waits := clock.Waits()
	if len(waits) != cfg.MaxAttempts-1 {
		t.Fatalf("waits = %v, want %d entries", waits, cfg.MaxAttempts-1)
	}
	for i, w := range waits {
		if w > cfg.MaxDelay {
			t.Errorf("wait[%d] = %v exceeds MaxDelay=%v", i, w, cfg.MaxDelay)
		}
	}
}

// =============================================================================
// (7) Dead-letter log on all-failed.
// =============================================================================

func TestPostResponseWithRetry_DeadLetterLog(t *testing.T) {
	// Deliberately serial — captureLogs swaps log.Default()'s writer,
	// which is global state. Even with the IEEE-081 mutex guarding
	// concurrent captureLogs callers, any OTHER t.Parallel() test in
	// this package that emits log.Printf lines while this test holds
	// the capture buffer will write into that buffer (the global
	// writer is shared) and race with buf.String(). The only
	// race-clean option is to run this test in the serial phase
	// (before parallel tests resume) — Go's testing runtime
	// guarantees serial tests complete before parallel tests start.
	// Refactoring production log.Printf to an injectable logger is
	// out of scope for IEEE-081 (would change package API).
	cfg := tightCfg()
	poster := &fakePoster{errs: []error{
		transientErr("a1"), transientErr("a2"), transientErr("a3"),
	}}
	clock := &fakeRetryClock{}

	var err error
	logs := captureLogs(t, func() {
		err = inverter.PostResponseWithRetryForTest(
			context.Background(), poster, "/rsps/1/rsp", sampleResp(), cfg, clock,
		)
	})
	if err == nil {
		t.Fatalf("PostResponseWithRetry: nil err, want failure")
	}
	for _, want := range []string{
		"DEAD-LETTER",
		`event="EVT-RETRY-001"`,
		fmt.Sprintf("status=%d", sep2.ResponseStatusEventStarted),
		"after 3 attempts",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("dead-letter log missing %q; got:\n%s", want, logs)
		}
	}
}

// =============================================================================
// (8) Defaults
// =============================================================================

func TestDefaultResponseRetryConfig_ValuesDocumented(t *testing.T) {
	t.Parallel()
	cfg := inverter.DefaultResponseRetryConfig()
	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", cfg.MaxAttempts)
	}
	if cfg.InitialDelay != 500*time.Millisecond {
		t.Errorf("InitialDelay = %v, want 500ms", cfg.InitialDelay)
	}
	if cfg.MaxDelay != 30*time.Second {
		t.Errorf("MaxDelay = %v, want 30s", cfg.MaxDelay)
	}
	if cfg.BackoffMultiplier != 2.0 {
		t.Errorf("BackoffMultiplier = %v, want 2.0", cfg.BackoffMultiplier)
	}
}

// TestPostResponseWithRetry_ZeroConfigDefaultsFire confirms a
// zero-valued ResponseRetryConfig flows through `withDefaults`. We
// observe this indirectly: a zero-config single-transient-then-OK run
// must still complete in 2 attempts with one wait at the default
// InitialDelay.
func TestPostResponseWithRetry_ZeroConfigDefaultsFire(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{errs: []error{transientErr("attempt-1")}}
	clock := &fakeRetryClock{}

	err := inverter.PostResponseWithRetryForTest(
		context.Background(), poster, "/rsps/1/rsp", sampleResp(),
		inverter.ResponseRetryConfig{}, // all zero values
		clock,
	)
	if err != nil {
		t.Fatalf("PostResponseWithRetry: %v", err)
	}
	if got := poster.Calls(); got != 2 {
		t.Errorf("PostResponse calls = %d, want 2", got)
	}
	waits := clock.Waits()
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want 1", waits)
	}
	if waits[0] != 500*time.Millisecond {
		t.Errorf("wait[0] = %v, want default InitialDelay=500ms", waits[0])
	}
}

// =============================================================================
// Production-path smoke: real clock + ctx-cancel mid-wait
// =============================================================================

// TestPostResponseWithRetry_RealClockCtxCancel exercises the
// production-path realRetryClock so the wait-vs-ctx select is covered
// without depending on the test seam. The test uses a tight InitialDelay
// (10s) but cancels the ctx within ~1ms, so wall-clock impact is
// negligible.
func TestPostResponseWithRetry_RealClockCtxCancel(t *testing.T) {
	t.Parallel()
	cfg := inverter.ResponseRetryConfig{
		MaxAttempts:       3,
		InitialDelay:      10 * time.Second, // would be ages if not cancelled
		MaxDelay:          30 * time.Second,
		BackoffMultiplier: 2.0,
	}
	poster := &fakePoster{errs: []error{transientErr("attempt-1")}}
	ctx, cancel := context.WithCancel(context.Background())
	// Fire cancel just after the first attempt returns — schedule it on
	// a goroutine bounded by t.Cleanup.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := inverter.PostResponseWithRetry(ctx, poster, "/rsps/1/rsp", sampleResp(), cfg)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("PostResponseWithRetry: nil err, want context.Canceled wrap")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, want well under 5s (ctx cancel must abort wait)", elapsed)
	}
	if got := poster.Calls(); got != 1 {
		t.Errorf("PostResponse calls = %d, want 1", got)
	}
}
