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
}

// Dial connects to a sidecar listening on sockPath. ctx bounds the dial
// only; per-call deadlines are set by Call's ctx.
func Dial(ctx context.Context, sockPath string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", sockPath, err)
	}
	return &Client{conn: conn, reader: bufio.NewReader(conn)}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}

// call sends one request and returns its result, or an error: *RemoteError
// for an ordinary refusal, or one wrapping ErrWorkerDead when the sidecar
// reports its worker died.
func (c *Client) call(ctx context.Context, op string, args map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++
	req := wireRequest{ID: id, Op: op, Args: args}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		if err := c.conn.SetDeadline(dl); err != nil {
			return nil, fmt.Errorf("set %s deadline: %w", op, err)
		}
	} else {
		_ = c.conn.SetDeadline(time.Time{})
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("write %s request: %w", op, err)
	}
	replyLine, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read %s reply: %w", op, err)
	}
	var reply wireReply
	if err := json.Unmarshal([]byte(replyLine), &reply); err != nil {
		return nil, fmt.Errorf("decode %s reply: %w", op, err)
	}
	if reply.ID != id {
		return nil, fmt.Errorf("%s reply id %d does not match request id %d", op, reply.ID, id)
	}
	if !reply.OK {
		code, msg := "", ""
		if reply.Error != nil {
			code, msg = reply.Error.Code, reply.Error.Message
		}
		if code == "worker_dead" {
			return nil, fmt.Errorf("%s: %w: %s", op, ErrWorkerDead, msg)
		}
		return nil, &RemoteError{Op: op, Code: code, Message: msg}
	}
	return reply.Result, nil
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
// call (protocol.py: get_property's status 3 covers both).
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
	out := make([]GetResult, len(results))
	for i, r := range results {
		out[i] = GetResult{Object: r.Object, Property: r.Property, Value: r.Value, ModelTime: r.Time}
	}
	return out, nil
}

// StepTo advances the model to target, which must be UTC (ADR-009: always
// step_to an explicit time, never a bare step). A target not after the
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
