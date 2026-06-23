// IEEESIM-004 dispatch.New factory and ADR-003 invariant tests.

package dispatch

import (
	"context"
	"testing"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-core/pkg/sep2"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// TestNew_RoleSimulator asserts dispatch.New with Role=="simulator" returns a
// *SimulatorDispatcher (the default simulator consumer policy).
func TestNew_RoleSimulator(t *testing.T) {
	t.Parallel()
	d, err := New(inverter.SimConfig{Role: "simulator"})
	if err != nil {
		t.Fatalf("New(simulator): %v", err)
	}
	if _, ok := d.(*SimulatorDispatcher); !ok {
		t.Errorf("New(simulator) returned %T; want *SimulatorDispatcher", d)
	}
}

// TestNew_RoleEmpty asserts dispatch.New with Role=="" (the zero value)
// returns a *SimulatorDispatcher (default preserves existing behavior).
func TestNew_RoleEmpty(t *testing.T) {
	t.Parallel()
	d, err := New(inverter.SimConfig{})
	if err != nil {
		t.Fatalf("New(empty role): %v", err)
	}
	if _, ok := d.(*SimulatorDispatcher); !ok {
		t.Errorf("New(empty role) returned %T; want *SimulatorDispatcher", d)
	}
}

// TestNew_RoleProduction asserts dispatch.New with Role=="production" returns
// a *ProductionDispatcher.
func TestNew_RoleProduction(t *testing.T) {
	t.Parallel()
	d, err := New(inverter.SimConfig{Role: "production"})
	if err != nil {
		t.Fatalf("New(production): %v", err)
	}
	if _, ok := d.(*ProductionDispatcher); !ok {
		t.Errorf("New(production) returned %T; want *ProductionDispatcher", d)
	}
}

// TestNew_UnknownRole asserts dispatch.New returns a non-nil error for an
// unrecognized role (fail-closed discipline).
func TestNew_UnknownRole(t *testing.T) {
	t.Parallel()
	_, err := New(inverter.SimConfig{Role: "bogus"})
	if err == nil {
		t.Fatal("New(bogus): expected non-nil error; got nil")
	}
}

// TestADR003_TypeIsTheRole asserts that the Dispatch method on both concrete
// types produces its behavior with NO role input: the type IS the role.
// Table-driven: both dispatchers process a DERControlList notification from
// construction alone, with no role branching at dispatch time.
func TestADR003_TypeIsTheRole(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"

	cases := []struct {
		name string
		d    RegisterableDispatcher
	}{
		{"SimulatorDispatcher", NewSimulatorDispatcher()},
		{"ProductionDispatcher", NewProductionDispatcher()},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeFetcher{
				returnVal: sep2.DERControlList{
					DERControl: []sep2.DERControl{mkDERControl("ctrl-adr003")},
				},
			}
			cache := inverter.NewDERControlCache()
			if err := tc.d.RegisterDERControlList(fake, cache, listHref); err != nil {
				t.Fatalf("RegisterDERControlList: %v", err)
			}

			n := sep2.Notification{
				Resource:           sep2.Resource{Href: listHref},
				SubscribedResource: listHref,
				Status:             sep2.NotificationStatusChanged,
			}

			// Dispatch without any role parameter: the type alone determines
			// the behavior. Both types must fetch and cache identically.
			tc.d.Dispatch(context.Background(), n)

			if got := fake.calls(); got != 1 {
				t.Errorf("expected 1 fetch; got %d", got)
			}
			if got := cache.Len(); got != 1 {
				t.Errorf("expected cache len 1; got %d", got)
			}
		})
	}
}

// TestCoherenceCheck_ProductionWithSynthetic asserts that the production role
// paired with the synthetic backend emits a warning log and does NOT return
// an error. This is Fork C: production+synthetic is a permitted dry-run
// combination, not a hard error.
func TestCoherenceCheck_ProductionWithSynthetic(t *testing.T) {
	t.Parallel()
	// coherenceCheck is tested via the function exposed in select.go.
	// The test asserts the warned=true, err=nil return for the dry-run case.
	warned, err := CheckRoleBackendCoherence("production", "synthetic")
	if err != nil {
		t.Errorf("production+synthetic should be permitted (dry-run); got error: %v", err)
	}
	if !warned {
		t.Error("production+synthetic should emit a warning; got warned=false")
	}
}

// TestCoherenceCheck_ProductionWithRealDevice asserts the production+realdevice
// combination is the expected production path: no warning, no error.
func TestCoherenceCheck_ProductionWithRealDevice(t *testing.T) {
	t.Parallel()
	warned, err := CheckRoleBackendCoherence("production", "realdevice")
	if err != nil {
		t.Errorf("production+realdevice should be clean; got error: %v", err)
	}
	if warned {
		t.Error("production+realdevice must not warn; got warned=true")
	}
}

// TestCoherenceCheck_SimulatorWithSynthetic asserts the default combination
// (simulator+synthetic) is clean.
func TestCoherenceCheck_SimulatorWithSynthetic(t *testing.T) {
	t.Parallel()
	warned, err := CheckRoleBackendCoherence("simulator", "synthetic")
	if err != nil {
		t.Errorf("simulator+synthetic should be clean; got error: %v", err)
	}
	if warned {
		t.Error("simulator+synthetic must not warn; got warned=true")
	}
}

// TestCoherenceCheck_ProductionWithGridLabD asserts that production+gridlabd
// triggers a warning (same as any non-realdevice backend) and returns no
// error. GridLAB-D is a co-simulation backend used for hardware-in-the-loop
// commissioning: the combination is intentional but unusual enough to warrant
// a log warning so the operator knows the mode.
func TestCoherenceCheck_ProductionWithGridLabD(t *testing.T) {
	t.Parallel()
	warned, err := CheckRoleBackendCoherence("production", "gridlabd")
	if err != nil {
		t.Errorf("production+gridlabd should be permitted with warning; got error: %v", err)
	}
	if !warned {
		t.Error("production+gridlabd should emit a warning; got warned=false")
	}
}
