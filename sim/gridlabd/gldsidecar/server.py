"""Unix-socket server loop: one connection, line-delimited JSON, processed in order."""

from __future__ import annotations

import os
import socket
import stat

from . import protocol
from .adapter import Adapter, ModelError

_RECV_CHUNK = 65536


def _require_items(args: dict) -> list:
    items = args.get("items")
    if not isinstance(items, list):
        raise protocol.ProtocolError("bad_request", "items must be a list")
    for item in items:
        if not isinstance(item, dict):
            raise protocol.ProtocolError("bad_request", "each item must be an object")
    return items


def _require_time(args: dict) -> str:
    time = args.get("time")
    if not isinstance(time, str):
        raise protocol.ProtocolError("bad_request", "time must be a string")
    return time


def _dispatch(adapter: Adapter, req: protocol.Request) -> str:
    try:
        if req.op == "hello":
            result: object = adapter.hello()
        elif req.op == "set":
            result = adapter.set(_require_items(req.args))
        elif req.op == "step_to":
            result = adapter.step_to(_require_time(req.args))
        elif req.op == "get":
            result = adapter.get(_require_items(req.args))
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
    except Exception as exc:
        # Last resort: an op handler must never take the serve loop down.
        # Every named case above should already be caught more precisely;
        # this is the backstop for whatever the next one turns out to be.
        return protocol.encode_error(req.id, "internal_error", f"{type(exc).__name__}: {exc}")
    return protocol.encode_ok(req.id, result)


def _unlink_stale_socket(sock_path: str) -> None:
    """Remove a leftover socket at sock_path. Refuse anything that is not
    actually a socket (a regular file, or a symlink to one), so a stray
    or planted file at this path is never silently deleted."""
    try:
        mode = os.lstat(sock_path).st_mode
    except FileNotFoundError:
        return
    if not stat.S_ISSOCK(mode):
        raise RuntimeError(f"refusing to remove {sock_path}: existing path is not a socket")
    os.unlink(sock_path)


def serve(sock_path: str, adapter: Adapter) -> None:
    """Bind sock_path, accept one connection, serve it until shutdown or EOF."""
    _unlink_stale_socket(sock_path)
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
        _unlink_stale_socket(sock_path)


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
                text = line.decode("utf-8")
            except UnicodeDecodeError as exc:
                conn.sendall((protocol.encode_error(-1, "bad_encoding", str(exc)) + "\n").encode("utf-8"))
                continue
            try:
                req = protocol.Request.parse(text)
            except protocol.ProtocolError as exc:
                conn.sendall((protocol.encode_error(-1, exc.code, exc.message) + "\n").encode("utf-8"))
                continue
            conn.sendall((_dispatch(adapter, req) + "\n").encode("utf-8"))
            if req.op == "shutdown":
                return
