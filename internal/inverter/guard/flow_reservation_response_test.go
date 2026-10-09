package guard

import (
	"net/http"
	"testing"
)

// The acknowledgement of a flow reservation response is the aggregator's
// own action. It is not a DER Response (KindResponsePost stays refused for
// aggregator self), and it is never for a managed device or the der role.
func TestGuard_FlowReservationResponsePost(t *testing.T) {
	t.Parallel()
	agg := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})
	der := New(RoleDER, testSelf, fakeManagedSet{testManaged: true})
	a := func(target string) Action {
		return Action{Kind: KindFlowReservationResponsePost, TargetLFDI: target}
	}

	if err := agg.Allow(http.MethodPost, a("")); err != nil {
		t.Errorf("aggregator self (empty target): %v", err)
	}
	if err := agg.Allow(http.MethodPost, a(testSelf)); err != nil {
		t.Errorf("aggregator self (own LFDI): %v", err)
	}
	if err := agg.Allow(http.MethodPost, a(testManaged)); err == nil {
		t.Error("aggregator for a managed device: want refusal, got allow")
	}
	if err := agg.Allow(http.MethodGet, a("")); err == nil {
		t.Error("wrong method: want refusal, got allow")
	}
	if err := der.Allow(http.MethodPost, a("")); err == nil {
		t.Error("der role: want refusal, got allow")
	}
	if err := agg.Allow(http.MethodPost, Action{Kind: KindResponsePost}); err == nil {
		t.Error("aggregator self KindResponsePost must stay refused")
	}
	if got := KindFlowReservationResponsePost.String(); got != "FlowReservationResponsePost" {
		t.Errorf("String() = %q", got)
	}
}

// The flow reservation kinds are named in refusal messages, so each name is
// pinned.
func TestKindString_FlowReservationKinds(t *testing.T) {
	for k, want := range map[Kind]string{
		KindFlowReservationPost:         "FlowReservationPost",
		KindFlowReservationResponsePost: "FlowReservationResponsePost",
		KindFlowReservationWithdraw:     "FlowReservationWithdraw",
	} {
		if got := k.String(); got != want {
			t.Errorf("Kind %d is named %q, want %q", int(k), got, want)
		}
	}
}
