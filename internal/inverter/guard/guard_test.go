package guard

import (
	"net/http"
	"testing"
)

const (
	testSelf      = "AAAA000000000000000000000000000000AAAA"
	testManaged   = "BBBB000000000000000000000000000000BBBB"
	testUnmanaged = "CCCC000000000000000000000000000000CCCC"
)

// fakeManagedSet is a stub ManagedSet: #72 wires the real device-mapping
// loader, this table only needs membership.
type fakeManagedSet map[string]bool

func (f fakeManagedSet) IsManaged(lfdi string) bool { return f[lfdi] }

func TestParseRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    Role
		wantErr bool
	}{
		{"der", RoleDER, false},
		{"aggregator", RoleAggregator, false},
		{"", "", true},
		{"DER", "", true},
		{"manager", "", true},
	}
	for _, tt := range tests {
		got, err := ParseRole(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseRole(%q): want error, got nil", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRole(%q): unexpected error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ParseRole(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestGuard_VerbKindMismatch_Refused is fix-round-1 finding 1's first proof
// point: a caller may not mislabel a write as a read (or any Kind whose
// legitimate verb differs from the one actually being invoked). Proven by
// run before this fix: a PUT /edev/7 labelled KindEndDeviceRead reached
// the server.
func TestGuard_VerbKindMismatch_Refused(t *testing.T) {
	t.Parallel()
	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})

	// A read-only Kind invoked as PUT.
	if err := g.Allow(http.MethodPut, Action{Kind: KindEndDeviceRead, TargetLFDI: testManaged}); err == nil {
		t.Error("PUT labelled KindEndDeviceRead: want refusal, got allow")
	}
	// A write-only Kind invoked as GET.
	if err := g.Allow(http.MethodGet, Action{Kind: KindDERResourceWrite, TargetLFDI: testManaged}); err == nil {
		t.Error("GET labelled KindDERResourceWrite: want refusal, got allow")
	}
	// The legitimate pairing still works (control: the check can fail).
	if err := g.Allow(http.MethodGet, Action{Kind: KindEndDeviceRead, TargetLFDI: testManaged}); err != nil {
		t.Errorf("GET labelled KindEndDeviceRead: want allow, got %v", err)
	}
}

// TestGuard_ZeroValueAction_Refused is fix-round-1 finding 1's second proof
// point: a zero-value Action{} (Kind == KindUnclassified, TargetLFDI == "")
// must never be treated as a permissive default. Proven by run before this
// fix: a zero-value Action let a POST /edev/9/dderc through.
func TestGuard_ZeroValueAction_Refused(t *testing.T) {
	t.Parallel()
	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		if err := g.Allow(method, Action{}); err == nil {
			t.Errorf("zero-value Action under %s: want refusal, got allow", method)
		}
	}
}

// TestGuard_UnrecognizedKind_Refused proves "refuse anything it cannot
// classify": a Kind value outside the known set has no kindMethod entry
// and is refused under every verb.
func TestGuard_UnrecognizedKind_Refused(t *testing.T) {
	t.Parallel()
	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})
	const bogus Kind = 999
	if err := g.Allow(http.MethodGet, Action{Kind: bogus, TargetLFDI: testManaged}); err == nil {
		t.Error("unrecognized Kind: want refusal, got allow")
	}
}

// TestGuard_DER covers issue #71's criterion: "in the der role, anything
// naming another device is refused", the der role's own self-registration
// (KindEndDeviceCreate) stays allowed, and der never sends a flow
// reservation or acts on a managed device (der has no manager standing).
func TestGuard_DER(t *testing.T) {
	t.Parallel()
	g := New(RoleDER, testSelf, fakeManagedSet{testManaged: true})

	selfAllowedKinds := []Kind{
		KindEndDeviceRead, KindRegistrationRead, KindDERResourceWrite,
		KindLogEventPost, KindSubscriptionPost, KindResponsePost,
		KindMirrorPost, KindEndDeviceCreate,
	}
	for _, k := range selfAllowedKinds {
		for _, target := range []string{"", testSelf} {
			if err := g.Allow(kindMethod[k], Action{Kind: k, TargetLFDI: target}); err != nil {
				t.Errorf("der self %v target=%q: want allow, got %v", k, target, err)
			}
		}
	}

	// Never allowed for der self, in either role: writing its own record,
	// PUTting a DefaultDERControl, deleting itself, posting a flow
	// reservation (der has no fleet to reserve for).
	neverSelf := []Kind{KindEndDeviceWrite, KindDefaultControlWrite, KindEndDeviceDelete, KindFlowReservationPost}
	for _, k := range neverSelf {
		if err := g.Allow(kindMethod[k], Action{Kind: k, TargetLFDI: ""}); err == nil {
			t.Errorf("der self %v: want refusal, got allow", k)
		}
	}

	// Naming another device, even one a fake ManagedSet reports as
	// managed, is refused in the der role: der never acts as a manager.
	for _, k := range selfAllowedKinds {
		if err := g.Allow(kindMethod[k], Action{Kind: k, TargetLFDI: testManaged}); err == nil {
			t.Errorf("der other-LFDI %v: want refusal, got allow", k)
		}
	}
}

// TestGuard_Aggregator_ManagerActions covers issue #71's table: the M1-M8
// manager actions are allowed for a managed device, and the named
// off-list shapes (writing the EndDevice or its default control, reading
// its Registration, a flow reservation on it, a mirror for an unmanaged
// device) are refused with nothing sent.
func TestGuard_Aggregator_ManagerActions(t *testing.T) {
	t.Parallel()
	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})

	tests := []struct {
		name   string
		action Action
		allow  bool
	}{
		{"M1 read managed EndDevice tree", Action{Kind: KindEndDeviceRead, TargetLFDI: testManaged}, true},
		{"M2 write managed DER resource", Action{Kind: KindDERResourceWrite, TargetLFDI: testManaged}, true},
		{"M3 post managed LogEvent", Action{Kind: KindLogEventPost, TargetLFDI: testManaged}, true},
		{"M4 subscribe for managed device", Action{Kind: KindSubscriptionPost, TargetLFDI: testManaged}, true},
		{"M5 post Response for managed device", Action{Kind: KindResponsePost, TargetLFDI: testManaged}, true},
		{"M6 post mirror for managed device", Action{Kind: KindMirrorPost, TargetLFDI: testManaged}, true},
		{"M7 delete off by default", Action{Kind: KindEndDeviceDelete, TargetLFDI: testManaged}, false},
		{"M8 create off by default", Action{Kind: KindEndDeviceCreate, TargetLFDI: testManaged}, false},

		{"write managed EndDevice record", Action{Kind: KindEndDeviceWrite, TargetLFDI: testManaged}, false},
		{"write managed DefaultDERControl", Action{Kind: KindDefaultControlWrite, TargetLFDI: testManaged}, false},
		{"read managed Registration", Action{Kind: KindRegistrationRead, TargetLFDI: testManaged}, false},
		{"flow reservation on managed device", Action{Kind: KindFlowReservationPost, TargetLFDI: testManaged}, false},
		{"mirror for unmanaged device", Action{Kind: KindMirrorPost, TargetLFDI: testUnmanaged}, false},
		{"read for unmanaged device", Action{Kind: KindEndDeviceRead, TargetLFDI: testUnmanaged}, false},

		{"self read", Action{Kind: KindEndDeviceRead, TargetLFDI: testSelf}, true},
		{"self flow reservation", Action{Kind: KindFlowReservationPost, TargetLFDI: testSelf}, true},
	}
	for _, tt := range tests {
		err := g.Allow(kindMethod[tt.action.Kind], tt.action)
		if tt.allow && err != nil {
			t.Errorf("%s: want allow, got refusal: %v", tt.name, err)
		}
		if !tt.allow && err == nil {
			t.Errorf("%s: want refusal, got allow", tt.name)
		}
	}
}

// TestGuard_Aggregator_SelfNeverActsAsDER is fix-round-1 finding 3: the
// aggregator's own EndDevice is not a DER, so DER-resource writes,
// mirrors and Responses for self are refused, in the guard, whatever
// main.go does or fails to skip.
func TestGuard_Aggregator_SelfNeverActsAsDER(t *testing.T) {
	t.Parallel()
	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})

	refused := []Kind{KindDERResourceWrite, KindMirrorPost, KindResponsePost, KindEndDeviceCreate}
	for _, k := range refused {
		for _, target := range []string{"", testSelf} {
			if err := g.Allow(kindMethod[k], Action{Kind: k, TargetLFDI: target}); err == nil {
				t.Errorf("aggregator self %v target=%q: want refusal, got allow", k, target)
			}
		}
	}

	// What the aggregator's own EndDevice still legitimately does.
	allowed := []Kind{KindEndDeviceRead, KindRegistrationRead, KindLogEventPost, KindSubscriptionPost, KindFlowReservationPost}
	for _, k := range allowed {
		if err := g.Allow(kindMethod[k], Action{Kind: k, TargetLFDI: ""}); err != nil {
			t.Errorf("aggregator self %v: want allow, got %v", k, err)
		}
	}
}

// TestGuard_Self_NeverAllows proves the self exclusions survive: if the
// code that keeps KindEndDeviceWrite, KindDefaultControlWrite and
// KindEndDeviceDelete out of the self tables were removed, this fails.
func TestGuard_Self_NeverAllows(t *testing.T) {
	t.Parallel()
	for _, role := range []Role{RoleDER, RoleAggregator} {
		g := New(role, testSelf, nil)
		for _, k := range []Kind{KindEndDeviceWrite, KindDefaultControlWrite, KindEndDeviceDelete} {
			if err := g.Allow(kindMethod[k], Action{Kind: k, TargetLFDI: ""}); err == nil {
				t.Errorf("role %s self %v: want refusal, got allow", role, k)
			}
		}
	}
}

// TestGuard_Aggregator_InBandOptIn proves M7/M8 open only when the
// aggregator's own in-band settings are on: only while the client's
// in-band delete or create setting is on, off by default.
func TestGuard_Aggregator_InBandOptIn(t *testing.T) {
	t.Parallel()
	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true}, WithInBandDelete(), WithInBandCreate())

	if err := g.Allow(http.MethodDelete, Action{Kind: KindEndDeviceDelete, TargetLFDI: testManaged}); err != nil {
		t.Errorf("delete with WithInBandDelete: want allow, got %v", err)
	}
	if err := g.Allow(http.MethodPost, Action{Kind: KindEndDeviceCreate, TargetLFDI: testManaged}); err != nil {
		t.Errorf("create with WithInBandCreate: want allow, got %v", err)
	}
}

// TestGuard_InBandOptions_Independent is fix-round-1 finding 5: proves
// WithInBandDelete does not also open create, and WithInBandCreate does
// not also open delete (a copy-paste bug in either Option func would pass
// unnoticed without this).
func TestGuard_InBandOptions_Independent(t *testing.T) {
	t.Parallel()

	deleteOnly := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true}, WithInBandDelete())
	if err := deleteOnly.Allow(http.MethodPost, Action{Kind: KindEndDeviceCreate, TargetLFDI: testManaged}); err == nil {
		t.Error("WithInBandDelete alone: want create refused, got allow")
	}

	createOnly := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true}, WithInBandCreate())
	if err := createOnly.Allow(http.MethodDelete, Action{Kind: KindEndDeviceDelete, TargetLFDI: testManaged}); err == nil {
		t.Error("WithInBandCreate alone: want delete refused, got allow")
	}
}

// TestGuard_Aggregator_CreateOptIn_UnmanagedTarget is fix-round-1 finding
// 5: management begins with the in-band create POST, so a target that is
// NOT yet in ManagedSet must still be allowed once WithInBandCreate is on.
// Gating M8 on IsManaged would make it impossible to ever propose a first,
// new device.
func TestGuard_Aggregator_CreateOptIn_UnmanagedTarget(t *testing.T) {
	t.Parallel()
	const newDevice = "DDDD000000000000000000000000000000DDDD"

	g := New(RoleAggregator, testSelf, fakeManagedSet{}, WithInBandCreate())
	if err := g.Allow(http.MethodPost, Action{Kind: KindEndDeviceCreate, TargetLFDI: newDevice}); err != nil {
		t.Errorf("create opt-in for a new, unmanaged device: want allow, got %v", err)
	}

	// Same with a nil ManagedSet, today's production default (#72 has not
	// wired a real loader yet).
	gNil := New(RoleAggregator, testSelf, nil, WithInBandCreate())
	if err := gNil.Allow(http.MethodPost, Action{Kind: KindEndDeviceCreate, TargetLFDI: newDevice}); err != nil {
		t.Errorf("create opt-in with nil ManagedSet: want allow, got %v", err)
	}
}

// TestGuard_Aggregator_NilManagedSet proves the fail-closed default: with
// no ManagedSet wired (#72 not yet built), every non-self LFDI is refused
// for the manager actions that DO require prior management.
func TestGuard_Aggregator_NilManagedSet(t *testing.T) {
	t.Parallel()
	g := New(RoleAggregator, testSelf, nil)

	if err := g.Allow(http.MethodGet, Action{Kind: KindEndDeviceRead, TargetLFDI: testUnmanaged}); err == nil {
		t.Error("nil ManagedSet: want refusal for any non-self LFDI, got allow")
	}
	if err := g.Allow(http.MethodGet, Action{Kind: KindEndDeviceRead, TargetLFDI: testSelf}); err != nil {
		t.Errorf("nil ManagedSet: self read: want allow, got %v", err)
	}
}

// TestGuard_TargetLFDI_CaseInsensitive matches LookupOwnEndDevice's
// existing case-insensitive LFDI comparison (client.go:494).
func TestGuard_TargetLFDI_CaseInsensitive(t *testing.T) {
	t.Parallel()
	const self = "aaaa000000000000000000000000000000aaaa"
	g := New(RoleDER, self, nil)
	if err := g.Allow(http.MethodGet, Action{Kind: KindEndDeviceRead, TargetLFDI: "AAAA000000000000000000000000000000AAAA"}); err != nil {
		t.Errorf("case-insensitive self match: want allow, got %v", err)
	}
}

func TestRefusalError_Message(t *testing.T) {
	t.Parallel()
	err := &RefusalError{Role: RoleDER, Method: http.MethodPost, Kind: KindMirrorPost, TargetLFDI: "BBBB"}
	if err.Error() == "" {
		t.Error("RefusalError.Error() returned empty string")
	}
}
