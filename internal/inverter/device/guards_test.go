package device

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// fakeDevice is a recording DERDevice that captures ApplySetpoint arguments
// for assertions and can be configured to return errors.
type fakeDevice struct {
	calls    []inverter.ControlOutputs
	retErr   error
	retState inverter.InverterState
}

func (f *fakeDevice) ReadState(_ context.Context) (StateReading, error) {
	return StateReading{Grid: inverter.GridState{VoltsPU: 1.0, FreqHz: 60.0}}, nil
}

func (f *fakeDevice) ApplySetpoint(_ context.Context, c inverter.ControlOutputs) (inverter.InverterState, error) {
	f.calls = append(f.calls, c)
	if f.retErr != nil {
		return inverter.InverterState{}, f.retErr
	}
	return f.retState, nil
}

func testNameplate() Nameplate {
	return Nameplate{
		RatedW:   inverter.Rating.RatedW,
		RatedVAr: inverter.Rating.RatedVAr,
	}
}

// TestGuard1_ClampActivePower asserts guard 1: a 12kW request on a 10kW
// nameplate delegates to inner with ActivePowerW == 10000 (exact field value).
func TestGuard1_ClampActivePower(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{ActivePowerW: 10000}}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     12000, // over nameplate
		ReactivePowerVAr: 0,
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("want 1 inner call, got %d", len(fake.calls))
	}
	if fake.calls[0].ActivePowerW != 10000 {
		t.Errorf("guard 1: inner received ActivePowerW=%.0f, want 10000", fake.calls[0].ActivePowerW)
	}
}

// TestGuard1_ClampNegativeActivePower asserts guard 1 clamps negative
// ActivePowerW to 0. Negative P is on-the-floor, not malformed (item 7).
func TestGuard1_ClampNegativeActivePower(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{}}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     -500, // below zero
		ReactivePowerVAr: 0,
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("want 1 inner call, got %d", len(fake.calls))
	}
	if fake.calls[0].ActivePowerW != 0 {
		t.Errorf("guard 1: inner received ActivePowerW=%.0f, want 0", fake.calls[0].ActivePowerW)
	}
}

// TestGuard1_ExactNameplateBoundary asserts that values exactly at the
// nameplate boundary are NOT clamped and reach inner unchanged (item 8).
// Guard 1 uses strict > and strict <, so on-the-dot must pass through.
func TestGuard1_ExactNameplateBoundary(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{}}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     inverter.Rating.RatedW,   // exactly 10000.0
		ReactivePowerVAr: inverter.Rating.RatedVAr, // exactly 4400.0
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("want 1 inner call, got %d", len(fake.calls))
	}
	if fake.calls[0].ActivePowerW != inverter.Rating.RatedW {
		t.Errorf("boundary: inner received ActivePowerW=%.4f, want %.4f",
			fake.calls[0].ActivePowerW, inverter.Rating.RatedW)
	}
	if fake.calls[0].ReactivePowerVAr != inverter.Rating.RatedVAr {
		t.Errorf("boundary: inner received ReactivePowerVAr=%.4f, want %.4f",
			fake.calls[0].ReactivePowerVAr, inverter.Rating.RatedVAr)
	}
}

// TestGuard1_ClampReactivePower_PositiveRail asserts guard 1 clamps positive
// VAr to +RatedVAr.
func TestGuard1_ClampReactivePower_PositiveRail(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{}}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: 9000, // over +RatedVAr (4400)
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("want 1 inner call, got %d", len(fake.calls))
	}
	if fake.calls[0].ReactivePowerVAr != inverter.Rating.RatedVAr {
		t.Errorf("guard 1: inner received ReactivePowerVAr=%.0f, want %.0f",
			fake.calls[0].ReactivePowerVAr, inverter.Rating.RatedVAr)
	}
}

// TestGuard1_ClampReactivePower_NegativeRail asserts guard 1 clamps negative
// VAr to -RatedVAr.
func TestGuard1_ClampReactivePower_NegativeRail(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{}}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: -9000, // over -RatedVAr (-4400)
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("want 1 inner call, got %d", len(fake.calls))
	}
	if fake.calls[0].ReactivePowerVAr != -inverter.Rating.RatedVAr {
		t.Errorf("guard 1: inner received ReactivePowerVAr=%.0f, want %.0f",
			fake.calls[0].ReactivePowerVAr, -inverter.Rating.RatedVAr)
	}
}

// TestGuard5_RejectNaN asserts guard 5: a NaN ActivePowerW returns an error
// and the inner backend ApplySetpoint is NEVER called (zero calls on the fake).
func TestGuard5_RejectNaN(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: math.NaN(),
	})
	if err == nil {
		t.Fatal("want error for NaN ActivePowerW, got nil")
	}
	if !errors.Is(err, ErrMalformedControl) {
		t.Errorf("want ErrMalformedControl, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 5: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard5_RejectNaN_ReactivePower asserts guard 5 rejects NaN VAr.
func TestGuard5_RejectNaN_ReactivePower(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: math.NaN(),
	})
	if err == nil {
		t.Fatal("want error for NaN ReactivePowerVAr, got nil")
	}
	if !errors.Is(err, ErrMalformedControl) {
		t.Errorf("want ErrMalformedControl, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 5: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard5_RejectInf asserts guard 5 rejects +Inf ActivePowerW (item 9).
func TestGuard5_RejectInf(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: math.Inf(1),
	})
	if err == nil {
		t.Fatal("want error for +Inf ActivePowerW, got nil")
	}
	if !errors.Is(err, ErrMalformedControl) {
		t.Errorf("want ErrMalformedControl, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 5: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard5_RejectNegInf_ActivePower asserts guard 5 rejects -Inf
// ActivePowerW (item 9).
func TestGuard5_RejectNegInf_ActivePower(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: math.Inf(-1),
	})
	if err == nil {
		t.Fatal("want error for -Inf ActivePowerW, got nil")
	}
	if !errors.Is(err, ErrMalformedControl) {
		t.Errorf("want ErrMalformedControl, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 5: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard5_RejectPosInf_ReactivePower asserts guard 5 rejects +Inf VAr
// (item 9).
func TestGuard5_RejectPosInf_ReactivePower(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: math.Inf(1),
	})
	if err == nil {
		t.Fatal("want error for +Inf ReactivePowerVAr, got nil")
	}
	if !errors.Is(err, ErrMalformedControl) {
		t.Errorf("want ErrMalformedControl, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 5: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard5_RejectNegInf_ReactivePower asserts guard 5 rejects -Inf VAr
// (item 9).
func TestGuard5_RejectNegInf_ReactivePower(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate())

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: math.Inf(-1),
	})
	if err == nil {
		t.Fatal("want error for -Inf ReactivePowerVAr, got nil")
	}
	if !errors.Is(err, ErrMalformedControl) {
		t.Errorf("want ErrMalformedControl, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 5: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard3_RateLimit asserts that rapid calls beyond the burst limit return
// ErrRateLimitExceeded and the inner device receives only the allowed count.
func TestGuard3_RateLimit(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{}}
	// Use a narrow bucket: burst=3 per 10 seconds so we can saturate quickly.
	guarded := &guardedDevice{
		inner:     fake,
		nameplate: testNameplate(),
		limiter:   newTokenBucket(3, 10*time.Second),
	}

	goodCtrl := inverter.ControlOutputs{
		ActivePowerW: 5000,
		Connected:    true,
		Energized:    true,
	}

	successCount := 0
	rateLimitCount := 0
	for range 10 {
		_, err := guarded.ApplySetpoint(context.Background(), goodCtrl)
		if err == nil {
			successCount++
		} else if errors.Is(err, ErrRateLimitExceeded) {
			rateLimitCount++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if successCount != 3 {
		t.Errorf("rate limit: want 3 successes, got %d", successCount)
	}
	if rateLimitCount != 7 {
		t.Errorf("rate limit: want 7 rate-limit errors, got %d", rateLimitCount)
	}
	if len(fake.calls) != 3 {
		t.Errorf("rate limit: inner received %d calls, want 3", len(fake.calls))
	}
}

// TestGuard3_RateLimitRefill asserts that the token bucket refills after the
// configured window, allowing calls to succeed again (item 10).
func TestGuard3_RateLimitRefill(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{}}
	// Very short window so the test can advance without sleeping.
	window := 5 * time.Millisecond
	guarded := &guardedDevice{
		inner:     fake,
		nameplate: testNameplate(),
		limiter:   newTokenBucket(2, window),
	}

	goodCtrl := inverter.ControlOutputs{
		ActivePowerW: 3000,
		Connected:    true,
		Energized:    true,
	}

	// Exhaust the burst.
	for i := range 2 {
		if _, err := guarded.ApplySetpoint(context.Background(), goodCtrl); err != nil {
			t.Fatalf("pre-exhaust call %d: %v", i, err)
		}
	}
	if _, err := guarded.ApplySetpoint(context.Background(), goodCtrl); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("post-exhaust: want ErrRateLimitExceeded, got %v", err)
	}

	// Advance past the window by sleeping slightly longer.
	time.Sleep(window + 2*time.Millisecond)

	// Bucket should be refilled; one more call must succeed.
	if _, err := guarded.ApplySetpoint(context.Background(), goodCtrl); err != nil {
		t.Errorf("after refill: want success, got %v", err)
	}
}

// TestGuard4_FailSafe_LastGood asserts guard 4: when the inner backend returns
// an error AFTER a successful call, the guarded device returns the cached
// lastGood state field-by-field (items 4, 14, 15).
func TestGuard4_FailSafe_LastGood(t *testing.T) {
	good := inverter.InverterState{
		ActivePowerW:     7500,
		ReactivePowerVAr: 500,
		PowerFactor:      0.998,
		VoltsPU:          1.01,
		FreqHz:           60.0,
		Connected:        true,
		Energized:        true,
		Mode:             inverter.ModeVoltVar,
	}
	fake := &fakeDevice{retState: good}
	guarded := WithSafetyGuards(fake, testNameplate())
	ctx := context.Background()

	// First call succeeds: caches lastGood.
	ctrl := inverter.ControlOutputs{
		ActivePowerW: 7500,
		Connected:    true,
		Energized:    true,
	}
	if _, err := guarded.ApplySetpoint(ctx, ctrl); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call: inner returns an error.
	fake.retErr = errors.New("simulated comms loss")
	state, err := guarded.ApplySetpoint(ctx, ctrl)
	if err == nil {
		t.Fatal("want error on comms loss, got nil")
	}
	// The returned state must be lastGood, field by field (items 4, 14, 15).
	if state.ActivePowerW != good.ActivePowerW {
		t.Errorf("fail-safe: ActivePowerW: want %.0f, got %.0f", good.ActivePowerW, state.ActivePowerW)
	}
	if state.ReactivePowerVAr != good.ReactivePowerVAr {
		t.Errorf("fail-safe: ReactivePowerVAr: want %.0f, got %.0f", good.ReactivePowerVAr, state.ReactivePowerVAr)
	}
	if state.PowerFactor != good.PowerFactor {
		t.Errorf("fail-safe: PowerFactor: want %.4f, got %.4f", good.PowerFactor, state.PowerFactor)
	}
	if state.VoltsPU != good.VoltsPU {
		t.Errorf("fail-safe: VoltsPU: want %.4f, got %.4f", good.VoltsPU, state.VoltsPU)
	}
	if state.Connected != good.Connected {
		t.Errorf("fail-safe: Connected: want %v, got %v", good.Connected, state.Connected)
	}
	if state.Energized != good.Energized {
		t.Errorf("fail-safe: Energized: want %v, got %v", good.Energized, state.Energized)
	}
	if state.Mode != good.Mode {
		t.Errorf("fail-safe: Mode: want %v, got %v", good.Mode, state.Mode)
	}
}

// TestGuard4_FailSafe_SafeDefault asserts guard 4: when no good state has ever
// been cached, the fail-safe returns the safe default (disconnected, zero P/Q,
// ModeDisconnected) (items 4, 15).
func TestGuard4_FailSafe_SafeDefault(t *testing.T) {
	fake := &fakeDevice{retErr: errors.New("device unreachable")}
	guarded := WithSafetyGuards(fake, testNameplate())

	state, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: 5000,
		Connected:    true,
		Energized:    true,
	})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if state.Connected {
		t.Errorf("safe default: want Connected=false, got true")
	}
	if state.Energized {
		t.Errorf("safe default: want Energized=false, got true")
	}
	if state.ActivePowerW != 0 {
		t.Errorf("safe default: want ActivePowerW=0, got %.0f", state.ActivePowerW)
	}
	if state.Mode != inverter.ModeDisconnected {
		t.Errorf("safe default: want Mode=ModeDisconnected, got %v", state.Mode)
	}
}

// TestGuard_ReadStatePassthrough asserts that ReadState bypasses all guards
// and delegates directly to inner.
func TestGuard_ReadStatePassthrough(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate())
	reading, err := guarded.ReadState(context.Background())
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	// fake always returns VoltsPU=1.0, FreqHz=60.0.
	if reading.Grid.VoltsPU != 1.0 {
		t.Errorf("ReadState passthrough: want VoltsPU=1.0, got %v", reading.Grid.VoltsPU)
	}
}

// TestGridLABD_ApplySetpoint_NotImplemented asserts that GridLABD.ApplySetpoint
// returns ErrBackendNotImplemented (item 11).
func TestGridLABD_ApplySetpoint_NotImplemented(t *testing.T) {
	g, err := NewGridLABD(GridLABDConfig{})
	if err != nil {
		t.Fatalf("NewGridLABD: %v", err)
	}
	_, applyErr := g.ApplySetpoint(context.Background(), inverter.ControlOutputs{})
	if !errors.Is(applyErr, ErrBackendNotImplemented) {
		t.Errorf("want ErrBackendNotImplemented, got %v", applyErr)
	}
}

// TestRealDevice_ApplySetpoint_NotImplemented asserts that
// RealDevice.ApplySetpoint returns ErrBackendNotImplemented (item 11).
func TestRealDevice_ApplySetpoint_NotImplemented(t *testing.T) {
	r, err := NewRealDevice(RealDeviceConfig{})
	if err != nil {
		t.Fatalf("NewRealDevice: %v", err)
	}
	_, applyErr := r.ApplySetpoint(context.Background(), inverter.ControlOutputs{})
	if !errors.Is(applyErr, ErrBackendNotImplemented) {
		t.Errorf("want ErrBackendNotImplemented, got %v", applyErr)
	}
}
