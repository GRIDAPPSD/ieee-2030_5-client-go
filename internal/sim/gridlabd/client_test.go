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

	start := time.Now()
	_, err := client.Hello(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Hello against a sidecar that never answers, with context.Background(): want error, got nil")
	}
	if elapsed > time.Second {
		t.Errorf("Hello against a stalled sidecar took %v to give up, want roughly its 150ms client timeout", elapsed)
	}

	start2 := time.Now()
	_, err2 := client.Hello(context.Background())
	elapsed2 := time.Since(start2)
	if !errors.Is(err2, ErrConnectionBroken) {
		t.Errorf("Hello after a timed-out call: err = %v, want ErrConnectionBroken", err2)
	}
	if elapsed2 > 50*time.Millisecond {
		t.Errorf("Hello after the connection was already broken took %v, want near-instant (fast path)", elapsed2)
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
