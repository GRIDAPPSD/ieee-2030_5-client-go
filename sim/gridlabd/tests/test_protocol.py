import json

import pytest

from gldsidecar import protocol


def test_parse_valid_request():
    req = protocol.Request.parse('{"id": 1, "op": "hello", "args": {}}')
    assert req.id == 1
    assert req.op == "hello"
    assert req.args == {}


def test_parse_defaults_args_to_empty_dict():
    req = protocol.Request.parse('{"id": 2, "op": "shutdown"}')
    assert req.args == {}


@pytest.mark.parametrize(
    "line,code",
    [
        ("not json", "bad_json"),
        ("[]", "bad_request"),
        ('{"op": "hello"}', "bad_request"),
        ('{"id": 1}', "bad_request"),
        ('{"id": "1", "op": "hello"}', "bad_request"),
        ('{"id": true, "op": "hello"}', "bad_request"),
        ('{"id": 1, "op": 5}', "bad_request"),
        ('{"id": 1, "op": "hello", "args": []}', "bad_request"),
    ],
)
def test_parse_rejects_malformed_requests(line, code):
    with pytest.raises(protocol.ProtocolError) as exc_info:
        protocol.Request.parse(line)
    assert exc_info.value.code == code


def test_encode_ok_round_trips_through_json():
    line = protocol.encode_ok(3, {"a": 1})
    obj = json.loads(line)
    assert obj == {"id": 3, "ok": True, "result": {"a": 1}}


def test_encode_error_round_trips_through_json():
    line = protocol.encode_error(4, "bad_time", "not RFC 3339")
    obj = json.loads(line)
    assert obj == {"id": 4, "ok": False, "error": {"code": "bad_time", "message": "not RFC 3339"}}


@pytest.mark.parametrize("bad", [float("nan"), float("inf"), float("-inf")])
def test_encode_ok_refuses_nan_and_infinity(bad):
    # item 5's NaN decision: allow_nan=False makes this raise rather than
    # emit the non-standard NaN/Infinity/-Infinity token, which Go's
    # encoding/json cannot parse at all. _handle_line (server.py) catches
    # this and turns it into a normal per-request encode_error reply.
    with pytest.raises(ValueError):
        protocol.encode_ok(5, {"value": bad})
