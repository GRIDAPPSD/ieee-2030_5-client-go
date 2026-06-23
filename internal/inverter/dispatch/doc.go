// Package dispatch is the consumer-policy seam for decoded IEEE 2030.5
// Notifications. It mirrors the internal/inverter/device seam: an interface
// selected once at construction, two concrete implementations, no runtime
// branching on role identity.
//
// Two implementations are provided:
//
//   - SimulatorDispatcher: refreshes the DERControlCache so the Phase 5
//     state machine applies the change on its next tick. This is the
//     spec-correct CSIP CORE-018 step-5 behavior and the existing simulator
//     policy.
//   - ProductionDispatcher: behaviorally equivalent to SimulatorDispatcher at
//     landing, with three clearly-marked TBD extension points where real
//     deployments will diverge (audit logging, urgent-path notifications,
//     revert-to-safe-default on cancel).
//
// Selection is type-level per ADR-003: the concrete Dispatcher type IS the
// role. There is no --mode flag; role is resolved once by dispatch.New and
// the resulting interface value is the only thing the receiver wiring holds.
//
// The measurable revisit trigger for extracting a separate hardened-gateway
// repo is documented in docs/adr/ADR-004-production-as-mode.md. Consult that
// ADR before adding in-process production-hardening concerns to this package.
package dispatch
