// Item 1 (round 5): a real crash loop, through startFleets, proves the
// DOWN transition logs exactly once now that Supervisor is the one layer
// that owns the line (fleet_start.go no longer logs it itself). This
// needs no gridlabd or the internal package's own unexported test fakes,
// which cmd/inverterclient cannot reach: a minimal Python responder,
// using only the standard library, answers hello once then exits,
// reproducing "healthy, then gone" on every restart attempt with a real
// exec'd process.

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/sim/gridlabd"
)

const crashLoopFleetName = "crashfleet"

// helloOnceThenExitScript binds the socket, answers exactly one hello
// with this test's fleet name and protocol version, then exits 1. It
// needs no gridlabd import (only json, os, socket, sys), so it runs
// under any python3 without the pinned venv this package's other real-
// sidecar tests need.
const helloOnceThenExitScript = `
import json, os, socket, sys
sock_path = sys.argv[sys.argv.index("--socket") + 1]
try:
    os.unlink(sock_path)
except FileNotFoundError:
    pass
srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
srv.bind(sock_path)
srv.listen(1)
conn, _ = srv.accept()
buf = b""
while b"\n" not in buf:
    chunk = conn.recv(4096)
    if not chunk:
        sys.exit(1)
    buf += chunk
line, _, _ = buf.partition(b"\n")
req = json.loads(line)
reply = {
    "id": req["id"],
    "ok": True,
    "result": {
        "protocol": 1,
        "gridlabd_version": "test",
        "python_version": "test",
        "fleet": "` + crashLoopFleetName + `",
        "objects": {},
    },
}
conn.sendall((json.dumps(reply) + "\n").encode())
conn.close()
srv.close()
sys.exit(1)
`

func writeHelloOnceScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hello_once.py")
	if err := os.WriteFile(path, []byte(helloOnceThenExitScript), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// crashLoopLogSpy collects every cfg.Log call, safe for concurrent use.
type crashLoopLogSpy struct {
	mu    sync.Mutex
	lines []string
}

func (s *crashLoopLogSpy) log(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

// stdLogWriter adapts the spy to an io.Writer for log.SetOutput.
type stdLogWriter struct{ spy *crashLoopLogSpy }

func (w stdLogWriter) Write(p []byte) (int, error) {
	w.spy.log("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (s *crashLoopLogSpy) lines_() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.lines))
	copy(out, s.lines)
	return out
}

// TestStartFleets_RealCrashLoop_LogsExactlyOneDownLine is item 1: with
// Supervisor.setDown the sole owner of the "fleet <name> DOWN" line
// (fleet_start.go no longer logs it), a real MaxRestarts-then-exhausted
// crash loop, driven entirely through startFleets exactly as main() drives
// it, must produce exactly one line, not one from each layer.
func TestStartFleets_RealCrashLoop_LogsExactlyOneDownLine(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("no python3 on PATH: %v", err)
	}
	script := writeHelloOnceScript(t)

	sockDir, err := os.MkdirTemp(os.TempDir(), "gld")
	if err != nil {
		t.Fatalf("mkdir sock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	spy := &crashLoopLogSpy{}
	// A DOWN line written through package log (what main's own logger
	// prints) must be counted too: a second layer logging it there would
	// otherwise slip past a spy that only sees cfg.Log.
	stdSpy := &crashLoopLogSpy{}
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(stdLogWriter{stdSpy})
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	cfg := gridlabd.SupervisorConfig{
		Fleet:         crashLoopFleetName,
		SocketPath:    filepath.Join(sockDir, "a.sock"),
		FleetFilePath: "unused.json",
		Command:       []string{python, script},
		Env:           gridlabd.DefaultEnv(),
		StartTimeout:  5 * time.Second,
		CallTimeout:   3 * time.Second,
		StopGrace:     200 * time.Millisecond,
		BackoffMin:    5 * time.Millisecond,
		BackoffMax:    20 * time.Millisecond,
		MaxRestarts:   2,
		RestartWindow: time.Minute,
		Log:           spy.log,
	}
	sup := gridlabd.NewSupervisor(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stop, err := startFleets(ctx, []fleetSupervisor{sup})
	if err != nil {
		t.Fatalf("startFleets: %v", err)
	}
	defer stop()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if state, _ := sup.State(); state == gridlabd.StateDown {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("State() did not reach StateDown within 15s (last: %v)", func() gridlabd.SupervisorState { s, _ := sup.State(); return s }())
		}
		time.Sleep(10 * time.Millisecond)
	}

	downLines := 0
	for _, line := range append(spy.lines_(), stdSpy.lines_()...) {
		if strings.Contains(line, "fleet "+crashLoopFleetName+" DOWN: ") {
			downLines++
		}
	}
	if downLines != 1 {
		t.Errorf("DOWN log lines = %d, want exactly 1 (cfg.Log: %v; package log: %v)", downLines, spy.lines_(), stdSpy.lines_())
	}
}
