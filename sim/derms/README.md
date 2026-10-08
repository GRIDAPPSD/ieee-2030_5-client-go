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
