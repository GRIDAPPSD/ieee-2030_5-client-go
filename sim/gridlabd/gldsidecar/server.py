"""Unix-socket server loop: one connection, line-delimited JSON, processed in order."""

from __future__ import annotations

import os
import socket
import stat
import sys

from . import protocol
from .adapter import Adapter, ModelError

_RECV_CHUNK = 65536

# gridlabd raises a plain RuntimeError once its worker subprocess has
# died, the same exception type for every call and with no dedicated
# class to catch (probed 2026-09-29: "Worker process closed stdout
# while processing STEP_TO." after driving a model past its crash
# condition). The model cannot be recovered in-process once this fires,
# so it gets its own error code and its own connection-closing
# response, distinct from a normal internal_error.
_WORKER_DEAD_MARKER = "Worker process"


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


def _is_worker_dead(exc: Exception) -> bool:
    return isinstance(exc, RuntimeError) and _WORKER_DEAD_MARKER in str(exc)


def _log_internal_error(req_id: int, code: str, exc: Exception) -> None:
    # Without this, a dead worker (or any other unexpected exception)
    # answered every call with a generic reply and left 0 lines on
    # stderr (probed 2026-09-29), which is indistinguishable from the
    # sidecar being silently idle.
    print(f"gldsidecar: {code} on request {req_id}: {type(exc).__name__}: {exc}", file=sys.stderr, flush=True)


def _handle_line(adapter: Adapter, text: str) -> tuple[str, bool]:
    """Decode, dispatch and encode one request; never raises. Returns
    (reply_line, stop): stop is True after a successful shutdown, or
    after the gridlabd worker is found dead, since neither leaves
    anything further this connection can usefully do."""
    req_id = -1
    try:
        req = protocol.Request.parse(text)
        req_id = req.id
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
            return protocol.encode_ok(req.id, None), True
        else:
            return protocol.encode_error(req.id, "bad_op", f"unknown op {req.op!r}"), False
        return protocol.encode_ok(req.id, result), False
    except protocol.ProtocolError as exc:
        return protocol.encode_error(req_id, exc.code, exc.message), False
    except ModelError as exc:
        return protocol.encode_error(req_id, "gridlabd_error", str(exc)), False
    except KeyError as exc:
        return protocol.encode_error(req_id, "bad_request", f"missing field {exc}"), False
    except RecursionError:
        # json.loads raises RecursionError, not JSONDecodeError, on
        # pathologically nested input (probed 2026-09-29: 200000 nested
        # '['), and recovers cleanly the same way any other Exception
        # subclass does (probed the same way). It is malformed client
        # input, not an internal bug, so it gets the same code
        # Request.parse already uses for unparseable JSON, and is not
        # logged as an internal error.
        return protocol.encode_error(req_id, "bad_json", "request is too deeply nested"), False
    except Exception as exc:
        # Last resort: nothing from parse, dispatch or encode above may
        # take the serve loop down.
        if _is_worker_dead(exc):
            _log_internal_error(req_id, "worker_dead", exc)
            return protocol.encode_error(req_id, "worker_dead", str(exc)), True
        _log_internal_error(req_id, "internal_error", exc)
        return protocol.encode_error(req_id, "internal_error", f"{type(exc).__name__}: {exc}"), False


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
    """Bind sock_path, accept one connection, serve it until shutdown,
    a dead worker, or EOF."""
    _unlink_stale_socket(sock_path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        server.bind(sock_path)
        # listen() immediately after bind(): a client polling for the
        # socket file's existence connects as soon as it sees it, so
        # anything between bind() and listen() is a window where that
        # connect gets refused. chmod moves after listen so it cannot
        # widen that window.
        server.listen(1)
        os.chmod(sock_path, 0o600)
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
            reply, stop = _handle_line(adapter, text)
            conn.sendall((reply + "\n").encode("utf-8"))
            if stop:
                return
