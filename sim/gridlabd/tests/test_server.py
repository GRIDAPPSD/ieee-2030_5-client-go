"""End-to-end: a real Unix socket, a real gridlabd model, every op in the table.

Every malformed-input test proves two things: the request gets a structured
error reply, and the socket is still serving afterward (a follow-up good
call succeeds). Fixing only the first half would leave a server that dies
quietly on the next line.
"""

import json
import os
import socket
import stat
import threading

from gldsidecar.adapter import Adapter
from gldsidecar.server import serve
from models import battery_fleet


def _expected_objects(fleet_data):
    expected = {}
    for device in fleet_data["devices"]:
        for class_name, obj_name in device["objects"].items():
            expected.setdefault(class_name, []).append(obj_name)
    return expected


class _Client:
    def __init__(self, sock_path):
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.settimeout(10)
        self._sock.connect(sock_path)
        self._buf = b""
        self._next_id = 0

    def call(self, op, args=None):
        req_id = self._next_id
        self._next_id += 1
        line = json.dumps({"id": req_id, "op": op, "args": args or {}})
        self._sock.sendall((line + "\n").encode("utf-8"))
        reply = json.loads(self.recv_line())
        assert reply["id"] == req_id
        return reply

    def send_raw(self, data: bytes) -> None:
        self._sock.sendall(data)

    def recv_line(self) -> bytes:
        while b"\n" not in self._buf:
            # recv() on a closed peer returns b"" immediately rather than
            # blocking, so this must not just loop on an empty chunk: that
            # spins the CPU forever instead of ever hitting the socket
            # timeout (probed 2026-09-29).
            chunk = self._sock.recv(65536)
            if not chunk:
                raise EOFError("server closed the connection before a full reply arrived")
            self._buf += chunk
        line, self._buf = self._buf.split(b"\n", 1)
        return line

    def close(self):
        self._sock.close()


def _start_server(tmp_path, sock_name="s.sock", count=1, fleet_name="f"):
    glm_path, fleet_path = battery_fleet.write(str(tmp_path), fleet_name, count=count, pen="000000ff")
    with open(fleet_path, encoding="utf-8") as f:
        fleet_data = json.load(f)
    adapter = Adapter(
        model_path=glm_path,
        fleet=fleet_data["fleet"],
        expected_version=fleet_data["gridlabd_version"],
        expected_objects=_expected_objects(fleet_data),
    )
    sock_path = str(tmp_path / sock_name)
    thread = threading.Thread(target=serve, args=(sock_path, adapter), daemon=True)
    thread.start()
    # serve() only starts accepting once bound; poll for the socket file
    # rather than sleeping a fixed guess.
    for _ in range(200):
        if os.path.exists(sock_path):
            break
        threading.Event().wait(0.05)
    return sock_path, thread, fleet_data


def test_full_op_sequence_over_the_socket(tmp_path):
    sock_path, server_thread, fleet_data = _start_server(tmp_path, count=2)
    client = _Client(sock_path)
    try:
        hello = client.call("hello")
        assert hello["ok"] is True
        assert hello["result"]["gridlabd_version"] == "6.0.0a1"
        assert hello["result"]["fleet"] == fleet_data["fleet"]

        inv = fleet_data["devices"][0]["objects"]["inverter"]
        set_reply = client.call("set", {"items": [{"object": inv, "property": "P_Out", "value": 777.0}]})
        assert set_reply["ok"] is True
        assert set_reply["result"][0]["value"] == 777.0

        step_reply = client.call("step_to", {"time": "2020-01-01T00:05:00Z"})
        assert step_reply["ok"] is True
        assert step_reply["result"] == "2020-01-01T00:05:00"

        get_reply = client.call("get", {"items": [{"object": inv, "property": "P_Out"}]})
        assert get_reply["ok"] is True
        assert get_reply["result"][0]["value"] == 777.0

        bad_op = client.call("no_such_op")
        assert bad_op["ok"] is False
        assert bad_op["error"]["code"] == "bad_op"

        shutdown_reply = client.call("shutdown")
        assert shutdown_reply["ok"] is True
    finally:
        client.close()

    server_thread.join(timeout=10)
    assert not server_thread.is_alive()


def test_socket_is_created_with_owner_only_permissions(tmp_path):
    sock_path, server_thread, _ = _start_server(tmp_path)
    client = _Client(sock_path)
    try:
        mode = os.stat(sock_path).st_mode
        assert stat.S_ISSOCK(mode)
        assert stat.S_IMODE(mode) == 0o600
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)


def test_serve_refuses_to_replace_a_regular_file_at_the_socket_path(tmp_path):
    sock_path = tmp_path / "s.sock"
    sock_path.write_text("not a socket")
    adapter_dir = tmp_path
    glm_path, fleet_path = battery_fleet.write(str(adapter_dir), "f", count=1, pen="000000ff")
    with open(fleet_path, encoding="utf-8") as fh:
        fleet_data = json.load(fh)
    adapter = Adapter(
        model_path=glm_path,
        fleet=fleet_data["fleet"],
        expected_version=fleet_data["gridlabd_version"],
        expected_objects=_expected_objects(fleet_data),
    )
    try:
        serve(str(sock_path), adapter)
        raised = False
    except RuntimeError:
        raised = True
    assert raised
    assert sock_path.read_text() == "not a socket"


# Each case: a bad request over the wire, then a good "hello" proving the
# server is still serving. The bad request must never take the loop down.
_BAD_REQUESTS = {
    "step_to_invalid_calendar_date": (
        '{"id": 1, "op": "step_to", "args": {"time": "2020-02-30T00:00:00Z"}}\n'
    ),
    "step_to_int_time": '{"id": 1, "op": "step_to", "args": {"time": 20200101}}\n',
    "step_to_trailing_newline_in_time": (
        '{"id": 1, "op": "step_to", "args": {"time": "2020-01-01T00:05:00Z\\n"}}\n'
    ),
    "set_items_is_a_string": '{"id": 1, "op": "set", "args": {"items": "not-a-list"}}\n',
    "get_items_is_null": '{"id": 1, "op": "get", "args": {"items": null}}\n',
}


def _run_bad_request_case(tmp_path, sock_name, line):
    sock_path, server_thread, _ = _start_server(tmp_path, sock_name=sock_name)
    client = _Client(sock_path)
    try:
        client.send_raw(line.encode("utf-8"))
        reply = json.loads(client.recv_line())
        assert reply["ok"] is False
        assert "code" in reply["error"]

        hello = client.call("hello")
        assert hello["ok"] is True
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)
    assert not server_thread.is_alive()


def test_step_to_invalid_calendar_date_gets_a_structured_error_and_server_stays_up(tmp_path):
    _run_bad_request_case(tmp_path, "a.sock", _BAD_REQUESTS["step_to_invalid_calendar_date"])


def test_step_to_int_time_gets_a_structured_error_and_server_stays_up(tmp_path):
    _run_bad_request_case(tmp_path, "b.sock", _BAD_REQUESTS["step_to_int_time"])


def test_step_to_trailing_newline_in_time_gets_a_structured_error_and_server_stays_up(tmp_path):
    _run_bad_request_case(tmp_path, "c.sock", _BAD_REQUESTS["step_to_trailing_newline_in_time"])


def test_set_items_as_a_string_gets_a_structured_error_and_server_stays_up(tmp_path):
    _run_bad_request_case(tmp_path, "d.sock", _BAD_REQUESTS["set_items_is_a_string"])


def test_get_items_null_gets_a_structured_error_and_server_stays_up(tmp_path):
    _run_bad_request_case(tmp_path, "e.sock", _BAD_REQUESTS["get_items_is_null"])


def test_invalid_utf8_line_gets_a_structured_error_and_server_stays_up(tmp_path):
    sock_path, server_thread, _ = _start_server(tmp_path, sock_name="g.sock")
    client = _Client(sock_path)
    try:
        client.send_raw(b"\xff\xfe not valid utf-8 \n")
        reply = json.loads(client.recv_line())
        assert reply["ok"] is False
        assert reply["error"]["code"] == "bad_encoding"

        hello = client.call("hello")
        assert hello["ok"] is True
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)
    assert not server_thread.is_alive()


def test_set_does_not_apply_earlier_items_when_a_later_item_is_bad(tmp_path):
    sock_path, server_thread, fleet_data = _start_server(tmp_path, sock_name="h.sock")
    client = _Client(sock_path)
    try:
        inv = fleet_data["devices"][0]["objects"]["inverter"]
        before = client.call("get", {"items": [{"object": inv, "property": "P_Out"}]})
        assert before["ok"] is True
        initial_value = before["result"][0]["value"]

        batch = client.call(
            "set",
            {
                "items": [
                    {"object": inv, "property": "P_Out", "value": 4242.0},
                    {"object": "no_such_object", "property": "P_Out", "value": 1.0},
                ]
            },
        )
        assert batch["ok"] is False
        assert batch["error"]["code"] == "gridlabd_error"

        after = client.call("get", {"items": [{"object": inv, "property": "P_Out"}]})
        assert after["ok"] is True
        assert after["result"][0]["value"] == initial_value

        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)
