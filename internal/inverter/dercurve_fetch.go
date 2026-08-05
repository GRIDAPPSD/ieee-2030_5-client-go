package inverter

import (
	"context"
	"fmt"
	"sync"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// DERCurve retrieval (Phase 5 closer) ===============================
//
// CSIP V1.2 CORE-012 step 6 ("apply the active event") requires the DER Client
// to honor curve-based controls when the active DERControl runs a curve-typed
// mode (Volt/Var, Volt/Watt, Freq/Watt). The curves themselves live on the
// program (DERProgram.DERCurveListLink) and are discriminated by
// DERCurve.CurveType : there is no per-mode link on DERControlBase in this
// codebase (verified against pkg/sep2/der.go).
//
// This file ships:
//
//   - DERCurveCache: in-memory cache keyed by CurveType (uint8 enum from
//     pkg/sep2, e.g. sep2.CurveTypeOpModVoltVar = 0). Mirrors the
//     DERControlCache concurrency shape (RWMutex; reads cheap, writes rare).
//   - MapCurveData: pure converter from []sep2.CurveData (int32 XY tuples)
//     to []CurvePoint (float64). EvaluateCurve consumes the latter.
//   - FetchProgramCurves: one-shot fetch + cache-population helper. Wraps
//     SEP2Client.GetDERCurveList and routes each curve
//     into the cache by CurveType. Errors are wrapped with %w so the caller
//     can errors.Is against context.Canceled / DeadlineExceeded.
//
// Out of scope for this file:
//   - DefaultDERControl curves : only active-event curves are covered here;
//     a follow-up can reuse the same cache + helper if it bites.
//   - Adding new inverter modes (dedicated Volt/Watt, Watt/PF). controller.go
//     already routes V/V via the OpModVoltVar guard; new mode files are
//     out of scope.
//   - Periodic curve refresh : curves are fetched at event-START transition
//     time only (CSIP V1.2 CORE-012 step 6: curves are part of "apply,"
//     not "discover"). If the spec adds periodic refresh later, the
//     scheduler hook lives in cmd/inverterclient/main.go.

// curveClient is the minimal interface FetchProgramCurves needs from the
// SEP2 client. Defined at the consumer (Pike review checklist #6) so the
// helper is unit-testable without spinning a TLS listener : the httptest
// fixture in dercurve_fetch_test.go passes a tiny stub.
//
// The exported FetchProgramCurves takes *SEP2Client to keep the call-site
// shape consistent with the rest of internal/inverter; the unexported
// fetchProgramCurves does the work against the interface.
type curveClient interface {
	GetDERCurveList(ctx context.Context, derCurveListHref string) (sep2.DERCurveList, error)
}

// DERCurveCache stores the most recent set of DERCurves keyed by CurveType.
// The zero value is NOT ready for use : callers MUST construct via
// NewDERCurveCache so the underlying map is non-nil.
//
// Concurrency: an RWMutex guards the map. Reads (ApplyControlsWithCurves) are
// frequent (every sim tick); writes (FetchProgramCurves) happen only on
// EVENT_RECEIVED → EVENT_STARTED transitions, so RW is the right shape.
type DERCurveCache struct {
	mu     sync.RWMutex
	curves map[uint8][]CurvePoint
}

// NewDERCurveCache constructs an empty cache ready for concurrent use.
func NewDERCurveCache() *DERCurveCache {
	return &DERCurveCache{curves: make(map[uint8][]CurvePoint)}
}

// Set stores `points` under `curveType`, replacing any prior entry. A
// defensive copy of the slice header is taken so the caller can mutate the
// supplied slice without affecting the cache. Pike rule (immutability): the
// stored slice MUST NOT be mutated by anyone : Lookup hands callers their
// own copy.
func (c *DERCurveCache) Set(curveType uint8, points []CurvePoint) {
	stored := make([]CurvePoint, len(points))
	copy(stored, points)
	c.mu.Lock()
	c.curves[curveType] = stored
	c.mu.Unlock()
}

// Lookup returns an independent copy of the curve points stored under
// `curveType` and ok=true if present. Mutating the returned slice does not
// affect the cache. ok=false means no curve of that type has been fetched :
// the caller should fall back to its compiled-in default curve.
func (c *DERCurveCache) Lookup(curveType uint8) (points []CurvePoint, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	stored, hit := c.curves[curveType]
	if !hit {
		return nil, false
	}
	out := make([]CurvePoint, len(stored))
	copy(out, stored)
	return out, true
}

// Len returns the number of cached curve types. Exposed for tests and
// operator logging.
func (c *DERCurveCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.curves)
}

// MapCurveData converts a []sep2.CurveData (int32 XY tuples on the wire) to
// []CurvePoint (float64 internal representation consumed by EvaluateCurve).
// Pure function : no I/O, no mutation of input. Returns a non-nil empty
// slice for an empty input so callers can range without nil-checks.
//
// IEEE 2030.5 §10.10 specifies xvalue/yvalue as Int32 unscaled. CurveData
// carries no scale factor of its own : the curveType plus the program's
// implicit per-unit convention determines the scaling. This converter
// preserves the unscaled values; the consumer (ApplyControlsWithCurves)
// applies the same fraction-of-rated semantics the IEEE 1547 default
// curves already encode.
func MapCurveData(points []sep2.CurveData) []CurvePoint {
	out := make([]CurvePoint, 0, len(points))
	for _, p := range points {
		out = append(out, CurvePoint{X: float64(p.XValue), Y: float64(p.YValue)})
	}
	return out
}

// FetchProgramCurves GETs the DERCurveList at `listHref` and populates
// `cache` keyed by each curve's CurveType. Wraps GetDERCurveList; errors are
// wrapped with %w so the caller can errors.Is against context.Canceled /
// context.DeadlineExceeded.
//
// When the same CurveType appears multiple times in the list the last
// occurrence wins : matches the IEEE 2030.5 §10.1.3 list-ordering "later
// resource shadows earlier" semantic the rest of the inverter follows.
//
// Empty href is a programmer error (the caller should have gated on
// DERCurveListLink != nil before invocation). Returns a non-wrapped error
// in that case : there is no upstream errors.Is target to preserve.
//
// FetchProgramCurves spawns NO goroutines. It runs synchronously on the
// caller's context. The caller (cmd/inverterclient/main.go state-machine
// hook) is responsible for spawning a ctx-bound goroutine if it doesn't
// want to block the state-machine transition path.
func FetchProgramCurves(
	ctx context.Context,
	client *SEP2Client,
	listHref string,
	cache *DERCurveCache,
) error {
	if client == nil {
		return fmt.Errorf("FetchProgramCurves: client required")
	}
	return fetchProgramCurves(ctx, client, listHref, cache)
}

// fetchProgramCurves does the actual work against the curveClient interface.
// Extracted so dercurve_fetch_test.go can drive the population path
// without spinning a TLS listener for every test case.
func fetchProgramCurves(
	ctx context.Context,
	client curveClient,
	listHref string,
	cache *DERCurveCache,
) error {
	if listHref == "" {
		return fmt.Errorf("FetchProgramCurves: listHref required")
	}
	if cache == nil {
		return fmt.Errorf("FetchProgramCurves: cache required")
	}
	list, err := client.GetDERCurveList(ctx, listHref)
	if err != nil {
		return fmt.Errorf("FetchProgramCurves %s: %w", listHref, err)
	}
	for _, curve := range list.DERCurve {
		points := MapCurveData(curve.CurveData)
		cache.Set(curve.CurveType, points)
	}
	return nil
}
