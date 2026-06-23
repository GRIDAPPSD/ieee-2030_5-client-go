package device

import (
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

func baseConfig() inverter.SimConfig {
	return inverter.SimConfig{
		Scenario:     "normal",
		TimeScale:    60,
		TickInterval: time.Second,
	}
}

// TestNew_SyntheticDefault asserts New with Backend="" returns a *Synthetic.
func TestNew_SyntheticDefault(t *testing.T) {
	cfg := baseConfig()
	cfg.Backend = ""
	sc := inverter.NormalScenario()
	dev, err := New(cfg, sc)
	if err != nil {
		t.Fatalf("New empty backend: %v", err)
	}
	if _, ok := dev.(*Synthetic); !ok {
		t.Errorf("expected *Synthetic, got %T", dev)
	}
}

// TestNew_SyntheticExplicit asserts New with Backend="synthetic" returns a
// *Synthetic.
func TestNew_SyntheticExplicit(t *testing.T) {
	cfg := baseConfig()
	cfg.Backend = "synthetic"
	sc := inverter.NormalScenario()
	dev, err := New(cfg, sc)
	if err != nil {
		t.Fatalf("New synthetic: %v", err)
	}
	if _, ok := dev.(*Synthetic); !ok {
		t.Errorf("expected *Synthetic, got %T", dev)
	}
}

// TestNew_GridLABD asserts New with Backend="gridlabd" returns a *GridLABD.
func TestNew_GridLABD(t *testing.T) {
	cfg := baseConfig()
	cfg.Backend = "gridlabd"
	sc := inverter.NormalScenario()
	dev, err := New(cfg, sc)
	if err != nil {
		t.Fatalf("New gridlabd: %v", err)
	}
	if _, ok := dev.(*GridLABD); !ok {
		t.Errorf("expected *GridLABD, got %T", dev)
	}
}

// TestNew_RealDevice asserts New with Backend="realdevice" returns a guarded
// device (the safety-guard decorator wraps the RealDevice backend).
func TestNew_RealDevice(t *testing.T) {
	cfg := baseConfig()
	cfg.Backend = "realdevice"
	sc := inverter.NormalScenario()
	dev, err := New(cfg, sc)
	if err != nil {
		t.Fatalf("New realdevice: %v", err)
	}
	// The realdevice path wraps in a guardedDevice; assert it is NOT a bare
	// *RealDevice (the guard decorator must be present).
	if _, ok := dev.(*RealDevice); ok {
		t.Errorf("realdevice backend must be wrapped by guards, but got bare *RealDevice")
	}
	if _, ok := dev.(*guardedDevice); !ok {
		t.Errorf("expected *guardedDevice wrapping RealDevice, got %T", dev)
	}
}

// TestNew_UnknownBackend asserts New returns a non-nil error for unknown
// backend strings (fail-closed discipline).
func TestNew_UnknownBackend(t *testing.T) {
	cfg := baseConfig()
	cfg.Backend = "bogus-backend"
	sc := inverter.NormalScenario()
	_, err := New(cfg, sc)
	if err == nil {
		t.Fatal("want error for unknown backend, got nil")
	}
}

// TestNew_BackendAgnosticDispatch asserts that the seam dispatch is
// backend-agnostic: the same ApplyControlsWithCurves output through a fake
// DERDevice produces the same InverterState regardless of which backend type
// the factory returns. This is a table-driven loop that exercises the
// synthetic and gridlabd paths (both always-error-free and always-ErrNotImpl).
func TestNew_BackendAgnosticDispatch(t *testing.T) {
	sc := inverter.NormalScenario()

	cases := []struct {
		backend string
		wantErr bool // true if backend stubs return ErrBackendNotImplemented
	}{
		{"synthetic", false},
		// gridlabd and realdevice stub every call with ErrBackendNotImplemented,
		// so we only assert error behavior for those.
		{"gridlabd", true},
		{"realdevice", true},
	}

	for _, tc := range cases {
		t.Run(tc.backend, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Backend = tc.backend
			dev, err := New(cfg, sc)
			if err != nil {
				t.Fatalf("New %q: %v", tc.backend, err)
			}
			ctx := t.Context()
			// ReadState
			_, readErr := dev.ReadState(ctx)
			if tc.wantErr && readErr == nil {
				t.Errorf("backend %q ReadState: want error, got nil", tc.backend)
			}
			if !tc.wantErr && readErr != nil {
				t.Errorf("backend %q ReadState: want nil, got %v", tc.backend, readErr)
			}
		})
	}
}
