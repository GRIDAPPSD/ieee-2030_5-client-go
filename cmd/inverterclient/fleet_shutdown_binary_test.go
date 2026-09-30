// Item 5: the real compiled binary, run end to end, proves the shutdown
// helper (fatalf) actually runs on a fatal exit reached after a fleet has
// started: the fleet sidecar's socket must not be left behind.

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

// TestMain_CreateClientFailureAfterFleetHealthy_RemovesFleetSocket is item
// 5's own test: build the binary, run it from the repo root in the
// aggregator role with a real one-device fleet file, a relative
// --run-dir, and no certificates (this checkout has none at its root), so
// it fails at "create client" after the fleet has become healthy. Without
// fatalf's shutdown step, a sidecar ended only by Pdeathsig does not reach
// its own cleanup and its socket is left behind (design "Not checked"
// section; INFERRED Python SIGTERM behavior, confirmed here by the build
// rather than assumed).
func TestMain_CreateClientFailureAfterFleetHealthy_RemovesFleetSocket(t *testing.T) {
	python := gldsidecarPython(t)
	root := repoRoot(t)

	if _, err := os.Stat(filepath.Join(root, "certs")); err == nil {
		t.Skip("a certs/ directory exists at the repo root; this test needs create-client to fail on missing certs")
	}

	binPath := filepath.Join(t.TempDir(), "inverterclient-test-bin")
	build := exec.Command("go", "build", "-o", binPath, ".")
	// Built from this package's own directory (go test's own cwd), not
	// the repo root: cmd/inverterclient's Go files live here, not there.
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	fleetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fleetDir, "probe.glm"), []byte(probeGLM), 0o600); err != nil {
		t.Fatalf("write glm: %v", err)
	}
	fleetFilePath := filepath.Join(fleetDir, "probe.fleet.json")
	if err := os.WriteFile(fleetFilePath, []byte(probeFleetJSON), 0o600); err != nil {
		t.Fatalf("write fleet file: %v", err)
	}

	// A relative run-dir, physically created at the repo root (so a
	// relative --run-dir resolves there once the subprocess's cwd is
	// root), with a short name: joined with root's own (non-trivial)
	// length and "probe.sock", it must still fit the AF_UNIX sun_path
	// limit.
	runDirAbs, err := os.MkdirTemp(root, "t")
	if err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDirAbs) })
	runDirRel := filepath.Base(runDirAbs)
	sockPath := filepath.Join(runDirAbs, "probe.sock")

	pythonDir := filepath.Dir(python)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath,
		"--client-role=aggregator",
		"--fleet-file="+fleetFilePath,
		"--run-dir="+runDirRel,
		"--notify-listen=", // disabled: no cert to load a listener with either
	)
	cmd.Dir = root
	// PATH must resolve "python3" to the pinned, gridlabd-importable
	// interpreter (production's own default, item 1); prepend it rather
	// than replace PATH, and otherwise pass this process's own
	// environment through.
	cmd.Env = append(os.Environ(), "PATH="+pythonDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, runErr := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if runErr == nil {
		t.Fatalf("binary exited 0, want 1 (create client should have failed on missing certs); output:\n%s", out)
	}
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("run binary: %v (not an ExitError; output:\n%s)", runErr, out)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1; output:\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "create client:") {
		t.Errorf("output does not contain \"create client:\"; output:\n%s", out)
	}
	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Errorf("fleet socket %s still exists after shutdown (stat err: %v); want it removed", sockPath, statErr)
	}
}
