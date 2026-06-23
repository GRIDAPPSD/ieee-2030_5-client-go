package dispatch

import (
	"context"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// Dispatcher is the consumer-policy seam for a decoded IEEE 2030.5
// Notification. Two implementations: the simulator dispatcher (refresh the
// DERControlCache so the Phase 5 state machine applies the change next tick)
// and the production dispatcher (same cache-refresh policy at landing, with
// three TBD extension points for production-specific behavior).
//
// Selection happens once at construction via New; the receiver wiring never
// branches on dispatcher identity. Per ADR-003, the concrete TYPE is the role.
type Dispatcher interface {
	// Dispatch handles one decoded Notification. It runs synchronously
	// inside the /notify handler, so it must be cheap; heavy work belongs
	// in a goroutine the implementation spawns with a lifetime-scoped
	// context.
	Dispatch(ctx context.Context, n sep2.Notification)
}

// RegisterableDispatcher extends Dispatcher with the two late-bind calls
// main() performs after Phase 5 wiring is known. Both SimulatorDispatcher
// and ProductionDispatcher implement this interface so main() never
// type-asserts on role identity.
type RegisterableDispatcher interface {
	Dispatcher

	// RegisterDERControlList wires the Phase 5 cache, client, and href into
	// the dispatcher. Idempotent: subsequent calls replace the previous
	// configuration. Returns an error rather than panicking on nil/empty
	// inputs so callers can degrade gracefully (log and continue in
	// polling-only mode) on misconfiguration discovered at wire-up time.
	RegisterDERControlList(client DERControlListFetcher, cache *inverter.DERControlCache, href string) error

	// RegisterCancelHook installs the cancel hook invoked on every status=1
	// (subscription cancelled by server) notification. Idempotent: subsequent
	// calls replace the previous hook. Passing nil clears the hook.
	RegisterCancelHook(hook inverter.CancelHook)
}

// DERControlListFetcher is the narrow consumer-side interface the dispatchers
// need from a SEP2Client. Defined at the consumer per Pike rule 6 (small
// interfaces) so tests can stub the HTTP path without spinning a real client.
// SEP2Client satisfies this implicitly.
type DERControlListFetcher interface {
	GetDERControlList(ctx context.Context, href string) (sep2.DERControlList, string, error)
}
