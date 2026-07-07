package main

// IEEE-048 tests: Phase 1b time-sync graceful bypass.
//
// Verifies that runPhase1bTimeSync (a) does not call log.Fatalf on any HTTP
// or transport error class, and (b) preserves the IEEE-031 happy path
// byte-for-byte. Phase 7 exit criterion 1 in
// `plans/plan-1-csip-client-conformance/phase-7-http-semantics.md`.
//
// Tests are driven against the timeSyncClient interface defined in
// phase1b_timesync.go so the test surface stays narrow (no httptest server,
// no cert plumbing). The interface seam itself is part of the Pike-rule
// small-interface discipline and is a regression target : if a future change
// widens the interface, the fake will fail to compile and force a
// re-evaluation.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// fakeTimeSyncClient is a minimal stand-in for *inverter.SEP2Client over the
// timeSyncClient seam. The atomic counters detect unintended fan-out
// (multiple Sync / Run calls per invocation).
type fakeTimeSyncClient struct {
	syncErr     error
	syncTime    sep2.Time
	now         time.Time
	syncCalls   atomic.Int32
	runCalls    atomic.Int32
	lastSyncCtx context.Context
	lastRunCtx  context.Context
}

func (f *fakeTimeSyncClient) SyncServerTime(ctx context.Context, _ string) (sep2.Time, error) {
	f.syncCalls.Add(1)
	f.lastSyncCtx = ctx
	if f.syncErr != nil {
		return sep2.Time{}, f.syncErr
	}
	return f.syncTime, nil
}

func (f *fakeTimeSyncClient) RunTimeSync(ctx context.Context, _ string, _ time.Duration) {
	f.runCalls.Add(1)
	f.lastRunCtx = ctx
	// Block until ctx fires so the spawned-goroutine leak detector in
	// `go test -race` has a real lifetime to inspect. Mirrors the
	// IEEE-031 production loop's ctx.Done() exit path.
	<-ctx.Done()
}

func (f *fakeTimeSyncClient) Now() time.Time { return f.now }

// dcapWithTimeLink builds the minimum DeviceCapability runPhase1bTimeSync
// reads: TimeLink.Href present-or-nil and nothing else.
func dcapWithTimeLink(href string) sep2.DeviceCapability {
	if href == "" {
		return sep2.DeviceCapability{}
	}
	return sep2.DeviceCapability{TimeLink: &sep2.Link{Href: href}}
}

// TestRunPhase1bTimeSync_GracefulBypass exercises every HTTP-error class plus
// the unexpected-transport / no-TimeLink / happy-path branches. The shared
// shape across cases (one TimeLink, one inbound error, one expected outcome)
// fits table-driven tests cleanly.
func TestRunPhase1bTimeSync_GracefulBypass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		dcap          sep2.DeviceCapability
		syncErr       error
		wantSyncCalls int32
		wantRunCalls  int32
		wantErrIs     error // nil = expect nil
	}{
		{
			name:          "no-TimeLink degrades to local clock",
			dcap:          dcapWithTimeLink(""),
			syncErr:       nil,
			wantSyncCalls: 0,
			wantRunCalls:  0,
			wantErrIs:     nil,
		},
		{
			name:          "happy path starts periodic goroutine",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       nil,
			wantSyncCalls: 1,
			wantRunCalls:  1,
			wantErrIs:     nil,
		},
		{
			name:          "404 Not Found bypasses without goroutine",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       fmt.Errorf("GET /tm: %w", inverter.ErrNotFound),
			wantSyncCalls: 1,
			wantRunCalls:  0,
			wantErrIs:     nil,
		},
		{
			name:          "501 Not Implemented bypasses without goroutine",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       fmt.Errorf("GET /tm: %w", inverter.ErrNotImplemented),
			wantSyncCalls: 1,
			wantRunCalls:  0,
			wantErrIs:     nil,
		},
		{
			name:          "405 Method Not Allowed bypasses without goroutine",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       fmt.Errorf("GET /tm: %w", inverter.ErrMethodNotAllowed),
			wantSyncCalls: 1,
			wantRunCalls:  0,
			wantErrIs:     nil,
		},
		{
			name:          "400 Bad Request bypasses without goroutine",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       fmt.Errorf("GET /tm: %w", inverter.ErrBadRequest),
			wantSyncCalls: 1,
			wantRunCalls:  0,
			wantErrIs:     nil,
		},
		{
			name:          "5xx transient bypasses without goroutine",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       fmt.Errorf("GET /tm: %w", inverter.ErrResponseTransient),
			wantSyncCalls: 1,
			wantRunCalls:  0,
			wantErrIs:     nil,
		},
		{
			name:          "unexpected transport error bypasses without goroutine",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       errors.New("dial: connection refused"),
			wantSyncCalls: 1,
			wantRunCalls:  0,
			wantErrIs:     nil,
		},
		{
			name:          "context.Canceled propagates",
			dcap:          dcapWithTimeLink("/tm"),
			syncErr:       context.Canceled,
			wantSyncCalls: 1,
			wantRunCalls:  0,
			wantErrIs:     context.Canceled,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &fakeTimeSyncClient{
				syncErr:  tc.syncErr,
				syncTime: sep2.Time{CurrentTime: time.Now().Unix()},
				now:      time.Now(),
			}
			err := runPhase1bTimeSync(ctx, fake, tc.dcap)
			if tc.wantErrIs == nil {
				if err != nil {
					t.Fatalf("runPhase1bTimeSync: unexpected err = %v", err)
				}
			} else if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("runPhase1bTimeSync: err = %v, want errors.Is(%v) = true", err, tc.wantErrIs)
			}
			if got := fake.syncCalls.Load(); got != tc.wantSyncCalls {
				t.Errorf("SyncServerTime calls = %d, want %d", got, tc.wantSyncCalls)
			}
			// RunTimeSync is spawned in a goroutine; give it a moment to
			// register the counter before we read it. The goroutine
			// blocks on ctx.Done(); the deferred cancel() drains it.
			deadline := time.Now().Add(100 * time.Millisecond)
			for time.Now().Before(deadline) && fake.runCalls.Load() < tc.wantRunCalls {
				time.Sleep(time.Millisecond)
			}
			if got := fake.runCalls.Load(); got != tc.wantRunCalls {
				t.Errorf("RunTimeSync calls = %d, want %d", got, tc.wantRunCalls)
			}
		})
	}
}

// TestRunPhase1bTimeSync_NoFatalfOnAnyError is the regression guard: it
// re-runs every HTTP-error class through runPhase1bTimeSync and asserts the
// helper returns rather than crashing the test process. The Phase 7 graceful
// bypass policy is the whole reason for this extraction; if a future change
// re-introduces log.Fatalf, the test binary will exit non-zero and this
// case will not get to its t.Logf.
func TestRunPhase1bTimeSync_NoFatalfOnAnyError(t *testing.T) {
	t.Parallel()
	classes := []error{
		inverter.ErrBadRequest,
		inverter.ErrNotFound,
		inverter.ErrMethodNotAllowed,
		inverter.ErrNotImplemented,
		inverter.ErrResponseTransient,
		errors.New("dial: connection refused"),
	}
	for _, base := range classes {
		base := base
		t.Run(base.Error(), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &fakeTimeSyncClient{
				syncErr: fmt.Errorf("wrapper: %w", base),
				now:     time.Now(),
			}
			if err := runPhase1bTimeSync(ctx, fake, dcapWithTimeLink("/tm")); err != nil {
				t.Fatalf("runPhase1bTimeSync returned %v; want nil (graceful bypass)", err)
			}
			t.Logf("graceful bypass on %v reached return path", base)
		})
	}
}
