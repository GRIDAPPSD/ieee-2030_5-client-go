// Package gridlabd is the Go side of the fleet sidecar protocol: dialing a
// per-fleet sidecar over a Unix socket, supervising the process, and
// exposing its devices as device.FleetTransport. The wire protocol (one
// request in flight per socket, line-delimited JSON) is defined by the
// Python side, sim/gridlabd/gldsidecar/protocol.py; this file mirrors it.
package gridlabd

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the protocol version this client speaks. It must match
// sim/gridlabd/gldsidecar/protocol.PROTOCOL_VERSION.
const ProtocolVersion = 1

// wireRequest and wireReply are the line-delimited JSON shapes protocol.py
// defines. IDs are int64 so an aggregator that never restarts a connection
// cannot overflow a 32-bit counter across a long run.
type wireRequest struct {
	ID   int64          `json:"id"`
	Op   string         `json:"op"`
	Args map[string]any `json:"args,omitempty"`
}

type wireReply struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireErrorBody  `json:"error,omitempty"`
}

type wireErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RemoteError is a reply the sidecar answered with ok: false, other than a
// worker_dead reply (see ErrWorkerDead).
type RemoteError struct {
	Op      string
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.Op, e.Code, e.Message)
}

// Value is one property value carried on the wire: a plain JSON scalar, or
// gridlabd's complex-number shape {"re":.., "im":..} for quantities such as
// VA_Out (protocol.py's own docstring, IEEE-2030.5-Client probe 2026-09-29).
// Float64 is the only accessor this package needs; a complex reply with a
// nonzero imaginary part is refused rather than silently truncated, per
// data-invariants: a dropped imaginary part would misreport delivered power.
type Value struct {
	raw json.RawMessage
}

// FloatValue wraps a real number for a set request.
func FloatValue(f float64) Value {
	b, err := json.Marshal(f)
	if err != nil {
		// f is a float64; json.Marshal only fails on NaN/Inf, which gridlabd
		// has no notion of accepting either (probed 2026-09-29 against the
		// dead-worker case), so this is a caller bug, not a runtime error.
		panic(fmt.Sprintf("gridlabd: FloatValue(%v): %v", f, err))
	}
	return Value{raw: b}
}

func (v Value) MarshalJSON() ([]byte, error) {
	if v.raw == nil {
		return []byte("null"), nil
	}
	return v.raw, nil
}

func (v *Value) UnmarshalJSON(b []byte) error {
	v.raw = append(json.RawMessage(nil), b...)
	return nil
}

// Float64 returns the value as a real number.
func (v Value) Float64() (float64, error) {
	var f float64
	if err := json.Unmarshal(v.raw, &f); err == nil {
		return f, nil
	}
	var c struct {
		Re float64 `json:"re"`
		Im float64 `json:"im"`
	}
	if err := json.Unmarshal(v.raw, &c); err == nil {
		if c.Im != 0 {
			return 0, fmt.Errorf("value %g+%gi is complex, not real", c.Re, c.Im)
		}
		return c.Re, nil
	}
	return 0, fmt.Errorf("value %s is neither a real number nor a complex object", v.raw)
}
