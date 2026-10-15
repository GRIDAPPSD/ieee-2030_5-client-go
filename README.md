# ieee-2030_5-client

[![ci](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/ci.yml)
[![CodeQL](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/github-code-scanning/codeql)
[![Go 1.26.8](https://img.shields.io/badge/Go-1.26.8-00ADD8?logo=go)](https://go.dev/)
[![License: Battelle BSD](https://img.shields.io/badge/License-Battelle_BSD-blue.svg)](LICENSE)

No GitHub release is published yet. The latest tag is `v0.1.0-client-on-core`.

IEEE 2030.5 (SEP2) Go client and inverter simulator.

## What this is

`ieee-2030_5-client` is a DER client and inverter simulator for IEEE 2030.5
(SEP2). It connects to an IEEE 2030.5 server over mutual TLS and exercises
the full DER function set: registration, time sync, FunctionSetAssignment and
DERProgram discovery, DERControl polling, status and metering reporting, and
inbound HTTPS notifications.

The binary is `inverterclient`, built from `cmd/inverterclient`.

## Roles

Two roles select the consumer-policy layer at startup via `--role`:

**simulator** (default): a scripted scenario drives the synthetic device
backend through a defined grid-state timeline. No hardware is required. This
is the intended role for protocol testing and interoperability runs.

**production**: every setpoint write to the physical device passes through
five safety guards before reaching hardware: nameplate range clamp,
units/scale validation, write-rate limiting (token bucket), fail-safe hold on
comms loss, and malformed-control rejection. `--role production` with
`--backend realdevice` is the production commissioning path. Pairing
`--role production` with `--backend synthetic` or `--backend gridlabd` is a
permitted commissioning dry-run; the binary logs a warning at startup to
confirm the intent.

## Device backends

`--backend` selects the physical-state source for the tick loop:

- `synthetic` (default): scenario-scripted grid states, no hardware.
- `gridlabd`: reads state from a co-simulated GridLAB-D instance.
- `replay`: plays back recorded GridLAB-D output from a file (sim mode; needs
  `replay.file` in the sim config).
- `realdevice`: talks to actual inverter hardware over SunSpec Modbus.

Any other value is refused at start. The role and backend axes are
independent on the command line, and `--role production --backend realdevice`
is the only path that writes to real hardware with all five safety guards
enforced. In sim mode (below) `backend` and `SEP2_BACKEND` accept only
`replay` or `synthetic`; `gridlabd` and `realdevice` are reachable only
through the `--backend` flag.

## Build

```
go build ./cmd/inverterclient/
```

## Usage

```
./inverterclient --help
./inverterclient --list-scenarios
./inverterclient --server https://sep2-server:8443 --scenario voltvar
./inverterclient --role production --backend realdevice --server https://sep2-server:8443
```

Key flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--server` | `https://localhost:8443` | IEEE 2030.5 server URL |
| `--cert` | `certs/device.crt` | Client certificate PEM |
| `--key` | `certs/device.key` | Client private key PEM |
| `--ca` | `certs/ca.crt` | CA certificate PEM |
| `--scenario` | `normal` | Scenario name (see below) |
| `--role` | `simulator` | Consumer-policy role: `simulator` or `production` |
| `--client-role` | `der` | IEEE 2030.5 client role: `der` or `aggregator` |
| `--backend` | `synthetic` | Device backend: `synthetic`, `gridlabd`, `replay`, or `realdevice` |
| `--timescale` | `60.0` | Simulation speed multiplier (60 = 1 real minute = 1 sim hour) |
| `--tick` | `1s` | Simulation tick interval |
| `--report-interval` | `10s` | Status report interval |
| `--hmi-port` | `0` | Local HMI web dashboard port (0 to disable; a nonzero default would collide across processes on one host) |
| `--csip` | false | CSIP mode: discover own EndDevice instead of POST-registering |
| `--allow-unregistered` | false | CSIP mode only: bypass the missing-RegistrationLink check (dev and test only) |
| `--pin` | `0` | Expected registration PIN (0 = skip check; nonzero mismatch is fatal) |
| `--notify-listen` | `127.0.0.1:0` | Inbound HTTPS Notification listener (empty to disable) |
| `--notify-advertise-host` | | Host or `host:port` (IPv6 with or without brackets) advertised in subscription notify URLs (default: the bound `--notify-listen` address). A bare host is combined with the port the listener actually bound; a `host:port` value overrides both. Refused at start if it carries a URL scheme, a path or query character, or a non-numeric port. A startup warning fires when the effective address is loopback or unspecified (`0.0.0.0`, `::`): unreachable from a server on another host, which is what the default `--notify-listen=127.0.0.1:0` produces |
| `--pen` | `0` | IANA Private Enterprise Number for outbound LogEvents (env: SEP2_PEN) |
| `--list-scenarios` | | Print available scenarios and exit |
| `--fleet-file` | none | Path to a GridLAB-D fleet JSON. Aggregator role only; repeat the flag once per fleet. Refused at start in the der role |
| `--run-dir` | empty | Directory for fleet sidecar sockets. Required with `--fleet-file` |
| `--sidecar-pythonpath` | `sim/gridlabd` | Directory containing the `gldsidecar` package, resolved against the working directory. Used only in the aggregator role with `--fleet-file`; empty means `gldsidecar` is installed in the interpreter's own environment |
| `--sim-config` | empty | Path to the sim-mode JSON config (env: `SEP2_SIM_CONFIG`); see "Sim mode" |

### Environment variables

Without a sim config the binary reads only `SEP2_PEN` and
`SEP2_NOTIFY_LISTEN`; an explicit flag always wins over both. `SEP2_SIM_CONFIG`
is read to find the sim config. The rest are read only when a sim config is
loaded.

| Variable | Overrides | Notes |
|----------|-----------|-------|
| `SEP2_PEN` | `--pen` default | Must be a uint32; an invalid value prints a warning and the default stays 0 |
| `SEP2_NOTIFY_LISTEN` | `--notify-listen` default | Empty means `127.0.0.1:0` |
| `SEP2_SIM_CONFIG` | `--sim-config` | Path to the sim config |
| `SEP2_CLIENT_ROLE` | `--client-role` | `der` or `aggregator` |
| `SEP2_SERVER` | `--server` | |
| `SEP2_CERT` | `--cert` | |
| `SEP2_KEY` | `--key` | |
| `SEP2_CA` | `--ca` | |
| `SEP2_LOOKUP_OWN_EDEV` | `--csip` | Boolean |
| `SEP2_PIN` | `--pin` | |
| `SEP2_BACKEND` | `--backend` | `replay` or `synthetic` only |
| `SEP2_REPORT_INTERVAL_S` | `--report-interval` | Whole seconds, positive |
| `SEP2_HMI_PORT` | `--hmi-port` | 0 to 65535 |

An empty variable counts as unset.

## Sim mode

`--sim-config FILE` (or `SEP2_SIM_CONFIG`) loads one JSON file that holds
every sim setting. Each setting resolves in this order: explicit flag, then
environment variable, then the file, then the default. With no file, every
flag behaves as in the tables above.

Rules:

- An unknown key at any depth is a load error, so a misspelled key cannot
  fall back to its default.
- `backend` is `replay` or `synthetic`. If omitted it is `replay` when
  `replay.file` is set, otherwise `synthetic`. `replay` without `replay.file`
  is refused.
- A relative `replay.file`, `frq.request_file` and `managed[].replay.file` is
  resolved against the directory of the config file.
- `role: aggregator` needs at least one `managed` device, and refuses
  `lookup_own_edev`. The aggregator never creates its own EndDevice.
- `device.type: battery` in the der role needs `replay.file`; the synthetic
  backend simulates PV only.
- `device.name` defaults to the config file name without its extension.

Keys, with defaults:

| Key | Default | Meaning |
|-----|---------|---------|
| `role` | `der` | `der` or `aggregator` (flag `--client-role`) |
| `server`, `cert`, `key`, `ca` | the flag defaults | Same as the flags of the same name |
| `lookup_own_edev` | `true` in the der role | Flag `--csip`; der only |
| `pin` | `0` | Flag `--pin` |
| `backend` | see above | `replay` or `synthetic` |
| `device.name` | file name | |
| `device.type` | `pv` | `pv` or `battery` |
| `device.rated_w` | `10000` | Nameplate watts, positive; also sets the VA (111 percent) and VAr (44 percent) ratings |
| `device.capacity_wh` | `10000` | Positive |
| `device.initial_soc` | `0.5` | 0 to 1 |
| `replay.file` | none | Recorded output to play back |
| `replay.clock` | `wall` | `wall` (minute of day, UTC) or `start` (minutes since start, looping daily) |
| `replay.scale` | `1` | Positive multiplier on recorded power |
| `report.interval_s` | `60` | Positive; flag `--report-interval` |
| `status.interval_s` | `60` | Positive; how often managed devices PUT DER status |
| `controls.response` | `follow` | `follow`, `ack` or `none`; `none` starts no control session |
| `controls.poll_s` | `0` | Not negative |
| `managed[]` | none | Aggregator devices: `name` (required), `lfdi` (40 hex characters, unique), and optional `device`, `replay` and `controls` objects with the keys above |
| `frq.energy_wh` | `6000` | Flow reservation energy, positive |
| `frq.power_w` | `3000` | Positive |
| `frq.start_in_s` | `2400` | Not negative; must be later than `min_lead_s` |
| `frq.duration_s` | `3600` | Positive |
| `frq.min_lead_s` | `60` | Not negative |
| `frq.poll_s` | `30` | Positive |
| `frq.answer_grace_s` | `60` | Positive, below `duration_s` |
| `frq.request_file` | `frq-request.json` | Per-request overrides, consumed on SIGUSR1 |
| `dispatch.tick_s` | `5` | Positive |
| `notify.listen` | the flag default | Empty string disables the listener |
| `hmi.port` | `0` | 0 to 65535 |

The aggregator posts a flow reservation on SIGUSR1 and withdraws it on SIGUSR2.

## Client role

`--client-role` selects the IEEE 2030.5 client role, independent of
`--role` above:

**der** (default): the process acts for exactly one EndDevice, the one
named by its own certificate. Every existing invocation is unaffected.

**aggregator**: the process holds its own EndDevice and, for a managed DER,
takes only the manager actions IEEE 2030.5 grants an Aggregator: read the
device's EndDevice tree, write its DER resources, post its log events,
subscribe on its behalf, and post Responses and mirrors naming it. A guard
refuses anything else before it is sent. The aggregator finds its own
EndDevice by certificate and never creates it, and its own EndDevice is
not a DER: it never PUTs DER resources, posts a mirror, or posts a
Response for itself; those steps are skipped, and the guard refuses them
too.

`--csip` is a der-only setting: it has no effect in the aggregator role
(which always looks up its own EndDevice, never POSTs it), so the binary
refuses to start with `--client-role aggregator --csip` rather than
silently ignoring the flag.

## Scenarios

Run `--list-scenarios` for the authoritative list. Current scenarios:

| Name | Duration | Description |
|------|----------|-------------|
| `normal` | 24 h | Sunny day, stable grid, PF=1.0, periodic metering |
| `voltvar` | 1 h | Voltage rises to 1.05-1.08 p.u., triggering var absorption |
| `powerlimit` | 30 min | Server sends opModMaxLimW=5000 W, inverter curtails |
| `disconnect` | 30 min | Server sends opModConnect=false, then re-enables |
| `freqdroop` | 30 min | Frequency drops below nominal, inverter adjusts power via droop |
| `voltageride` | 15 min | Voltage sag to 0.7 p.u. (ride-through), then 0.45 p.u. (trip) |
| `enterservice` | 15 min | Inverter starts with low voltage, waits for Table 4 enter-service criteria |
| `lifecycle` | 1 h | Full protocol lifecycle: register, setup, control, metering, disconnect, reconnect |

## Dependency

This module depends on `github.com/GRIDAPPSD/ieee-2030_5-core-go`, pinned in
`go.mod` at `v0.23.0`. It is an ordinary versioned Go module import; no
`replace` directive is used.

## License

Battelle BSD (3-clause). Copyright Battelle Memorial Institute. See LICENSE and NOTICE.
