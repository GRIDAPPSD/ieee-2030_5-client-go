# ieee-2030_5-client

IEEE 2030.5 (SEP2) Go client and inverter simulator for the PNNL GRIDAPPSD project.

## What this is

This module contains the inverter simulator client extracted from the IEEE 2030.5
monorepo. The source is the inverter archive at
`/home/debian/repos/inverter-extraction-scratch/inverter-archive`, which was
history-rewritten out of the server repository on 2026-06-01 to preserve the
per-file commit history of the `internal/inverter/` subtree and the
`cmd/inverterclient/` entry point.

The simulator includes scenario modes (normal, voltvar, freqdroop, ridethrough),
an HMI port, and a configurable timescale. It connects to an IEEE 2030.5 server
over TLS and exercises the full DER function set.

## Status

Phase E1 (archive imported). Imports are stale (`github.com/GRIDAPPSD/ieee-2030_5-go/...`);
rewriting them to consume `ieee-2030_5-core` happens in Phase E2. See
`noor-extraction-plan-2026-06-18.md` in the workspace at
`projects/ieee-2030_5/ieee-2030_5-go/artifacts/outputs/` for the full phase
sequencing. Once Phase E2 lands, this module depends on
`gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core`.

## License

BSD-2-Clause. Copyright Battelle Memorial Institute. See LICENSE.
