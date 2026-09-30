"""Adapter over gridlabd.GridLabD exposing the protocol's five operations.

gridlabd's calls report failure as a nonzero status code, not a Python
exception: get_property/set_property/step_to all return a status alongside
their value, and step_to accepts a malformed time string with status 0 and
a garbage epoch-second result instead of failing (probed 2026-09-29,
`"not-a-time"` -> `(0, '-9223372036854775808')`). Every call here validates
its own input before it reaches gridlabd and checks every native status.
"""

from __future__ import annotations

import datetime
import importlib.metadata
import re
import sys
from typing import Any

import gridlabd

from . import protocol

# No ^/$ anchors: $ matches before a trailing newline too, so
# "...Z\n" would otherwise pass (probed 2026-09-29). fullmatch() requires
# the pattern to cover the whole string, closing that gap.
_RFC3339_UTC = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z")
_OK = 0


class ModelError(Exception):
    """A gridlabd call reported a nonzero status, or the model does not match the fleet file."""

    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status


def _parse_rfc3339_utc(value: object) -> str:
    if not isinstance(value, str) or not _RFC3339_UTC.fullmatch(value):
        raise protocol.ProtocolError(
            "bad_time", f"expected RFC 3339 UTC (YYYY-MM-DDTHH:MM:SSZ), got {value!r}"
        )
    try:
        datetime.datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ")  # rejects e.g. 2020-02-30
    except ValueError as exc:
        raise protocol.ProtocolError("bad_time", f"not a real UTC instant: {value!r} ({exc})") from exc
    return value


class Adapter:
    """One GridLAB-D model per process. Time only moves through step_to."""

    def __init__(
        self,
        model_path: str,
        fleet: str,
        expected_version: str,
        expected_objects: dict[str, list[str]],
        *,
        native: Any = None,
    ):
        """native is a test seam: an already-"loaded" stand-in for the
        gridlabd.GridLabD instance, so a test can drive a specific native
        status (set_property/step_to/get_property) without needing a real
        gridlabd failure to reproduce it. Production code never passes it.
        """
        self._fleet = fleet
        self._expected_version = expected_version
        self._expected_objects = expected_objects
        if native is not None:
            self._gld = native
            return
        self._gld = gridlabd.GridLabD(verbose=False)
        self._gld.set_working_directory(".")
        self._gld.setup_before_load()
        status = self._gld.load_glm([model_path])
        if status != _OK:
            raise ModelError(status, f"load_glm({model_path!r}) failed, status {status}")
        self._gld.setup_after_load()
        self._gld.start()

    def hello(self) -> dict[str, Any]:
        installed_version = importlib.metadata.version("gridlabd")
        if installed_version != self._expected_version:
            raise ModelError(
                _OK,
                f"fleet file expects gridlabd {self._expected_version}, installed {installed_version}",
            )
        present: dict[str, list[str]] = {}
        for class_name, names in self._expected_objects.items():
            loaded = set(self._gld.get_object_names_by_class(class_name))
            missing = [n for n in names if n not in loaded]
            if missing:
                raise ModelError(_OK, f"model is missing {class_name} objects {missing}")
            present[class_name] = names
        return {
            "protocol": protocol.PROTOCOL_VERSION,
            "gridlabd_version": installed_version,
            "python_version": sys.version.split()[0],
            "fleet": self._fleet,
            "objects": present,
        }

    def set(self, items: list[dict[str, Any]]) -> list[dict[str, Any]]:
        # All-or-nothing: check every (object, property) exists before
        # writing any of them, so a batch that names one bad object never
        # leaves an earlier device's setpoint applied while the rest of
        # the batch is refused (probed 2026-09-29: a bad object amid good
        # ones replied gridlabd_error but had already applied the first).
        # A nonexistent object or property answers the same status (3)
        # from get_property as it would from set_property, so this check
        # is a reliable stand-in for "would this write succeed".
        for item in items:
            obj, prop = item["object"], item["property"]
            status, _ = self._gld.get_property(obj, prop)
            if status != _OK:
                raise ModelError(status, f"set {obj}.{prop} would fail, status {status}")

        applied = []
        for item in items:
            obj, prop, value = item["object"], item["property"], item["value"]
            status = self._gld.set_property(obj, prop, value)
            if status != _OK:
                raise ModelError(status, f"set {obj}.{prop} failed, status {status}")
            read_status, read_value = self._gld.get_property(obj, prop)
            if read_status != _OK:
                raise ModelError(read_status, f"read-back {obj}.{prop} failed, status {read_status}")
            applied.append({"object": obj, "property": prop, "value": read_value})
        return applied

    def step_to(self, time: str) -> str:
        target = _parse_rfc3339_utc(time)
        status, reached = self._gld.step_to(target)
        if status != _OK:
            raise ModelError(status, f"step_to {target} failed, status {status}")
        return reached

    def get(self, items: list[dict[str, Any]]) -> list[dict[str, Any]]:
        results = []
        for item in items:
            obj, prop = item["object"], item["property"]
            status, value = self._gld.get_property(obj, prop)
            if status != _OK:
                raise ModelError(status, f"get {obj}.{prop} failed, status {status}")
            results.append(
                {"object": obj, "property": prop, "value": value, "time": self._gld.get_clock()}
            )
        return results

    def shutdown(self) -> None:
        self._gld.stop()
