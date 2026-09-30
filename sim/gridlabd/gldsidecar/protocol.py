"""Line-delimited JSON request/reply protocol, version 1.

One request in flight per socket, processed in order. Request:
{"id": <int>, "op": <string>, "args": {...}}. Reply:
{"id": <int>, "ok": true, "result": ...} or
{"id": <int>, "ok": false, "error": {"code": ..., "message": ...}}.

A complex value (gridlabd returns one for VA_Out and similar power
quantities, probed 2026-09-29: 0j, type complex) encodes as the object
{"re": <float>, "im": <float>}, never as a bare JSON number and never by
dropping the imaginary part.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any

PROTOCOL_VERSION = 1


class ProtocolError(Exception):
    """A request could not be decoded, or named an op that never reaches the model."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


@dataclass
class Request:
    id: int
    op: str
    args: dict[str, Any]

    @classmethod
    def parse(cls, line: str) -> "Request":
        try:
            obj = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ProtocolError("bad_json", str(exc)) from exc
        if not isinstance(obj, dict):
            raise ProtocolError("bad_request", "request must be a JSON object")
        if "id" not in obj or "op" not in obj:
            raise ProtocolError("bad_request", "request needs id and op")
        req_id, op = obj["id"], obj["op"]
        if not isinstance(req_id, int) or isinstance(req_id, bool):
            raise ProtocolError("bad_request", "id must be an integer")
        if not isinstance(op, str):
            raise ProtocolError("bad_request", "op must be a string")
        args = obj.get("args", {})
        if not isinstance(args, dict):
            raise ProtocolError("bad_request", "args must be an object")
        return cls(id=req_id, op=op, args=args)


def _json_default(obj: Any) -> Any:
    if isinstance(obj, complex):
        return {"re": obj.real, "im": obj.imag}
    raise TypeError(f"object of type {type(obj).__name__} is not JSON serializable")


# allow_nan=False on both encoders: item 5's NaN decision. gridlabd can
# return NaN or +/-Infinity for a property (a divergent or uninitialized
# solve), and Python's json module, by default, would write the
# non-standard tokens NaN/Infinity/-Infinity for those, which is not valid
# JSON. Go's encoding/json rejects those tokens outright, which used to
# fail the whole reply's decode and mark the CONNECTION broken
# (ErrConnectionBroken) over one bad value. allow_nan=False makes json.dumps
# raise ValueError instead of emitting the token; _handle_line's existing
# catch-all (server.py) turns that into a normal encode_error reply for
# just this request, so a NaN reading surfaces as a per-request error, not
# a torn-down connection every other in-flight or future call also pays for.
def encode_ok(req_id: int, result: Any) -> str:
    return json.dumps({"id": req_id, "ok": True, "result": result}, default=_json_default, allow_nan=False)


def encode_error(req_id: int, code: str, message: str) -> str:
    return json.dumps(
        {"id": req_id, "ok": False, "error": {"code": code, "message": message}},
        default=_json_default,
        allow_nan=False,
    )
