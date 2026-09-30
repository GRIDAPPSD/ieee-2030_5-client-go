package gridlabd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/device"
)

func testSupervisorConfig(sockPath string) SupervisorConfig {
	return SupervisorConfig{
		Fleet:         fakeFleetName,
		SocketPath:    sockPath,
		FleetFilePath: "unused.json",
		StartTimeout:  2 * time.Second,
		CallTimeout:   2 * time.Second,
		StopGrace:     100 * time.Millisecond,
		BackoffMin:    5 * time.Millisecond,
		BackoffMax:    20 * time.Millisecond,
		MaxRestarts:   3,
		RestartWindow: time.Minute,
	}
}

func TestSupervisor_StartsAndBecomesHealthy(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, nil, reg))

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	waitForHealthy(t, sup, true)
	if got := sup.Healthy(); !got {
		t.Fatalf("Healthy() = %v, want true", got)
	}

	cancel()
	if err := <-runErr; err != context.Canceled {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
	if sup.Healthy() {
		t.Error("Healthy() after ctx cancel: want false")
	}
}

func TestSupervisor_RefusesOnHelloMismatch(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	configure := func(fp *fakeProcess) {
		fp.helloErr = &wireErrorBody{Code: "gridlabd_error", Message: "version mismatch"}
	}
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // bounded: a regression must fail fast, not hang the package
	defer cancel()
	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("Run(): want error on hello mismatch, got nil")
	}
	if sup.Healthy() {
		t.Error("Healthy() after refused start: want false")
	}
	// The refused process must not be left running.
	waitForFinished(t, reg.list()[0])
}

// TestSupervisor_RestartsAfterCrash proves the DOWN-then-restart path: a
// mid-run crash is followed by a fresh, healthy sidecar, without the test
// having to wait out the full production backoff (the config here uses a
// millisecond-scale backoff).
func TestSupervisor_RestartsAfterCrash(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, nil, reg))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	waitForHealthy(t, sup, true)
	first := reg.list()[0]
	first.SimulateCrash()

	waitForHealthy(t, sup, false) // unhealthy the instant the crash is seen
	waitForCreatedCount(t, reg, 2)
	waitForHealthy(t, sup, true) // then healthy again once the restart's hello succeeds

	second := reg.list()[1]
	if second.InstanceID() == first.InstanceID() {
		t.Error("restart reused the crashed fake's instance")
	}
}

// TestSupervisor_RestartBudgetExhausted proves the fleet stays DOWN, and
// Run returns an error, once restarts exceed MaxRestarts within the
// window: it does not retry forever.
// logSpy collects every cfg.Log call, safe for concurrent use (Run logs
// from its own goroutine while a test reads lines() from the test
// goroutine).
type logSpy struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSpy) log(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *logSpy) lines_() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.lines))
	copy(out, s.lines)
	return out
}

// TestSupervisor_RestartBudgetExhausted is item 3: the terminal state on
// budget exhaustion is State Down, with the reason, and the design's "logs
// the fleet and cause once per state change" fires exactly once for this
// transition (not once per failed restart attempt).
func TestSupervisor_RestartBudgetExhausted(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	cfg := testSupervisorConfig(sockPath)
	cfg.MaxRestarts = 2
	spy := &logSpy{}
	cfg.Log = spy.log
	sup := newSupervisor(cfg, newFakeLauncher(t, nil, reg))

	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(context.Background()) }()

	waitForHealthy(t, sup, true)
	for i := 0; i < cfg.MaxRestarts+1; i++ {
		waitForCreatedCount(t, reg, i+1)
		reg.list()[i].SimulateCrash()
	}

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("Run(): want error after restart budget exhausted, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after restart budget exhausted")
	}
	if sup.Healthy() {
		t.Error("Healthy() after budget exhausted: want false")
	}
	if state, reason := sup.State(); state != StateDown {
		t.Errorf("State() = %v (reason %v), want StateDown", state, reason)
	} else if reason == nil {
		t.Error("State() reason = nil, want the budget-exhausted error")
	}
	downLines := 0
	for _, line := range spy.lines_() {
		if strings.HasPrefix(line, fmt.Sprintf("fleet %s DOWN: ", fakeFleetName)) {
			downLines++
		}
	}
	if downLines != 1 {
		t.Errorf("DOWN log lines = %d, want exactly 1 (got: %v)", downLines, spy.lines_())
	}
}

// TestSupervisor_CancelAtLastAttemptReturnsCanceledNotBudgetError is item
// 3's ctx-before-budget ordering: Run checks ctx.Done()/stopCh before the
// budget on every pass through the restart loop, so a cancel that lands
// exactly when the budget would otherwise be exhausted is reported as
// context.Canceled, not a budget-exhausted diagnosis the caller did not
// ask for. cfg.Log's hook cancels ctx synchronously from inside Run's own
// call stack (Log is never called from a separate goroutine), the instant
// Run logs the second crash and is about to re-enter the restart loop
// with its one-attempt budget already spent: this makes the race
// deterministic rather than hoping the test goroutine's cancel() call
// wins a scheduling race against Run's.
func TestSupervisor_CancelAtLastAttemptReturnsCanceledNotBudgetError(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	cfg := testSupervisorConfig(sockPath)
	cfg.MaxRestarts = 1

	ctx, cancel := context.WithCancel(context.Background())
	crashLogs := 0
	cfg.Log = func(format string, args ...any) {
		if strings.HasPrefix(format, "fleet %s: sidecar exited") {
			crashLogs++
			if crashLogs == 2 {
				cancel()
			}
		}
	}
	sup := newSupervisor(cfg, newFakeLauncher(t, nil, reg))

	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	waitForHealthy(t, sup, true)
	waitForCreatedCount(t, reg, 1)
	reg.list()[0].SimulateCrash() // consumes the only restart the budget allows
	waitForHealthy(t, sup, true)
	waitForCreatedCount(t, reg, 2)

	reg.list()[1].SimulateCrash() // budget now exhausted; the Log hook above cancels ctx as Run logs this

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() after cancel racing the exhausted restart budget = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after ctx cancel")
	}
}

// TestSupervisor_StopByPID proves Stop signals the actual running process
// (by PID, via os.Process) and blocks until it has exited.
func TestSupervisor_StopByPID(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, nil, reg))

	go sup.Run(context.Background())
	waitForHealthy(t, sup, true)

	sup.Stop()

	if sup.Healthy() {
		t.Error("Healthy() after Stop: want false")
	}
	if !reg.list()[0].Finished() {
		t.Error("Stop returned before the sidecar process exited")
	}
}

func waitForHealthy(t *testing.T, sup *Supervisor, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sup.Healthy() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Healthy() did not reach %v within 2s", want)
}

func waitForFinished(t *testing.T, fp *fakeProcess) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fp.Finished() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("process did not finish within 2s")
}

func waitForCreatedCount(t *testing.T, reg *fakeRegistry, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(reg.list()) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("only %d fake processes created, want at least %d", len(reg.list()), want)
}

// TestSupervisor_StopWhileCallStalled_BoundedByCallTimeout is item 1's
// "Stop ... never hang behind c.mu": a device's Get call is stalled inside
// Client.call, holding c.mu for up to CallTimeout. Stop must still return
// in roughly that bound, not hang until the stalled call would have timed
// out on its own accord some other way, and not forever.
func TestSupervisor_StopWhileCallStalled_BoundedByCallTimeout(t *testing.T) {
	reg := &fakeRegistry{}
	cfg := testSupervisorConfig(filepath.Join(shortSockDir(t), "a.sock"))
	cfg.CallTimeout = 150 * time.Millisecond
	configure := func(fp *fakeProcess) { fp.stallOp = "get" }
	sup := newSupervisor(cfg, newFakeLauncher(t, configure, reg))

	go sup.Run(context.Background())
	waitForHealthy(t, sup, true)

	transport := sup.Transport()
	go func() {
		_, _ = transport.Get(context.Background(), []device.FleetGetItem{{Object: "x", Property: "y"}})
	}()
	time.Sleep(20 * time.Millisecond) // let the Get call acquire c.mu and start stalling

	start := time.Now()
	sup.Stop()
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("Stop while a call was stalled took %v, want bounded near CallTimeout (150ms)", elapsed)
	}
}

// TestSupervisor_BrokenConnectionTriggersRestart proves the other half of
// item 1: a connection Client itself gives up on (Broken) restarts the
// fleet even though the sidecar process never exited on its own, so a
// later call never risks reading a stale reply from the abandoned request.
func TestSupervisor_BrokenConnectionTriggersRestart(t *testing.T) {
	reg := &fakeRegistry{}
	cfg := testSupervisorConfig(filepath.Join(shortSockDir(t), "a.sock"))
	cfg.CallTimeout = 100 * time.Millisecond
	configure := func(fp *fakeProcess) { fp.stallOp = "get" }
	sup := newSupervisor(cfg, newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitForHealthy(t, sup, true)
	first := reg.list()[0]

	transport := sup.Transport()
	_, err := transport.Get(context.Background(), []device.FleetGetItem{{Object: "x", Property: "y"}})
	if err == nil {
		t.Fatal("Get against a stalled sidecar: want error, got nil")
	}

	waitForCreatedCount(t, reg, 2)
	waitForHealthy(t, sup, true)
	second := reg.list()[1]
	if second.InstanceID() == first.InstanceID() {
		t.Error("restart after a broken connection reused the same fake instance")
	}
}

// TestSupervisor_RefusesOnProtocolMismatch is the mutation-killing case
// for "if hello.Protocol != ProtocolVersion" (a mutant collapsing this to
// "if false" survives against TestSupervisor_RefusesOnHelloMismatch,
// which never varies the protocol number at all): the fake answers a real
// hello, protocol 999.
func TestSupervisor_RefusesOnProtocolMismatch(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	configure := func(fp *fakeProcess) { fp.helloProtocol = 999 }
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // bounded: a regression must fail fast, not hang the package
	defer cancel()
	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("Run(): want error on protocol mismatch, got nil")
	}
	if sup.Healthy() {
		t.Error("Healthy() after a protocol mismatch: want false")
	}
}

// TestSupervisor_RefusesOnFleetNameMismatch is item 2's hello.Fleet check.
func TestSupervisor_RefusesOnFleetNameMismatch(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	configure := func(fp *fakeProcess) { fp.fleet = "a-different-fleet" }
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // bounded: a regression must fail fast, not hang the package
	defer cancel()
	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("Run(): want error when hello.Fleet does not match the configured fleet, got nil")
	}
	if sup.Healthy() {
		t.Error("Healthy() after a fleet name mismatch: want false")
	}
}

// TestSupervisor_ShutdownEscalatesToSIGKILLWhenSignalIgnored is the
// mutation-killing case for "return before SIGTERM and SIGKILL": the
// fake's Signal ignores SIGTERM entirely, so shutdown can only have ended
// it by reaching Kill.
func TestSupervisor_ShutdownEscalatesToSIGKILLWhenSignalIgnored(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	cfg := testSupervisorConfig(sockPath)
	cfg.StopGrace = 20 * time.Millisecond
	configure := func(fp *fakeProcess) {
		fp.ignoreSignal = true
		// Otherwise the fake's own cooperative reply to "shutdown" ends
		// it (server.py's stop=True) before the SIGTERM/SIGKILL
		// escalation this test is proving is ever reached.
		fp.ignoreShutdown = true
	}
	sup := newSupervisor(cfg, newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	waitForHealthy(t, sup, true)
	fp := reg.list()[0]

	cancel()
	waitForFinished(t, fp)
	if fp.SignalCount() == 0 {
		t.Error("SignalCount() = 0, want at least 1 (SIGTERM attempted)")
	}
	if fp.KillCount() == 0 {
		t.Error("KillCount() = 0, want at least 1 (SIGKILL reached after SIGTERM was ignored)")
	}
}

// TestSupervisor_StopWithoutRun_DoesNotHang is error-handling's LOW
// finding: Stop called before Run has ever started must return, not block
// forever waiting on a doneCh nothing will ever close.
func TestSupervisor_StopWithoutRun_DoesNotHang(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, nil, reg))

	done := make(chan struct{})
	go func() {
		sup.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() without a prior Run() did not return within 2s")
	}

	// A Run() called afterward must see the stop and do nothing. Bounded:
	// if the stopped check regresses, Run() would otherwise try a real
	// (unbounded) start attempt against a socket that will never appear.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := sup.Run(ctx)
	if err != nil {
		t.Errorf("Run() after Stop() preceded it: %v, want nil", err)
	}
	if len(reg.list()) != 0 {
		t.Errorf("Run() after a preceding Stop() started %d process(es), want 0", len(reg.list()))
	}
}

func TestBackoffDelay_NeverZeroOrBelowMin(t *testing.T) {
	min, max := 2*time.Second, 60*time.Second
	for attempt := 0; attempt < 10; attempt++ {
		d := backoffDelay(attempt, min, max)
		if d < min {
			t.Errorf("backoffDelay(%d) = %v, want >= %v", attempt, d, min)
		}
		if d > max {
			t.Errorf("backoffDelay(%d) = %v, want <= %v", attempt, d, max)
		}
	}
}

func TestBackoffDelay_DoublesThenCaps(t *testing.T) {
	min, max := 2*time.Second, 60*time.Second
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{5, 60 * time.Second}, // 2*2^5=64s, capped
		{20, 60 * time.Second},
	}
	for _, tt := range tests {
		if got := backoffDelay(tt.attempt, min, max); got != tt.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestPruneRestarts_DropsOutsideWindow(t *testing.T) {
	now := time.Now()
	restarts := []time.Time{now.Add(-20 * time.Minute), now.Add(-5 * time.Minute), now.Add(-1 * time.Minute)}
	got := pruneRestarts(restarts, 10*time.Minute)
	if len(got) != 2 {
		t.Fatalf("pruneRestarts kept %d entries, want 2 (the two within the last 10m)", len(got))
	}
	for _, r := range got {
		if now.Sub(r) > 10*time.Minute {
			t.Errorf("pruneRestarts kept an entry %v old, want <= 10m", now.Sub(r))
		}
	}
}

func TestPruneRestarts_KeepsAllWithinWindow(t *testing.T) {
	now := time.Now()
	restarts := []time.Time{now.Add(-1 * time.Minute), now.Add(-2 * time.Minute)}
	got := pruneRestarts(restarts, 10*time.Minute)
	if len(got) != 2 {
		t.Errorf("pruneRestarts kept %d entries, want 2 (both within the window)", len(got))
	}
}

// TestSupervisor_ConcurrentCallersDoNotChargeQueueTimeAgainstTimeout is
// item 1: N devices sharing one fleet's connection each queue behind
// Client.call's serialization. A caller must not be charged for time
// spent waiting for c.mu: each of 4 concurrent Gets, individually served
// in 100 ms, against a 250 ms CallTimeout, must all succeed even though
// the 4th is queued behind roughly 300 ms of the others' round trips.
// Proves no break and no restart: generation stays 1 throughout.
func TestSupervisor_ConcurrentCallersDoNotChargeQueueTimeAgainstTimeout(t *testing.T) {
	reg := &fakeRegistry{}
	cfg := testSupervisorConfig(filepath.Join(shortSockDir(t), "a.sock"))
	cfg.CallTimeout = 250 * time.Millisecond
	configure := func(fp *fakeProcess) { fp.replyDelay = 100 * time.Millisecond }
	sup := newSupervisor(cfg, newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitForHealthy(t, sup, true)

	transport := sup.Transport()
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := transport.Get(context.Background(), []device.FleetGetItem{{Object: "x", Property: "y"}})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v, want nil (queue wait must not consume its CallTimeout budget)", i, err)
		}
	}
	if gen := sup.generation(); gen != 1 {
		t.Errorf("generation() = %d, want 1 (no restart)", gen)
	}
	if !sup.Healthy() {
		t.Error("Healthy() = false, want true (connection must not have broken)")
	}
}

// TestSupervisor_CtxDoneAfterQueueingReturnsCleanly is the other half of
// item 1: a caller whose own ctx is already done by the time it is served
// (not c.timeout, an explicit caller deadline that expired while queued)
// gets its ctx error back, and the connection stays healthy for the
// caller queued behind it.
func TestSupervisor_CtxDoneAfterQueueingReturnsCleanly(t *testing.T) {
	reg := &fakeRegistry{}
	cfg := testSupervisorConfig(filepath.Join(shortSockDir(t), "a.sock"))
	configure := func(fp *fakeProcess) { fp.replyDelay = 150 * time.Millisecond }
	sup := newSupervisor(cfg, newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	waitForHealthy(t, sup, true)

	transport := sup.Transport()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Occupies c.mu for ~150ms.
		_, _ = transport.Get(context.Background(), []device.FleetGetItem{{Object: "x", Property: "y"}})
	}()
	time.Sleep(10 * time.Millisecond) // let the first call acquire the lock

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer shortCancel()
	_, err := transport.Get(shortCtx, []device.FleetGetItem{{Object: "x", Property: "y"}})
	if err == nil {
		t.Fatal("Get with a ctx that expires while queued: want error, got nil")
	}

	wg.Wait()
	if !sup.Healthy() {
		t.Error("Healthy() = false after a queued caller's own ctx expired, want true (nothing was written for it)")
	}
	if gen := sup.generation(); gen != 1 {
		t.Errorf("generation() = %d, want 1 (no restart)", gen)
	}
}

// diesInstantlyProcess is a sidecarProcess whose Wait() has already fired
// before it is ever returned: no socket is ever bound at the path
// waitForSocket polls, so the only way that function can return before
// the full StartTimeout is by watching proc.Wait() directly.
type diesInstantlyProcess struct {
	done chan error
}

func newDiesInstantlyProcess(cause error) *diesInstantlyProcess {
	ch := make(chan error, 1)
	ch <- cause
	return &diesInstantlyProcess{done: ch}
}

func (p *diesInstantlyProcess) Pid() int               { return os.Getpid() }
func (p *diesInstantlyProcess) Wait() <-chan error     { return p.done }
func (p *diesInstantlyProcess) Signal(os.Signal) error { return nil }
func (p *diesInstantlyProcess) Kill() error            { return nil }

// TestWaitForSocket_NoticesProcessExitBeforeSocketAppears is the
// mutation-killing case for the early-exit watch in waitForSocket
// (`case werr := <-proc.Wait():`): StartTimeout is generous (5s) so a
// removed watch would make this test wait out the full timeout instead of
// returning almost immediately.
func TestWaitForSocket_NoticesProcessExitBeforeSocketAppears(t *testing.T) {
	cfg := testSupervisorConfig(filepath.Join(shortSockDir(t), "never-appears.sock"))
	cfg.StartTimeout = 5 * time.Second
	launch := func(context.Context, []string) (sidecarProcess, error) {
		return newDiesInstantlyProcess(fmt.Errorf("exited before bind")), nil
	}
	sup := newSupervisor(cfg, launch)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second) // bounded: a regression must fail fast, not hang the package
	defer cancel()
	err := sup.Run(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run() with a process that exits before binding a socket: want error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run() took %v to notice the process already exited, want well under the 5s StartTimeout", elapsed)
	}
}

// TestSupervisor_RefusesOnPeerPIDMismatch is the supervisor-level
// peer-identity mutation-killing case: TestVerifyPeerPID proves the
// function alone, but every fake Supervisor dials in other tests reports
// Pid() == os.Getpid(), the real peer, so nothing at the Supervisor level
// ever exercised a mismatch before this. The error text is asserted (not
// only err != nil), and WaitStarted is asserted to return promptly: item
// 3's own mutant is verifyPeerPID replaced by a no-op (always nil), which
// would make Run start successfully and WaitStarted hang past the 1s
// bound since startedCh would never see a failure worth reporting this
// fast.
func TestSupervisor_RefusesOnPeerPIDMismatch(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	configure := func(fp *fakeProcess) { fp.pidOverride = os.Getpid() + 999999 }
	sup := newSupervisor(testSupervisorConfig(sockPath), newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // bounded: a regression must fail fast
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	startErr := sup.WaitStarted(waitCtx)
	if startErr == nil {
		t.Fatal("WaitStarted() with a mismatched peer pid: want error, got nil")
	}
	if !strings.Contains(startErr.Error(), "peer credential check") {
		t.Errorf("WaitStarted() error = %q, does not name the peer credential check", startErr.Error())
	}

	err := <-runErr
	if err == nil {
		t.Fatal("Run() with a mismatched peer pid: want error, got nil")
	}
	if !strings.Contains(err.Error(), "peer credential check") {
		t.Errorf("Run() error = %q, does not name the peer credential check", err.Error())
	}
	if sup.Healthy() {
		t.Error("Healthy() after a peer pid mismatch: want false")
	}
	if state, _ := sup.State(); state != StateDown {
		t.Errorf("State() after a refused first start = %v, want StateDown", state)
	}
}

// TestSupervisor_BrokenConnectionCase_NotEquivalentToProcessExit is the
// mutation-killing case for `case <-client.Broken():` in Run: the fake's
// stall blocks on killCh (never on the connection), so it cannot exit on
// its own just because the client gave up. Only Run reacting to
// client.Broken() and calling killAndReap can end it; TestClient_Call_*
// prove Broken() fires, this proves Run actually watches it.
func TestSupervisor_BrokenConnectionCase_NotEquivalentToProcessExit(t *testing.T) {
	reg := &fakeRegistry{}
	cfg := testSupervisorConfig(filepath.Join(shortSockDir(t), "a.sock"))
	cfg.CallTimeout = 100 * time.Millisecond
	configure := func(fp *fakeProcess) { fp.stallOp = "get" }
	sup := newSupervisor(cfg, newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second) // bounded: a regression must fail fast
	defer cancel()
	go sup.Run(ctx)
	waitForHealthy(t, sup, true)
	fp := reg.list()[0]

	transport := sup.Transport()
	_, err := transport.Get(context.Background(), []device.FleetGetItem{{Object: "x", Property: "y"}})
	if err == nil {
		t.Fatal("Get against a stalled fake that never reads or replies: want error, got nil")
	}

	// Without Run watching client.Broken(), fp stays alive forever (it
	// blocks on killCh, not on the connection): this is the assertion a
	// removed case makes fail (by timing out the whole package, which the
	// bounded ctx above turns into a fast, readable failure instead).
	waitForFinished(t, fp)
}

// TestSupervisor_RepeatedCallerCancelsDoNotExhaustRestartBudget is item
// 6's Supervisor-level proof, the security review's own probe: under the
// old break-on-cancel behavior a caller cancel marked the connection
// broken, which Run's client.Broken() case treated exactly like a crash
// and spent a restart on it, so MaxRestarts+1 caller cancels alone used
// to exhaust the budget and end Run (RED at fb767fc). Each reply here is
// only delayed, not withheld, so every cancelled call's drain genuinely
// succeeds and the connection never breaks.
func TestSupervisor_RepeatedCallerCancelsDoNotExhaustRestartBudget(t *testing.T) {
	reg := &fakeRegistry{}
	cfg := testSupervisorConfig(filepath.Join(shortSockDir(t), "a.sock"))
	cfg.CallTimeout = 2 * time.Second
	cfg.MaxRestarts = 3
	configure := func(fp *fakeProcess) { fp.replyDelay = 100 * time.Millisecond }
	sup := newSupervisor(cfg, newFakeLauncher(t, configure, reg))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // bounded: a regression must fail fast
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()
	waitForHealthy(t, sup, true)

	transport := sup.Transport()
	for i := 0; i < cfg.MaxRestarts+1; i++ {
		callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err := transport.Get(callCtx, []device.FleetGetItem{{Object: "x", Property: "y"}})
		callCancel()
		if err == nil {
			t.Fatalf("Get %d with a 30ms ctx against a 100ms-delayed reply: want error, got nil", i)
		}
		// Let this iteration's drain finish (its reply arrives ~100ms
		// from its own write) before the next iteration writes, so each
		// cancel is independently proven rather than queuing behind an
		// unrelated backlog.
		time.Sleep(150 * time.Millisecond)
	}

	select {
	case err := <-runErr:
		t.Fatalf("Run() exited after %d caller cancels: %v, want it still running (no restart should have been needed)", cfg.MaxRestarts+1, err)
	case <-time.After(300 * time.Millisecond):
	}
	if !sup.Healthy() {
		t.Error("Healthy() after repeated caller cancels alone: want true")
	}
}
