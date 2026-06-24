// Tests for IEEE-044 : state-machine → Response POST hook
// (Phase 6 ticket 2 of 3).
//
// The hook produced by responsePOSTHook is the leaf consumer that turns
// every state-machine transition into a PostResponse call, filtered by:
//
//   - evt != nil       (auto-revert edges carry nil)
//   - evt.ReplyTo != ""
//   - mapTransitionToStatus(prev, next) != 0
//   - evt.ResponseRequired != nil && responseRequiredOn(mask, status)
//
// Coverage (one assertion concept per test, table-driven where the input
// space is enumerable):
//
//   - mapTransitionToStatus table (5 documented edges + the silent edges)
//   - responseRequiredOn bitmask (0x00, 0x01, 0x07, nil-mask handled)
//   - deriveResponseHref determinism (1000 calls identical)
//   - End-to-end happy path: DEFAULT→RECEIVED→STARTED→COMPLETED→DEFAULT
//     with responseRequired=0x07 produces exactly 3 POSTs
//   - Cancellation: DEFAULT→RECEIVED→CANCELLED→DEFAULT with mask=0x27
//     produces 2 POSTs (status 1 + status 6)
//   - Empty ReplyTo → 0 POSTs
//   - nil ResponseRequired → 0 POSTs
//   - POST 500 → log, next transition fires
//
// Tests use a hand-rolled `fakePoster` to record PostResponse calls
// directly at the consumer-side interface boundary. The full SEP2Client
// HTTP/TLS path is covered by IEEE-043's response_post_test.go in
// internal/inverter : this file's scope ends at the hook contract.

package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"
)

// fixedTestNow is the canonical clock for tests that don't need to
// advance. UTC for determinism across machines.
var fixedTestNow = time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

// fixedNowFn returns a nowFunc closed over fixedTestNow.
func fixedNowFn() time.Time { return fixedTestNow }

// =============================================================================
// (b) mapTransitionToStatus : table of all documented edges.
// =============================================================================

func TestMapTransitionToStatus_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		prev inverter.EventState
		next inverter.EventState
		want uint8
	}{
		{
			name: "default to received emits ResponseStatusEventReceived",
			prev: inverter.StateDefault,
			next: inverter.StateEventReceived,
			want: sep2.ResponseStatusEventReceived,
		},
		{
			name: "received to started emits ResponseStatusEventStarted",
			prev: inverter.StateEventReceived,
			next: inverter.StateEventStarted,
			want: sep2.ResponseStatusEventStarted,
		},
		{
			name: "started to completed emits ResponseStatusEventCompleted",
			prev: inverter.StateEventStarted,
			next: inverter.StateEventCompleted,
			want: sep2.ResponseStatusEventCompleted,
		},
		{
			name: "received to cancelled emits ResponseStatusEventCancelled",
			prev: inverter.StateEventReceived,
			next: inverter.StateEventCancelled,
			want: sep2.ResponseStatusEventCancelled,
		},
		{
			name: "started to cancelled emits ResponseStatusEventCancelled",
			prev: inverter.StateEventStarted,
			next: inverter.StateEventCancelled,
			want: sep2.ResponseStatusEventCancelled,
		},
		{
			// Auto-revert from terminal to DEFAULT carries no wire status :
			// the acknowledgement fired one transition earlier on the
			// COMPLETED / CANCELLED record itself.
			name: "completed to default returns 0 (silent edge)",
			prev: inverter.StateEventCompleted,
			next: inverter.StateDefault,
			want: 0,
		},
		{
			name: "cancelled to default returns 0 (silent edge)",
			prev: inverter.StateEventCancelled,
			next: inverter.StateDefault,
			want: 0,
		},
		{
			// Out-of-band edges the state machine never produces but the
			// function must not crash on.
			name: "default to default returns 0",
			prev: inverter.StateDefault,
			next: inverter.StateDefault,
			want: 0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapTransitionToStatus(tc.prev, tc.next)
			if got != tc.want {
				t.Errorf("mapTransitionToStatus(%s, %s) = %d, want %d", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// =============================================================================
// (c) responseRequiredOn : bitmap selection per Table 32.
// =============================================================================

func TestResponseRequiredOn_Bitmask(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mask   uint8
		status uint8
		want   bool
	}{
		{
			name:   "0x00 mask selects nothing for status 1",
			mask:   0x00,
			status: sep2.ResponseStatusEventReceived,
			want:   false,
		},
		{
			name:   "0x00 mask selects nothing for status 2",
			mask:   0x00,
			status: sep2.ResponseStatusEventStarted,
			want:   false,
		},
		{
			name:   "0x00 mask selects nothing for status 3",
			mask:   0x00,
			status: sep2.ResponseStatusEventCompleted,
			want:   false,
		},
		{
			name:   "0x00 mask selects nothing for status 6",
			mask:   0x00,
			status: sep2.ResponseStatusEventCancelled,
			want:   false,
		},
		{
			name:   "0x01 mask selects status 1 (received)",
			mask:   0x01,
			status: sep2.ResponseStatusEventReceived,
			want:   true,
		},
		{
			name:   "0x01 mask does not select status 2 (started)",
			mask:   0x01,
			status: sep2.ResponseStatusEventStarted,
			want:   false,
		},
		{
			name:   "0x07 mask selects status 1 (received)",
			mask:   0x07,
			status: sep2.ResponseStatusEventReceived,
			want:   true,
		},
		{
			name:   "0x07 mask selects status 2 (started)",
			mask:   0x07,
			status: sep2.ResponseStatusEventStarted,
			want:   true,
		},
		{
			name:   "0x07 mask selects status 3 (completed)",
			mask:   0x07,
			status: sep2.ResponseStatusEventCompleted,
			want:   true,
		},
		{
			name:   "0x07 mask does not select status 6 (cancelled : bit 5)",
			mask:   0x07,
			status: sep2.ResponseStatusEventCancelled,
			want:   false,
		},
		{
			name:   "0x20 mask selects status 6 (cancelled)",
			mask:   0x20,
			status: sep2.ResponseStatusEventCancelled,
			want:   true,
		},
		{
			name:   "0x27 mask selects status 1 and status 6",
			mask:   0x27,
			status: sep2.ResponseStatusEventCancelled,
			want:   true,
		},
		{
			// Sentinel 0 ("no wire status") must never be selected,
			// regardless of mask.
			name:   "0xff mask does not select status 0 sentinel",
			mask:   0xff,
			status: 0,
			want:   false,
		},
		{
			// Out-of-IEEE-044-scope statuses (4/5/7+) are not emitted by
			// this hook and must read as false even if the bitmap would
			// select them in principle.
			name:   "0xff mask does not select status 4 (opt-out, out of scope)",
			mask:   0xff,
			status: sep2.ResponseStatusOptOut,
			want:   false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := responseRequiredOn(tc.mask, tc.status)
			if got != tc.want {
				t.Errorf("responseRequiredOn(%#x, %d) = %v, want %v", tc.mask, tc.status, got, tc.want)
			}
		})
	}
}

// =============================================================================
// (d) deriveResponseHref : determinism.
// =============================================================================

func TestDeriveResponseHref_Deterministic(t *testing.T) {
	t.Parallel()
	const mrid = "EVT-DET-0001"
	const status = sep2.ResponseStatusEventStarted
	const iterations = 1000

	want := deriveResponseHref(mrid, status)
	if want == "" {
		t.Fatalf("deriveResponseHref returned empty string for valid input")
	}
	if !strings.HasPrefix(want, "/response/") {
		t.Errorf("deriveResponseHref = %q, want /response/ prefix", want)
	}

	for i := 0; i < iterations; i++ {
		got := deriveResponseHref(mrid, status)
		if got != want {
			t.Fatalf("deriveResponseHref iteration %d = %q, want %q (non-deterministic)", i, got, want)
		}
	}
}

// TestDeriveResponseHref_StatusSensitive confirms different statuses for
// the same mRID derive distinct hrefs : protects the server-side router
// from collisions when an event walks through Received → Started →
// Completed.
func TestDeriveResponseHref_StatusSensitive(t *testing.T) {
	t.Parallel()
	const mrid = "EVT-SENS-0001"
	hrefs := map[string]uint8{}
	for _, s := range []uint8{
		sep2.ResponseStatusEventReceived,
		sep2.ResponseStatusEventStarted,
		sep2.ResponseStatusEventCompleted,
		sep2.ResponseStatusEventCancelled,
	} {
		h := deriveResponseHref(mrid, s)
		if existing, dup := hrefs[h]; dup {
			t.Errorf("href collision: status %d and status %d both produced %q", existing, s, h)
		}
		hrefs[h] = s
	}
}

// =============================================================================
// End-to-end test scaffolding: fakePoster + newTestStateMachine.
// =============================================================================

// recordedPost captures one observed PostResponse invocation.
type recordedPost struct {
	ReplyTo string
	Resp    sep2.DERControlResponse
}

// fakePoster implements responsePoster. It records every PostResponse
// call and can be programmed to return errors on specific calls. The
// 1-method consumer-side interface (responsePoster, defined in
// response_hook.go) makes the fake trivial.
type fakePoster struct {
	mu       sync.Mutex
	calls    []recordedPost
	errsLeft []error // pop from front; nil entry = success
}

func (f *fakePoster) PostResponse(ctx context.Context, replyToHref string, resp sep2.DERControlResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedPost{ReplyTo: replyToHref, Resp: resp.Copy()})
	if len(f.errsLeft) == 0 {
		return nil
	}
	err := f.errsLeft[0]
	f.errsLeft = f.errsLeft[1:]
	return err
}

func (f *fakePoster) Calls() []recordedPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedPost, len(f.calls))
	for i, c := range f.calls {
		out[i] = recordedPost{ReplyTo: c.ReplyTo, Resp: c.Resp.Copy()}
	}
	return out
}

// buildControl returns a DERControl with the given mRID, replyTo,
// responseRequired (nil-able), startOffset (added to fixedTestNow), and
// duration.
func buildControl(mrid, replyTo string, mask *uint8, startOffset time.Duration, durationSec uint32) sep2.DERControl {
	dc := sep2.DERControl{}
	dc.MRID = mrid
	dc.ReplyTo = replyTo
	dc.ResponseRequired = mask
	dc.Interval = &sep2.DateTimeInterval{
		Start:    fixedTestNow.Add(startOffset).Unix(),
		Duration: durationSec,
	}
	return dc
}

// newTestStateMachine returns a state machine + scheduler driven by a
// caller-controlled clock. Uses the public Scheduler constructor so it
// works from the cmd/inverterclient test package (the scheduler's
// _test.go-only helpers live in internal/inverter and aren't visible
// here).
func newTestStateMachine(now func() time.Time) (*inverter.StateMachine, *inverter.Scheduler) {
	sched := inverter.NewScheduler(now, rand.New(rand.NewPCG(1, 2)))
	sm := inverter.NewStateMachine()
	return sm, sched
}

// =============================================================================
// (e) end-to-end happy: full lifecycle, mask=0x07, 3 POSTs.
// =============================================================================

func TestResponsePOSTHook_FullLifecycleEmitsThreePOSTs(t *testing.T) {
	t.Parallel()
	const replyTo = "https://server.example/rsps"
	const lfdi = "0011223344556677889900aabbccddeeff001122"
	poster := &fakePoster{}

	mask := uint8(0x07) // bits 0,1,2 → received, started, completed.
	dc := buildControl("EVT-E2E-A", replyTo, &mask, 1*time.Minute, 60)

	tnow := fixedTestNow
	clock := func() time.Time { return tnow }
	sm, sched := newTestStateMachine(clock)
	sm.AddTransitionHook(responsePOSTHook(poster, lfdi, fixedNowFn))

	// Tick 1: add → DEFAULT→RECEIVED (status 1).
	sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	// Tick 2: clock past start → RECEIVED→STARTED (status 2).
	tnow = fixedTestNow.Add(90 * time.Second)
	sm.Tick(clock(), nil, nil, sched)
	// Tick 3: clock past expire → STARTED→COMPLETED→DEFAULT
	// (status 3; DEFAULT auto-revert is silent).
	tnow = fixedTestNow.Add(5 * time.Minute)
	sm.Tick(clock(), nil, nil, sched)

	calls := poster.Calls()
	if len(calls) != 3 {
		t.Fatalf("PostResponse call count = %d, want 3 (full lifecycle with mask=0x07); calls=%+v", len(calls), calls)
	}

	wantStatuses := []uint8{
		sep2.ResponseStatusEventReceived,
		sep2.ResponseStatusEventStarted,
		sep2.ResponseStatusEventCompleted,
	}
	for i, c := range calls {
		if c.ReplyTo != replyTo {
			t.Errorf("calls[%d].ReplyTo = %q, want %q", i, c.ReplyTo, replyTo)
		}
		if c.Resp.Status == nil {
			t.Errorf("calls[%d].Resp.Status = nil, want %d", i, wantStatuses[i])
			continue
		}
		if *c.Resp.Status != wantStatuses[i] {
			t.Errorf("calls[%d].Resp.Status = %d, want %d", i, *c.Resp.Status, wantStatuses[i])
		}
		if c.Resp.Subject != "EVT-E2E-A" {
			t.Errorf("calls[%d].Resp.Subject = %q, want %q", i, c.Resp.Subject, "EVT-E2E-A")
		}
		if c.Resp.EndDeviceLFDI != lfdi {
			t.Errorf("calls[%d].Resp.EndDeviceLFDI = %q, want %q", i, c.Resp.EndDeviceLFDI, lfdi)
		}
		if c.Resp.CreatedDateTime != fixedTestNow.Unix() {
			t.Errorf("calls[%d].Resp.CreatedDateTime = %d, want %d", i, c.Resp.CreatedDateTime, fixedTestNow.Unix())
		}
	}
}

// =============================================================================
// (f) cancellation: RECEIVED→CANCELLED→DEFAULT with mask=0x27, 2 POSTs.
// =============================================================================

func TestResponsePOSTHook_CancellationEmitsTwoPOSTs(t *testing.T) {
	t.Parallel()
	const replyTo = "https://server.example/rsps"
	const lfdi = "lfdi-cancel-test"
	poster := &fakePoster{}

	mask := uint8(0x27) // received + started + completed + cancelled.
	dc := buildControl("EVT-E2E-B", replyTo, &mask, 5*time.Minute, 60)

	tnow := fixedTestNow
	clock := func() time.Time { return tnow }
	sm, sched := newTestStateMachine(clock)
	sm.AddTransitionHook(responsePOSTHook(poster, lfdi, fixedNowFn))

	// Tick 1: add → DEFAULT→RECEIVED (status 1, mask bit 0).
	sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	// Tick 2: cancel before fire → RECEIVED→CANCELLED→DEFAULT
	// (status 6, mask bit 5; DEFAULT auto-revert silent).
	cancelled := dc.Copy()
	cancelled.EventStatus = &sep2.EventStatus{CurrentStatus: sep2.EventStatusCancelled}
	sm.Tick(clock(), nil, []sep2.DERControl{cancelled}, sched)

	calls := poster.Calls()
	if len(calls) != 2 {
		t.Fatalf("PostResponse call count = %d, want 2 (received + cancelled); calls=%+v", len(calls), calls)
	}

	wantStatuses := []uint8{
		sep2.ResponseStatusEventReceived,
		sep2.ResponseStatusEventCancelled,
	}
	for i, c := range calls {
		if c.Resp.Status == nil || *c.Resp.Status != wantStatuses[i] {
			gotS := uint8(0)
			if c.Resp.Status != nil {
				gotS = *c.Resp.Status
			}
			t.Errorf("calls[%d].Resp.Status = %d, want %d", i, gotS, wantStatuses[i])
		}
	}
}

// =============================================================================
// (g) empty ReplyTo → 0 POSTs.
// =============================================================================

func TestResponsePOSTHook_EmptyReplyToEmitsZeroPOSTs(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{}

	mask := uint8(0x07)
	dc := buildControl("EVT-NO-REPLYTO", "", &mask, 1*time.Minute, 60) // empty ReplyTo

	tnow := fixedTestNow
	clock := func() time.Time { return tnow }
	sm, sched := newTestStateMachine(clock)
	sm.AddTransitionHook(responsePOSTHook(poster, "lfdi-empty", fixedNowFn))

	sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	tnow = fixedTestNow.Add(5 * time.Minute)
	sm.Tick(clock(), nil, nil, sched)

	if calls := poster.Calls(); len(calls) != 0 {
		t.Errorf("PostResponse call count = %d, want 0 (empty ReplyTo); calls=%+v", len(calls), calls)
	}
}

// =============================================================================
// (h) nil ResponseRequired → 0 POSTs.
// =============================================================================

func TestResponsePOSTHook_NilResponseRequiredEmitsZeroPOSTs(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{}

	dc := buildControl("EVT-NIL-MASK", "https://server.example/rsps", nil, 1*time.Minute, 60) // nil mask

	tnow := fixedTestNow
	clock := func() time.Time { return tnow }
	sm, sched := newTestStateMachine(clock)
	sm.AddTransitionHook(responsePOSTHook(poster, "lfdi-nil", fixedNowFn))

	sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	tnow = fixedTestNow.Add(5 * time.Minute)
	sm.Tick(clock(), nil, nil, sched)

	if calls := poster.Calls(); len(calls) != 0 {
		t.Errorf("PostResponse call count = %d, want 0 (nil ResponseRequired); calls=%+v", len(calls), calls)
	}
}

// =============================================================================
// (i) POST error on first transition → log, next transition still fires.
// =============================================================================

func TestResponsePOSTHook_PostErrorDoesNotAbortNextTransition(t *testing.T) {
	t.Parallel()
	// First call returns a transient-wrapped error mirroring what
	// PostResponse would surface on a persistent 5xx. With IEEE-045's
	// PostResponseWithRetry layered in, the hook will retry : so the
	// first transition takes 2 PostResponse calls (attempt 1: transient,
	// attempt 2: success). The hook must still log + move on with no
	// panic / no state-machine wedge.
	transient := fmt.Errorf("simulated 500: %w", inverter.ErrResponseTransient)
	poster := &fakePoster{errsLeft: []error{transient}}

	mask := uint8(0x07)
	dc := buildControl("EVT-500-RECOVER", "https://server.example/rsps", &mask, 1*time.Minute, 60)

	tnow := fixedTestNow
	clock := func() time.Time { return tnow }
	sm, sched := newTestStateMachine(clock)
	// Tight retry cfg so the test does not burn the IEEE-045 default
	// 500ms initial backoff per recovered failure.
	tightRetry := inverter.ResponseRetryConfig{
		MaxAttempts:       3,
		InitialDelay:      1 * time.Millisecond,
		MaxDelay:          5 * time.Millisecond,
		BackoffMultiplier: 2.0,
	}
	sm.AddTransitionHook(responsePOSTHook(poster, "lfdi-500", fixedNowFn, tightRetry))

	// Transition 1: DEFAULT→RECEIVED : attempt 1 transient, attempt 2 OK.
	sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	if got := len(poster.Calls()); got != 2 {
		t.Errorf("after RECEIVED transition: calls = %d, want 2 (one transient retry, then success)", got)
	}

	// Transition 2: RECEIVED→STARTED : hook MUST still fire.
	tnow = fixedTestNow.Add(90 * time.Second)
	sm.Tick(clock(), nil, nil, sched)
	if got := len(poster.Calls()); got != 3 {
		t.Errorf("after STARTED transition: calls = %d, want 3 (next transition must fire)", got)
	}

	// Transition 3: STARTED→COMPLETED + silent revert.
	tnow = fixedTestNow.Add(5 * time.Minute)
	sm.Tick(clock(), nil, nil, sched)
	if got := len(poster.Calls()); got != 4 {
		t.Errorf("after COMPLETED transition: calls = %d, want 4", got)
	}
}

// =============================================================================
// IEEE-045 integration: state-machine transition → retry wrapper →
// dead-letter log when all attempts fail.
// =============================================================================

// TestResponsePOSTHook_IEEE045DeadLetterOnPersistentTransient is the
// Phase 6 end-to-end smoke test that ties IEEE-044 (hook wiring) and
// IEEE-045 (retry + dead-letter) together. A persistent 5xx response
// at the IEEE-040 state machine's DEFAULT→RECEIVED edge must:
//
//  1. Drive cfg.MaxAttempts PostResponse calls (retry schedule consumed).
//  2. Emit a dead-letter log line carrying the event mRID and Table 31
//     status so an operator can audit the dropped Response.
//  3. NOT wedge the state machine : the next transition
//     (RECEIVED→STARTED) must still fire its own Response POST.
//
// Pike rules satisfied: errors are values (dead-letter line preserves
// %w chain to ErrResponseTransient), no goroutine leak (the test runs
// synchronously on a fake retry clock effectively : InitialDelay is
// 1ms via the tight cfg).
func TestResponsePOSTHook_IEEE045DeadLetterOnPersistentTransient(t *testing.T) {
	// IEEE-081: deliberately serial : captureLog (and the inline
	// SetOutput pattern below) swaps log.Default()'s writer, which is
	// process-global. Any t.Parallel() sibling test that emits log
	// lines via production code while we hold the capture would race
	// on the buffer. Go's test runtime runs serial tests before
	// resuming parallel tests, so this captures cleanly.
	transient := fmt.Errorf("simulated 500: %w", inverter.ErrResponseTransient)
	// Queue MaxAttempts transients so attempt 1 + 2 + 3 all fail on the
	// first transition. Anything after that returns nil → the second
	// transition succeeds in one shot.
	poster := &fakePoster{errsLeft: []error{transient, transient, transient}}

	mask := uint8(0x07)
	dc := buildControl("EVT-DL-001", "https://server.example/rsps", &mask, 1*time.Minute, 60)

	tnow := fixedTestNow
	clock := func() time.Time { return tnow }
	sm, sched := newTestStateMachine(clock)
	tightRetry := inverter.ResponseRetryConfig{
		MaxAttempts:       3,
		InitialDelay:      1 * time.Millisecond,
		MaxDelay:          5 * time.Millisecond,
		BackoffMultiplier: 2.0,
	}
	sm.AddTransitionHook(responsePOSTHook(poster, "lfdi-dl", fixedNowFn, tightRetry))

	// Capture log output so the dead-letter line can be asserted.
	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	// Transition 1: DEFAULT→RECEIVED : 3 PostResponse attempts, all fail
	// transiently → dead-letter log emitted.
	sm.Tick(clock(), []sep2.DERControl{dc}, nil, sched)
	if got := len(poster.Calls()); got != tightRetry.MaxAttempts {
		t.Errorf("after RECEIVED transition: calls = %d, want %d (all retry attempts exhausted)", got, tightRetry.MaxAttempts)
	}

	logs := buf.String()
	if !strings.Contains(logs, "DEAD-LETTER") {
		t.Errorf("dead-letter log line missing; got:\n%s", logs)
	}
	if !strings.Contains(logs, "EVT-DL-001") {
		t.Errorf("dead-letter log missing event mRID; got:\n%s", logs)
	}
	if !strings.Contains(logs, fmt.Sprintf("status=%d", sep2.ResponseStatusEventReceived)) {
		t.Errorf("dead-letter log missing status=%d; got:\n%s", sep2.ResponseStatusEventReceived, logs)
	}

	// Transition 2: RECEIVED→STARTED : no queued errs left → succeeds
	// on the first attempt. Proves the state machine kept advancing.
	tnow = fixedTestNow.Add(90 * time.Second)
	sm.Tick(clock(), nil, nil, sched)
	if got := len(poster.Calls()); got != tightRetry.MaxAttempts+1 {
		t.Errorf("after STARTED transition: calls = %d, want %d (state machine must keep advancing)",
			got, tightRetry.MaxAttempts+1)
	}
}

// =============================================================================
// Compile-time guards.
// =============================================================================

// fakePoster must satisfy the consumer-side interface that responsePOSTHook
// depends on. If responsePOSTHook ever widens that interface, this guard
// forces an update.
var _ responsePoster = (*fakePoster)(nil)

// *SEP2Client must also satisfy responsePoster so the production wiring
// in main.go continues to work.
var _ responsePoster = (*inverter.SEP2Client)(nil)
