package gridlabd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ErrWorkerDead marks a reply whose error code is "worker_dead": the
// gridlabd worker subprocess died and the sidecar closes the connection
// right after this reply (server.py's stop=True). The model cannot be
// recovered on this connection; the caller must redial after a restart.
var ErrWorkerDead = errors.New("gridlabd worker process is dead")

// ErrConnectionBroken marks a Client whose wire state can no longer be
// trusted: a timeout, a decode error, a reply id mismatch, or worker_dead.
// In every one of those cases a reply for the abandoned request might
// still arrive later, so the connection is closed and refused for any
// further call rather than risking a later call reading that stale reply.
var ErrConnectionBroken = errors.New("gridlabd connection is broken")

// timeLayout is the format the sidecar's model clock returns from step_to
// and get: no trailing Z, no offset (probed 2026-09-29 against a running
// sidecar: get_clock() returned "2020-01-01T00:01:00"). This differs from
// the RFC 3339 UTC form (trailing Z) the adapter requires on step_to's
// input.
const (
	stepToInputLayout = "2006-01-02T15:04:05Z"
	modelTimeLayout   = "2006-01-02T15:04:05"
)

// Client speaks the fleet sidecar protocol over one Unix socket connection.
// The protocol allows one request in flight per socket, processed in
// order, so Client serializes calls with a mutex rather than assuming the
// caller will.
type Client struct {
	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	nextID int64

	timeout     time.Duration // bounds every call when ctx carries no earlier deadline
	broken      chan struct{}
	brokenOnce  sync.Once
	brokenCause error // set once, before broken is closed; read only after <-Broken()
}

// Dial connects to a sidecar listening on sockPath. ctx bounds the dial
// only. timeout bounds every subsequent call whose own ctx carries no
// earlier deadline, so a caller with a bare context.Background() still
// gets a bounded round trip rather than blocking forever. timeout must be
// positive: it is also the only bound on how long Close can wait behind a
// drain (a detached caller's abandoned reply still being read off the
// wire, client.go's call and drain), and a zero or negative value would
// leave that wait unbounded (error-handling finding at 377eb1b: Close
// still blocked after 3s against a zero timeout; production never passes
// one).
func Dial(ctx context.Context, sockPath string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("dial %s: timeout must be positive, got %s", sockPath, timeout)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", sockPath, err)
	}
	return &Client{conn: conn, reader: bufio.NewReader(conn), timeout: timeout, broken: make(chan struct{})}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}

// Broken returns a channel that is closed once this connection can no
// longer be trusted and must not be reused; see ErrConnectionBroken.
func (c *Client) Broken() <-chan struct{} { return c.broken }

// BrokenReason returns why Broken fired (nil until it has). Safe to call
// any time after a receive from Broken(): brokenCause is written before
// the close that receive observed, and channel close/receive is a
// happens-before edge, so no further synchronization is needed here.
func (c *Client) BrokenReason() error { return c.brokenCause }

// markBroken records cause, closes broken (once) and closes the
// connection, so any I/O blocked on it (this call's own goroutine, or a
// concurrent one queued behind c.mu) unblocks immediately instead of
// waiting out a timeout.
func (c *Client) markBroken(cause error) {
	c.brokenOnce.Do(func() {
		c.brokenCause = cause
		close(c.broken)
		_ = c.conn.Close() // forces any blocked Read/Write on this conn to return
	})
}

type callResult struct {
	line string
	err  error
}

// objProp is one (object, property) pair, used only to compare a reply's
// per-item identity against the request's, in order: Set and Get each build
// a want and a got slice of these from their own typed items and results.
type objProp struct {
	Object   string
	Property string
}

// validateReplyMatchesRequest checks a decoded reply's items against the
// request's, in order: the count must match and no item may be reordered or
// substituted. Set and Get both enforce this identically, since either
// reply is never read positionally and a short, long or reordered one is
// always an error, never a silent partial result.
func validateReplyMatchesRequest(op string, want, got []objProp) error {
	if len(got) != len(want) {
		return fmt.Errorf("%s: sidecar replied with %d results for %d items", op, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("%s: result %d is %s.%s, want %s.%s (reordered or mismatched reply)",
				op, i, got[i].Object, got[i].Property, want[i].Object, want[i].Property)
		}
	}
	return nil
}

// decodeReply parses one reply line for request id, and returns its
// result or an error: *RemoteError for an ordinary refusal (the wire
// stayed in sync: one request got one matching reply), or one wrapping
// ErrConnectionBroken or ErrWorkerDead when a decode failure, an id
// mismatch, or worker_dead means the connection can no longer be trusted.
// Shared between call's own reply path and drain's (below): both apply
// exactly the same rule to decide whether this connection is still good
// for the next caller.
func (c *Client) decodeReply(op string, id int64, line string) (json.RawMessage, error) {
	var reply wireReply
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		cause := fmt.Errorf("decode %s reply: %w: %w", op, ErrConnectionBroken, err)
		c.markBroken(cause)
		return nil, cause
	}
	if reply.ID != id {
		cause := fmt.Errorf("%s reply id %d does not match request id %d: %w", op, reply.ID, id, ErrConnectionBroken)
		c.markBroken(cause)
		return nil, cause
	}
	if !reply.OK {
		code, msg := "", ""
		if reply.Error != nil {
			code, msg = reply.Error.Code, reply.Error.Message
		}
		if code == "worker_dead" {
			cause := fmt.Errorf("%s: %w: %w: %s", op, ErrConnectionBroken, ErrWorkerDead, msg)
			c.markBroken(cause)
			return nil, cause
		}
		return nil, &RemoteError{Op: op, Code: code, Message: msg}
	}
	return reply.Result, nil
}

// drain finishes a call whose caller has already given up on it (call's
// ctx.Done() path, below): the request for id was already written, so
// exactly one reply for it is still owed on the wire, and it must be read
// before this connection may serve another request, or the next caller
// desyncs (reads this abandoned reply as its own). call hands drain the
// connection lock rather than releasing it, so drain is the only thing
// touching the wire until this reply is accounted for.
//
// No timer of its own: the conn deadline was already set to c.timeout
// from when the request was written (call, below), and the read this
// waits on already honors that deadline at the socket level, so drain's
// own wait is naturally bounded by "up to c.timeout from when the request
// was written" (design Decision 3) with no separate clock to keep in
// sync with it.
//
// Design Decision 3: the connection breaks only when this drain fails
// (the bound above passes, the reply does not decode, its id does not
// match, or it is worker_dead); a clean, matching reply is decoded and
// discarded, and the connection stays good for the next caller. A caller
// cancelling its own round trip must never, by itself, cost the fleet a
// restart.
func (c *Client) drain(op string, id int64, done <-chan callResult) {
	defer c.mu.Unlock()
	res := <-done
	if res.err != nil {
		cause := fmt.Errorf("%s: %w: %w", op, ErrConnectionBroken, res.err)
		c.markBroken(cause)
		return
	}
	// Discarded: the caller that owned this id is long gone, and
	// decodeReply already marks the connection broken on the only
	// outcomes this drain needs to act on (a bad decode, a mismatched
	// id, or worker_dead); a clean, matching reply needs no further
	// action beyond being read off the wire.
	_, _ = c.decodeReply(op, id, res.line)
}

// call sends one request and returns its result, or an error: *RemoteError
// for an ordinary refusal (the wire stays in sync: one request got one
// reply), or one wrapping ErrConnectionBroken or ErrWorkerDead when it does
// not.
//
// Every call is bounded even when ctx carries no deadline of its own
// (c.timeout), and ctx.Done() is honored directly by running the round
// trip in a goroutine and selecting on it: SetDeadline alone only reacts
// to wall-clock expiry, never to an explicit cancel.
//
// The protocol serializes every call behind c.mu (one request in flight
// per socket), so a caller can queue behind another call already in
// progress. Two things follow, and both matter for N devices sharing one
// fleet's connection: c.timeout is applied only after the lock is held,
// so time spent queued is never charged against this call's own budget;
// and ctx is checked again right after the lock, so a caller whose own
// ctx expired while queued is answered with its ctx error and nothing
// else, never touching the wire or marking the connection broken.
//
// Design Decision 3 (detach and drain, not break): once the request is
// written, this caller's OWN ctx being cancelled no longer breaks the
// connection. call returns the caller's error at once and hands the
// connection lock to drain (above), which finishes reading the one reply
// still owed on the wire. The conn deadline is set from c.timeout ALONE,
// computed once here before the write, never from the caller's own
// (possibly shorter) ctx deadline: a short caller deadline must end only
// THIS caller's wait, never make the socket read itself fail and break
// the connection for whoever is queued behind it. A Set whose ctx errors
// this way has an UNKNOWN outcome: the sidecar may have applied the
// write; see Set's own doc comment.
func (c *Client) call(ctx context.Context, op string, args map[string]any) (json.RawMessage, error) {
	select {
	case <-c.broken:
		return nil, fmt.Errorf("%s: %w", op, ErrConnectionBroken)
	default:
	}

	c.mu.Lock()

	select {
	case <-c.broken:
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", op, ErrConnectionBroken)
	default:
	}

	// Answer a caller whose own ctx is already done now, before this call
	// has written anything: the connection stays exactly as it was, so
	// there is nothing to break, and nothing to drain either.
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	id := c.nextID
	c.nextID++
	req := wireRequest{ID: id, Op: op, Args: args}
	line, err := json.Marshal(req)
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	if c.timeout > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
			cause := fmt.Errorf("%s: %w: set deadline: %w", op, ErrConnectionBroken, err)
			c.markBroken(cause)
			c.mu.Unlock()
			return nil, cause
		}
	}

	done := make(chan callResult, 1)
	go func() {
		if _, err := c.conn.Write(append(line, '\n')); err != nil {
			done <- callResult{err: fmt.Errorf("write %s request: %w", op, err)}
			return
		}
		replyLine, err := c.reader.ReadString('\n')
		if err != nil {
			done <- callResult{err: fmt.Errorf("read %s reply: %w", op, err)}
			return
		}
		done <- callResult{line: replyLine}
	}()

	var res callResult
	select {
	case res = <-done:
	case <-ctx.Done():
		cerr := ctx.Err()
		go c.drain(op, id, done) // takes over c.mu; releases it once this reply is accounted for
		return nil, fmt.Errorf("%s: %w", op, cerr)
	}
	defer c.mu.Unlock()

	if res.err != nil {
		cause := fmt.Errorf("%s: %w: %w", op, ErrConnectionBroken, res.err)
		c.markBroken(cause)
		return nil, cause
	}
	return c.decodeReply(op, id, res.line)
}

// HelloResult is the sidecar's answer to hello: its versions, the fleet
// name it loaded, and the object names present per class.
type HelloResult struct {
	Protocol        int                 `json:"protocol"`
	GridlabdVersion string              `json:"gridlabd_version"`
	PythonVersion   string              `json:"python_version"`
	Fleet           string              `json:"fleet"`
	Objects         map[string][]string `json:"objects"`
}

// Hello performs the handshake. A version or object mismatch against the
// fleet file the sidecar loaded comes back as a *RemoteError with code
// "gridlabd_error", not a transport failure.
func (c *Client) Hello(ctx context.Context) (HelloResult, error) {
	raw, err := c.call(ctx, "hello", map[string]any{"protocol": ProtocolVersion})
	if err != nil {
		return HelloResult{}, err
	}
	var res HelloResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return HelloResult{}, fmt.Errorf("decode hello result: %w", err)
	}
	return res, nil
}

// SetItem is one property write.
type SetItem struct {
	Object   string
	Property string
	Value    Value
}

// SetResult is the value the sidecar actually applied, read back after the
// write (protocol.py: the sidecar reads each property back rather than
// echoing the request).
type SetResult struct {
	Object   string
	Property string
	Value    Value
}

// Set writes every item, all-or-nothing (the adapter restores any already
// applied item if a later one in the batch fails).
//
// If ctx is done while the request is on the wire, Set returns ctx's own
// error, and the OUTCOME IS UNKNOWN: the sidecar may have already applied
// the write before this caller gave up on hearing back (call, in
// client.go, detaches rather than breaking the connection to find out).
// The caller must not read a ctx error here as "not applied". The design
// this relies on: every ApplySetpoint tick re-sends the caller's current
// setpoint regardless of the previous tick's outcome, so a lost
// acknowledgement converges on the next tick rather than needing its own
// retry or reconciliation logic.
func (c *Client) Set(ctx context.Context, items []SetItem) ([]SetResult, error) {
	wireItems := make([]map[string]any, len(items))
	for i, it := range items {
		wireItems[i] = map[string]any{"object": it.Object, "property": it.Property, "value": it.Value}
	}
	raw, err := c.call(ctx, "set", map[string]any{"items": wireItems})
	if err != nil {
		return nil, err
	}
	var results []struct {
		Object   string `json:"object"`
		Property string `json:"property"`
		Value    Value  `json:"value"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("decode set result: %w", err)
	}
	want := make([]objProp, len(items))
	for i, it := range items {
		want[i] = objProp{it.Object, it.Property}
	}
	got := make([]objProp, len(results))
	for i, r := range results {
		got[i] = objProp{r.Object, r.Property}
	}
	if err := validateReplyMatchesRequest("set", want, got); err != nil {
		return nil, err
	}
	out := make([]SetResult, len(results))
	for i, r := range results {
		out[i] = SetResult{Object: r.Object, Property: r.Property, Value: r.Value}
	}
	return out, nil
}

// GetItem names one property to read.
type GetItem struct {
	Object   string
	Property string
}

// GetResult is one read value plus the model time it held at (ModelTime is
// the raw sidecar clock string; the caller compares it, it does not parse
// it, since nothing in this package's own logic needs a parsed per-item
// time: StepTo's return value is the timestamp that matters to a caller).
type GetResult struct {
	Object    string
	Property  string
	Value     Value
	ModelTime string
}

// Get reads every item. A nonexistent object or property fails the whole
// call (protocol.py: get_property's status 3 covers both). The reply is
// checked against the request: a short, long or reordered reply is an
// error, never read positionally and never a silent 0.
func (c *Client) Get(ctx context.Context, items []GetItem) ([]GetResult, error) {
	wireItems := make([]map[string]any, len(items))
	for i, it := range items {
		wireItems[i] = map[string]any{"object": it.Object, "property": it.Property}
	}
	raw, err := c.call(ctx, "get", map[string]any{"items": wireItems})
	if err != nil {
		return nil, err
	}
	var results []struct {
		Object   string `json:"object"`
		Property string `json:"property"`
		Value    Value  `json:"value"`
		Time     string `json:"time"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("decode get result: %w", err)
	}
	want := make([]objProp, len(items))
	for i, it := range items {
		want[i] = objProp{it.Object, it.Property}
	}
	got := make([]objProp, len(results))
	for i, r := range results {
		got[i] = objProp{r.Object, r.Property}
	}
	if err := validateReplyMatchesRequest("get", want, got); err != nil {
		return nil, err
	}
	out := make([]GetResult, len(results))
	for i, r := range results {
		out[i] = GetResult{Object: r.Object, Property: r.Property, Value: r.Value, ModelTime: r.Time}
	}
	return out, nil
}

// StepTo advances the model to target, which must be UTC; always call
// this with an explicit time, never a bare step. A target not after the
// model's current time is a no-op that returns the current time
// (protocol.py, probed 2026-09-29), so calling StepTo more than once for
// the same tick is safe.
func (c *Client) StepTo(ctx context.Context, target time.Time) (time.Time, error) {
	raw, err := c.call(ctx, "step_to", map[string]any{"time": target.UTC().Format(stepToInputLayout)})
	if err != nil {
		return time.Time{}, err
	}
	var reached string
	if err := json.Unmarshal(raw, &reached); err != nil {
		return time.Time{}, fmt.Errorf("decode step_to result: %w", err)
	}
	t, err := time.Parse(modelTimeLayout, reached)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse step_to result %q: %w", reached, err)
	}
	return t.UTC(), nil
}

// Shutdown asks the sidecar to stop gridlabd cleanly. The sidecar closes
// the connection right after replying (server.py's stop=True).
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.call(ctx, "shutdown", nil)
	return err
}
