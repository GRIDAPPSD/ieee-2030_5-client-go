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
	readErr  error // if non-nil, ReadState returns this error and does not update lastReadTime
	retState inverter.InverterState
}

func (f *fakeDevice) ReadState(_ context.Context) (StateReading, error) {
	if f.readErr != nil {
		return StateReading{}, f.readErr
	}
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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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

// ---------------------------------------------------------------------------
// Guard 2: scale-bounds validation
// ---------------------------------------------------------------------------

// TestGuard2_RejectActivePower_OverScaleBound asserts that guard 2 rejects
// ActivePowerW values exceeding defaultScaleMultiplier times RatedW.
// The inner backend must receive zero calls (not reached on reject).
func TestGuard2_RejectActivePower_OverScaleBound(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

	// Submit a value that is 101x the scale bound to ensure guard 2 fires.
	// scaleMaxW = RatedW * 100 = 1_000_000 W; 1_010_000 exceeds it.
	overScale := inverter.Rating.RatedW*defaultScaleMultiplier + 10000

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: overScale,
	})
	if err == nil {
		t.Fatal("guard 2: want error for over-scale ActivePowerW, got nil")
	}
	if !errors.Is(err, ErrScaleMismatch) {
		t.Errorf("guard 2: want ErrScaleMismatch, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 2: inner was called %d time(s), want 0 (guard must block write)", len(fake.calls))
	}
}

// TestGuard2_RejectActivePower_NegativeOverScaleBound asserts guard 2 rejects
// ActivePowerW whose negative magnitude exceeds the scale bound.
func TestGuard2_RejectActivePower_NegativeOverScaleBound(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

	negOverScale := -(inverter.Rating.RatedW*defaultScaleMultiplier + 10000)

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: negOverScale,
	})
	if err == nil {
		t.Fatal("guard 2: want error for negative over-scale ActivePowerW, got nil")
	}
	if !errors.Is(err, ErrScaleMismatch) {
		t.Errorf("guard 2: want ErrScaleMismatch, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 2: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard2_RejectReactivePower_OverScaleBound asserts guard 2 rejects
// positive ReactivePowerVAr values exceeding defaultScaleMultiplier times
// RatedVAr. Inner must not be called.
func TestGuard2_RejectReactivePower_OverScaleBound(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

	overScale := inverter.Rating.RatedVAr*defaultScaleMultiplier + 5000

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: overScale,
	})
	if err == nil {
		t.Fatal("guard 2: want error for over-scale ReactivePowerVAr, got nil")
	}
	if !errors.Is(err, ErrScaleMismatch) {
		t.Errorf("guard 2: want ErrScaleMismatch, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 2: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard2_RejectReactivePower_NegativeOverScaleBound asserts guard 2
// rejects negative VAr whose magnitude exceeds the scale bound.
func TestGuard2_RejectReactivePower_NegativeOverScaleBound(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

	negOverScale := -(inverter.Rating.RatedVAr*defaultScaleMultiplier + 5000)

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     5000,
		ReactivePowerVAr: negOverScale,
	})
	if err == nil {
		t.Fatal("guard 2: want error for negative over-scale ReactivePowerVAr, got nil")
	}
	if !errors.Is(err, ErrScaleMismatch) {
		t.Errorf("guard 2: want ErrScaleMismatch, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 2: inner was called %d time(s), want 0", len(fake.calls))
	}
}

// TestGuard2_AcceptsWithinBound asserts that a well-scaled setpoint passes
// guard 2 and reaches the inner backend with the exact submitted values
// (before guard 1 clamps, the values below nameplate should be unchanged).
func TestGuard2_AcceptsWithinBound(t *testing.T) {
	t.Parallel()
	wantPW := 5000.0
	wantVAr := 2000.0
	fake := &fakeDevice{retState: inverter.InverterState{ActivePowerW: wantPW, ReactivePowerVAr: wantVAr}}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     wantPW,
		ReactivePowerVAr: wantVAr,
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("guard 2: unexpected error for in-bound setpoint: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("guard 2: want 1 inner call, got %d", len(fake.calls))
	}
	// Values within nameplate pass guard 1 unchanged; assert exact field values.
	if fake.calls[0].ActivePowerW != wantPW {
		t.Errorf("guard 2: inner ActivePowerW=%.0f, want %.0f", fake.calls[0].ActivePowerW, wantPW)
	}
	if fake.calls[0].ReactivePowerVAr != wantVAr {
		t.Errorf("guard 2: inner ReactivePowerVAr=%.0f, want %.0f", fake.calls[0].ReactivePowerVAr, wantVAr)
	}
}

// TestGuard2_FailsClosedOnZeroNameplate asserts that guard 2 refuses the write
// when the nameplate has zero values (indeterminate scale bounds). The guard
// must not pass the write through as a default.
func TestGuard2_FailsClosedOnZeroNameplate(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{}
	// Zero nameplate: scaleMaxW and scaleMaxVAr derive as 0.
	guarded := WithSafetyGuards(fake, Nameplate{RatedW: 0, RatedVAr: 0}, GuardConfig{})

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: 5000,
	})
	if err == nil {
		t.Fatal("guard 2: want error for zero-nameplate (indeterminate scale), got nil")
	}
	if !errors.Is(err, ErrScaleMismatch) {
		t.Errorf("guard 2: want ErrScaleMismatch, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 2: inner called %d time(s), want 0 (fail-closed)", len(fake.calls))
	}
}

// ---------------------------------------------------------------------------
// Guard 5: malformed / NaN / infinite reject
// ---------------------------------------------------------------------------

// TestGuard5_RejectNaN asserts guard 5: a NaN ActivePowerW returns an error
// and the inner backend ApplySetpoint is NEVER called (zero calls on the fake).
func TestGuard5_RejectNaN(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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

// ---------------------------------------------------------------------------
// Guard 3: write-rate limiting
// ---------------------------------------------------------------------------

// TestGuard3_RateLimit asserts that rapid calls beyond the burst limit return
// ErrRateLimitExceeded and the inner device receives only the allowed count.
func TestGuard3_RateLimit(t *testing.T) {
	fake := &fakeDevice{retState: inverter.InverterState{}}
	// Use a narrow bucket: burst=3 per 10 seconds so we can saturate quickly.
	guarded := &guardedDevice{
		inner:       fake,
		nameplate:   testNameplate(),
		scaleMaxW:   testNameplate().RatedW * defaultScaleMultiplier,
		scaleMaxVAr: testNameplate().RatedVAr * defaultScaleMultiplier,
		limiter:     newTokenBucket(3, 10*time.Second),
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
		inner:       fake,
		nameplate:   testNameplate(),
		scaleMaxW:   testNameplate().RatedW * defaultScaleMultiplier,
		scaleMaxVAr: testNameplate().RatedVAr * defaultScaleMultiplier,
		limiter:     newTokenBucket(2, window),
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

// ---------------------------------------------------------------------------
// Guard 4: staleness bound
// ---------------------------------------------------------------------------

// TestGuard4_Staleness_NeverRead asserts that guard 4 refuses ApplySetpoint
// when ReadState has never been called (lastReadTime is zero). The inner
// backend must not be called; this is the fail-closed posture.
func TestGuard4_Staleness_NeverRead(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{retState: inverter.InverterState{ActivePowerW: 5000}}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{MaxStateAge: 100 * time.Millisecond})

	// No ReadState call before ApplySetpoint.
	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: 5000,
		Connected:    true,
		Energized:    true,
	})
	if err == nil {
		t.Fatal("guard 4: want error when ReadState never called, got nil")
	}
	if !errors.Is(err, ErrStaleState) {
		t.Errorf("guard 4: want ErrStaleState, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 4: inner called %d time(s), want 0 (fail-closed)", len(fake.calls))
	}
}

// TestGuard4_Staleness_TooOld asserts that guard 4 refuses ApplySetpoint
// when the last ReadState was called longer ago than MaxStateAge. Inner must
// not be called (the write must be blocked before reaching the device).
func TestGuard4_Staleness_TooOld(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{retState: inverter.InverterState{ActivePowerW: 5000}}
	maxAge := 100 * time.Millisecond
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{MaxStateAge: maxAge})

	// Call ReadState to establish a valid timestamp.
	if _, err := guarded.ReadState(context.Background()); err != nil {
		t.Fatalf("ReadState: %v", err)
	}

	// Wait past the staleness window with generous margin to avoid CI flakes.
	time.Sleep(maxAge + 50*time.Millisecond)

	ctrl := inverter.ControlOutputs{ActivePowerW: 5000, Connected: true, Energized: true}
	_, err := guarded.ApplySetpoint(context.Background(), ctrl)
	if err == nil {
		t.Fatal("guard 4: want error for stale state, got nil")
	}
	if !errors.Is(err, ErrStaleState) {
		t.Errorf("guard 4: want ErrStaleState, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 4: inner called %d time(s) after stale rejection, want 0", len(fake.calls))
	}
}

// TestGuard4_Staleness_Fresh asserts that a setpoint is accepted when
// ReadState was called within MaxStateAge. The inner backend receives the
// call with the exact clamped field values.
func TestGuard4_Staleness_Fresh(t *testing.T) {
	t.Parallel()
	wantPW := 5000.0
	wantVAr := 1000.0
	fake := &fakeDevice{
		retState: inverter.InverterState{
			ActivePowerW:     wantPW,
			ReactivePowerVAr: wantVAr,
			Connected:        true,
			Energized:        true,
		},
	}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{MaxStateAge: 500 * time.Millisecond})

	// ReadState immediately before ApplySetpoint: state is fresh.
	if _, err := guarded.ReadState(context.Background()); err != nil {
		t.Fatalf("ReadState: %v", err)
	}

	state, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW:     wantPW,
		ReactivePowerVAr: wantVAr,
		Connected:        true,
		Energized:        true,
	})
	if err != nil {
		t.Fatalf("guard 4: unexpected error for fresh state: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("guard 4: want 1 inner call, got %d", len(fake.calls))
	}
	// Assert field values on both the argument that reached inner and the
	// returned state. This confirms the write reached the device with the
	// correct payload and returned meaningful data.
	if fake.calls[0].ActivePowerW != wantPW {
		t.Errorf("guard 4: inner ActivePowerW=%.0f, want %.0f", fake.calls[0].ActivePowerW, wantPW)
	}
	if fake.calls[0].ReactivePowerVAr != wantVAr {
		t.Errorf("guard 4: inner ReactivePowerVAr=%.0f, want %.0f", fake.calls[0].ReactivePowerVAr, wantVAr)
	}
	if state.ActivePowerW != wantPW {
		t.Errorf("guard 4: returned state ActivePowerW=%.0f, want %.0f", state.ActivePowerW, wantPW)
	}
	if state.ReactivePowerVAr != wantVAr {
		t.Errorf("guard 4: returned state ReactivePowerVAr=%.0f, want %.0f", state.ReactivePowerVAr, wantVAr)
	}
}

// TestGuard4_Staleness_Disabled asserts that when MaxStateAge is zero
// (guard 4 disabled), ApplySetpoint succeeds even without a prior ReadState
// call. This is the correct configuration for simulator backends and tests
// that exercise other guards in isolation.
func TestGuard4_Staleness_Disabled(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{retState: inverter.InverterState{ActivePowerW: 5000}}
	// GuardConfig{} has MaxStateAge == 0: staleness guard disabled.
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

	_, err := guarded.ApplySetpoint(context.Background(), inverter.ControlOutputs{
		ActivePowerW: 5000,
		Connected:    true,
		Energized:    true,
	})
	if err != nil {
		t.Fatalf("guard 4 disabled: unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("guard 4 disabled: want 1 inner call, got %d", len(fake.calls))
	}
}

// TestGuard4_Staleness_FailedReadStateDoesNotAdvanceClock asserts that a
// failed ReadState call does NOT advance the staleness clock. Only a
// successful ReadState extends the freshness window. The test calls ReadState
// on a device that returns an error, then verifies ApplySetpoint is still
// refused with ErrStaleState because the clock was never advanced.
func TestGuard4_Staleness_FailedReadStateDoesNotAdvanceClock(t *testing.T) {
	t.Parallel()
	// fakeDevice whose ReadState always returns an error.
	fake := &fakeDevice{
		readErr:  errors.New("simulated read failure"),
		retState: inverter.InverterState{ActivePowerW: 5000},
	}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{MaxStateAge: 100 * time.Millisecond})

	// Call ReadState: the inner returns an error, so lastReadTime must NOT advance.
	if _, err := guarded.ReadState(context.Background()); err == nil {
		t.Fatal("ReadState: want error from failing device, got nil")
	}

	// ApplySetpoint must be refused: the staleness clock was never advanced.
	ctrl := inverter.ControlOutputs{ActivePowerW: 5000, Connected: true, Energized: true}
	_, err := guarded.ApplySetpoint(context.Background(), ctrl)
	if err == nil {
		t.Fatal("guard 4: want ErrStaleState when only failed ReadState was called, got nil")
	}
	if !errors.Is(err, ErrStaleState) {
		t.Errorf("guard 4: want ErrStaleState, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("guard 4: inner ApplySetpoint called %d time(s), want 0", len(fake.calls))
	}
}

// ---------------------------------------------------------------------------
// Fail-safe recovery (post-write, not a numbered pre-write guard)
// ---------------------------------------------------------------------------

// TestGuard4_FailSafe_LastGood asserts the fail-safe recovery: when the inner
// backend returns an error AFTER a successful call, the guarded device returns
// the cached lastGood state field-by-field (items 4, 14, 15).
func TestGuard4_FailSafe_LastGood(t *testing.T) {
	t.Parallel()
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
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})
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

// TestGuard4_FailSafe_SafeDefault asserts the fail-safe recovery: when no
// good state has ever been cached, the fail-safe returns the safe default
// (disconnected, zero P/Q, ModeDisconnected) (items 4, 15).
func TestGuard4_FailSafe_SafeDefault(t *testing.T) {
	t.Parallel()
	fake := &fakeDevice{retErr: errors.New("device unreachable")}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})

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

// ---------------------------------------------------------------------------
// ReadState passthrough
// ---------------------------------------------------------------------------

// TestGuard_ReadStatePassthrough asserts that ReadState bypasses all write
// guards and delegates directly to inner.
func TestGuard_ReadStatePassthrough(t *testing.T) {
	fake := &fakeDevice{}
	guarded := WithSafetyGuards(fake, testNameplate(), GuardConfig{})
	reading, err := guarded.ReadState(context.Background())
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	// fake always returns VoltsPU=1.0, FreqHz=60.0.
	if reading.Grid.VoltsPU != 1.0 {
		t.Errorf("ReadState passthrough: want VoltsPU=1.0, got %v", reading.Grid.VoltsPU)
	}
}

// ---------------------------------------------------------------------------
// Backend stub assertions
// ---------------------------------------------------------------------------

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
