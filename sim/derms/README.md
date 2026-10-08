# sim/derms

A three-device GridLAB-D model and the recordings the `replay` backend of
`inverterclient` reports. The recordings are committed so a run needs no
GridLAB-D; `regenerate.sh` rebuilds them.

| Device | Object | Rating | Recording | Holds |
|---|---|---|---|---|
| `agg-bat-1` | `agg_bat1` on `agg_bat1_inv` | 5 kW, 10 kWh | `recordings/agg-bat-1.csv` | `battery_load` (charging positive) |
| `agg-pv-1` | `agg_pv1_inv` | 4 kW | `recordings/agg-pv-1.csv` | inverter `P_Out` (delivering positive) |
| `solo-pv-1` | `solo_pv1_inv` | 3 kW | `recordings/solo-pv-1.csv` | inverter `P_Out` (delivering positive) |

Recorded with GridLAB-D 5.3.0 (`recordings/GRIDLABD_VERSION` holds the
exact build line). Each file has one row per minute of the day. The replay
backend reads the row for the current minute of day (UTC), or for the
minutes since start with `replay.clock` set to `start`, and loops daily.

The battery baseline is `battery_load`, not the inverter's `P_Out`:
GridLAB-D keeps recording the commanded `P_Out` after the battery is full or
empty, while `battery_load` goes to 0. The backend reports a charging
battery as negative power, the DER convention.

Run `./regenerate.sh` with GridLAB-D 5.3.0 on `PATH` (or set `GRIDLABD`).
It refuses any other version, and a rerun writes byte-identical files.

`solo-pv-1.example.json` is a sim config for the standalone PV device:
`inverterclient --sim-config sim/derms/solo-pv-1.example.json ...`.

## Protocol decisions

Where each choice in the aggregator's managed-device sessions comes from.

* DER output reports flowDirection 19. Source: IEEE 2030.5-2018 Table E.2,
  which gives 19 for DER active power and no other value.
* A charging battery reports flowDirection 1. Source: FlowDirectionType
  alone; Table E.2 does not give a value for charging, so this is a reading
  of that type, not a table row.
* The reading carries the magnitude and flowDirection carries the sign. This
  is this client's choice, not a clause of the standard; issue 72.
* The aggregator acts for a managed device by naming its LFDI in the request
  and in the mirror. Source: issue 72 (a design choice, not a clause).
* A recording whose timestamps are not UTC is refused. Source: review of #100.
* Config keys are written lowercase and matched case-insensitively. Source:
  operator decision, 2026-10-07.
* The flow reservation `reserve` trigger, with command-line overrides of the
  config defaults. Source: operator decision, 2026-10-07, issue 73.
