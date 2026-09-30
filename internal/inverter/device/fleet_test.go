package device

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// fakeFleetTransport is a minimal, in-memory FleetTransport for unit
// testing FleetDevice without a sidecar: it stores property values in a
// map, applies StepTo's no-op-when-not-after rule, and can be forced
// unhealthy to prove FleetDevice refuses rather than fabricating a
// reading. mu guards every field below so two goroutines driving the same
// fake (a concurrency test) never race on it; FleetDevice's own locking is
// what is under test, not this fake's.
type fakeFleetTransport struct {
	mu sync.Mutex

	healthy    bool
	generation uint64
	values     map[[2]string]float64
	modelTime  time.Time
	getErr     error
	setErr     error

	// getOverride, when set for a key, makes Get return this value
	// regardless of what Set stored. Proves a result comes from a real Get
	// call rather than being echoed from the request: only a real Get
	// would surface an override the caller never set.
	getOverride map[[2]string]FleetValue
	// shortGetBy truncates Get's reply by this many items, proving the
	// caller checks reply length rather than indexing positionally.
	shortGetBy int
	// setCalls records every Set call, so a test can assert whether a
	// restart resync happened.
	setCalls [][]FleetSetItem

	// pauseSetOnP, when nonzero, makes Set() pause (after recording the
	// call, before returning) the first time it sees an item whose
	// Property is "P_Out" and whose Value equals pauseSetOnP: it closes
	// pauseReached, then blocks on pauseResume. This lets a test force a
	// specific interleaving between a resync's Set and a concurrent
	// caller's own Set, deterministically, rather than relying on
	// goroutine scheduling to reproduce a race window.
	pauseSetOnP  float64
	pauseReached chan struct{}
	pauseResume  chan struct{}
}

func newFakeFleetTransport() *fakeFleetTransport {
	return &fakeFleetTransport{
		healthy:    true,
		generation: 1,
		values:     map[[2]string]float64{},
		modelTime:  time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (f *fakeFleetTransport) Healthy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy
}

func (f *fakeFleetTransport) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation
}

func (f *fakeFleetTransport) Set(_ context.Context, items []FleetSetItem) ([]float64, error) {
	f.mu.Lock()
	if f.setErr != nil {
		err := f.setErr
		f.mu.Unlock()
		return nil, err
	}
	f.setCalls = append(f.setCalls, items)
	out := make([]float64, len(items))
	for i, it := range items {
		f.values[[2]string{it.Object, it.Property}] = it.Value
		out[i] = it.Value
	}
	pauseP, reached, resume := f.pauseSetOnP, f.pauseReached, f.pauseResume
	f.mu.Unlock()

	if pauseP != 0 {
		for _, it := range items {
			if it.Property == "P_Out" && it.Value == pauseP {
				close(reached)
				<-resume
				break
			}
		}
	}
	return out, nil
}

func (f *fakeFleetTransport) StepTo(_ context.Context, target time.Time) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if target.After(f.modelTime) {
		f.modelTime = target
	}
	return f.modelTime, nil
}

func (f *fakeFleetTransport) Get(_ context.Context, items []FleetGetItem) ([]FleetValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make([]FleetValue, len(items))
	for i, it := range items {
		key := [2]string{it.Object, it.Property}
		if ov, ok := f.getOverride[key]; ok {
			out[i] = ov
			continue
		}
		out[i] = FleetValue{Re: f.values[key]}
	}
	if f.shortGetBy > 0 && f.shortGetBy <= len(out) {
		out = out[:len(out)-f.shortGetBy]
	}
	return out, nil
}

// resetModel simulates a sidecar restart: the GLM reloads from scratch
// (every property back to the template's defaults) and the generation the
// fleet is on advances.
func (f *fakeFleetTransport) resetModel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values = map[[2]string]float64{}
	f.generation++
}

// valueOf reads a stored property directly, for test assertions, without
// going through Get (which may be overridden).
func (f *fakeFleetTransport) valueOf(object, property string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[[2]string{object, property}]
}

// setCallCount reads the number of Set calls made so far.
func (f *fakeFleetTransport) setCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.setCalls)
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestNewFleetDevice_RequiresTransportAndObjects(t *testing.T) {
	if _, err := NewFleetDevice(nil, "LFDI1", "inv0", "meter0", nil); err == nil {
		t.Error("NewFleetDevice(nil transport): want error, got nil")
	}
	if _, err := NewFleetDevice(newFakeFleetTransport(), "LFDI1", "", "meter0", nil); err == nil {
		t.Error("NewFleetDevice(empty inverterObj): want error, got nil")
	}
	if _, err := NewFleetDevice(newFakeFleetTransport(), "LFDI1", "inv0", "", nil); err == nil {
		t.Error("NewFleetDevice(empty meterObj): want error, got nil")
	}
}

// TestFleetDevice_ApplySetpoint_ResultComesFromGetNotEcho is the
// mutation-killing case: if ApplySetpoint echoed controls.ActivePowerW and
// controls.ReactivePowerVAr instead of reading them back, this test would
// not notice unless the transport's Get can answer something the request
// never carried. getOverride does exactly that.
func TestFleetDevice_ApplySetpoint_ResultComesFromGetNotEcho(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.getOverride = map[[2]string]FleetValue{
		{"inv0", "P_Out"}: {Re: 777},
		{"inv0", "Q_Out"}: {Re: -333},
	}
	now := time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC)
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", fixedClock(now))
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
	if state.ActivePowerW != 777 {
		t.Errorf("ActivePowerW = %v, want 777 (read back via Get, not the requested 2500 echoed)", state.ActivePowerW)
	}
	if state.ReactivePowerVAr != -333 {
		t.Errorf("ReactivePowerVAr = %v, want -333 (read back via Get, not the requested -100 echoed)", state.ReactivePowerVAr)
	}
	if !state.Time.Equal(now) {
		t.Errorf("Time = %v, want %v", state.Time, now)
	}
	if !state.Connected || !state.Energized {
		t.Error("Connected/Energized: want both true, carried from controls")
	}
}

// TestFleetDevice_ApplySetpoint_TimeComesFromStepToNotClock is the
// mutation-killing case for InverterState.Time: the test above sets the
// clock to a value that is AFTER the model's current time, so StepTo moves
// the model to exactly that value and returns it, leaving d.now() and
// StepTo's return indistinguishable. Here the model is already ahead of
// the clock, so StepTo is a no-op that returns the model's own (later, and
// distinct) time; the assertion only holds if ApplySetpoint uses that
// returned value rather than d.now() directly.
func TestFleetDevice_ApplySetpoint_TimeComesFromStepToNotClock(t *testing.T) {
	transport := newFakeFleetTransport()
	laterModelTime := time.Date(2020, 1, 1, 0, 5, 0, 0, time.UTC)
	transport.modelTime = laterModelTime
	clockTime := time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC) // before the model's current time
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", fixedClock(clockTime))
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}

	state, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 100})
	if err != nil {
		t.Fatalf("ApplySetpoint: %v", err)
	}
	if !state.Time.Equal(laterModelTime) {
		t.Errorf("Time = %v, want %v (StepTo's returned model time; clockTime is before it, so StepTo is a no-op and the two are distinct)", state.Time, laterModelTime)
	}
}

// TestFleetDevice_ReadState_GridComesFromSimulator is the mutation-killing
// case for the constant-Grid bug: the fake reports voltage and frequency
// values that are deliberately NOT 1.0 pu / 60 Hz, so a hardcoded constant
// would fail this test where it would pass against nominal values.
func TestFleetDevice_ReadState_GridComesFromSimulator(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.values[[2]string{"inv0", "rated_power"}] = 5000
	transport.values[[2]string{"meter0", "nominal_voltage"}] = 120
	transport.getOverride = map[[2]string]FleetValue{
		{"meter0", "measured_voltage_A"}: {Re: 126, Im: 0}, // 1.05 pu, not nominal
		{"meter0", "measured_frequency"}: {Re: 59.5},       // not the 60 Hz nominal constant
	}
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	state, err := dev.ReadState(context.Background())
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Grid.VoltsPU != 1.05 {
		t.Errorf("VoltsPU = %v, want 1.05 (from the simulator, not a constant 1.0)", state.Grid.VoltsPU)
	}
	if state.Grid.FreqHz != 59.5 {
		t.Errorf("FreqHz = %v, want 59.5 (from the simulator, not the nominal constant)", state.Grid.FreqHz)
	}
	if state.MaxPowerW != 5000 {
		t.Errorf("MaxPowerW = %v, want 5000 (rated_power)", state.MaxPowerW)
	}
}

// TestFleetDevice_ReadState_AcceptsComplexVoltageViaMagnitude proves
// Magnitude is used for voltage (a phasor), not Float64: a complex reading
// with a nonzero angle is accepted and converted by magnitude, not refused
// the way a genuinely scalar property with a nonzero imaginary part would
// be. Renamed from ...RejectsComplexVoltageMismodel, whose own body always
// asserted success: the old name described the mismodel this test guards
// against, not the outcome it checks.
func TestFleetDevice_ReadState_AcceptsComplexVoltageViaMagnitude(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.values[[2]string{"inv0", "rated_power"}] = 1000
	transport.values[[2]string{"meter0", "nominal_voltage"}] = 120
	transport.getOverride = map[[2]string]FleetValue{
		{"meter0", "measured_voltage_A"}: {Re: 90, Im: 90}, // magnitude ~127.28, a nonzero angle
		{"meter0", "measured_frequency"}: {Re: 60},
	}
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	state, err := dev.ReadState(context.Background())
	if err != nil {
		t.Fatalf("ReadState on a complex voltage reading: want success via Magnitude, got error: %v", err)
	}
	wantVoltsPU := 127.27922061357855 / 120
	if diff := state.Grid.VoltsPU - wantVoltsPU; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("VoltsPU = %v, want %v (magnitude of 90+90i over nominal 120)", state.Grid.VoltsPU, wantVoltsPU)
	}
}

// TestFleetDevice_ReadState_RefusesZeroNominalVoltage proves ReadState
// refuses rather than dividing by zero (producing +Inf/NaN) when the
// meter's nominal_voltage reads 0: a misconfigured or not-yet-initialized
// meter object must surface as an error, never a fabricated or
// infinite VoltsPU.
func TestFleetDevice_ReadState_RefusesZeroNominalVoltage(t *testing.T) {
	transport := newFakeFleetTransport()
	// nominal_voltage is left at the map's zero value (0) deliberately.
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	if _, err := dev.ReadState(context.Background()); err == nil {
		t.Fatal("ReadState with nominal_voltage 0: want error, got nil")
	}
}

func TestFleetDevice_ReadState_ShortReplyErrors(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.shortGetBy = 1
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	if _, err := dev.ReadState(context.Background()); err == nil {
		t.Fatal("ReadState with a short Get reply: want error, got nil")
	}
}

func TestFleetDevice_ApplySetpoint_ShortReplyErrors(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.shortGetBy = 1
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	if _, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 1}); err == nil {
		t.Fatal("ApplySetpoint with a short Get reply: want error, got nil")
	}
}

// TestFleetDevice_Restart_ReappliesLastSetpoint is the restart case: after
// a simulated restart (the model resets and the generation advances), the
// next call must re-apply the last commanded setpoint before doing
// anything else, not silently read the reset model as if it still held.
func TestFleetDevice_Restart_ReappliesLastSetpoint(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.values[[2]string{"meter0", "nominal_voltage"}] = 120
	now := time.Date(2020, 1, 1, 0, 5, 0, 0, time.UTC)
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", fixedClock(now))
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}

	if _, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 2500, ReactivePowerVAr: -100}); err != nil {
		t.Fatalf("ApplySetpoint: %v", err)
	}

	transport.resetModel()
	// nominal_voltage is a GLM template default, not runtime state a
	// setpoint touches, so a real reload restores it; resetModel's
	// whole-map clear is coarser than that, so the fixture restores it.
	transport.values[[2]string{"meter0", "nominal_voltage"}] = 120
	if got := transport.values[[2]string{"inv0", "P_Out"}]; got != 0 {
		t.Fatalf("test setup: P_Out after resetModel = %v, want 0", got)
	}

	if _, err := dev.ReadState(context.Background()); err != nil {
		t.Fatalf("ReadState after restart: %v", err)
	}
	if got := transport.values[[2]string{"inv0", "P_Out"}]; got != 2500 {
		t.Errorf("P_Out after restart resync = %v, want 2500 (re-applied)", got)
	}
	if got := transport.values[[2]string{"inv0", "Q_Out"}]; got != -100 {
		t.Errorf("Q_Out after restart resync = %v, want -100 (re-applied)", got)
	}

	// A second call on the same generation must not resync again.
	setCallsAfterFirstResync := len(transport.setCalls)
	if _, err := dev.ReadState(context.Background()); err != nil {
		t.Fatalf("ReadState (second, same generation): %v", err)
	}
	if len(transport.setCalls) != setCallsAfterFirstResync {
		t.Errorf("ReadState on an unchanged generation issued %d more Set call(s), want 0",
			len(transport.setCalls)-setCallsAfterFirstResync)
	}
}

// TestFleetDevice_ConcurrentResyncDoesNotClobberNewerSetpoint is the
// mutation-killing case for a resync and a newer ApplySetpoint racing on
// the same device: a resync re-applies the LAST setpoint recorded before it
// started, so if that resync's own Set call is allowed to land on the wire
// after a concurrent, newer ApplySetpoint's Set call, the older value wins
// and the newer commanded setpoint is silently lost. d.mu must serialize
// the two across their full Set calls, not just around the struct field
// reads, or this reproduces.
//
// The fake's pauseSetOnP forces the interleaving deterministically: a
// ReadState goroutine is paused mid-resync, inside its Set(200) call,
// holding FleetDevice's lock (proved below by observing ApplySetpoint(300)
// has not yet reached the wire while paused). Releasing the pause lets the
// resync finish; ApplySetpoint(300) must then run strictly after it, so the
// final model value is 300, never the resynced 200.
func TestFleetDevice_ConcurrentResyncDoesNotClobberNewerSetpoint(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.values[[2]string{"meter0", "nominal_voltage"}] = 120
	now := time.Date(2020, 1, 1, 0, 5, 0, 0, time.UTC)
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", fixedClock(now))
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}

	// Establish the "old" setpoint that the restart resync will re-apply.
	if _, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 200}); err != nil {
		t.Fatalf("ApplySetpoint(200): %v", err)
	}
	transport.resetModel()
	transport.values[[2]string{"meter0", "nominal_voltage"}] = 120

	// setCalls is cumulative for the whole fake, and the setup call above
	// already added one; baseline here so the "while paused" assertion
	// below counts only what happens after this point.
	baseline := transport.setCallCount()

	transport.mu.Lock()
	transport.pauseSetOnP = 200
	transport.pauseReached = make(chan struct{})
	transport.pauseResume = make(chan struct{})
	transport.mu.Unlock()

	resyncDone := make(chan error, 1)
	go func() {
		_, err := dev.ReadState(context.Background())
		resyncDone <- err
	}()

	select {
	case <-transport.pauseReached:
	case <-time.After(2 * time.Second):
		t.Fatal("resync's Set(200) never reached the pause point")
	}

	// ApplySetpoint(300) must be blocked on d.mu, since ReadState's resync
	// holds it across the paused Set call: give it a moment to prove it has
	// NOT completed (and so cannot yet have written 300 to the wire).
	applyDone := make(chan error, 1)
	go func() {
		_, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 300})
		applyDone <- err
	}()
	select {
	case <-applyDone:
		t.Fatal("ApplySetpoint(300) completed while the resync was still paused: d.mu did not serialize them")
	case <-time.After(100 * time.Millisecond):
	}
	if got := transport.setCallCount() - baseline; got != 1 {
		t.Fatalf("Set calls since baseline while paused = %d, want 1 (only the resync's own call so far)", got)
	}

	close(transport.pauseResume)

	if err := <-resyncDone; err != nil {
		t.Fatalf("ReadState (resync): %v", err)
	}
	if err := <-applyDone; err != nil {
		t.Fatalf("ApplySetpoint(300): %v", err)
	}

	if got := transport.valueOf("inv0", "P_Out"); got != 300 {
		t.Errorf("final P_Out = %v, want 300 (the newer setpoint, not the resynced 200)", got)
	}
}

// TestFleetDevice_ReadState_NoResyncWithoutPriorSetpoint proves a device
// that has never had a setpoint commanded does not invent one to re-apply
// on its first read, restart or not.
func TestFleetDevice_ReadState_NoResyncWithoutPriorSetpoint(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.values[[2]string{"meter0", "nominal_voltage"}] = 120
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	if _, err := dev.ReadState(context.Background()); err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if len(transport.setCalls) != 0 {
		t.Errorf("ReadState with no prior ApplySetpoint issued %d Set call(s), want 0", len(transport.setCalls))
	}
}

// TestFleetDevice_Down_ReturnsErrorNoReading proves the DOWN-fleet
// invariant: an unhealthy transport makes ReadState and ApplySetpoint both
// return an error, and never a populated StateReading/InverterState.
func TestFleetDevice_Down_ReturnsErrorNoReading(t *testing.T) {
	transport := newFakeFleetTransport()
	transport.healthy = false
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
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
	dev, err := NewFleetDevice(transport, "LFDI1", "inv0", "meter0", nil)
	if err != nil {
		t.Fatalf("NewFleetDevice: %v", err)
	}
	if _, err := dev.ApplySetpoint(context.Background(), inverter.ControlOutputs{ActivePowerW: 1}); err == nil {
		t.Fatal("ApplySetpoint with a failing Set: want error, got nil")
	}
}

var _ FleetTransport = (*fakeFleetTransport)(nil)
