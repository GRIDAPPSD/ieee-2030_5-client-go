// Package guard implements a fail-closed action guard: a closed list, per
// client role, of what the process may send for a given EndDevice before
// any request reaches the network.
//
// Known limit: Kind pins the HTTP method (kindMethod), not the resource
// the path actually names. A caller that mislabels a same-verb action
// (a MirrorUsagePoint POST tagged KindLogEventPost, say) is not caught:
// Allow has no way to tell the two apart without knowing what a href
// names, which requires the per-device href provenance tracking #72
// builds (the session that knows which hrefs it reached from which
// EndDevice's own tree). Until then, every production caller hardcodes
// its Kind at the call site (client.go), so this gap is only reachable by
// a bug in that hardcoding, not by anything a remote server controls.
package guard

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
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
	// KindFlowReservationResponsePost acknowledges a FlowReservationResponse
	// on the aggregator's own EndDevice. It is a different kind from
	// KindResponsePost because an aggregator never answers DER events for
	// itself, yet must answer the response to its own reservation; and it is
	// never for another device, since the reservation is the aggregator's.
	KindFlowReservationResponsePost
	// KindFlowReservationWithdraw is the PUT that withdraws the
	// aggregator's own FlowReservationRequest by changing its
	// RequestStatus. It is never for another device, for the same reason
	// as KindFlowReservationPost.
	KindFlowReservationWithdraw
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
	case KindFlowReservationResponsePost:
		return "FlowReservationResponsePost"
	case KindFlowReservationWithdraw:
		return "FlowReservationWithdraw"
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
	KindEndDeviceRead:               http.MethodGet,
	KindRegistrationRead:            http.MethodGet,
	KindDERResourceWrite:            http.MethodPut,
	KindLogEventPost:                http.MethodPost,
	KindSubscriptionPost:            http.MethodPost,
	KindResponsePost:                http.MethodPost,
	KindMirrorPost:                  http.MethodPost,
	KindEndDeviceCreate:             http.MethodPost,
	KindFlowReservationPost:         http.MethodPost,
	KindFlowReservationResponsePost: http.MethodPost,
	KindFlowReservationWithdraw:     http.MethodPut,
	KindEndDeviceDelete:             http.MethodDelete,
	KindEndDeviceWrite:              http.MethodPut,
	KindDefaultControlWrite:         http.MethodPut,
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
	KindEndDeviceRead:               true,
	KindRegistrationRead:            true,
	KindLogEventPost:                true,
	KindSubscriptionPost:            true,
	KindFlowReservationPost:         true,
	KindFlowReservationResponsePost: true,
	KindFlowReservationWithdraw:     true,
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
// manages. The aggregator builds it from its configured devices that the
// server also lists (StaticManagedSet); a Guard given a nil ManagedSet refuses every non-self LFDI (fail closed), except a
// KindEndDeviceCreate proposing a brand new device: management begins with
// that POST, so it is never gated on the target already being managed.
type ManagedSet interface {
	IsManaged(lfdi string) bool
}

// StaticManagedSet is a ManagedSet fixed at construction. LFDIs compare
// case-insensitively, as the guard compares the process's own LFDI.
type StaticManagedSet struct {
	lfdis map[string]struct{}
}

// NewStaticManagedSet builds the set from lfdis. An empty LFDI is dropped,
// so it can never make an unnamed target look managed.
func NewStaticManagedSet(lfdis ...string) *StaticManagedSet {
	m := &StaticManagedSet{lfdis: make(map[string]struct{}, len(lfdis))}
	for _, l := range lfdis {
		if l != "" {
			m.lfdis[strings.ToUpper(l)] = struct{}{}
		}
	}
	return m
}

// IsManaged reports whether lfdi is in the set.
func (m *StaticManagedSet) IsManaged(lfdi string) bool {
	if m == nil || lfdi == "" {
		return false
	}
	_, ok := m.lfdis[strings.ToUpper(lfdi)]
	return ok
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

// DefaultRefusalLogWindow is the per-refusal-shape rate-limit window
// WithRefusalLogger falls back to when given zero or a negative window:
// at most one log line per (role, method, kind, target) combination per
// minute. Mirrors this repo's existing bounded-logging pattern
// (inverter.DefaultLogEventWindow / PerCodeLogEventLimiter): a caller
// that retries a refused action on every tick must not flood the log.
const DefaultRefusalLogWindow = time.Minute

// WithRefusalLogger installs a bounded logger: each distinct refusal
// (keyed by role, method, kind and target LFDI) is logged via log at most
// once per window, then silenced until the window elapses. log receives
// one formatted line naming the action and the LFDI (RefusalError's own
// Error() string) and its args; a nil log disables logging (the default
// when this option is not used). A zero or negative window is replaced by
// DefaultRefusalLogWindow; a nil now defaults to time.Now.
func WithRefusalLogger(log func(format string, args ...any), window time.Duration, now func() time.Time) Option {
	return func(g *Guard) {
		if log == nil {
			return
		}
		if window <= 0 {
			window = DefaultRefusalLogWindow
		}
		if now == nil {
			now = time.Now
		}
		g.refusalLog = log
		g.refusalWindow = window
		g.refusalNow = now
		g.refusalLast = make(map[string]time.Time)
	}
}

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

	// refusalLog is nil unless WithRefusalLogger installed one: logging is
	// opt-in so a Guard built with New alone (most existing tests, and any
	// caller that does not want log output) never logs.
	refusalLog    func(format string, args ...any)
	refusalWindow time.Duration
	refusalNow    func() time.Time
	refusalMu     sync.Mutex
	refusalLast   map[string]time.Time
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

// SetManagedSet installs the set of devices the aggregator manages. It is
// for the start-up path, before any goroutine sends a request: it is not
// safe to call while Allow runs concurrently. A nil guard ignores it.
func (g *Guard) SetManagedSet(m ManagedSet) {
	if g != nil {
		g.managed = m
	}
}

// Allow classifies a request whose actual HTTP method is method (the verb
// the caller is about to invoke, not a claim). It returns nil when the
// role's closed list permits the action, or a *RefusalError naming what
// was refused and for whom. A nil *Guard refuses every request (fail
// closed): Allow is safe to call on an unconstructed Guard.
//
// Classification order:
//  1. The method itself must match a.Kind's one legitimate verb (closes
//     the caller-mislabeling exploit: a PUT declared as a read-only Kind,
//     or an unset zero-value Action, both fail here, before any self or
//     managed branch is reached).
//  2. TargetLFDI is compared against the process's own LFDI. For self,
//     the role's self allow-list (derSelfKinds / aggregatorSelfKinds)
//     decides; nothing past this step runs for a self-targeted request.
//  3. For a non-self target, a der-role process is refused outright (it
//     never acts as a manager). An aggregator-role KindEndDeviceCreate
//     is decided by WithInBandCreate alone, without requiring prior
//     management (see KindEndDeviceCreate). Every other non-self kind
//     requires ManagedSet.IsManaged first, then managerKinds (M1-M6) or
//     WithInBandDelete for KindEndDeviceDelete.
//
// Each refusal is logged at most once per window when a logger is
// installed (WithRefusalLogger); logging never affects the returned
// error.
func (g *Guard) Allow(method string, a Action) error {
	if g == nil {
		return &RefusalError{Method: method, Kind: a.Kind, TargetLFDI: a.TargetLFDI}
	}
	refuse := func() error {
		re := &RefusalError{Role: g.role, Method: method, Kind: a.Kind, TargetLFDI: a.TargetLFDI}
		g.logRefusal(re)
		return re
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

// logRefusal logs re via the installed WithRefusalLogger, at most once per
// window for each distinct (role, method, kind, target) shape. A Guard
// with no logger installed (refusalLog nil, the New default) is a no-op:
// logging is opt-in so building a Guard for a test never produces output.
// refusalPruneFactor bounds how long a quiet refusal-shape key survives in
// refusalLast before logRefusal prunes it: refusalPruneFactor * window. A
// key that logs again later gets a fresh entry; pruning only drops keys
// that have gone quiet. #72 will add real per-device targets, and without
// this the key space (one entry per distinct role|method|kind|target ever
// refused) would grow without bound; today it does not, because
// production always passes an empty TargetLFDI.
const refusalPruneFactor = 4

func (g *Guard) logRefusal(re *RefusalError) {
	if g.refusalLog == nil {
		return
	}
	key := string(re.Role) + "|" + re.Method + "|" + re.Kind.String() + "|" + re.TargetLFDI
	now := g.refusalNow()

	g.refusalMu.Lock()
	last, seen := g.refusalLast[key]
	if seen && now.Sub(last) < g.refusalWindow {
		g.refusalMu.Unlock()
		return
	}
	g.refusalLast[key] = now
	for k, t := range g.refusalLast {
		if now.Sub(t) >= refusalPruneFactor*g.refusalWindow {
			delete(g.refusalLast, k)
		}
	}
	g.refusalMu.Unlock()

	g.refusalLog("%s", re.Error())
}
