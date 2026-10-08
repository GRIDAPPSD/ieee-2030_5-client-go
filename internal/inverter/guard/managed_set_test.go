package guard

import (
	"net/http"
	"testing"
)

func TestStaticManagedSet_MembershipIsCaseInsensitiveAndNeverEmpty(t *testing.T) {
	m := NewStaticManagedSet("abcdef", "", "0123")
	for lfdi, want := range map[string]bool{
		"ABCDEF": true, "abcdef": true, "0123": true,
		"": false, "fedcba": false,
	} {
		if got := m.IsManaged(lfdi); got != want {
			t.Errorf("IsManaged(%q) = %v, want %v", lfdi, got, want)
		}
	}
	var nilSet *StaticManagedSet
	if nilSet.IsManaged("abcdef") {
		t.Error("nil set reports a device managed")
	}
}

func TestGuard_SetManagedSetChangesTheDecision(t *testing.T) {
	g := New(RoleAggregator, "SELF", nil)
	a := Action{Kind: KindMirrorPost, TargetLFDI: "DEV1"}
	if err := g.Allow(http.MethodPost, a); err == nil {
		t.Fatal("mirror for DEV1 with no managed set: want refusal, got nil")
	}
	g.SetManagedSet(NewStaticManagedSet("dev1"))
	if err := g.Allow(http.MethodPost, a); err != nil {
		t.Errorf("mirror for managed DEV1: %v", err)
	}
	if err := g.Allow(http.MethodPost, Action{Kind: KindMirrorPost, TargetLFDI: "DEV2"}); err == nil {
		t.Error("mirror for unmanaged DEV2: want refusal, got nil")
	}
	var nilGuard *Guard
	nilGuard.SetManagedSet(NewStaticManagedSet("dev1")) // must not panic
}
