"""Unix-socket server loop: one connection, line-delimited JSON, processed in order."""

from __future__ import annotations

import os
import socket

from . import protocol
from .adapter import Adapter, ModelError

_RECV_CHUNK = 65536


def _dispatch(adapter: Adapter, req: protocol.Request) -> str:
    try:
        if req.op == "hello":
            result: object = adapter.hello()
        elif req.op == "set":
            result = adapter.set(req.args["items"])
        elif req.op == "step_to":
            result = adapter.step_to(req.args["time"])
        elif req.op == "get":
            result = adapter.get(req.args["items"])
        elif req.op == "shutdown":
            adapter.shutdown()
            result = None
        else:
            return protocol.encode_error(req.id, "bad_op", f"unknown op {req.op!r}")
    except protocol.ProtocolError as exc:
        return protocol.encode_error(req.id, exc.code, exc.message)
    except ModelError as exc:
        return protocol.encode_error(req.id, "gridlabd_error", str(exc))
    except KeyError as exc:
        return protocol.encode_error(req.id, "bad_request", f"missing field {exc}")
    return protocol.encode_ok(req.id, result)


def serve(sock_path: str, adapter: Adapter) -> None:
    """Bind sock_path, accept one connection, serve it until shutdown or EOF."""
    if os.path.exists(sock_path):
        os.unlink(sock_path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        server.bind(sock_path)
        os.chmod(sock_path, 0o600)
        server.listen(1)
        conn, _ = server.accept()
        try:
            _serve_connection(conn, adapter)
        finally:
            conn.close()
    finally:
        server.close()
        if os.path.exists(sock_path):
            os.unlink(sock_path)


def _serve_connection(conn: socket.socket, adapter: Adapter) -> None:
    buf = b""
    while True:
        chunk = conn.recv(_RECV_CHUNK)
        if not chunk:
            return
        buf += chunk
        while b"\n" in buf:
            line, buf = buf.split(b"\n", 1)
            if not line.strip():
                continue
            try:
                req = protocol.Request.parse(line.decode("utf-8"))
            except protocol.ProtocolError as exc:
                conn.sendall((protocol.encode_error(-1, exc.code, exc.message) + "\n").encode("utf-8"))
                continue
            conn.sendall((_dispatch(adapter, req) + "\n").encode("utf-8"))
            if req.op == "shutdown":
                return
