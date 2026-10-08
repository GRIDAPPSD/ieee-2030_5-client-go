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
