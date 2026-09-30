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

// fakeFleetName is what every fakeProcess answers hello.Fleet as by
// default (testSupervisorConfig's SupervisorConfig.Fleet uses the same
// value), since item 2 refuses a hello whose Fleet does not match.
const fakeFleetName = "test"

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
	// instanceID distinguishes one fake "process" from another (e.g.
	// across a restart), for tests that assert a restart did not reuse
	// the crashed instance. Pid() is deliberately NOT this: this fake
	// runs its listener in-process rather than exec'ing a real child, so
	// the genuine OS-level peer of its socket is this test binary itself,
	// and Pid() must answer that (os.Getpid()) for the SO_PEERCRED check
	// in waitForSocket to accept it, the same as it would a real child.
	instanceID int
	pid        int
	// pidOverride, when nonzero, makes Pid() answer this instead of
	// os.Getpid(), so a test can present a peer whose real OS-level
	// identity (os.Getpid(), what SO_PEERCRED actually reports) does not
	// match what Supervisor was told to expect.
	pidOverride int

	mu      sync.Mutex
	ln      net.Listener
	conn    net.Conn
	objects map[[2]string]float64
	model   time.Time

	helloErr *wireErrorBody
	// fleet is what hello answers as HelloResult.Fleet; must match the
	// SupervisorConfig.Fleet of whichever Supervisor dials this fake
	// (fakeFleetName), since item 2 refuses a mismatch.
	fleet string
	// helloProtocol overrides the protocol version hello answers with,
	// when nonzero; 0 (the default) answers ProtocolVersion.
	helloProtocol int

	// ignoreSignal, when true, makes Signal a no-op instead of ending the
	// fake, so a test can prove shutdown escalates to Kill.
	ignoreSignal bool
	// ignoreShutdown, when true, makes the "shutdown" op reply OK but
	// keep the connection (and the fake) alive, simulating a sidecar
	// whose process hangs after acknowledging shutdown.
	ignoreShutdown bool
	signalCount    int32
	killCount      int32

	// workerDeadOn, when set, makes that op return ok:false with code
	// worker_dead and close the connection (protocol.py's contract),
	// proving Client.call's ErrWorkerDead mapping.
	workerDeadOn string

	// stallOp, when set, makes the fake never reply to that op: the
	// handler blocks on killCh, which only die() (Kill, or Signal not
	// ignored) closes. It deliberately does NOT block on reading the
	// connection: a stall that unblocks when the CLIENT gives up and
	// closes its own end would end this fake via that disconnect alone,
	// which is indistinguishable from Supervisor's proc.Wait() path and
	// would let a test pass whether or not the Client.Broken() case in
	// Run is even there.
	stallOp string
	// replyDelay, when nonzero, sleeps before every reply (any op), so a
	// test can prove several concurrent callers queued on one socket each
	// get their own timeout budget starting when they are served, not
	// when they first called.
	replyDelay time.Duration
	// shortReplyBy truncates a "get" or "set" reply by this many items.
	shortReplyBy int
	// reorderReply swaps the first two items of a "get" or "set" reply.
	reorderReply bool

	killCh   chan struct{}
	killOnce sync.Once
	done     chan error
	doneOnce sync.Once
	finished bool

	// reqCounts counts every request handle() actually received, by op:
	// a test proves a caller never wrote to the wire by checking this
	// stays unchanged, which a mere "got an error back" assertion cannot
	// distinguish from "wrote, then the reply was discarded."
	reqCounts map[string]int
}

// reqCount reads how many requests of op this fake has handled so far.
func (fp *fakeProcess) reqCount(op string) int {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.reqCounts[op]
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
			instanceID: int(atomic.AddInt32(&fakePidSeq, 1)),
			pid:        os.Getpid(),
			ln:         ln,
			objects:    map[[2]string]float64{},
			model:      time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			killCh:     make(chan struct{}),
			done:       make(chan error, 1),
			fleet:      fakeFleetName,
		}
		if configure != nil {
			configure(fp)
		}
		reg.add(fp)
		go fp.serve()
		return fp, nil
	}
}

func (fp *fakeProcess) Pid() int {
	if fp.pidOverride != 0 {
		return fp.pidOverride
	}
	return fp.pid
}
func (fp *fakeProcess) InstanceID() int    { return fp.instanceID }
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
	atomic.AddInt32(&fp.signalCount, 1)
	if fp.ignoreSignal {
		// A real process that does not exit on SIGTERM: proves the
		// shutdown sequence actually escalates to SIGKILL instead of
		// assuming SIGTERM always worked.
		return nil
	}
	fp.die(nil)
	return nil
}

func (fp *fakeProcess) Kill() error {
	atomic.AddInt32(&fp.killCount, 1)
	fp.die(nil)
	return nil
}

func (fp *fakeProcess) SignalCount() int { return int(atomic.LoadInt32(&fp.signalCount)) }
func (fp *fakeProcess) KillCount() int   { return int(atomic.LoadInt32(&fp.killCount)) }

// SimulateCrash ends the fake sidecar as an unexpected exit, the shape
// Supervisor's restart-with-backoff path reacts to.
func (fp *fakeProcess) SimulateCrash() {
	fp.die(fmt.Errorf("simulated crash"))
}

func (fp *fakeProcess) die(err error) {
	fp.killOnce.Do(func() { close(fp.killCh) })
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
			if fp.ignoreShutdown {
				// The client gave up on this connection (e.g. closed it
				// after a shutdown RPC this fake deliberately did not
				// close its own end for), but the process is simulated as
				// surviving that: only an explicit Kill or a non-ignored
				// Signal ends it via die().
				return
			}
			fp.finish(nil)
			return
		}
		var req wireRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if fp.stallOp != "" && req.Op == fp.stallOp {
			// Block on killCh, not on reading the connection: this must
			// NOT unblock just because the client gave up and closed its
			// own end (see the stallOp field comment for why).
			<-fp.killCh
			return
		}
		if fp.replyDelay > 0 {
			time.Sleep(fp.replyDelay)
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

	if fp.reqCounts == nil {
		fp.reqCounts = map[string]int{}
	}
	fp.reqCounts[req.Op]++

	if fp.workerDeadOn != "" && req.Op == fp.workerDeadOn {
		return wireReply{ID: req.ID, OK: false, Error: &wireErrorBody{Code: "worker_dead", Message: "worker process is dead"}}, true
	}

	switch req.Op {
	case "hello":
		if fp.helloErr != nil {
			return wireReply{ID: req.ID, OK: false, Error: fp.helloErr}, false
		}
		protocol := fp.helloProtocol
		if protocol == 0 {
			protocol = ProtocolVersion
		}
		result, _ := json.Marshal(HelloResult{
			Protocol:        protocol,
			GridlabdVersion: "fake",
			PythonVersion:   "fake",
			Fleet:           fp.fleet,
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
		if fp.reorderReply && len(out) >= 2 {
			out[0], out[1] = out[1], out[0]
		}
		if fp.shortReplyBy > 0 && fp.shortReplyBy <= len(out) {
			out = out[:len(out)-fp.shortReplyBy]
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
		// ignoreShutdown answers OK but keeps the connection open and the
		// fake "alive": a real sidecar that acknowledges shutdown but
		// whose python process then hangs. Without this, the cooperative
		// RPC alone would end the fake before a test proving SIGTERM/
		// SIGKILL escalation ever reaches Signal or Kill.
		return wireReply{ID: req.ID, OK: true, Result: json.RawMessage("null")}, !fp.ignoreShutdown

	default:
		return wireReply{ID: req.ID, OK: false, Error: &wireErrorBody{Code: "bad_op", Message: "unknown op"}}, false
	}
}
