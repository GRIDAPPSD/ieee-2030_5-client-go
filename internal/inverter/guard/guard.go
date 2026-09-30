// Package guard implements a fail-closed action guard: a closed list, per
// client role, of what the process may send for a given EndDevice before
// any request reaches the network.
package guard

import (
	"fmt"
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
	// KindEndDeviceRead is M1: GET/HEAD an EndDevice and everything linked
	// from it (assignments, programs, controls, curves, time, DER
	// resources, log list), wherever the server places them.
	KindEndDeviceRead Kind = iota
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
	// KindEndDeviceDelete is M7: DELETE the EndDevice, gated by
	// WithInBandDelete (off by default).
	KindEndDeviceDelete
	// KindEndDeviceCreate is M8: POST a proposed EndDevice, gated by
	// WithInBandCreate (off by default).
	KindEndDeviceCreate

	// KindRegistrationRead, KindEndDeviceWrite and KindDefaultControlWrite
	// are never a manager action for another device, whatever the server
	// would otherwise admit.
	KindRegistrationRead
	KindEndDeviceWrite
	KindDefaultControlWrite
	// KindFlowReservationPost is never for another device: an aggregator's
	// flow reservation is its own, posted on its own EndDevice.
	KindFlowReservationPost
)

func (k Kind) String() string {
	switch k {
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

// selfKinds are the actions either role may take for its own EndDevice.
// KindEndDeviceWrite, KindEndDeviceCreate, KindEndDeviceDelete and
// KindDefaultControlWrite are excluded even for self: nothing in this
// design PUTs its own EndDevice record, creates or deletes itself, or PUTs
// a DefaultDERControl (the utility authors it; neither client role PUTs one).
var selfKinds = map[Kind]bool{
	KindEndDeviceRead:       true,
	KindRegistrationRead:    true,
	KindDERResourceWrite:    true,
	KindLogEventPost:        true,
	KindSubscriptionPost:    true,
	KindResponsePost:        true,
	KindMirrorPost:          true,
	KindFlowReservationPost: true,
}

// managerKinds are the M1-M6 manager actions: always open for a managed
// device once ManagedSet confirms it. M7/M8 (delete/create) are handled
// separately since each needs its own opt-in.
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
// EndDevice.
type Action struct {
	Kind       Kind
	TargetLFDI string
}

// ManagedSet answers whether an LFDI is a device the aggregator currently
// manages. Issue #72 wires the real device-mapping loader; a Guard given a
// nil ManagedSet refuses every non-self LFDI (fail closed).
type ManagedSet interface {
	IsManaged(lfdi string) bool
}

// RefusalError reports an Action the Guard refused, naming the role, the
// action kind and the target LFDI it was refused for.
type RefusalError struct {
	Role       Role
	Kind       Kind
	TargetLFDI string
}

func (e *RefusalError) Error() string {
	target := e.TargetLFDI
	if target == "" {
		target = "(self)"
	}
	return fmt.Sprintf("guard: role %q refuses %s for %s", e.Role, e.Kind, target)
}

// Option configures a Guard at construction.
type Option func(*Guard)

// WithInBandDelete opens M7 (DELETE a managed EndDevice) for a managed
// device. Off by default.
func WithInBandDelete() Option { return func(g *Guard) { g.allowInBandDelete = true } }

// WithInBandCreate opens M8 (POST a proposed EndDevice) for the managed
// EndDeviceList. Off by default.
func WithInBandCreate() Option { return func(g *Guard) { g.allowInBandCreate = true } }

// Guard is a fail-closed action guard, wrapping the process's single
// client. Every Action is classified by Allow before it reaches the
// transport; an Action Allow refuses is never sent.
type Guard struct {
	role     Role
	selfLFDI string
	managed  ManagedSet

	allowInBandDelete bool
	allowInBandCreate bool
}

// New builds a Guard for role, whose own EndDevice is identified by
// selfLFDI (the process's certificate LFDI). managed may be nil; a nil
// ManagedSet treats every non-self LFDI as unmanaged.
func New(role Role, selfLFDI string, managed ManagedSet, opts ...Option) *Guard {
	g := &Guard{role: role, selfLFDI: selfLFDI, managed: managed}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Allow classifies a. It returns nil when the role's closed list permits
// the action, or a *RefusalError naming what was refused and for whom.
func (g *Guard) Allow(a Action) error {
	if a.TargetLFDI == "" || strings.EqualFold(a.TargetLFDI, g.selfLFDI) {
		if selfKinds[a.Kind] {
			return nil
		}
		return &RefusalError{Role: g.role, Kind: a.Kind, TargetLFDI: a.TargetLFDI}
	}

	// Not self. A der-role process never acts as a manager: anything
	// naming another device is refused regardless of kind.
	if g.role != RoleAggregator {
		return &RefusalError{Role: g.role, Kind: a.Kind, TargetLFDI: a.TargetLFDI}
	}
	if g.managed == nil || !g.managed.IsManaged(a.TargetLFDI) {
		return &RefusalError{Role: g.role, Kind: a.Kind, TargetLFDI: a.TargetLFDI}
	}
	if managerKinds[a.Kind] {
		return nil
	}
	if a.Kind == KindEndDeviceDelete && g.allowInBandDelete {
		return nil
	}
	if a.Kind == KindEndDeviceCreate && g.allowInBandCreate {
		return nil
	}
	return &RefusalError{Role: g.role, Kind: a.Kind, TargetLFDI: a.TargetLFDI}
}
