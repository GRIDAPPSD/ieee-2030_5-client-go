"""End-to-end: a real Unix socket, a real gridlabd model, every op in the table."""

import json
import socket
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
        while b"\n" not in self._buf:
            self._buf += self._sock.recv(65536)
        reply_line, self._buf = self._buf.split(b"\n", 1)
        reply = json.loads(reply_line)
        assert reply["id"] == req_id
        return reply

    def close(self):
        self._sock.close()


def test_full_op_sequence_over_the_socket(tmp_path):
    glm_path, fleet_path = battery_fleet.write(str(tmp_path), "srv_fleet", count=2, pen="00000099")
    with open(fleet_path, encoding="utf-8") as f:
        fleet_data = json.load(f)

    adapter = Adapter(
        model_path=glm_path,
        fleet=fleet_data["fleet"],
        expected_version=fleet_data["gridlabd_version"],
        expected_objects=_expected_objects(fleet_data),
    )
    sock_path = str(tmp_path / "srv_fleet.sock")
    server_thread = threading.Thread(target=serve, args=(sock_path, adapter), daemon=True)
    server_thread.start()

    # serve() only starts accepting once bound; poll for the socket file
    # rather than sleeping a fixed guess.
    for _ in range(200):
        if __import__("os").path.exists(sock_path):
            break
        threading.Event().wait(0.05)

    client = _Client(sock_path)
    try:
        hello = client.call("hello")
        assert hello["ok"] is True
        assert hello["result"]["gridlabd_version"] == "6.0.0a1"
        assert hello["result"]["fleet"] == "srv_fleet"

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
