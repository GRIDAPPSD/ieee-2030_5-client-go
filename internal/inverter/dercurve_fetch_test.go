// Tests for DERCurve retrieval and curve-typed control
// application (Phase 5 closer).
//
// Three layers:
//
//  1. Pure tests of MapCurveData and DERCurveCache. No HTTP, no
//     goroutines, fast.
//  2. fetchProgramCurves (unexported) driven via the curveClient interface
//     fixture so we exercise the populate path without spinning a TLS
//     listener. Covers happy + 404 + transient stub errors.
//  3. ApplyControlsWithCurves: cached-curve consumption + empty-cache
//     fallback + multi-mode dispatch. Pinned against the IEEE 1547
//     default-curve baseline.
package inverter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// =============================================================================
// MapCurveData : pure converter.
// =============================================================================

// TestMapCurveData_HappyPath exercises a 10-point CurveData slice (the typical
// IEEE 2030.5 server payload size) and asserts the float64 round-trip is
// exact for representable int32 values.
func TestMapCurveData_HappyPath(t *testing.T) {
	t.Parallel()

	in := make([]sep2.CurveData, 10)
	for i := range in {
		in[i] = sep2.CurveData{XValue: int32(i * 10), YValue: int32(100 - i*5)}
	}

	out := MapCurveData(in)
	if len(out) != 10 {
		t.Fatalf("MapCurveData len = %d, want 10", len(out))
	}
	for i, p := range out {
		wantX := float64(i * 10)
		wantY := float64(100 - i*5)
		if p.X != wantX || p.Y != wantY {
			t.Errorf("MapCurveData[%d] = (%v, %v), want (%v, %v)", i, p.X, p.Y, wantX, wantY)
		}
	}
}

// TestMapCurveData_EmptyInputReturnsNonNilEmpty asserts the caller can
// range over the result without a nil-check.
func TestMapCurveData_EmptyInputReturnsNonNilEmpty(t *testing.T) {
	t.Parallel()

	out := MapCurveData(nil)
	if out == nil {
		t.Fatal("MapCurveData(nil) = nil, want non-nil empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("MapCurveData(nil) len = %d, want 0", len(out))
	}

	out = MapCurveData([]sep2.CurveData{})
	if out == nil {
		t.Fatal("MapCurveData([]) = nil, want non-nil empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("MapCurveData([]) len = %d, want 0", len(out))
	}
}

// =============================================================================
// DERCurveCache : Set / Lookup / Len round-trip.
// =============================================================================

// TestDERCurveCache_RoundTrip pins Set/Lookup/Len. Verifies the cache
// returns an independent slice (mutation by caller doesn't poison the
// store) and Lookup ok=false on a missing key.
func TestDERCurveCache_RoundTrip(t *testing.T) {
	t.Parallel()

	cache := NewDERCurveCache()
	if got := cache.Len(); got != 0 {
		t.Errorf("Len on fresh cache = %d, want 0", got)
	}

	// Miss.
	if _, ok := cache.Lookup(sep2.CurveTypeOpModVoltVar); ok {
		t.Error("Lookup on empty cache returned ok=true, want false")
	}

	// Hit.
	vvPts := []CurvePoint{
		{X: 0.92, Y: 0.6},
		{X: 1.0, Y: 0.0},
		{X: 1.08, Y: -0.6},
	}
	cache.Set(sep2.CurveTypeOpModVoltVar, vvPts)
	if got := cache.Len(); got != 1 {
		t.Errorf("Len after one Set = %d, want 1", got)
	}

	got, ok := cache.Lookup(sep2.CurveTypeOpModVoltVar)
	if !ok {
		t.Fatal("Lookup after Set returned ok=false")
	}
	if len(got) != len(vvPts) {
		t.Fatalf("Lookup len = %d, want %d", len(got), len(vvPts))
	}
	for i, p := range got {
		if p != vvPts[i] {
			t.Errorf("Lookup[%d] = %v, want %v", i, p, vvPts[i])
		}
	}

	// Independence: mutating the returned slice MUST NOT poison the cache.
	got[0].Y = -999
	regot, _ := cache.Lookup(sep2.CurveTypeOpModVoltVar)
	if regot[0].Y == -999 {
		t.Error("Lookup returned a slice that aliases the cache (mutation leaked)")
	}

	// Independence: mutating the originally-Set slice MUST NOT poison
	// the cache either (Set takes a defensive copy).
	vvPts[0].Y = -888
	regot, _ = cache.Lookup(sep2.CurveTypeOpModVoltVar)
	if regot[0].Y == -888 {
		t.Error("Set did not take a defensive copy (post-Set mutation leaked)")
	}

	// Second curve type.
	vwPts := []CurvePoint{{X: 1.06, Y: 1.0}, {X: 1.10, Y: 0.2}}
	cache.Set(sep2.CurveTypeOpModVoltWatt, vwPts)
	if got := cache.Len(); got != 2 {
		t.Errorf("Len after two Sets = %d, want 2", got)
	}

	// Replace under same key.
	cache.Set(sep2.CurveTypeOpModVoltVar, []CurvePoint{{X: 1, Y: 1}})
	if got := cache.Len(); got != 2 {
		t.Errorf("Len after replace under existing key = %d, want 2", got)
	}
	regot, _ = cache.Lookup(sep2.CurveTypeOpModVoltVar)
	if len(regot) != 1 || regot[0] != (CurvePoint{X: 1, Y: 1}) {
		t.Errorf("Lookup after replace = %v, want [{1 1}]", regot)
	}
}

// =============================================================================
// fetchProgramCurves : drives the curveClient interface against in-memory
// stubs. No TLS, no goroutines.
// =============================================================================

// stubCurveClient implements curveClient and records the args of the most
// recent GetDERCurveList call.
type stubCurveClient struct {
	lastHref string
	list     sep2.DERCurveList
	err      error
	calls    int
}

func (s *stubCurveClient) GetDERCurveList(_ context.Context, href string) (sep2.DERCurveList, error) {
	s.calls++
	s.lastHref = href
	if s.err != nil {
		return sep2.DERCurveList{}, s.err
	}
	return s.list, nil
}

// TestFetchProgramCurves_HappyPath asserts a 2-curve list populates the
// cache under both CurveType keys.
func TestFetchProgramCurves_HappyPath(t *testing.T) {
	t.Parallel()

	stub := &stubCurveClient{
		list: sep2.DERCurveList{
			DERCurve: []sep2.DERCurve{
				{
					CurveType: sep2.CurveTypeOpModVoltVar,
					CurveData: []sep2.CurveData{
						{XValue: 92, YValue: 60},
						{XValue: 98, YValue: 0},
						{XValue: 102, YValue: 0},
						{XValue: 108, YValue: -60},
					},
				},
				{
					CurveType: sep2.CurveTypeOpModVoltWatt,
					CurveData: []sep2.CurveData{
						{XValue: 106, YValue: 100},
						{XValue: 110, YValue: 20},
					},
				},
			},
		},
	}
	cache := NewDERCurveCache()

	if err := fetchProgramCurves(context.Background(), stub, "/derp/0/dc", cache); err != nil {
		t.Fatalf("fetchProgramCurves returned %v, want nil", err)
	}
	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1", stub.calls)
	}
	if stub.lastHref != "/derp/0/dc" {
		t.Errorf("stub.lastHref = %q, want %q", stub.lastHref, "/derp/0/dc")
	}
	if got := cache.Len(); got != 2 {
		t.Errorf("cache.Len = %d, want 2", got)
	}

	vv, ok := cache.Lookup(sep2.CurveTypeOpModVoltVar)
	if !ok {
		t.Fatal("VoltVar curve not in cache")
	}
	if len(vv) != 4 {
		t.Errorf("VoltVar len = %d, want 4", len(vv))
	}
	if vv[0].X != 92 || vv[0].Y != 60 {
		t.Errorf("VoltVar[0] = %v, want {92 60}", vv[0])
	}

	vw, ok := cache.Lookup(sep2.CurveTypeOpModVoltWatt)
	if !ok {
		t.Fatal("VoltWatt curve not in cache")
	}
	if len(vw) != 2 {
		t.Errorf("VoltWatt len = %d, want 2", len(vw))
	}
}

// errFakeFetch is a sentinel for the 404 path so the test asserts the
// wrap chain stays intact via errors.Is.
var errFakeFetch = errors.New("fake 404")

// TestFetchProgramCurves_FetchError asserts the cache is untouched when
// GetDERCurveList returns an error, and the wrap chain preserves the
// underlying error for errors.Is.
func TestFetchProgramCurves_FetchError(t *testing.T) {
	t.Parallel()

	stub := &stubCurveClient{err: fmt.Errorf("GET DERCurveList: %w", errFakeFetch)}
	cache := NewDERCurveCache()

	err := fetchProgramCurves(context.Background(), stub, "/derp/0/dc", cache)
	if err == nil {
		t.Fatal("fetchProgramCurves err = nil on stub error, want wrapped error")
	}
	if !errors.Is(err, errFakeFetch) {
		t.Errorf("errors.Is(err, errFakeFetch) = false; wrap chain broken: %v", err)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("cache.Len after failed fetch = %d, want 0 (cache must be untouched)", got)
	}
}

// TestFetchProgramCurves_GuardErrors covers the empty-href and nil-cache
// programmer-error paths.
func TestFetchProgramCurves_GuardErrors(t *testing.T) {
	t.Parallel()

	cache := NewDERCurveCache()
	stub := &stubCurveClient{}

	if err := fetchProgramCurves(context.Background(), stub, "", cache); err == nil {
		t.Error("fetchProgramCurves(empty href) = nil, want error")
	}
	if err := fetchProgramCurves(context.Background(), stub, "/derp/0/dc", nil); err == nil {
		t.Error("fetchProgramCurves(nil cache) = nil, want error")
	}
	if stub.calls != 0 {
		t.Errorf("stub.calls = %d, want 0 (guard rejected before HTTP)", stub.calls)
	}
}

// TestFetchProgramCurves_NilClient covers the exported entry-point guard.
func TestFetchProgramCurves_NilClient(t *testing.T) {
	t.Parallel()

	cache := NewDERCurveCache()
	if err := FetchProgramCurves(context.Background(), nil, "/derp/0/dc", cache); err == nil {
		t.Error("FetchProgramCurves(nil client) = nil, want error")
	}
}

// TestFetchProgramCurves_DuplicateTypeLastWins pins the IEEE 2030.5 §10.1.3
// list-ordering "later resource shadows earlier" semantic for curves
// sharing a CurveType.
func TestFetchProgramCurves_DuplicateTypeLastWins(t *testing.T) {
	t.Parallel()

	stub := &stubCurveClient{
		list: sep2.DERCurveList{
			DERCurve: []sep2.DERCurve{
				{
					CurveType: sep2.CurveTypeOpModVoltVar,
					CurveData: []sep2.CurveData{{XValue: 1, YValue: 1}},
				},
				{
					CurveType: sep2.CurveTypeOpModVoltVar,
					CurveData: []sep2.CurveData{{XValue: 9, YValue: 9}, {XValue: 10, YValue: 10}},
				},
			},
		},
	}
	cache := NewDERCurveCache()
	if err := fetchProgramCurves(context.Background(), stub, "/derp/0/dc", cache); err != nil {
		t.Fatalf("fetchProgramCurves err = %v", err)
	}

	vv, _ := cache.Lookup(sep2.CurveTypeOpModVoltVar)
	if len(vv) != 2 || vv[0].X != 9 {
		t.Errorf("late-wins: VoltVar = %v, want second curve (len 2 starting at X=9)", vv)
	}
}

// =============================================================================
// ApplyControlsWithCurves : cached-curve consumption + fallback + multi-mode.
// =============================================================================

// vvHelper returns a DERControlBase with OpModVoltVar set to a non-nil value
// (the integer value itself is irrelevant : the controller only checks
// the pointer for nil-ness when deciding to route through the V/V curve).
func vvHelper() *sep2.DERControlBase {
	v := int32(1)
	return &sep2.DERControlBase{OpModVoltVar: &v}
}

// TestApplyControlsWithCurves_CachedVoltVarCurve injects a custom V/V curve
// that produces Q(V=1.0) = +0.5 (i.e., 50% var injection at nominal
// voltage). The IEEE 1547 default sits in its 0.98-1.02 deadband at
// V=1.0, so Q would be 0 : the cached-curve path is visibly different.
func TestApplyControlsWithCurves_CachedVoltVarCurve(t *testing.T) {
	t.Parallel()

	cache := NewDERCurveCache()
	// Custom curve: flat Y=0.5 across the operating band. At V=1.0,
	// EvaluateCurve returns 0.5 (clamped between X=0.92 and X=1.10).
	cache.Set(sep2.CurveTypeOpModVoltVar, []CurvePoint{
		{X: 0.92, Y: 0.5},
		{X: 1.08, Y: 0.5},
	})

	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	const maxP = 8000.0

	out := ApplyControlsWithCurves(vvHelper(), grid, maxP, cache)
	if out.Mode != ModeVoltVar {
		t.Errorf("Mode = %s, want VoltVar", out.Mode)
	}
	wantQ := 0.5 * Rating.RatedVAr
	if math.Abs(out.ReactivePowerVAr-wantQ) > 1e-6 {
		t.Errorf("ReactivePowerVAr = %.3f, want %.3f (cached V/V curve at V=1.0)", out.ReactivePowerVAr, wantQ)
	}

	// Confirm the IEEE 1547 default differs at V=1.0 : pinning that the
	// cached path actually overrode the default.
	defaultOut := ApplyControlsWithCurves(vvHelper(), grid, maxP, nil)
	if defaultOut.ReactivePowerVAr == out.ReactivePowerVAr {
		t.Error("cached curve produced same Q as default at V=1.0 : cached path not exercised")
	}
	if defaultOut.ReactivePowerVAr != 0 {
		t.Errorf("IEEE 1547 default Q at V=1.0 = %.3f, want 0 (deadband)", defaultOut.ReactivePowerVAr)
	}
}

// TestApplyControlsWithCurves_EmptyCacheFallsBackToDefault asserts that
// a non-nil but empty cache produces the same outputs as the legacy
// ApplyControls call.
func TestApplyControlsWithCurves_EmptyCacheFallsBackToDefault(t *testing.T) {
	t.Parallel()

	emptyCache := NewDERCurveCache()
	grid := GridState{VoltsPU: 1.05, FreqHz: 60.0}
	const maxP = 8000.0

	withEmpty := ApplyControlsWithCurves(vvHelper(), grid, maxP, emptyCache)
	withNil := ApplyControlsWithCurves(vvHelper(), grid, maxP, nil)
	legacy := ApplyControls(vvHelper(), grid, maxP)

	if withEmpty != withNil {
		t.Errorf("empty-cache result != nil-cache result; empty=%+v nil=%+v", withEmpty, withNil)
	}
	if withEmpty != legacy {
		t.Errorf("empty-cache result != legacy ApplyControls; empty=%+v legacy=%+v", withEmpty, legacy)
	}
}

// TestApplyControlsWithCurves_MultiModeDispatch covers a base that
// activates both Volt/Var and Volt/Watt paths simultaneously, with both
// curves cached. Asserts each mode consumes its OWN curve (no cross-talk
// between CurveType keys).
//
// The controller's pre-existing quirk is that OpModVoltVar drives BOTH the
// V/V reactive-power branch AND the V/W active-power-limit branch (the V/W
// branch is gated on OpModVoltVar in controller.go : a known quirk,
// untouched here). The multi-mode test exploits this: a single
// non-nil OpModVoltVar lights both code paths so we can pin both curves.
func TestApplyControlsWithCurves_MultiModeDispatch(t *testing.T) {
	t.Parallel()

	cache := NewDERCurveCache()
	// V/V curve: Y=0.25 across the band → expected Q = 0.25 * RatedVAr.
	cache.Set(sep2.CurveTypeOpModVoltVar, []CurvePoint{
		{X: 0.92, Y: 0.25},
		{X: 1.08, Y: 0.25},
	})
	// V/W curve: Y=0.40 across the band → expected V/W limit = 0.40 * RatedW.
	cache.Set(sep2.CurveTypeOpModVoltWatt, []CurvePoint{
		{X: 0.92, Y: 0.40},
		{X: 1.08, Y: 0.40},
	})

	grid := GridState{VoltsPU: 1.0, FreqHz: 60.0}
	const maxP = 8000.0

	out := ApplyControlsWithCurves(vvHelper(), grid, maxP, cache)

	wantQ := 0.25 * Rating.RatedVAr
	if math.Abs(out.ReactivePowerVAr-wantQ) > 1e-6 {
		t.Errorf("V/V dispatch: ReactivePowerVAr = %.3f, want %.3f", out.ReactivePowerVAr, wantQ)
	}

	wantP := 0.40 * Rating.RatedW
	if math.Abs(out.ActivePowerW-wantP) > 1e-6 {
		t.Errorf("V/W dispatch: ActivePowerW = %.3f, want %.3f", out.ActivePowerW, wantP)
	}

	// Cross-talk guard: if the V/V curve had bled into the V/W slot we'd
	// see 0.25*RatedW, not 0.40*RatedW. The two distinct Y values
	// (0.25 vs 0.40) make the cross-talk failure mode loud.
}

// TestApplyControlsWithCurves_NilBaseSafe pins the no-controls fallback :
// a nil base must still produce the no-op output regardless of cache
// contents.
func TestApplyControlsWithCurves_NilBaseSafe(t *testing.T) {
	t.Parallel()

	cache := NewDERCurveCache()
	cache.Set(sep2.CurveTypeOpModVoltVar, []CurvePoint{{X: 1, Y: 1}})

	out := ApplyControlsWithCurves(nil, GridState{VoltsPU: 1.0, FreqHz: 60.0}, 5000, cache)
	if out.ActivePowerW != 5000 || out.ReactivePowerVAr != 0 || !out.Connected {
		t.Errorf("nil-base output = %+v, want {5000W, 0VAr, connected}", out)
	}
}
