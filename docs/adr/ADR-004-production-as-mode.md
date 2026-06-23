# ADR-004: Production client as a mode (role) of the single binary

Date: 2026-06-23
Status: Accepted
Extends: ADR-003 (type-level role separation), IEEESIM-004

## Context

The IEEE 2030.5 inverter client has two axes of variation:

1. The **device backend**: what source of physical state the tick loop reads and
   writes (Synthetic scenario harness, GridLAB-D co-simulation, or real
   SunSpec/Modbus hardware). This seam landed in IEEESIM-003.

2. The **consumer-policy role**: what the notification dispatcher does with a
   decoded notification (the simulator refreshes the DERControlCache so the
   Phase 5 state machine picks up the change on its next tick; a production
   deployment may additionally emit audit records, take an urgent fast path, or
   revert a physical setpoint on subscription cancellation). This seam lands in
   IEEESIM-004.

These are independent axes. The production deployment path is:

```
inverterclient --role production --backend realdevice
```

The simulator deployment path is the existing default:

```
inverterclient  (or --role simulator --backend synthetic)
```

A dry-run commissioning path is also valid (Fork C decision, IEEESIM-004):

```
inverterclient --role production --backend synthetic
```

This triggers a loud startup warning but is not a hard error.

## Decision

1. **One binary, no separate cmd/**. A separate `cmd/productionclient/` is not
   warranted. `main()` already does all of: flag parsing, TLS/cert loading,
   SEP2 client construction, EndDevice discovery, DERProgram walk, the notify
   receiver, the device seam, and the tick loop. Production changes exactly two
   construction choices (`--role production`, `--backend realdevice`) and zero
   control-flow. Every phase of `main()` is identical. A second cmd would
   duplicate the entire startup sequence, creating double-maintenance the
   revisit trigger below is designed to catch before it becomes a problem.

2. **Type-level role separation per ADR-003**. The concrete `Dispatcher` TYPE
   is the role. There is no `--mode` runtime flag. `cfg.Role` is consumed only
   by `dispatch.New` at construction; after that, the `dispatch.Dispatcher`
   interface value is the only thing the receiver wiring holds. The dispatch
   path never branches on role identity at runtime.

3. **Two independent seams, one coherence check**. Role (dispatcher) and backend
   (device) are resolved at one call site in `main()` after flag parsing.
   A coherence check at that call site (Fork C) warns when `production` role is
   paired with a non-realdevice backend (a permitted commissioning dry-run, not
   a hard error) but does not otherwise couple the two axes. Any (role, backend)
   pair is constructible.

4. **ProductionDispatcher scaffold with three TBD extension points**. At landing,
   `ProductionDispatcher.Dispatch` is behaviorally identical to
   `SimulatorDispatcher.Dispatch` (spec-correct CSIP CORE-018 cache-refresh
   routing). The production difference at landing time lives in the device
   backend (realdevice writes real registers through safety guards), not in the
   dispatcher. Three extension points are clearly marked TBD in the code:

   - Audit logging: structured audit record per applied control.
   - Urgent-path notifications: fire-now path for emergency curtailment.
   - Revert-to-safe-default on cancel: immediate setpoint revert on
     subscription cancellation, tied to the fail-safe guard on the real-device
     backend.

## Measurable revisit trigger

Extract a separate hardened-gateway repo only when **three or more** of the
following production-hardening concerns become code-level in-process:

- [ ] Custom OTA agent (over-the-air firmware update subsystem in-process)
- [ ] Config-attestation subsystem (signed configuration verification)
- [ ] OT-protocol supervisor (out-of-band watchdog or protocol bridge)
- [ ] Signed-config validator beyond a small function (more than ~50 lines)
- [ ] In-process secrets vault (key material managed inside the binary)

When three or more boxes are checked, the binary has grown into a hardened
gateway and the separation overhead is justified. Until then, a single binary
with two construction flags is the right posture: lower maintenance cost, easier
testing, identical startup sequence, and no duplicated flag parsing.

This checklist mirrors the operationalizing doc (see
`ieee-2030_5-client-go/artifacts/outputs/operationalizing-the-sep2-inverter-client-2026-06-23.md`
Section 7) verbatim. The two must not drift.

## Consequences

- The simulator path is UNCHANGED. Default `--role simulator` is identical to
  the pre-IEEESIM-004 behavior at the byte level.
- The `PhaseStateDispatcher` type is relocated from `package inverter` to
  `package dispatch` (renamed `SimulatorDispatcher`) for topology symmetry with
  the device seam. Behavior is unchanged; only the package path changes.
- The `internal/inverter/dispatch` package is the canonical home for both
  dispatcher implementations. The `internal/inverter` package retains the
  `DERControlCache`, `CancelHook`, and `NotificationDispatcher` types that the
  dispatch package imports.
- The three TBD extension points in `ProductionDispatcher` are the engineering
  surface for future production-specific behavior. Each is a clearly-commented
  location in `production.go`; a reviewer adding production behavior to one of
  these slots should also check the revisit-trigger checklist above.

## Alternatives considered

- **Separate `cmd/productionclient/`**: rejected because the entry point does
  not genuinely diverge. Every phase of `main()` is identical; a second cmd
  would duplicate ~1000 lines of startup sequence. The revisit trigger above is
  the gate for re-evaluating this decision.
- **A `--mode` runtime flag**: rejected per ADR-003. The type-level separation
  is the design: the concrete Dispatcher TYPE is the role. A runtime flag would
  violate ADR-003's type-level invariant and would require the dispatch path to
  branch on role identity at runtime, making the seam a leaky abstraction.
- **Fail-closed on production+synthetic**: rejected (Fork C decision). A
  production dry-run against the simulator is a legitimate commissioning step
  (operationalizing doc Section 5.4 stage 1). The warned=true return from
  `CheckRoleBackendCoherence` is the right posture.
