"""Line-delimited JSON request/reply protocol, version 1.

One request in flight per socket, processed in order. Request:
{"id": <int>, "op": <string>, "args": {...}}. Reply:
{"id": <int>, "ok": true, "result": ...} or
{"id": <int>, "ok": false, "error": {"code": ..., "message": ...}}.
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


def encode_ok(req_id: int, result: Any) -> str:
    return json.dumps({"id": req_id, "ok": True, "result": result})


def encode_error(req_id: int, code: str, message: str) -> str:
    return json.dumps({"id": req_id, "ok": False, "error": {"code": code, "message": message}})
