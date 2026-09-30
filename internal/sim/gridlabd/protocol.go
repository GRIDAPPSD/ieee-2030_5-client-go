// Package gridlabd is the Go side of the fleet sidecar protocol: dialing a
// per-fleet sidecar over a Unix socket, supervising the process, and
// exposing its devices as device.FleetTransport. The wire protocol (one
// request in flight per socket, line-delimited JSON) is defined by the
// Python side, sim/gridlabd/gldsidecar/protocol.py; this file mirrors it.
package gridlabd

import (
	"encoding/json"
	"fmt"
	"math"
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
// VA_Out or a phase voltage (protocol.py's own docstring, probed
// 2026-09-29). Float64 and Magnitude are the two accessors: Float64 for a
// genuinely scalar property, Magnitude for a phasor one.
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

// components decodes the wire value strictly: a JSON number is a real pair
// (re, 0); a JSON object is the complex shape only when it carries both
// "re" and "im" as numbers. Anything else, including null (which
// encoding/json leaves a *float64 destination silently unchanged, the
// classic Go gotcha) and {} (which decodes into a struct as zero values
// with no error), is refused here rather than read as a false 0. That is
// the fix for a short or malformed reply reading as 0 instead of erroring.
func (v Value) components() (re, im float64, err error) {
	var raw any
	if err := json.Unmarshal(v.raw, &raw); err != nil {
		return 0, 0, fmt.Errorf("value %s is not valid JSON: %w", v.raw, err)
	}
	switch x := raw.(type) {
	case float64:
		return x, 0, nil
	case map[string]any:
		reRaw, hasRe := x["re"]
		imRaw, hasIm := x["im"]
		if !hasRe || !hasIm {
			return 0, 0, fmt.Errorf("value %s is an object but not the complex {re,im} shape", v.raw)
		}
		reF, reOK := reRaw.(float64)
		imF, imOK := imRaw.(float64)
		if !reOK || !imOK {
			return 0, 0, fmt.Errorf("value %s has non-numeric re/im", v.raw)
		}
		return reF, imF, nil
	default:
		return 0, 0, fmt.Errorf("value %s is neither a real number nor a complex object", v.raw)
	}
}

// Float64 returns the value as a real number. A complex value with a
// nonzero imaginary part is refused rather than silently truncated, per
// data-invariants: a dropped imaginary part would misreport delivered
// power. Use for a genuinely scalar property (P_Out, Q_Out, rated_power).
func (v Value) Float64() (float64, error) {
	re, im, err := v.components()
	if err != nil {
		return 0, err
	}
	if im != 0 {
		return 0, fmt.Errorf("value %g+%gi is complex, not real", re, im)
	}
	return re, nil
}

// Magnitude returns sqrt(re^2+im^2): the correct reading for a phasor
// quantity such as a phase voltage, where gridlabd's own representation
// may carry a nonzero angle and only the magnitude is wanted.
func (v Value) Magnitude() (float64, error) {
	re, im, err := v.components()
	if err != nil {
		return 0, err
	}
	return math.Hypot(re, im), nil
}
