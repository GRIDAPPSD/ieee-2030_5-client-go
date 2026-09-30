package gridlabd

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dialFake(t *testing.T, configure func(*fakeProcess)) (*Client, *fakeProcess) {
	t.Helper()
	return dialFakeWithTimeout(t, configure, 2*time.Second)
}

func dialFakeWithTimeout(t *testing.T, configure func(*fakeProcess), clientTimeout time.Duration) (*Client, *fakeProcess) {
	t.Helper()
	reg := &fakeRegistry{}
	launch := newFakeLauncher(t, configure, reg)
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	proc, err := launch(context.Background(), []string{"--socket", sockPath, "--fleet-file", "unused.json"})
	if err != nil {
		t.Fatalf("launch fake: %v", err)
	}
	t.Cleanup(func() { _ = proc.Kill() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := waitForSocket(ctx, sockPath, proc, clientTimeout)
	if err != nil {
		t.Fatalf("dial fake: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, reg.list()[0]
}

// TestDial_RefusesNonPositiveTimeout is item 3: timeout must be positive,
// since it is also the only bound on how long Close can wait behind a
// drain; a zero or negative value would leave that wait unbounded.
func TestDial_RefusesNonPositiveTimeout(t *testing.T) {
	// A live listener, so the dial itself would succeed: only the timeout
	// guard can produce the error. Against a nonexistent path the dial
	// fails anyway and the guard is unpinned (round 6 LOW).
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	for _, timeout := range []time.Duration{0, -1 * time.Second} {
		c, err := Dial(context.Background(), sockPath, timeout)
		if err == nil {
			_ = c.Close()
			t.Errorf("Dial(timeout=%v): want error, got nil", timeout)
			continue
		}
		if !strings.Contains(err.Error(), "timeout must be positive") {
			t.Errorf("Dial(timeout=%v): err = %v, want the positive-timeout refusal", timeout, err)
		}
	}
	c, err := Dial(context.Background(), sockPath, time.Nanosecond)
	if err != nil {
		t.Fatalf("Dial(timeout=1ns) against a live listener: %v (the guard must be strictly <= 0)", err)
	}
	_ = c.Close()
}

func TestClient_Hello(t *testing.T) {
	client, _ := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if hello.Protocol != ProtocolVersion {
		t.Errorf("Protocol = %d, want %d", hello.Protocol, ProtocolVersion)
	}
}

func TestClient_Hello_RemoteError(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) {
		fp.helloErr = &wireErrorBody{Code: "gridlabd_error", Message: "version mismatch"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Hello(ctx)
	if err == nil {
		t.Fatal("Hello: want error, got nil")
	}
	remoteErr, ok := err.(*RemoteError)
	if !ok {
		t.Fatalf("Hello error type = %T, want *RemoteError", err)
	}
	if remoteErr.Code != "gridlabd_error" {
		t.Errorf("Code = %q, want gridlabd_error", remoteErr.Code)
	}
}

// TestClient_Set_Get_ReadsBackAppliedValue proves the set-then-get round
// trip: the value read back is the value that was actually applied on the
// far side of the protocol, not merely an echo the client itself invented.
func TestClient_Set_Get_ReadsBackAppliedValue(t *testing.T) {
	client, _ := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p2500, err := FloatValue(2500)
	if err != nil {
		t.Fatalf("FloatValue: %v", err)
	}
	setResults, err := client.Set(ctx, []SetItem{
		{Object: "inv0", Property: "P_Out", Value: p2500},
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := setResults[0].Value.Float64()
	if err != nil {
		t.Fatalf("Set result Float64: %v", err)
	}
	if got != 2500 {
		t.Errorf("Set applied value = %v, want 2500", got)
	}

	getResults, err := client.Get(ctx, []GetItem{{Object: "inv0", Property: "P_Out"}})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err = getResults[0].Value.Float64()
	if err != nil {
		t.Fatalf("Get result Float64: %v", err)
	}
	if got != 2500 {
		t.Errorf("Get after Set = %v, want 2500 (the value Set actually applied)", got)
	}
}

func TestClient_StepTo_AdvancesAndIsIdempotent(t *testing.T) {
	client, _ := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	target := time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC)
	reached, err := client.StepTo(ctx, target)
	if err != nil {
		t.Fatalf("StepTo: %v", err)
	}
	if !reached.Equal(target) {
		t.Errorf("StepTo reached = %v, want %v", reached, target)
	}

	// A step_to at or before the model's current time is a no-op: it must
	// return the current time, not move backwards or error.
	earlier := target.Add(-30 * time.Second)
	reached2, err := client.StepTo(ctx, earlier)
	if err != nil {
		t.Fatalf("StepTo (earlier): %v", err)
	}
	if !reached2.Equal(target) {
		t.Errorf("StepTo(earlier) = %v, want unchanged %v", reached2, target)
	}
}

func TestClient_Shutdown_ClosesConnection(t *testing.T) {
	client, fp := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-fp.Wait():
	case <-time.After(2 * time.Second):
		t.Fatal("fake process did not exit after Shutdown")
	}
}

// TestClient_Call_BoundedEvenWithNoDeadlineCtx proves the client-timeout
// fallback: a sidecar that never answers, called with a bare
// context.Background() (no deadline of its own), must still give up
// within roughly the client's configured timeout rather than blocking
// forever. Then proves the second half of item 1: a call issued after a
// timed-out one fails immediately with ErrConnectionBroken, never reading
// a reply that might still be in flight for the abandoned request.
func TestClient_Call_BoundedEvenWithNoDeadlineCtx(t *testing.T) {
	client, _ := dialFakeWithTimeout(t, func(fp *fakeProcess) { fp.stallOp = "hello" }, 150*time.Millisecond)

	// This call is deliberately given context.Background(): the whole
	// point is that c.timeout alone must bound it. If that regresses,
	// the call blocks forever; helloBounded runs it in a goroutine under
	// an outer 5s guard so the failure is a fast, readable test failure
	// rather than a 10-minute package-wide hang.
	start := time.Now()
	_, err := helloBounded(t, client, 5*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Hello against a sidecar that never answers, with context.Background(): want error, got nil")
	}
	if elapsed > time.Second {
		t.Errorf("Hello against a stalled sidecar took %v to give up, want roughly its 150ms client timeout", elapsed)
	}

	start2 := time.Now()
	_, err2 := helloBounded(t, client, 5*time.Second)
	elapsed2 := time.Since(start2)
	if !errors.Is(err2, ErrConnectionBroken) {
		t.Errorf("Hello after a timed-out call: err = %v, want ErrConnectionBroken", err2)
	}
	if elapsed2 > 50*time.Millisecond {
		t.Errorf("Hello after the connection was already broken took %v, want near-instant (fast path)", elapsed2)
	}
}

// helloBounded calls client.Hello(context.Background()), which is the
// scenario under test (see callers), but fails the test after guard
// instead of hanging the whole package if the timeout mechanism under
// test regresses.
func helloBounded(t *testing.T, client *Client, guard time.Duration) (HelloResult, error) {
	t.Helper()
	type result struct {
		res HelloResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := client.Hello(context.Background())
		done <- result{res, err}
	}()
	select {
	case r := <-done:
		return r.res, r.err
	case <-time.After(guard):
		t.Fatalf("Hello(context.Background()) did not return within %v: the c.timeout fallback has likely regressed", guard)
		return HelloResult{}, nil
	}
}

// TestClient_Call_CtxCancelIsHonoredIndependentlyOfDeadline proves
// ctx.Done() is honored directly, not only a socket deadline: a ctx
// cancelled for a reason other than reaching its own (later) deadline
// still interrupts the call promptly.
func TestClient_Call_CtxCancelIsHonoredIndependentlyOfDeadline(t *testing.T) {
	client, _ := dialFakeWithTimeout(t, func(fp *fakeProcess) { fp.stallOp = "hello" }, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel() // cancelled well before the 30s deadline and the 30s client timeout
	}()

	start := time.Now()
	_, err := client.Hello(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Hello with an early-cancelled ctx: want error, got nil")
	}
	if elapsed > time.Second {
		t.Errorf("Hello took %v to honor an explicit cancel, want near the 50ms cancel delay", elapsed)
	}
}

// TestClient_Call_CancelDetachesInsteadOfBreaking is item 6, design
// Decision 3: a caller ctx cancelled during its own round trip must not
// cost the connection. The reply is only delayed (100ms), not withheld
// (fp.replyDelay, not stallOp), so the drain call hands the lock to
// genuinely succeeds within this test: Broken() must stay unfired, and a
// later call on the SAME connection must still work.
func TestClient_Call_CancelDetachesInsteadOfBreaking(t *testing.T) {
	client, _ := dialFakeWithTimeout(t, func(fp *fakeProcess) { fp.replyDelay = 100 * time.Millisecond }, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get with a cancel mid-round-trip: err = %v, want context.Canceled", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("Get took %v to return after cancel, want near the 20ms cancel delay", elapsed)
	}

	select {
	case <-client.Broken():
		t.Error("Broken() fired after a caller cancel alone: want it to stay open")
	default:
	}

	// The abandoned reply is still in flight (the fake replies ~100ms
	// after the original write); this call must still succeed once drain
	// has read and discarded it, proving the connection stayed usable for
	// the next caller.
	if _, err := client.Hello(context.Background()); err != nil {
		t.Fatalf("Hello after the cancelled call: %v, want nil (the connection must still be usable)", err)
	}
}

// TestClient_Call_QueuedBehindDrainGetsOwnCtxErrorWithoutWriting proves a
// second caller, queued behind a first call's drain (the first caller
// already cancelled and detached), is answered with ITS OWN ctx error
// once it is finally given the lock, and never writes to the wire: the
// existing "ctx already done" fast path in call fires for it exactly as
// it would for any other queued caller, drain or not.
func TestClient_Call_QueuedBehindDrainGetsOwnCtxErrorWithoutWriting(t *testing.T) {
	client, fp := dialFakeWithTimeout(t, func(fp *fakeProcess) { fp.replyDelay = 150 * time.Millisecond }, 5*time.Second)

	// Cancelled once the server has read the request, not after a fixed
	// sleep: on a slow host a fixed 20ms can land before the write, and the
	// call then returns without writing at all.
	firstCtx := cancelOnceReceived(t, fp)
	if _, err := client.Get(firstCtx, []GetItem{{Object: "a", Property: "x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Get: err = %v, want context.Canceled", err)
	}

	// The first call's own write already reached the server (that write
	// is exactly what the drain is now waiting to hear back on); wait for
	// the server side to have actually recorded it before taking the
	// baseline, so the assertion below is not racing the drain's own
	// request against the count snapshot.
	deadline := time.Now().Add(2 * time.Second)
	for fp.reqCount("get") < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	callsBefore := fp.reqCount("get")
	if callsBefore != 1 {
		t.Fatalf("get request count before the second call = %d, want 1 (the first call's own write)", callsBefore)
	}

	// The first call's drain holds the lock until ~150ms from its OWN
	// write; a ctx already expired before this call is even made
	// guarantees the second caller is still queued (or, at best, just
	// acquiring the lock) when its own deadline has already passed.
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer secondCancel()
	<-secondCtx.Done() // wait for it to actually expire, not race it
	_, err := client.Get(secondCtx, []GetItem{{Object: "b", Property: "y"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second Get, queued behind the first's drain: err = %v, want context.DeadlineExceeded", err)
	}
	// Give any write the second call might have started (a regression
	// would launch one asynchronously, in a goroutine this call does not
	// wait on) time to reach the server before checking it never did: the
	// first call's own drain finishes around 150ms after its write, well
	// past this margin, so a real write from the second call would show
	// up here if one happened.
	time.Sleep(250 * time.Millisecond)
	if got := fp.reqCount("get"); got != callsBefore {
		t.Errorf("second Get reached the wire (get request count %d -> %d), want unchanged: an already-expired ctx must be answered without writing", callsBefore, got)
	}
}

// TestClient_Drain_StalledReplyBreaksConnectionWithinCallTimeout is item
// 3's first drain-failure branch: a caller cancels mid-round-trip, and
// the reply it wrote for never arrives at all (coverage MEDIUM at
// 377eb1b: "dropping the break on a stalled reply survives"). The drain
// must still mark the connection broken, bounded by the same conn
// deadline (client's own timeout, set once from the original write) as
// any other call, not left open waiting forever.
func TestClient_Drain_StalledReplyBreaksConnectionWithinCallTimeout(t *testing.T) {
	const callTimeout = 150 * time.Millisecond
	client, _ := dialFakeWithTimeout(t, func(fp *fakeProcess) { fp.stallOp = "get" }, callTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: err = %v, want context.Canceled", err)
	}

	select {
	case <-client.Broken():
	case <-time.After(callTimeout + time.Second):
		t.Fatal("Broken() did not fire within CallTimeout plus a margin after a cancel followed by a stalled reply")
	}
	if reason := client.BrokenReason(); reason == nil {
		t.Error("BrokenReason() = nil after the drain's stalled reply broke the connection")
	}
}

// cancelOnceReceived returns a ctx that is cancelled as soon as fp has read
// its first request off the wire, so a cancel provably lands after the write
// and before any (delayed or stalled) reply.
func cancelOnceReceived(t *testing.T, fp *fakeProcess) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for fp.received() < 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	return ctx
}

// TestClient_Call_ReplyIDMismatchBreaksConnection: a well-formed ok reply
// whose id is not the request's must break the connection on the ordinary
// call path (round 6 MEDIUM: removing decodeReply's id check survived the
// whole package). RED with `if reply.ID != id {` removed.
func TestClient_Call_ReplyIDMismatchBreaksConnection(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) { fp.wrongIDReplyOn = "get" })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}})
	if !errors.Is(err, ErrConnectionBroken) {
		t.Fatalf("Get with a mismatched reply id: err = %v, want ErrConnectionBroken", err)
	}
	select {
	case <-client.Broken():
	default:
		t.Error("Broken() not fired after a mismatched reply id")
	}
	if reason := client.BrokenReason(); reason == nil || !strings.Contains(reason.Error(), "does not match request id") {
		t.Errorf("BrokenReason() = %v, want it to name the id mismatch", reason)
	}
}

// TestClient_Drain_ReplyIDMismatchBreaksConnection: the same, on the drain
// path. The reply is valid JSON and ok:true, so only the id check can break
// the connection; a drain that merely validated JSON would leave it open.
func TestClient_Drain_ReplyIDMismatchBreaksConnection(t *testing.T) {
	client, fp := dialFakeWithTimeout(t, func(fp *fakeProcess) {
		fp.replyDelay = 60 * time.Millisecond
		fp.wrongIDReplyOn = "get"
	}, 5*time.Second)

	ctx := cancelOnceReceived(t, fp)
	if _, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: err = %v, want context.Canceled", err)
	}
	select {
	case <-client.Broken():
	case <-time.After(time.Second):
		t.Fatal("Broken() did not fire after a cancel followed by a mismatched-id reply")
	}
	if reason := client.BrokenReason(); reason == nil || !strings.Contains(reason.Error(), "does not match request id") {
		t.Errorf("BrokenReason() = %v, want it to name the id mismatch", reason)
	}
}

// TestClient_Drain_WorkerDeadReplyBreaksConnection: a worker_dead reply that
// arrives at the drain must break the connection with ErrWorkerDead as the
// reason. The fake closes its end right after replying, but the client only
// learns of a dead worker by decoding the reply.
func TestClient_Drain_WorkerDeadReplyBreaksConnection(t *testing.T) {
	client, fp := dialFakeWithTimeout(t, func(fp *fakeProcess) {
		fp.replyDelay = 60 * time.Millisecond
		fp.workerDeadOn = "get"
	}, 5*time.Second)

	ctx := cancelOnceReceived(t, fp)
	if _, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: err = %v, want context.Canceled", err)
	}
	select {
	case <-client.Broken():
	case <-time.After(time.Second):
		t.Fatal("Broken() did not fire after a cancel followed by a worker_dead reply")
	}
	if reason := client.BrokenReason(); !errors.Is(reason, ErrWorkerDead) {
		t.Errorf("BrokenReason() = %v, want it to wrap ErrWorkerDead", reason)
	}
}

// TestClient_Drain_UndecodableReplyBreaksConnection is item 3's second
// drain-failure branch: a caller cancels mid-round-trip, and the reply
// that does arrive for it is not valid JSON (coverage MEDIUM at 377eb1b:
// "skipping the decode (id, worker_dead) survives"). The drain must
// still mark the connection broken, promptly, since the reply is already
// on the wire and does not need to wait out any timeout.
func TestClient_Drain_UndecodableReplyBreaksConnection(t *testing.T) {
	client, _ := dialFakeWithTimeout(t, func(fp *fakeProcess) {
		fp.replyDelay = 50 * time.Millisecond
		fp.corruptReplyOn = "get"
	}, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: err = %v, want context.Canceled", err)
	}

	select {
	case <-client.Broken():
	case <-time.After(time.Second):
		t.Fatal("Broken() did not fire promptly after a cancel followed by an undecodable reply")
	}
	if reason := client.BrokenReason(); reason == nil {
		t.Error("BrokenReason() = nil after the drain's undecodable reply broke the connection")
	}
}

func TestClient_Get_RejectsShortReply(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) { fp.shortReplyBy = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}, {Object: "b", Property: "y"}}); err == nil {
		t.Fatal("Get with a short reply: want error, got nil")
	}
}

func TestClient_Get_RejectsReorderedReply(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) { fp.reorderReply = true })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Get(ctx, []GetItem{{Object: "a", Property: "x"}, {Object: "b", Property: "y"}}); err == nil {
		t.Fatal("Get with a reordered reply: want error, got nil")
	}
}

// TestClient_Set_RejectsShortReply and TestClient_Set_RejectsReorderedReply
// are Set's own versions of the Get tests above: Set decodes and validates
// its reply the same way Get does (both go through
// validateReplyMatchesRequest), and each path needs its own proof, since a
// regression specific to Set's call site would not be caught by Get's.
func TestClient_Set_RejectsShortReply(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) { fp.shortReplyBy = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	items := []SetItem{
		{Object: "a", Property: "x", Value: mustFloatValue(t, 1)},
		{Object: "b", Property: "y", Value: mustFloatValue(t, 2)},
	}
	if _, err := client.Set(ctx, items); err == nil {
		t.Fatal("Set with a short reply: want error, got nil")
	}
}

func TestClient_Set_RejectsReorderedReply(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) { fp.reorderReply = true })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	items := []SetItem{
		{Object: "a", Property: "x", Value: mustFloatValue(t, 1)},
		{Object: "b", Property: "y", Value: mustFloatValue(t, 2)},
	}
	if _, err := client.Set(ctx, items); err == nil {
		t.Fatal("Set with a reordered reply: want error, got nil")
	}
}

func mustFloatValue(t *testing.T, f float64) Value {
	t.Helper()
	v, err := FloatValue(f)
	if err != nil {
		t.Fatalf("FloatValue(%v): %v", f, err)
	}
	return v
}

// TestClient_WorkerDead_MapsToErrWorkerDead is the mutation-killing case
// for "if code == worker_dead" in call: nothing else exercised this
// mapping, so a mutant that always skips it, or the branch being deleted
// entirely, would have compiled and passed every other test.
func TestClient_WorkerDead_MapsToErrWorkerDead(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) { fp.workerDeadOn = "hello" })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Hello(ctx)
	if !errors.Is(err, ErrWorkerDead) {
		t.Fatalf("Hello after a worker_dead reply: err = %v, want ErrWorkerDead", err)
	}
	if !errors.Is(err, ErrConnectionBroken) {
		t.Errorf("Hello after a worker_dead reply: err = %v, want also ErrConnectionBroken", err)
	}
	select {
	case <-client.Broken():
	default:
		t.Error("Broken() channel not closed after a worker_dead reply")
	}
}
