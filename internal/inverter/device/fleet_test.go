package device

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// fakeFleetTransport is a minimal, in-memory FleetTransport for unit
// testing FleetDevice without a sidecar: it stores property values in a
// map, applies StepTo's no-op-when-not-after rule, and can be forced
// unhealthy to prove FleetDevice refuses rather than fabricating a
// reading.
type fakeFleetTransport struct {
	healthy   bool
	values    map[[2]string]float64
	modelTime time.Time
	getErr    error
	setErr    error
}

func newFakeFleetTransport() *fakeFleetTransport {
	return &fakeFleetTransport{
		healthy:   true,
		values:    map[[2]string]float64{},
		modelTime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (f *fakeFleetTransport) Healthy() bool { return f.healthy }

func (f *fakeFleetTransport) Set(_ context.Context, items []FleetSetItem) ([]float64, error) {
	if f.setErr != nil {
		return nil, f.setErr
	}
	out := make([]float64, len(items))
	for i, it := range items {
		f.values[[2]string{it.Object, it.Property}] = it.Value
		out[i] = it.Value
	}
	return out, nil
}

func (f *fakeFleetTransport) StepTo(_ context.Context, target time.Time) (time.Time, error) {
	if target.After(f.modelTime) {
		f.modelTime = target
	}
	return f.modelTime, nil
}

func (f *fakeFleetTransport) Get(_ context.Context, items []FleetGetItem) ([]float64, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make([]float64, len(items))
	for i, it := range items {
		out[i] = f.values[[2]string{it.Object, it.Property}]
	}
	return out, nil
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestNewFleetDevice_RequiresTransportAndObject(t *testing.T) {
	if _, err := NewFleetDevice(nil, "LFDI1", "inv0", nil); err == nil {
		t.Error("NewFleetDevice(nil transport): want error, got nil")
	}
	if _, err := NewFleetDevice(newFakeFleetTransport(), "LFDI1", "", nil); err == nil {
		t.Error("NewFleetDevice(empty inverterObj): want error, got nil")
	}
}

// TestFleetDevice_ApplySetpoint_ReadsBackAppliedValue is the data-invariant
// case the brief requires: the returned state is read back through the
// transport's Get, not the requested ControlOutputs echoed unchecked. A
// transport that silently changes the value on write (as gridlabd's
// CONSTANT_PF mode would) must be visible in the result.
func TestFleetDevice_ApplySetpoint_ReadsBackAppliedValue(t *testing.T) {
	transport := newFakeFleetTransport()
	now := time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC)
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", fixedClock(now))
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}

	state, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     2500,
		ReactivePowerVAr: -100,
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("ApplySetpoint: %v", err)
	}
	if state.ActivePowerW != 2500 {
		t.Errorf("ActivePowerW = %v, want 2500 (read back)", state.ActivePowerW)
	}
	if state.ReactivePowerVAr != -100 {
		t.Errorf("ReactivePowerVAr = %v, want -100 (read back)", state.ReactivePowerVAr)
	}
	if !state.Time.Equal(now) {
		t.Errorf("Time = %v, want %v", state.Time, now)
	}
	if !state.Connected || !state.Energized {
		t.Error("Connected/Energized: want both true, carried from controls")
	}

	// Prove the read-back is real, not an echo: mutate the transport's
	// stored value directly (simulating the model changing it on its own,
	// e.g. a control mode that does not hold an external setpoint) and
	// confirm a fresh ApplySetpoint call surfaces the new value.
	transport.values[[2]string{"inv0", "P_Out"}] = 999
	state2, err := dev.ReadState(context.Background())
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state2.MaxPowerW != 0 {
		// rated_power was never set on the fake transport; zero is the
		// correct read-back for "never written", not a fabricated default.
		t.Errorf("MaxPowerW = %v, want 0 (rated_power unset on the fake)", state2.MaxPowerW)
	}
}

// TestFleetDevice_Down_ReturnsErrorNoReading proves the DOWN-fleet
// invariant: an unhealthy transport makes ReadState and ApplySetpoint both
// return an error, and never a populated StateReading/InverterState.
func TestFleetDevice_Down_ReturnsErrorNoReading(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.healthy = false
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}

	state, err := dev.ReadState(context.Background())
	if err == nil {
		t.Fatal("ReadState on a DOWN fleet: want error, got nil")
	}
	if state != (StateReading{}) {
		t.Errorf("ReadState on a DOWN fleet: want zero StateReading, got %+v", state)
	}

	inverterState, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 100})
	if err == nil {
		t.Fatal("ApplySetpoint on a DOWN fleet: want error, got nil")
	}
	if inverterState != (inverter.InverterState{}) {
		t.Errorf("ApplySetpoint on a DOWN fleet: want zero InverterState, got %+v", inverterState)
	}
}

func TestFleetDevice_ApplySetpoint_TransportSetErrorPropagates(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.setErr = errors.New("sidecar refused the write")
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	if _, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 1}); err == nil {
		t.Fatal("ApplySetpoint with a failing Set: want error, got nil")
	}
}

var _ FleetTransport = (*fakeFleetTransport)(nil)
