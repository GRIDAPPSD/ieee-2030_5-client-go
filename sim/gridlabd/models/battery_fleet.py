"""Generates a battery fleet GLM and its fleet-file entries.

Each device's LFDI is 128 "random" bits with the aggregator's PEN
right-concatenated (IEEE 2030.5-2023 S23 L2691-2693). The 128 bits come from
SHA-256(fleet, index, seed) rather than a true RNG so two generator runs
with the same arguments write byte-identical fleets (the issue's "stable
across runs" criterion); a device's identity depends only on its own
index, never on how many devices the fleet holds, so appending a device
never reshuffles the earlier ones.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os

GRIDLABD_VERSION = "6.0.0a1"

# CONSTANT_PQ holds an externally set P_Out/Q_Out across step_to; the class
# default, CONSTANT_PF, recomputes P_Out from power_factor on the next step
# and silently discards a setpoint (verified 2026-09-29).
_DEVICE_TEMPLATE = """\
object inverter {{
  name {inv_name};
  parent fleet_meter;
  phases ABCN;
  generator_status ONLINE;
  four_quadrant_control_mode CONSTANT_PQ;
  inverter_type FOUR_QUADRANT;
  rated_power {rated_power};
  P_Out 0;
  Q_Out 0;
}}
object battery {{
  name {bat_name};
  parent {inv_name};
  use_internal_battery_model TRUE;
  battery_type LI_ION;
  battery_capacity {battery_capacity};
  state_of_charge {state_of_charge};
  round_trip_efficiency 0.95;
  rated_power {rated_power};
  V_Max 480;
  base_efficiency 0.95;
}}
"""

_GLM_HEADER = """\
clock {{
  starttime '{starttime}';
  stoptime '{stoptime}';
}}
module powerflow;
module generators;

object meter {{
  name fleet_meter;
  phases ABCN;
  nominal_voltage 120;
}}
"""


def _lfdi(fleet_name: str, index: int, seed: str, pen: str) -> str:
    if len(pen) != 8:
        raise ValueError(f"pen must be 8 hex digits (32 bits), got {pen!r}")
    int(pen, 16)  # raises ValueError when pen is not hex
    digest = hashlib.sha256(f"{fleet_name}:{index}:{seed}".encode("utf-8")).digest()
    random_128 = digest[:16].hex()
    return (random_128 + pen.lower()).upper()


def generate(
    fleet_name: str,
    count: int,
    pen: str,
    seed: str = "",
    rated_power: int = 5000,
    battery_capacity: int = 10000,
    state_of_charge: float = 0.5,
    starttime: str = "2020-01-01 00:00:00",
    stoptime: str = "2020-01-01 02:00:00",
) -> tuple[str, dict]:
    """Return (glm_text, fleet_file_dict) for a battery fleet of count devices."""
    devices = []
    body = [_GLM_HEADER.format(starttime=starttime, stoptime=stoptime)]
    for i in range(count):
        inv_name = f"{fleet_name}_bat{i}_inv"
        bat_name = f"{fleet_name}_bat{i}"
        body.append(
            _DEVICE_TEMPLATE.format(
                inv_name=inv_name,
                bat_name=bat_name,
                rated_power=rated_power,
                battery_capacity=battery_capacity,
                state_of_charge=state_of_charge,
            )
        )
        devices.append(
            {
                "name": f"{fleet_name}-{i:03d}",
                "lfdi": _lfdi(fleet_name, i, seed, pen),
                "objects": {"inverter": inv_name, "battery": bat_name},
            }
        )
    glm_text = "\n".join(body)
    fleet_file = {
        "fleet": fleet_name,
        "gridlabd_version": GRIDLABD_VERSION,
        "aggregator_pen": pen.lower(),
        "glm": f"{fleet_name}.glm",
        "devices": devices,
    }
    return glm_text, fleet_file


def write(out_dir: str, fleet_name: str, count: int, pen: str, seed: str = "") -> tuple[str, str]:
    """Write the GLM and fleet-file JSON under out_dir; return their paths."""
    os.makedirs(out_dir, exist_ok=True)
    glm_text, fleet_file = generate(fleet_name, count, pen, seed)
    glm_path = os.path.join(out_dir, fleet_file["glm"])
    fleet_path = os.path.join(out_dir, f"{fleet_name}.fleet.json")
    with open(glm_path, "w", encoding="utf-8") as f:
        f.write(glm_text)
    with open(fleet_path, "w", encoding="utf-8") as f:
        json.dump(fleet_file, f, indent=2)
        f.write("\n")
    return glm_path, fleet_path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="battery_fleet")
    parser.add_argument("--fleet", required=True, help="fleet name, used as the object-name prefix")
    parser.add_argument("--count", type=int, required=True, help="number of battery devices")
    parser.add_argument("--pen", required=True, help="aggregator's 32-bit PEN, 8 hex digits")
    parser.add_argument("--seed", default="", help="extra seed material for LFDI derivation")
    parser.add_argument("--out", required=True, help="output directory for the GLM and fleet file")
    args = parser.parse_args(argv)
    glm_path, fleet_path = write(args.out, args.fleet, args.count, args.pen, args.seed)
    print(glm_path)
    print(fleet_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
