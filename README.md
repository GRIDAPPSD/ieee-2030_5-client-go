# ieee-2030_5-client

[![ci](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/ci.yml)
[![CodeQL](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/GRIDAPPSD/ieee-2030_5-client-go/actions/workflows/github-code-scanning/codeql)
[![Go 1.26.3](https://img.shields.io/badge/Go-1.26.3-00ADD8?logo=go)](https://go.dev/)
[![License: Battelle BSD](https://img.shields.io/badge/License-Battelle_BSD-blue.svg)](LICENSE)

No release badge yet; this repo has not cut a tagged release.

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
- `realdevice`: talks to actual inverter hardware over SunSpec Modbus.

The role and backend axes are independent. Any combination is valid, but
`--role production --backend realdevice` is the only path that writes to
real hardware with all five safety guards enforced.

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
| `--backend` | `synthetic` | Device backend: `synthetic`, `gridlabd`, or `realdevice` |
| `--timescale` | `60.0` | Simulation speed multiplier (60 = 1 real minute = 1 sim hour) |
| `--tick` | `1s` | Simulation tick interval |
| `--hmi-port` | `0` | Local HMI web dashboard port (0 to disable; a nonzero default would collide across processes on one host) |
| `--csip` | false | CSIP mode: discover own EndDevice instead of POST-registering |
| `--pin` | `0` | Expected registration PIN (0 = skip check; nonzero mismatch is fatal) |
| `--notify-listen` | `127.0.0.1:0` | Inbound HTTPS Notification listener (empty to disable) |
| `--notify-advertise-host` | | Host or `host:port` (IPv6 with or without brackets) advertised in subscription notify URLs (default: the bound `--notify-listen` address). A bare host is combined with the port the listener actually bound; a `host:port` value overrides both. Refused at start if it carries a URL scheme, a path or query character, or a non-numeric port. A startup warning fires when the effective address is loopback or unspecified (`0.0.0.0`, `::`): unreachable from a server on another host, which is what the default `--notify-listen=127.0.0.1:0` produces |
| `--pen` | `0` | IANA Private Enterprise Number for outbound LogEvents (env: SEP2_PEN) |
| `--list-scenarios` | | Print available scenarios and exit |

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
too. The managed-device set, fleet loops and provisioning are a separate
piece of work.

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
`go.mod` at `v0.6.0`. It is an ordinary versioned Go module import; no
`replace` directive is used.

## License

Battelle BSD (3-clause). Copyright Battelle Memorial Institute. See LICENSE and NOTICE.
