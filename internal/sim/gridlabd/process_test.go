package gridlabd

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestStartExecProcess_WaitReturnsDespiteChildHoldingStderrOpen is item 3's
// WaitDelay case: exec.Cmd's Wait() also waits for its Stderr pipe to
// reach EOF, and a backgrounded grandchild that inherits that pipe (no
// redirection of its own) keeps the write end open long after the direct
// child we launched has exited, which would otherwise make Wait() block
// for as long as the grandchild lives rather than for how long our own
// process ran. cmd.WaitDelay (process.go) bounds that: Wait must return
// within roughly waitDelayAfterExit of the process exiting, not wait out
// the grandchild.
func TestStartExecProcess_WaitReturnsDespiteChildHoldingStderrOpen(t *testing.T) {
	p, err := startExecProcess(context.Background(), execOptions{
		// The direct child exits at once; the backgrounded sleep inherits
		// its stderr (no redirection) and holds it open for 30s, far
		// longer than waitDelayAfterExit (5s).
		Command: []string{"sh", "-c", "sleep 30 & exit 0"},
		Env:     DefaultEnv(),
		Stderr:  io.Discard,
	}, nil)
	if err != nil {
		t.Fatalf("startExecProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	start := time.Now()
	select {
	case <-p.Wait():
	case <-time.After(waitDelayAfterExit + 5*time.Second):
		t.Fatal("Wait() did not return within waitDelayAfterExit plus a margin; the grandchild's held-open stderr pipe blocked it")
	}
	elapsed := time.Since(start)
	if elapsed > waitDelayAfterExit+3*time.Second {
		t.Errorf("Wait() took %v, want at most roughly waitDelayAfterExit (%v) plus a margin", elapsed, waitDelayAfterExit)
	}
}
