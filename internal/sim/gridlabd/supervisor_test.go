package gridlabd

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testSupervisorConfig(sockPath string) SupervisorConfig {
	return SupervisorConfig{
		Fleet:         "test",
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
	if second.Pid() == first.Pid() {
		t.Error("restart reused the crashed process's pid")
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
