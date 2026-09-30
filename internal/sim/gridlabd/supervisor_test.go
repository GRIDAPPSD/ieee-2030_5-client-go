package gridlabd

import (
	"context"
	"path/filepath"
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

	err := sup.Run(context.Background())
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
func TestSupervisor_RestartBudgetExhausted(t *testing.T) {
	reg := &fakeRegistry{}
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	cfg := testSupervisorConfig(sockPath)
	cfg.MaxRestarts = 2
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

	err := sup.Run(context.Background())
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

	err := sup.Run(context.Background())
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

	// A Run() called afterward must see the stop and do nothing.
	err := sup.Run(context.Background())
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
