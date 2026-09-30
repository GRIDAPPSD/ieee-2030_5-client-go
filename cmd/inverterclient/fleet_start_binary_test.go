// Round 6 item 1: main must decide "signal during fleet start" from its own
// signal ctx, not from the shape of the error startFleets returns. A fleet
// whose first start hits its StartTimeout returns a refusal that wraps
// context.DeadlineExceeded while the signal ctx is still live; that is a
// refusal (exit 1), never a cancellation (exit 0). Both directions are run
// against the built binary, with an interpreter that never binds its socket.

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// neverBindsFleet builds the binary and a one-device fleet whose
// interpreter is a script that sleeps instead of binding the socket, so the
// first start can only end by StartTimeout or by a signal. It returns the
// binary path, the fleet file path and a relative run dir under the repo
// root (the binary's cwd), cleaned up with the test.
func neverBindsFleet(t *testing.T) (binPath, fleetFilePath, runDirRel, root string) {
	t.Helper()
	root = repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "certs")); err == nil {
		t.Skip("a certs/ directory exists at the repo root; this test needs the run to stop before client creation")
	}

	binPath = filepath.Join(t.TempDir(), "inverterclient-test-bin")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	dir := t.TempDir()
	// exec, not a child: the supervisor's kill by PID must reach the sleep
	// itself, leaving nothing behind.
	interp := filepath.Join(dir, "never_binds.sh")
	if err := os.WriteFile(interp, []byte("#!/bin/sh\nexec sleep 120\n"), 0o700); err != nil {
		t.Fatalf("write interpreter: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.glm"), []byte(probeGLM), 0o600); err != nil {
		t.Fatalf("write glm: %v", err)
	}
	fleetFilePath = filepath.Join(dir, "probe.fleet.json")
	fleetJSON := strings.Replace(probeFleetJSON, `"glm": "probe.glm",`, `"glm": "probe.glm",
  "interpreter": "`+interp+`",`, 1)
	if !strings.Contains(fleetJSON, interp) {
		t.Fatal("test setup: interpreter field was not injected into the fleet JSON")
	}
	if err := os.WriteFile(fleetFilePath, []byte(fleetJSON), 0o600); err != nil {
		t.Fatalf("write fleet file: %v", err)
	}

	runDirAbs, err := os.MkdirTemp(root, "t")
	if err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDirAbs) })
	return binPath, fleetFilePath, filepath.Base(runDirAbs), root
}

func exitCodeOf(t *testing.T, runErr error, out []byte) int {
	t.Helper()
	if runErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("run binary: %v (not an ExitError; output:\n%s)", runErr, out)
	}
	return exitErr.ExitCode()
}

// TestMain_FirstStartTimeoutExitsOneNotZero: the HIGH finding. A first
// start that hits StartTimeout (the default 30s; no flag shortens it, so
// this test waits it out) must exit 1 with the refusal, print no "signal
// during fleet start" line, and log exactly one DOWN line. RED with main's
// check widened back to errors.Is(err, context.DeadlineExceeded): the
// refusal wraps DeadlineExceeded, so the binary exits 0.
func TestMain_FirstStartTimeoutExitsOneNotZero(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the default 30s StartTimeout")
	}
	binPath, fleetFilePath, runDirRel, root := neverBindsFleet(t)

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath,
		"--client-role=aggregator",
		"--fleet-file="+fleetFilePath,
		"--run-dir="+runDirRel,
		"--notify-listen=",
	)
	cmd.Dir = root
	out, runErr := cmd.CombinedOutput()

	if code := exitCodeOf(t, runErr, out); code != 1 {
		t.Errorf("exit code = %d, want 1 (a StartTimeout refusal is not a signal); output:\n%s", code, out)
	}
	if strings.Contains(string(out), "signal during fleet start") {
		t.Errorf("output reports a signal for a StartTimeout refusal; output:\n%s", out)
	}
	if !strings.Contains(string(out), "initial start refused") {
		t.Errorf("output does not carry the refusal; output:\n%s", out)
	}
	if n := strings.Count(string(out), "fleet probe DOWN:"); n != 1 {
		t.Errorf("DOWN lines = %d, want exactly 1; output:\n%s", n, out)
	}
}

// TestMain_SignalDuringFleetStartExitsZero: the other direction. SIGINT
// while the first start is still waiting is a cancellation: exit 0, the
// "signal during fleet start" line, and no DOWN line. The process is
// started here, so it is signalled, and on any failure killed, by the PID
// this test recorded.
func TestMain_SignalDuringFleetStartExitsZero(t *testing.T) {
	binPath, fleetFilePath, runDirRel, root := neverBindsFleet(t)

	var outBuf strings.Builder
	cmd := exec.Command(binPath,
		"--client-role=aggregator",
		"--fleet-file="+fleetFilePath,
		"--run-dir="+runDirRel,
		"--notify-listen=",
	)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = &outBuf, &outBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	pid := cmd.Process.Pid
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill() // no-op error if it already exited and was reaped
		}
	})

	// Give the binary time to reach the first-start wait (it is blocked
	// there for 30s otherwise), then interrupt it.
	time.Sleep(1500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal pid %d: %v", pid, err)
	}

	select {
	case runErr := <-waitErr:
		out := outBuf.String()
		if code := exitCodeOf(t, runErr, []byte(out)); code != 0 {
			t.Errorf("exit code = %d, want 0 (SIGINT during start is a cancellation); output:\n%s", code, out)
		}
		if !strings.Contains(out, "signal during fleet start") {
			t.Errorf("output lacks the signal-during-start line; output:\n%s", out)
		}
		if strings.Contains(out, "DOWN") {
			t.Errorf("output logs DOWN for a cancellation; output:\n%s", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("binary (pid %d) did not exit within 20s of SIGINT; output so far:\n%s", pid, outBuf.String())
	}
}
