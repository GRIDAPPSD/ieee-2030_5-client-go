# ieee-2030_5-client

IEEE 2030.5 (SEP2) Go client and inverter simulator.

## What this is

This module provides an inverter simulator client that connects to an
IEEE 2030.5 server over TLS and exercises the full DER function set. It
supports four scenario modes (normal, voltvar, freqdroop, ridethrough),
exposes an HMI port, and accepts a configurable timescale.

## Dependency

This module depends on
`gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core`.

## Build

```
go build ./cmd/inverterclient/
```

Run with `--help` to see available flags, including `--server` for the
IEEE 2030.5 server URL (default: `https://localhost:8443`).

## License

BSD-2-Clause. Copyright Battelle Memorial Institute. See LICENSE and NOTICE.
