package gridlabd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortSockDir returns a fresh, short-named directory for a Unix socket,
// removed at test cleanup. t.TempDir() embeds the test's name, which is
// long enough in this package (descriptive Go test names) to push a
// socket path past the AF_UNIX 108-byte limit; MkdirTemp's short "gld*"
// prefix avoids that instead of hoping the ambient TMPDIR is short. Unlike
// a counter-based name, MkdirTemp cannot collide with a directory a prior,
// abnormally-terminated test run left behind under the same TMPDIR.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "gld")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeProcess is an in-process stand-in for the sidecar: it implements
// sidecarProcess AND serves the real protocol over a Unix socket, so
// Client and Supervisor tests exercise the actual wire format without
// needing python. Only the real-sidecar test (sidecar_integration_test.go)
// runs the genuine gldsidecar.
type fakeProcess struct {
	pid int

	mu      sync.Mutex
	ln      net.Listener
	conn    net.Conn
	objects map[[2]string]float64
	model   time.Time

	helloErr *wireErrorBody

	// stallOp, when set, makes the fake never reply to that op: the
	// handler blocks on reading the connection (which the client never
	// writes more to while awaiting a reply) until the client gives up
	// and closes it, simulating an unresponsive sidecar.
	stallOp string
	// shortReplyBy truncates a "get" reply by this many items.
	shortReplyBy int
	// reorderReply swaps the first two items of a "get" reply.
	reorderReply bool

	done     chan error
	doneOnce sync.Once
	finished bool
}

func extractArg(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

var fakePidSeq int32

// fakeRegistry collects every fakeProcess a launcher creates, safe for a
// test goroutine to read concurrently with the launcher's own goroutine.
type fakeRegistry struct {
	mu      sync.Mutex
	created []*fakeProcess
}

func (r *fakeRegistry) add(fp *fakeProcess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created = append(r.created, fp)
}

func (r *fakeRegistry) list() []*fakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeProcess(nil), r.created...)
}

// newFakeLauncher returns a processLauncher whose sidecars are fakeProcess
// instances. configure runs on each one right after it starts listening,
// so a test can preset hello errors or object values before Supervisor
// dials it. reg records every fakeProcess built, in order, so a test can
// reach into one (e.g. to simulate a crash).
func newFakeLauncher(t *testing.T, configure func(*fakeProcess), reg *fakeRegistry) processLauncher {
	return func(_ context.Context, args []string) (sidecarProcess, error) {
		sockPath := extractArg(args, "--socket")
		_ = os.Remove(sockPath)
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			return nil, err
		}
		fp := &fakeProcess{
			pid:     int(atomic.AddInt32(&fakePidSeq, 1)),
			ln:      ln,
			objects: map[[2]string]float64{},
			model:   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			done:    make(chan error, 1),
		}
		if configure != nil {
			configure(fp)
		}
		reg.add(fp)
		go fp.serve()
		return fp, nil
	}
}

func (fp *fakeProcess) Pid() int           { return fp.pid }
func (fp *fakeProcess) Wait() <-chan error { return fp.done }

// Finished reports whether the process has exited, safe to poll from a
// test repeatedly. Wait's channel is single-shot per the sidecarProcess
// contract (Supervisor itself never reads it twice for one process), so a
// test that wants to observe exit independently of Supervisor's own read
// uses this instead of racing Supervisor for the one value on done.
func (fp *fakeProcess) Finished() bool {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.finished
}

func (fp *fakeProcess) Signal(os.Signal) error {
	fp.die(nil)
	return nil
}

func (fp *fakeProcess) Kill() error {
	fp.die(nil)
	return nil
}

// SimulateCrash ends the fake sidecar as an unexpected exit, the shape
// Supervisor's restart-with-backoff path reacts to.
func (fp *fakeProcess) SimulateCrash() {
	fp.die(fmt.Errorf("simulated crash"))
}

func (fp *fakeProcess) die(err error) {
	fp.mu.Lock()
	if fp.conn != nil {
		_ = fp.conn.Close()
	}
	_ = fp.ln.Close()
	fp.mu.Unlock()
	fp.finish(err)
}

func (fp *fakeProcess) finish(err error) {
	fp.doneOnce.Do(func() {
		fp.mu.Lock()
		fp.finished = true
		fp.mu.Unlock()
		fp.done <- err
	})
}

func (fp *fakeProcess) serve() {
	conn, err := fp.ln.Accept()
	if err != nil {
		fp.finish(nil)
		return
	}
	fp.mu.Lock()
	fp.conn = conn
	fp.mu.Unlock()

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			fp.finish(nil)
			return
		}
		var req wireRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if fp.stallOp != "" && req.Op == fp.stallOp {
			// Block on a read the client never satisfies (it is waiting on
			// its own read for our reply), until the client gives up and
			// closes the connection.
			buf := make([]byte, 1)
			_, _ = conn.Read(buf)
			fp.finish(nil)
			return
		}
		reply, stop := fp.handle(req)
		b, _ := json.Marshal(reply)
		if _, err := conn.Write(append(b, '\n')); err != nil {
			fp.finish(nil)
			return
		}
		if stop {
			_ = conn.Close()
			fp.finish(nil)
			return
		}
	}
}

func (fp *fakeProcess) handle(req wireRequest) (wireReply, bool) {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	switch req.Op {
	case "hello":
		if fp.helloErr != nil {
			return wireReply{ID: req.ID, OK: false, Error: fp.helloErr}, false
		}
		result, _ := json.Marshal(HelloResult{
			Protocol:        ProtocolVersion,
			GridlabdVersion: "fake",
			PythonVersion:   "fake",
			Fleet:           "fake-fleet",
			Objects:         map[string][]string{},
		})
		return wireReply{ID: req.ID, OK: true, Result: result}, false

	case "set":
		items, _ := req.Args["items"].([]any)
		var out []map[string]any
		for _, raw := range items {
			m := raw.(map[string]any)
			obj, prop := m["object"].(string), m["property"].(string)
			val, _ := m["value"].(float64)
			fp.objects[[2]string{obj, prop}] = val
			out = append(out, map[string]any{"object": obj, "property": prop, "value": val})
		}
		result, _ := json.Marshal(out)
		return wireReply{ID: req.ID, OK: true, Result: result}, false

	case "get":
		items, _ := req.Args["items"].([]any)
		var out []map[string]any
		for _, raw := range items {
			m := raw.(map[string]any)
			obj, prop := m["object"].(string), m["property"].(string)
			out = append(out, map[string]any{
				"object": obj, "property": prop,
				"value": fp.objects[[2]string{obj, prop}],
				"time":  fp.model.Format(modelTimeLayout),
			})
		}
		if fp.reorderReply && len(out) >= 2 {
			out[0], out[1] = out[1], out[0]
		}
		if fp.shortReplyBy > 0 && fp.shortReplyBy <= len(out) {
			out = out[:len(out)-fp.shortReplyBy]
		}
		result, _ := json.Marshal(out)
		return wireReply{ID: req.ID, OK: true, Result: result}, false

	case "step_to":
		target, err := time.Parse(stepToInputLayout, req.Args["time"].(string))
		if err != nil {
			return wireReply{ID: req.ID, OK: false, Error: &wireErrorBody{Code: "bad_time", Message: err.Error()}}, false
		}
		if target.After(fp.model) {
			fp.model = target
		}
		result, _ := json.Marshal(fp.model.Format(modelTimeLayout))
		return wireReply{ID: req.ID, OK: true, Result: result}, false

	case "shutdown":
		return wireReply{ID: req.ID, OK: true, Result: json.RawMessage("null")}, true

	default:
		return wireReply{ID: req.ID, OK: false, Error: &wireErrorBody{Code: "bad_op", Message: "unknown op"}}, false
	}
}
