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

import pytest

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
    glm_path, fleet_path = battery_fleet.write(str(tmp_path), "f", count=1, pen="000000ff")
    with open(fleet_path, encoding="utf-8") as fh:
        fleet_data = json.load(fh)
    adapter = Adapter(
        model_path=glm_path,
        fleet=fleet_data["fleet"],
        expected_version=fleet_data["gridlabd_version"],
        expected_objects=_expected_objects(fleet_data),
    )
    outcome = {}

    def run():
        try:
            serve(str(sock_path), adapter)
            outcome["raised"] = None
        except RuntimeError as exc:
            outcome["raised"] = exc

    # A regression that drops the socket-type guard leaves serve() blocked
    # forever in accept(), since nothing legitimate ever connects to a
    # phantom socket (reproduced 2026-09-29 with an external `timeout`,
    # which pytest itself cannot bound). Running it in a joined-with-timeout
    # thread makes that regression fail this test instead of hanging the
    # whole run.
    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    thread.join(timeout=5)
    assert not thread.is_alive(), "serve() did not return; the socket-type guard likely regressed"
    assert isinstance(outcome.get("raised"), RuntimeError)
    assert sock_path.read_text() == "not a socket"


# Each case: a bad request over the wire, then a good "hello" proving the
# server is still serving. The bad request must never take the loop down.
# The expected code is asserted exactly, not just "some error", so that
# removing _require_items, _require_time or the catch-all changes which
# code comes back and the test catches it (probed 2026-09-29: dropping
# _require_items on "set" turns "bad_request" into "internal_error").
_BAD_REQUESTS = {
    "step_to_invalid_calendar_date": (
        '{"id": 1, "op": "step_to", "args": {"time": "2020-02-30T00:00:00Z"}}\n',
        "bad_time",
    ),
    "step_to_int_time": (
        '{"id": 1, "op": "step_to", "args": {"time": 20200101}}\n',
        "bad_request",
    ),
    "step_to_trailing_newline_in_time": (
        '{"id": 1, "op": "step_to", "args": {"time": "2020-01-01T00:05:00Z\\n"}}\n',
        "bad_time",
    ),
    "set_items_is_a_string": (
        '{"id": 1, "op": "set", "args": {"items": "not-a-list"}}\n',
        "bad_request",
    ),
    "get_items_is_null": (
        '{"id": 1, "op": "get", "args": {"items": null}}\n',
        "bad_request",
    ),
}


def _run_bad_request_case(tmp_path, sock_name, line, expected_code):
    sock_path, server_thread, _ = _start_server(tmp_path, sock_name=sock_name)
    client = _Client(sock_path)
    try:
        client.send_raw(line.encode("utf-8"))
        reply = json.loads(client.recv_line())
        assert reply["ok"] is False
        assert reply["error"]["code"] == expected_code

        hello = client.call("hello")
        assert hello["ok"] is True
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)
    assert not server_thread.is_alive()


def test_step_to_invalid_calendar_date_gets_a_structured_error_and_server_stays_up(tmp_path):
    line, code = _BAD_REQUESTS["step_to_invalid_calendar_date"]
    _run_bad_request_case(tmp_path, "a.sock", line, code)


def test_step_to_int_time_gets_a_structured_error_and_server_stays_up(tmp_path):
    line, code = _BAD_REQUESTS["step_to_int_time"]
    _run_bad_request_case(tmp_path, "b.sock", line, code)


def test_step_to_trailing_newline_in_time_gets_a_structured_error_and_server_stays_up(tmp_path):
    line, code = _BAD_REQUESTS["step_to_trailing_newline_in_time"]
    _run_bad_request_case(tmp_path, "c.sock", line, code)


def test_set_items_as_a_string_gets_a_structured_error_and_server_stays_up(tmp_path):
    line, code = _BAD_REQUESTS["set_items_is_a_string"]
    _run_bad_request_case(tmp_path, "d.sock", line, code)


def test_get_items_null_gets_a_structured_error_and_server_stays_up(tmp_path):
    line, code = _BAD_REQUESTS["get_items_is_null"]
    _run_bad_request_case(tmp_path, "e.sock", line, code)


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


def test_get_of_a_complex_valued_property_encodes_as_re_im(tmp_path):
    # gridlabd returns VA_Out as a native Python complex (probed
    # 2026-09-29: 0j); encoding used to sit outside the handler's try, so
    # json.dumps' TypeError on a complex took the serve loop down instead
    # of ever reaching this reply.
    sock_path, server_thread, fleet_data = _start_server(tmp_path, sock_name="i.sock")
    client = _Client(sock_path)
    try:
        inv = fleet_data["devices"][0]["objects"]["inverter"]
        reply = client.call("get", {"items": [{"object": inv, "property": "VA_Out"}]})
        assert reply["ok"] is True
        assert reply["result"][0]["value"] == {"re": 0.0, "im": 0.0}

        hello = client.call("hello")
        assert hello["ok"] is True
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)
    assert not server_thread.is_alive()


def test_get_of_a_nan_valued_property_gets_a_structured_error_and_server_stays_up(tmp_path, monkeypatch):
    # item 5's NaN decision: gridlabd can return NaN/Infinity for a
    # divergent or uninitialized solve. protocol.encode_ok's allow_nan=False
    # makes that a ValueError at encode time rather than a non-standard
    # NaN/Infinity token on the wire (which Go's encoding/json cannot parse
    # at all, and which used to fail the whole reply's decode and mark the
    # CONNECTION broken over one bad value). This proves the ValueError is
    # caught by the same catch-all as the complex-value TypeError above, and
    # this one request's error does not take the rest of the connection
    # down with it.
    sock_path, server_thread, fleet_data = _start_server(tmp_path, sock_name="k.sock")
    client = _Client(sock_path)
    try:
        real_get = Adapter.get

        def get_returns_nan(self, items):
            results = real_get(self, items)
            results[0]["value"] = float("nan")
            return results

        monkeypatch.setattr(Adapter, "get", get_returns_nan)

        inv = fleet_data["devices"][0]["objects"]["inverter"]
        reply = client.call("get", {"items": [{"object": inv, "property": "P_Out"}]})
        assert reply["ok"] is False
        assert reply["error"]["code"] == "internal_error"

        hello = client.call("hello")
        assert hello["ok"] is True
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)
    assert not server_thread.is_alive()


def test_a_pathologically_nested_line_gets_a_structured_error_and_server_stays_up(tmp_path):
    # json.loads raises RecursionError, not JSONDecodeError, on deeply
    # nested input (probed 2026-09-29: 200000 nested '['); Request.parse
    # used to sit outside the catch-all, so this took the serve loop down.
    sock_path, server_thread, _ = _start_server(tmp_path, sock_name="j.sock")
    client = _Client(sock_path)
    try:
        client.send_raw(("[" * 200000).encode("utf-8") + b"\n")
        reply = json.loads(client.recv_line())
        assert reply["ok"] is False
        assert reply["error"]["code"] == "bad_json"

        hello = client.call("hello")
        assert hello["ok"] is True
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)
    assert not server_thread.is_alive()


class _DeadWorkerNative:
    """Every call raises the exact exception gridlabd raises once its
    worker subprocess has died (probed 2026-09-29, reproduced by driving
    a real model with a bare step() past its last event): a plain
    RuntimeError reading "Worker process closed stdout while processing
    <OP>.", the same shape for every op and with no dedicated class."""

    def _dead(self, op):
        raise RuntimeError(f"Worker process closed stdout while processing {op}.")

    def get_property(self, obj, prop):
        self._dead("GET_PROPERTY")

    def set_property(self, obj, prop, value):
        self._dead("SET_PROPERTY")

    def step_to(self, time):
        self._dead("STEP_TO")

    def get_clock(self):
        return "2020-01-01T00:00:00"

    def get_object_names_by_class(self, class_name):
        return []

    def stop(self):
        pass


def test_a_dead_worker_gets_its_own_error_code_and_the_connection_closes(tmp_path, capsys):
    adapter = Adapter(
        model_path="unused",
        fleet="f",
        expected_version="6.0.0a1",
        expected_objects={},
        native=_DeadWorkerNative(),
    )
    sock_path = str(tmp_path / "k.sock")
    thread = threading.Thread(target=serve, args=(sock_path, adapter), daemon=True)
    thread.start()
    for _ in range(200):
        if os.path.exists(sock_path):
            break
        threading.Event().wait(0.05)

    client = _Client(sock_path)
    try:
        reply = client.call("step_to", {"time": "2020-01-01T00:05:00Z"})
        assert reply["ok"] is False
        assert reply["error"]["code"] == "worker_dead"

        # Chosen: the model cannot be recovered in-process once the
        # worker is dead, so the connection closes after this one
        # diagnostic reply rather than answering worker_dead forever;
        # recv() blocks until that close arrives, then returns EOF.
        with pytest.raises(EOFError):
            client.recv_line()
    finally:
        client.close()
    thread.join(timeout=10)
    assert not thread.is_alive()

    captured = capsys.readouterr()
    assert "worker_dead" in captured.err


def test_set_reply_echoes_the_native_read_back_value_not_the_request(tmp_path):
    # Sending an int (no decimal point in the JSON) but asserting the
    # reply carries a float kills a mutant that echoes item["value"]
    # back unchanged: gridlabd's own get_property always returns a float
    # for P_Out (probed 2026-09-29: set 500 (int) -> read back 500.0),
    # so an echo would come back as the int 500, not 500.0.
    sock_path, server_thread, fleet_data = _start_server(tmp_path, sock_name="l.sock")
    client = _Client(sock_path)
    try:
        inv = fleet_data["devices"][0]["objects"]["inverter"]
        reply = client.call("set", {"items": [{"object": inv, "property": "P_Out", "value": 500}]})
        assert reply["ok"] is True
        assert isinstance(reply["result"][0]["value"], float)
        assert reply["result"][0]["value"] == 500.0

        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)


def test_listen_runs_before_chmod_not_after(tmp_path, monkeypatch):
    # Records the call order rather than timing a race: a client polling
    # for the socket file's existence connects as soon as bind() creates
    # it, so anything between bind() and listen() is a window where that
    # connect gets refused. Proves listen() is not delayed behind chmod().
    import gldsidecar.server as server_module

    calls = []
    real_listen = socket.socket.listen
    real_chmod = os.chmod

    def recording_listen(self, *args, **kwargs):
        calls.append("listen")
        return real_listen(self, *args, **kwargs)

    def recording_chmod(path, mode):
        calls.append("chmod")
        return real_chmod(path, mode)

    monkeypatch.setattr(socket.socket, "listen", recording_listen)
    monkeypatch.setattr(server_module.os, "chmod", recording_chmod)

    sock_path, server_thread, _ = _start_server(tmp_path, sock_name="n.sock")
    client = _Client(sock_path)
    try:
        client.call("shutdown")
    finally:
        client.close()
    server_thread.join(timeout=10)

    assert calls == ["listen", "chmod"]
