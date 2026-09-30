package guard

import "testing"

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

// TestGuard_DER covers the issue #71 criterion: "in the der role, anything
// naming another device is refused", and that a der process's own-device
// requests are unchanged (every kind it uses today stays allowed for self).
func TestGuard_DER(t *testing.T) {
	t.Parallel()
	const self = "AAAA000000000000000000000000000000AAAA"
	const other = "BBBB000000000000000000000000000000BBBB"
	g := New(RoleDER, self, fakeManagedSet{other: true})

	selfKinds := []Kind{
		KindEndDeviceRead, KindRegistrationRead, KindDERResourceWrite,
		KindLogEventPost, KindSubscriptionPost, KindResponsePost,
		KindMirrorPost, KindFlowReservationPost,
	}
	for _, k := range selfKinds {
		for _, target := range []string{"", self} {
			if err := g.Allow(Action{Kind: k, TargetLFDI: target}); err != nil {
				t.Errorf("der self %v target=%q: want allow, got %v", k, target, err)
			}
		}
	}

	// Naming another device, even one a fake ManagedSet reports as
	// managed, is refused in the der role: der never acts as a manager.
	for _, k := range selfKinds {
		if err := g.Allow(Action{Kind: k, TargetLFDI: other}); err == nil {
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
	const self = "AAAA000000000000000000000000000000AAAA"
	const managed = "BBBB000000000000000000000000000000BBBB"
	const unmanaged = "CCCC000000000000000000000000000000CCCC"
	g := New(RoleAggregator, self, fakeManagedSet{managed: true})

	tests := []struct {
		name   string
		action Action
		allow  bool
	}{
		{"M1 read managed EndDevice tree", Action{Kind: KindEndDeviceRead, TargetLFDI: managed}, true},
		{"M2 write managed DER resource", Action{Kind: KindDERResourceWrite, TargetLFDI: managed}, true},
		{"M3 post managed LogEvent", Action{Kind: KindLogEventPost, TargetLFDI: managed}, true},
		{"M4 subscribe for managed device", Action{Kind: KindSubscriptionPost, TargetLFDI: managed}, true},
		{"M5 post Response for managed device", Action{Kind: KindResponsePost, TargetLFDI: managed}, true},
		{"M6 post mirror for managed device", Action{Kind: KindMirrorPost, TargetLFDI: managed}, true},
		{"M7 delete off by default", Action{Kind: KindEndDeviceDelete, TargetLFDI: managed}, false},
		{"M8 create off by default", Action{Kind: KindEndDeviceCreate, TargetLFDI: managed}, false},

		{"write managed EndDevice record", Action{Kind: KindEndDeviceWrite, TargetLFDI: managed}, false},
		{"write managed DefaultDERControl", Action{Kind: KindDefaultControlWrite, TargetLFDI: managed}, false},
		{"read managed Registration", Action{Kind: KindRegistrationRead, TargetLFDI: managed}, false},
		{"flow reservation on managed device", Action{Kind: KindFlowReservationPost, TargetLFDI: managed}, false},
		{"mirror for unmanaged device", Action{Kind: KindMirrorPost, TargetLFDI: unmanaged}, false},
		{"read for unmanaged device", Action{Kind: KindEndDeviceRead, TargetLFDI: unmanaged}, false},

		{"self read", Action{Kind: KindEndDeviceRead, TargetLFDI: self}, true},
		{"self flow reservation", Action{Kind: KindFlowReservationPost, TargetLFDI: self}, true},
	}
	for _, tt := range tests {
		err := g.Allow(tt.action)
		if tt.allow && err != nil {
			t.Errorf("%s: want allow, got refusal: %v", tt.name, err)
		}
		if !tt.allow && err == nil {
			t.Errorf("%s: want refusal, got allow", tt.name)
		}
	}
}

// TestGuard_Aggregator_InBandOptIn proves M7/M8 open only when the
// aggregator's own in-band settings are on (ADR-009 decision 4: "only
// while the client's in-band ... setting is on").
func TestGuard_Aggregator_InBandOptIn(t *testing.T) {
	t.Parallel()
	const self = "AAAA000000000000000000000000000000AAAA"
	const managed = "BBBB000000000000000000000000000000BBBB"
	g := New(RoleAggregator, self, fakeManagedSet{managed: true}, WithInBandDelete(), WithInBandCreate())

	if err := g.Allow(Action{Kind: KindEndDeviceDelete, TargetLFDI: managed}); err != nil {
		t.Errorf("delete with WithInBandDelete: want allow, got %v", err)
	}
	if err := g.Allow(Action{Kind: KindEndDeviceCreate, TargetLFDI: managed}); err != nil {
		t.Errorf("create with WithInBandCreate: want allow, got %v", err)
	}
}

// TestGuard_Aggregator_NilManagedSet proves the fail-closed default: with
// no ManagedSet wired (#72 not yet built), every non-self LFDI is refused.
func TestGuard_Aggregator_NilManagedSet(t *testing.T) {
	t.Parallel()
	const self = "AAAA000000000000000000000000000000AAAA"
	const other = "BBBB000000000000000000000000000000BBBB"
	g := New(RoleAggregator, self, nil)

	if err := g.Allow(Action{Kind: KindEndDeviceRead, TargetLFDI: other}); err == nil {
		t.Error("nil ManagedSet: want refusal for any non-self LFDI, got allow")
	}
	if err := g.Allow(Action{Kind: KindEndDeviceRead, TargetLFDI: self}); err != nil {
		t.Errorf("nil ManagedSet: self read: want allow, got %v", err)
	}
}

// TestGuard_TargetLFDI_CaseInsensitive matches LookupOwnEndDevice's
// existing case-insensitive LFDI comparison (client.go:494).
func TestGuard_TargetLFDI_CaseInsensitive(t *testing.T) {
	t.Parallel()
	const self = "aaaa000000000000000000000000000000aaaa"
	g := New(RoleDER, self, nil)
	if err := g.Allow(Action{Kind: KindEndDeviceRead, TargetLFDI: "AAAA000000000000000000000000000000AAAA"}); err != nil {
		t.Errorf("case-insensitive self match: want allow, got %v", err)
	}
}

func TestRefusalError_Message(t *testing.T) {
	t.Parallel()
	err := &RefusalError{Role: RoleDER, Kind: KindMirrorPost, TargetLFDI: "BBBB"}
	if err.Error() == "" {
		t.Error("RefusalError.Error() returned empty string")
	}
}
