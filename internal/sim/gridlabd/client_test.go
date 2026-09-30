package gridlabd

import (
	"context"
	"errors"
	"path/filepath"
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

	firstCtx, firstCancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		firstCancel()
	}()
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
