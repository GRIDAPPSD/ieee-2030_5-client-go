// Item 4: startFleets tests against fakeFleetSupervisor, a minimal,
// channel-driven fleetSupervisor that lets a test control exactly when
// Run returns, when WaitStarted resolves, and when Stop unblocks, without
// a real sidecar process.

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeFleetSupervisor is a minimal fleetSupervisor for startFleets tests.
type fakeFleetSupervisor struct {
	name string

	runOnce     sync.Once
	runCalled   chan struct{} // closed the first time Run is invoked
	runErrCh    chan error    // a value sent here becomes Run's return
	stopRelease chan struct{} // Stop blocks on this until closed

	startedCh  chan struct{} // closed to make WaitStarted resolve
	startedErr error         // WaitStarted's return once startedCh closes

	stopStarted  chan struct{} // closed the moment Stop is called; also unblocks a still-running Run with a nil return
	stopReturned chan struct{} // closed once Stop actually returns
}

func newFakeFleetSupervisor(name string) *fakeFleetSupervisor {
	return &fakeFleetSupervisor{
		name:         name,
		runCalled:    make(chan struct{}),
		runErrCh:     make(chan error, 1),
		stopRelease:  make(chan struct{}),
		startedCh:    make(chan struct{}),
		stopStarted:  make(chan struct{}),
		stopReturned: make(chan struct{}),
	}
}

func (f *fakeFleetSupervisor) Fleet() string { return f.name }

func (f *fakeFleetSupervisor) Run(ctx context.Context) error {
	f.runOnce.Do(func() { close(f.runCalled) })
	select {
	case err := <-f.runErrCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-f.stopStarted:
		// Matches the real Supervisor's contract: Stop makes a running
		// Run return nil, not an error.
		return nil
	}
}

func (f *fakeFleetSupervisor) WaitStarted(ctx context.Context) error {
	select {
	case <-f.startedCh:
		return f.startedErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeFleetSupervisor) Stop() {
	close(f.stopStarted)
	<-f.stopRelease
	close(f.stopReturned)
}

// healthyFake returns a fake whose WaitStarted resolves nil immediately
// (a healthy first start) and whose Stop returns as soon as it is called
// (stopRelease pre-closed).
func healthyFake(name string) *fakeFleetSupervisor {
	f := newFakeFleetSupervisor(name)
	close(f.startedCh)
	close(f.stopRelease)
	return f
}

func waitClosed(t *testing.T, ch chan struct{}, bound time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(bound):
		t.Fatalf("%s did not happen within %v", what, bound)
	}
}

// TestStartFleets_RunCalledOnEach proves every fleet's Run is actually
// invoked, not just constructed. RED with the Run-calling goroutine loop
// removed from startFleets.
func TestStartFleets_RunCalledOnEach(t *testing.T) {
	a, b := healthyFake("a"), healthyFake("b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := startFleets(ctx, []fleetSupervisor{a, b})
	if err != nil {
		t.Fatalf("startFleets: %v", err)
	}
	defer stop()

	waitClosed(t, a.runCalled, time.Second, "fleet a's Run")
	waitClosed(t, b.runCalled, time.Second, "fleet b's Run")
}

// instantRefusalFake's WaitStarted returns an unrelated, already-set
// refusal immediately, ignoring ctx entirely: used only to prove
// startFleets's own ctx.Err() check overrides whatever a fleet's
// WaitStarted happened to return, rather than relying on that return
// value already equaling ctx.Err() by coincidence (which a fake whose
// WaitStarted resolves via <-ctx.Done() would, defeating the point).
type instantRefusalFake struct {
	*fakeFleetSupervisor
	refusal error
}

func (f *instantRefusalFake) WaitStarted(context.Context) error { return f.refusal }

// TestStartFleets_CtxCancelledDuringWaitIsReportedAsCancellation is item
// 4: a signal arriving while startFleets is still waiting for the first
// start is reported as the outer ctx's own error, even when a fleet's own
// WaitStarted has already resolved with some unrelated refusal, and it
// still stops every fleet before returning. RED with the
// `if ctx.Err() != nil { return func(){}, ctx.Err() }` branch removed
// (main.go's own signal-during-start branch, `errors.Is(err,
// context.Canceled)`, would then be looking at the unrelated refusal
// instead).
func TestStartFleets_CtxCancelledDuringWaitIsReportedAsCancellation(t *testing.T) {
	base := newFakeFleetSupervisor("a")
	close(base.stopRelease)
	a := &instantRefusalFake{fakeFleetSupervisor: base, refusal: errors.New("unrelated refusal, resolved before the signal")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before startFleets is even called

	stop, err := startFleets(ctx, []fleetSupervisor{a})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("startFleets error = %v, want context.Canceled (not the fake's unrelated refusal)", err)
	}
	stop()
	waitClosed(t, a.stopStarted, time.Second, "fleet a's Stop")
}

// TestStartFleets_RefusalStopsTheHealthyFakeAndReturnsTheError is item 4's
// central refusal case: one fleet's first start is refused, and startFleets
// must stop every fleet that did start (including the healthy one) and
// return the refusing fleet's error. RED with the Run-calling goroutine
// loop removed (nothing to refuse) or Stop removed from stopFn (the
// healthy fake is never asked to stop).
func TestStartFleets_RefusalStopsTheHealthyFakeAndReturnsTheError(t *testing.T) {
	refused := newFakeFleetSupervisor("refused")
	refusalErr := errors.New("version mismatch")
	refused.startedErr = refusalErr
	close(refused.startedCh)
	close(refused.stopRelease)

	healthy := healthyFake("healthy")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := startFleets(ctx, []fleetSupervisor{refused, healthy})
	if !errors.Is(err, refusalErr) {
		t.Fatalf("startFleets error = %v, want %v", err, refusalErr)
	}

	waitClosed(t, refused.stopStarted, time.Second, "the refused fleet's Stop")
	waitClosed(t, healthy.stopStarted, time.Second, "the healthy fleet's Stop")
}

// TestStartFleets_LaterErrorLeavesTheOtherFakeRunning is item 4: once
// every fleet is healthy, a LATER Run error (the restart budget
// exhausted) does not stop, or otherwise touch, any other fleet.
// startFleets logs nothing of its own for this transition (item 1, round
// 5: Supervisor.setDown is the one layer that owns the "fleet <name>
// DOWN" line now; TestStartFleets_RealCrashLoop_LogsExactlyOneDownLine
// proves that composition end to end with a real Supervisor). RED with a
// later error turned into an exit: this test cannot directly observe an
// os.Exit from inside itself, so it asserts the property such a mutant
// would violate instead: the healthy fleet is never stopped by fleet a's
// own later error.
func TestStartFleets_LaterErrorLeavesTheOtherFakeRunning(t *testing.T) {
	a, b := healthyFake("a"), healthyFake("b")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := startFleets(ctx, []fleetSupervisor{a, b})
	if err != nil {
		t.Fatalf("startFleets: %v", err)
	}
	defer stop()

	a.runErrCh <- errors.New("restart budget exhausted")

	select {
	case <-b.stopStarted:
		t.Error("fleet b was stopped after fleet a's later error: want it untouched")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStartFleets_StopReturnsOnlyAfterEveryFakesStopHasFinished is item
// 4's idempotent-stop contract: the returned stop blocks until every
// fleet's own Stop call has itself returned, not merely been invoked. RED
// with wg.Wait removed from stopFn (stop would return the moment both
// Stop calls started, not once they finished).
func TestStartFleets_StopReturnsOnlyAfterEveryFakesStopHasFinished(t *testing.T) {
	a, b := newFakeFleetSupervisor("a"), newFakeFleetSupervisor("b")
	close(a.startedCh)
	close(b.startedCh)
	// stopRelease left open: each fake's Stop blocks until this test
	// releases it, below.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := startFleets(ctx, []fleetSupervisor{a, b})
	if err != nil {
		t.Fatalf("startFleets: %v", err)
	}

	stopReturned := make(chan struct{})
	go func() {
		stop()
		close(stopReturned)
	}()

	waitClosed(t, a.stopStarted, time.Second, "fleet a's Stop being called")
	waitClosed(t, b.stopStarted, time.Second, "fleet b's Stop being called")

	select {
	case <-stopReturned:
		t.Fatal("stop() returned before either fake's Stop had finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(a.stopRelease)
	select {
	case <-stopReturned:
		t.Fatal("stop() returned before fleet b's Stop had finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(b.stopRelease)
	waitClosed(t, stopReturned, time.Second, "stop()")
}
