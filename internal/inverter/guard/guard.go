// Package guard implements a fail-closed action guard: a closed list, per
// client role, of what the process may send for a given EndDevice before
// any request reaches the network.
package guard

import (
	"fmt"
	"net/http"
	"strings"
)

// Role is the IEEE 2030.5 client role a process runs as, chosen once at
// start (--client-role der|aggregator).
type Role string

const (
	RoleDER        Role = "der"
	RoleAggregator Role = "aggregator"
)

// ParseRole validates a --client-role flag value. An unknown value is a
// loud startup error, matching this repo's existing role/backend flags
// (dispatch.New, device.New).
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleDER, RoleAggregator:
		return Role(s), nil
	default:
		return "", fmt.Errorf("unknown client role %q (want der|aggregator)", s)
	}
}

// Kind identifies what an outbound request does, at the granularity the
// aggregator manager-action table (M1-M8) and its refused shapes
// distinguish.
type Kind int

const (
	// KindUnclassified is the zero value: a caller that never set Kind, or
	// a Kind the guard does not recognize, is refused unconditionally. A
	// caller that forgets to classify a request must not fall through to a
	// permissive default.
	KindUnclassified Kind = iota
	// KindEndDeviceRead is M1: GET an EndDevice and everything linked from
	// it (assignments, programs, controls, curves, time, DER resources,
	// log list), wherever the server places them.
	KindEndDeviceRead
	// KindDERResourceWrite is M2: PUT DERCapability, DERSettings,
	// DERStatus or DERAvailability.
	KindDERResourceWrite
	// KindLogEventPost is M3: POST a LogEvent.
	KindLogEventPost
	// KindSubscriptionPost is M4: POST a Subscription naming a resource
	// reached as M1 describes.
	KindSubscriptionPost
	// KindResponsePost is M5: POST a Response naming the target.
	KindResponsePost
	// KindMirrorPost is M6: POST a MirrorUsagePoint or reading naming the
	// target.
	KindMirrorPost
	// KindEndDeviceDelete is M7: DELETE a managed EndDevice, gated by
	// WithInBandDelete (off by default). For self this kind is never
	// allowed in either role: a process never deletes its own record.
	KindEndDeviceDelete
	// KindEndDeviceCreate is POST of an EndDevice. For self it is the der
	// role's in-band self-registration (POST /edev, always on: it is how
	// a der process gets its own record). For a non-self target it is M8,
	// proposing a new managed device, gated by WithInBandCreate (off by
	// default) and NOT gated on the target already being managed: the
	// POST is what starts management.
	KindEndDeviceCreate

	// KindRegistrationRead, KindEndDeviceWrite and KindDefaultControlWrite
	// are never a manager action for another device, whatever the server
	// would otherwise admit. KindEndDeviceWrite and KindDefaultControlWrite
	// are refused for self too: nothing in this design PUTs its own
	// EndDevice record or a DefaultDERControl (the utility authors it;
	// neither client role PUTs one).
	KindRegistrationRead
	KindEndDeviceWrite
	KindDefaultControlWrite
	// KindFlowReservationPost is never for another device: an aggregator's
	// flow reservation is its own, posted on its own EndDevice. A der
	// process never posts one (not a manager, no fleet to reserve for).
	KindFlowReservationPost
)

func (k Kind) String() string {
	switch k {
	case KindUnclassified:
		return "Unclassified"
	case KindEndDeviceRead:
		return "EndDeviceRead"
	case KindDERResourceWrite:
		return "DERResourceWrite"
	case KindLogEventPost:
		return "LogEventPost"
	case KindSubscriptionPost:
		return "SubscriptionPost"
	case KindResponsePost:
		return "ResponsePost"
	case KindMirrorPost:
		return "MirrorPost"
	case KindEndDeviceDelete:
		return "EndDeviceDelete"
	case KindEndDeviceCreate:
		return "EndDeviceCreate"
	case KindRegistrationRead:
		return "RegistrationRead"
	case KindEndDeviceWrite:
		return "EndDeviceWrite"
	case KindDefaultControlWrite:
		return "DefaultControlWrite"
	case KindFlowReservationPost:
		return "FlowReservationPost"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

// kindMethod names the one HTTP method a legitimate request of that Kind
// uses. Allow refuses whenever the method actually being invoked (which of
// Guarded's Get/Post/Put/Delete the caller called, or the method a
// SEP2Client transport call names) does not match: a caller label may
// narrow what it claims, but it can never turn a write into a read, or a
// read into a write, by mislabeling Kind. KindUnclassified and any Kind
// outside this table has no entry, so it never matches any method: an
// unclassified or unrecognized Kind is refused under every verb.
var kindMethod = map[Kind]string{
	KindEndDeviceRead:       http.MethodGet,
	KindRegistrationRead:    http.MethodGet,
	KindDERResourceWrite:    http.MethodPut,
	KindLogEventPost:        http.MethodPost,
	KindSubscriptionPost:    http.MethodPost,
	KindResponsePost:        http.MethodPost,
	KindMirrorPost:          http.MethodPost,
	KindEndDeviceCreate:     http.MethodPost,
	KindFlowReservationPost: http.MethodPost,
	KindEndDeviceDelete:     http.MethodDelete,
	KindEndDeviceWrite:      http.MethodPut,
	KindDefaultControlWrite: http.MethodPut,
}

// derSelfKinds are the actions a der-role process may take for its own
// EndDevice. KindEndDeviceCreate here is the in-band self-registration POST
// (Register): the der role's normal way to get its own record.
var derSelfKinds = map[Kind]bool{
	KindEndDeviceRead:    true,
	KindRegistrationRead: true,
	KindDERResourceWrite: true,
	KindLogEventPost:     true,
	KindSubscriptionPost: true,
	KindResponsePost:     true,
	KindMirrorPost:       true,
	KindEndDeviceCreate:  true,
}

// aggregatorSelfKinds are the actions an aggregator-role process may take
// for its own EndDevice. Deliberately excluded: KindDERResourceWrite,
// KindMirrorPost, KindResponsePost, KindEndDeviceCreate. The aggregator's
// own EndDevice is not a DER (ADR: "Its own EndDevice is not a DER
// session. It has no event engine, MirrorUsagePoint or DER status."): it
// never PUTs DER resources or posts mirrors or Responses for itself, and
// the utility creates its record, so it never POSTs its own EndDevice.
var aggregatorSelfKinds = map[Kind]bool{
	KindEndDeviceRead:       true,
	KindRegistrationRead:    true,
	KindLogEventPost:        true,
	KindSubscriptionPost:    true,
	KindFlowReservationPost: true,
}

// selfAllowed reports whether role may take kind for its own EndDevice.
// KindEndDeviceWrite, KindDefaultControlWrite and KindEndDeviceDelete are
// absent from both role tables: neither role ever writes its own EndDevice
// record, PUTs a DefaultDERControl, or deletes itself.
func selfAllowed(role Role, kind Kind) bool {
	if role == RoleAggregator {
		return aggregatorSelfKinds[kind]
	}
	return derSelfKinds[kind]
}

// managerKinds are the M1-M6 manager actions: always open for a managed
// device once ManagedSet confirms it. M7 (delete) and M8 (create, handled
// before the managed-set check; see Allow) are handled separately since
// each needs its own opt-in.
var managerKinds = map[Kind]bool{
	KindEndDeviceRead:    true,
	KindDERResourceWrite: true,
	KindLogEventPost:     true,
	KindSubscriptionPost: true,
	KindResponsePost:     true,
	KindMirrorPost:       true,
}

// Action describes one outbound IEEE 2030.5 request: what it does and
// which EndDevice it targets. An empty TargetLFDI means the process's own
// EndDevice. Kind is a label the caller supplies; Allow cross-checks it
// against the method actually invoked and never trusts it standing alone.
type Action struct {
	Kind       Kind
	TargetLFDI string
}

// ManagedSet answers whether an LFDI is a device the aggregator currently
// manages. Issue #72 wires the real device-mapping loader; a Guard given a
// nil ManagedSet refuses every non-self LFDI (fail closed), except a
// KindEndDeviceCreate proposing a brand new device: management begins with
// that POST, so it is never gated on the target already being managed.
type ManagedSet interface {
	IsManaged(lfdi string) bool
}

// RefusalError reports an Action the Guard refused, naming the role, the
// method, the action kind and the target LFDI it was refused for.
type RefusalError struct {
	Role       Role
	Method     string
	Kind       Kind
	TargetLFDI string
}

func (e *RefusalError) Error() string {
	target := e.TargetLFDI
	if target == "" {
		target = "(self)"
	}
	return fmt.Sprintf("guard: role %q refuses %s %s for %s", e.Role, e.Method, e.Kind, target)
}

// Option configures a Guard at construction.
type Option func(*Guard)

// WithInBandDelete opens M7 (DELETE a managed EndDevice) for a managed
// device. Off by default.
func WithInBandDelete() Option { return func(g *Guard) { g.allowInBandDelete = true } }

// WithInBandCreate opens M8 (POST a proposed EndDevice naming a new,
// not-yet-managed device) for the managed EndDeviceList. Off by default.
func WithInBandCreate() Option { return func(g *Guard) { g.allowInBandCreate = true } }

// Guard is a fail-closed action guard, wrapping the process's single
// client. Every Action is classified by Allow, against the method actually
// invoked, before it reaches the transport; an Action Allow refuses is
// never sent.
type Guard struct {
	role     Role
	selfLFDI string
	managed  ManagedSet

	allowInBandDelete bool
	allowInBandCreate bool
}

// New builds a Guard for role, whose own EndDevice is identified by
// selfLFDI (the process's certificate LFDI). managed may be nil; a nil
// ManagedSet treats every non-self LFDI as unmanaged, except a proposed
// create (see ManagedSet).
func New(role Role, selfLFDI string, managed ManagedSet, opts ...Option) *Guard {
	g := &Guard{role: role, selfLFDI: selfLFDI, managed: managed}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Allow classifies a request whose actual HTTP method is method (the verb
// the caller is about to invoke, not a claim). It returns nil when the
// role's closed list permits the action, or a *RefusalError naming what
// was refused and for whom.
//
// Classification order: first the method itself must match a.Kind's one
// legitimate verb (closes the caller-mislabeling exploit: a PUT declared
// as a read-only Kind, or an unset zero-value Action, both fail here,
// before any self/managed branch is reached). Only a Kind that survives
// that check is evaluated further.
func (g *Guard) Allow(method string, a Action) error {
	refuse := func() error {
		return &RefusalError{Role: g.role, Method: method, Kind: a.Kind, TargetLFDI: a.TargetLFDI}
	}
	if kindMethod[a.Kind] != method {
		return refuse()
	}

	if a.TargetLFDI == "" || strings.EqualFold(a.TargetLFDI, g.selfLFDI) {
		if selfAllowed(g.role, a.Kind) {
			return nil
		}
		return refuse()
	}

	// Not self. A der-role process never acts as a manager: anything
	// naming another device is refused regardless of kind.
	if g.role != RoleAggregator {
		return refuse()
	}
	// M8: proposing a new managed device is never gated on the target
	// already being managed, because the POST is what starts management.
	if a.Kind == KindEndDeviceCreate {
		if g.allowInBandCreate {
			return nil
		}
		return refuse()
	}
	if g.managed == nil || !g.managed.IsManaged(a.TargetLFDI) {
		return refuse()
	}
	if managerKinds[a.Kind] {
		return nil
	}
	if a.Kind == KindEndDeviceDelete && g.allowInBandDelete {
		return nil
	}
	return refuse()
}
