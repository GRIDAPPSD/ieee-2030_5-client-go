"""Sidecar entry point: python -m gldsidecar --socket <path> --fleet-file <path>."""

from __future__ import annotations

import argparse
import json
import os
from typing import Sequence

from .adapter import Adapter
from .server import serve


def _load_fleet_file(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def _expected_objects(fleet: dict) -> dict[str, list[str]]:
    expected: dict[str, list[str]] = {}
    for device in fleet["devices"]:
        for class_name, obj_name in device["objects"].items():
            expected.setdefault(class_name, []).append(obj_name)
    return expected


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="gldsidecar")
    parser.add_argument("--socket", required=True, help="Unix socket path to serve on")
    parser.add_argument("--fleet-file", required=True, help="fleet JSON written by the generator")
    args = parser.parse_args(argv)

    fleet = _load_fleet_file(args.fleet_file)
    fleet_dir = os.path.dirname(os.path.abspath(args.fleet_file))
    glm_path = os.path.join(fleet_dir, fleet["glm"])

    adapter = Adapter(
        model_path=glm_path,
        fleet=fleet["fleet"],
        expected_version=fleet["gridlabd_version"],
        expected_objects=_expected_objects(fleet),
    )
    serve(args.socket, adapter)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
