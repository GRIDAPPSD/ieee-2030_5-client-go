// Package inverter (Phase 6 closer):
// `PostResponseWithRetry` layers a multi-attempt exponential-backoff retry
// plus a dead-letter log on top of the one-shot `PostResponse`.
//
// CSIP V1.2 CORE-022 conformance requires the inverter to keep applying
// controls regardless of Response delivery : the dead letter is
// informational, not blocking. Callers (the state-machine hook)
// receive a wrapped error on terminal failure but MUST NOT short-circuit
// the state machine on it.
//
// PostResponse already retries once internally on transient 5xx; this
// helper wraps that with a documented exponential schedule:
//
//	attempt 1 → fail (transient)            → sleep cfg.InitialDelay
//	attempt 2 → fail (transient)            → sleep cfg.InitialDelay × Multiplier (capped at MaxDelay)
//	attempt 3 → fail (transient)            → terminal: log dead-letter, return wrapped err.
//
// The retry loop honors context cancellation at the wait gate (no spin
// after the request returns), so a caller-cancelled ctx aborts within at
// most one in-flight request plus the small constant of channel select.
//
// Pike rules satisfied:
//   - Errors are values: every PostResponse failure is wrapped with %w
//     so callers can `errors.Is(err, ErrResponseTransient)` /
//     `errors.Is(err, context.Canceled)`.
//   - No goroutine leaks: this file spawns no goroutines. The retry
//     waits on `time.After(...)` (auto-collected after fire) or on
//     `ctx.Done()`.
//   - Scope: PostResponse internals untouched. This file is a leaf
//     consumer of (*SEP2Client).PostResponse.
package inverter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// ResponseRetryConfig parameterises `PostResponseWithRetry`. Zero values
// pass straight through `withDefaults` to documented defaults; a fully
// custom config overrides every field. Production callers should rely on
// `DefaultResponseRetryConfig()` for the documented defaults.
type ResponseRetryConfig struct {
	// MaxAttempts is the upper bound on total attempts (including the
	// first). Must be ≥ 1; 0 (zero value) is replaced with the default 3.
	MaxAttempts int

	// InitialDelay is the wait after attempt 1 before attempt 2. Must be
	// > 0; 0 is replaced with the default 500ms.
	InitialDelay time.Duration

	// MaxDelay caps the post-multiplier delay. Must be > 0; 0 is replaced
	// with the default 30s. Setting MaxDelay below InitialDelay clamps
	// every wait to MaxDelay (legal : useful for tight-loop tests).
	MaxDelay time.Duration

	// BackoffMultiplier scales the previous delay each attempt. Must be
	// ≥ 1.0; 0 (zero value) is replaced with the default 2.0. A
	// multiplier of 1.0 yields a flat schedule of InitialDelay between
	// every attempt.
	BackoffMultiplier float64
}

// Documented defaults : exposed as a function so callers cannot mutate
// the shared struct.
const (
	defaultRetryMaxAttempts       = 3
	defaultRetryInitialDelay      = 500 * time.Millisecond
	defaultRetryMaxDelay          = 30 * time.Second
	defaultRetryBackoffMultiplier = 2.0
)

// DefaultResponseRetryConfig returns the documented Phase 6 defaults:
// 3 attempts, 500ms initial backoff, 30s ceiling, 2.0× multiplier.
func DefaultResponseRetryConfig() ResponseRetryConfig {
	return ResponseRetryConfig{
		MaxAttempts:       defaultRetryMaxAttempts,
		InitialDelay:      defaultRetryInitialDelay,
		MaxDelay:          defaultRetryMaxDelay,
		BackoffMultiplier: defaultRetryBackoffMultiplier,
	}
}

// withDefaults returns a copy of cfg with zero-valued fields replaced by
// documented defaults. Treating zero as "use default" lets callers pass
// `ResponseRetryConfig{MaxAttempts: 5}` and inherit the rest.
func (cfg ResponseRetryConfig) withDefaults() ResponseRetryConfig {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultRetryMaxAttempts
	}
	if cfg.InitialDelay <= 0 {
		cfg.InitialDelay = defaultRetryInitialDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = defaultRetryMaxDelay
	}
	if cfg.BackoffMultiplier < 1.0 {
		cfg.BackoffMultiplier = defaultRetryBackoffMultiplier
	}
	return cfg
}

// ResponsePoster is the consumer-side interface that captures the single
// method `PostResponseWithRetry` depends on. Defined here so the helper
// stays trivially testable : a fake recording PostResponse calls is the
// whole fixture. `*SEP2Client` already satisfies this surface.
//
// Pike rule: interfaces defined at the consumer. The SEP2Client does not
// need to know its method is being wrapped.
type ResponsePoster interface {
	PostResponse(ctx context.Context, replyToHref string, resp sep2.DERControlResponse) error
}

// retryClock is the (sleep, now) seam used by `PostResponseWithRetry`.
// Production uses `realRetryClock` which delegates to `time.After` /
// `time.Now`. Tests inject a deterministic fake.
//
// `wait` returns nil when the requested duration elapses normally and
// ctx.Err() when ctx fires first. Implementations MUST honor ctx.
type retryClock interface {
	wait(ctx context.Context, d time.Duration) error
}

// realRetryClock is the production implementation: a `time.After`-driven
// wait that selects on ctx.Done() so the helper never blocks past a
// cancelled context.
type realRetryClock struct{}

func (realRetryClock) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PostResponseWithRetry posts `resp` to `replyToHref` via `client`,
// retrying on transient failures (`errors.Is(err, ErrResponseTransient)`)
// up to `cfg.MaxAttempts` times with exponential backoff.
//
// Behavior matrix:
//
//   - Success (nil err from PostResponse) → returns nil immediately.
//   - Non-transient err (4xx, build error, etc.) → returns the err
//     unchanged. NO retry.
//   - Context cancelled / deadline exceeded : either before, during, or
//     between attempts → returns a wrapped ctx error.
//   - Transient err, attempts remaining → sleep the current delay,
//     scale by `BackoffMultiplier`, cap at `MaxDelay`, retry.
//   - Transient err, attempts exhausted → emit a dead-letter log line
//     (`response POST DEAD-LETTER: event=<mRID> status=<status> after N
//     attempts: <err>`) and return an error wrapping the final transient
//     failure so callers preserve `errors.Is(err, ErrResponseTransient)`.
//
// The helper does NOT mutate `resp`; the same payload (including its
// idempotent Href hint, see deriveResponseHref) is replayed on
// every attempt so a server that honors the suggested href dedupes
// across retries server-side.
func PostResponseWithRetry(
	ctx context.Context,
	client ResponsePoster,
	replyToHref string,
	resp sep2.DERControlResponse,
	cfg ResponseRetryConfig,
) error {
	return postResponseWithRetryClock(ctx, client, replyToHref, resp, cfg, realRetryClock{})
}

// postResponseWithRetryClock is the testable seam: same as
// `PostResponseWithRetry` but with an injectable clock so tests can run
// the retry schedule without burning wall-clock seconds.
func postResponseWithRetryClock(
	ctx context.Context,
	client ResponsePoster,
	replyToHref string,
	resp sep2.DERControlResponse,
	cfg ResponseRetryConfig,
	clock retryClock,
) error {
	cfg = cfg.withDefaults()

	// Pre-check: a context cancelled before the first attempt should not
	// trigger an attempt. Saves one wasted POST against a server the
	// caller has already abandoned.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("post response: %w", err)
	}

	delay := cfg.InitialDelay
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		err := client.PostResponse(ctx, replyToHref, resp)
		if err == nil {
			return nil
		}
		lastErr = err

		// Context errors always abort : never retry, never sentinel-wrap.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("post response: %w", err)
		}
		// Non-transient errors (4xx, build errors, anything that does not
		// wrap ErrResponseTransient) are terminal. Return unchanged so
		// callers see the original wrap chain.
		if !errors.Is(err, ErrResponseTransient) {
			return err
		}
		// Transient : retry unless we're out of attempts.
		if attempt == cfg.MaxAttempts {
			break
		}
		if waitErr := clock.wait(ctx, delay); waitErr != nil {
			// Context cancelled during the backoff wait. Surface the
			// ctx error wrapped : same contract as the per-attempt
			// cancellation branch above.
			return fmt.Errorf("post response: %w", waitErr)
		}
		// Advance the schedule for the NEXT wait. Cap at MaxDelay.
		delay = time.Duration(float64(delay) * cfg.BackoffMultiplier)
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
	}

	// All attempts failed transiently. Emit the dead-letter line and
	// return a wrapped error that still satisfies
	// `errors.Is(err, ErrResponseTransient)` so upstream callers can
	// pattern-match the terminal state.
	eventMRID, statusVal := identifyResponse(resp)
	log.Printf("response POST DEAD-LETTER: event=%q status=%d after %d attempts: %v",
		eventMRID, statusVal, cfg.MaxAttempts, lastErr)
	return fmt.Errorf("post response failed after %d attempts: %w", cfg.MaxAttempts, lastErr)
}

// identifyResponse extracts the event mRID + status from a
// DERControlResponse for the dead-letter log line. Returns zero values
// for missing fields so logging never panics on a partially populated
// struct.
func identifyResponse(resp sep2.DERControlResponse) (mrid string, status uint8) {
	mrid = resp.Subject
	if resp.Status != nil {
		status = *resp.Status
	}
	return mrid, status
}
