package main

// IEEE-048 tests: Phase 3 DER list fetch graceful bypass.
//
// Verifies that fetchDERListForSetup (a) does not call log.Fatalf on any
// HTTP or transport error class, and (b) preserves the IEEE-047 happy path
// byte-for-byte. Phase 7 exit criterion 1 in
// `plans/plan-1-csip-client-conformance/phase-7-http-semantics.md`.
//
// Tests are driven against the derListClient interface defined in
// phase3_derlist.go so the test surface stays narrow.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// fakeDERListClient is a minimal stand-in for *inverter.SEP2Client over the
// derListClient seam. The atomic counter detects unintended fan-out (Phase 3
// fetch should be one-shot — never re-tried on bypass).
type fakeDERListClient struct {
	getErr     error
	getList    sep2.DERList
	getNewHref string
	calls      atomic.Int32
	lastPath   string
}

func (f *fakeDERListClient) Get(_ context.Context, path string, out interface{}) (string, error) {
	f.calls.Add(1)
	f.lastPath = path
	if f.getErr != nil {
		return "", f.getErr
	}
	// Copy the staged DERList into the caller's pointer-to-DERList output.
	if dst, ok := out.(*sep2.DERList); ok {
		*dst = f.getList
	}
	return f.getNewHref, nil
}

// TestFetchDERListForSetup_GracefulBypass exercises every HTTP-error class
// plus the unexpected-transport / happy-path branches.
func TestFetchDERListForSetup_GracefulBypass(t *testing.T) {
	t.Parallel()

	stagedList := sep2.DERList{DER: []sep2.DER{{}}}

	cases := []struct {
		name        string
		getErr      error
		stagedList  sep2.DERList
		stagedHref  string
		wantOK      bool
		wantNewHref string
		wantErrIs   error
		wantDERLen  int
	}{
		{
			name:       "happy path returns list, ok=true",
			getErr:     nil,
			stagedList: stagedList,
			wantOK:     true,
			wantDERLen: 1,
		},
		{
			name:        "happy path with 301 follow surfaces newHref",
			getErr:      nil,
			stagedList:  stagedList,
			stagedHref:  "/v2/der",
			wantOK:      true,
			wantNewHref: "/v2/der",
			wantDERLen:  1,
		},
		{
			name:      "404 Not Found bypasses",
			getErr:    fmt.Errorf("GET /der: %w", inverter.ErrNotFound),
			wantOK:    false,
			wantErrIs: nil,
		},
		{
			name:      "501 Not Implemented bypasses",
			getErr:    fmt.Errorf("GET /der: %w", inverter.ErrNotImplemented),
			wantOK:    false,
			wantErrIs: nil,
		},
		{
			name:      "405 Method Not Allowed bypasses",
			getErr:    fmt.Errorf("GET /der: %w", inverter.ErrMethodNotAllowed),
			wantOK:    false,
			wantErrIs: nil,
		},
		{
			name:      "400 Bad Request bypasses",
			getErr:    fmt.Errorf("GET /der: %w", inverter.ErrBadRequest),
			wantOK:    false,
			wantErrIs: nil,
		},
		{
			name:      "5xx transient bypasses",
			getErr:    fmt.Errorf("GET /der: %w", inverter.ErrResponseTransient),
			wantOK:    false,
			wantErrIs: nil,
		},
		{
			name:      "unexpected transport error bypasses",
			getErr:    errors.New("dial: connection refused"),
			wantOK:    false,
			wantErrIs: nil,
		},
		{
			name:      "context.Canceled propagates",
			getErr:    context.Canceled,
			wantOK:    false,
			wantErrIs: context.Canceled,
		},
		{
			name:      "context.DeadlineExceeded propagates",
			getErr:    context.DeadlineExceeded,
			wantOK:    false,
			wantErrIs: context.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &fakeDERListClient{
				getErr:     tc.getErr,
				getList:    tc.stagedList,
				getNewHref: tc.stagedHref,
			}
			list, ok, newHref, err := fetchDERListForSetup(ctx, fake, "/der")
			if tc.wantErrIs == nil {
				if err != nil {
					t.Fatalf("fetchDERListForSetup: unexpected err = %v", err)
				}
			} else if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("fetchDERListForSetup: err = %v, want errors.Is(%v) = true", err, tc.wantErrIs)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if newHref != tc.wantNewHref {
				t.Errorf("newHref = %q, want %q", newHref, tc.wantNewHref)
			}
			if len(list.DER) != tc.wantDERLen {
				t.Errorf("len(list.DER) = %d, want %d", len(list.DER), tc.wantDERLen)
			}
			if got := fake.calls.Load(); got != 1 {
				t.Errorf("Get called %d times, want exactly 1 (one-shot)", got)
			}
			if fake.lastPath != "/der" {
				t.Errorf("Get called with path %q, want %q", fake.lastPath, "/der")
			}
		})
	}
}

// TestFetchDERListForSetup_NoFatalfOnAnyError is the regression guard: every
// HTTP-error class through fetchDERListForSetup must return rather than
// crash the test process. If a future change re-introduces log.Fatalf here,
// the test binary exits non-zero before this case logs.
func TestFetchDERListForSetup_NoFatalfOnAnyError(t *testing.T) {
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
			fake := &fakeDERListClient{getErr: fmt.Errorf("wrapper: %w", base)}
			_, ok, _, err := fetchDERListForSetup(ctx, fake, "/der")
			if err != nil {
				t.Fatalf("fetchDERListForSetup returned err %v; want nil (graceful bypass)", err)
			}
			if ok {
				t.Fatalf("fetchDERListForSetup ok=true on %v; want false (bypass)", base)
			}
			t.Logf("graceful bypass on %v reached return path", base)
		})
	}
}
