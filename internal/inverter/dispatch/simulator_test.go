// IEEESIM-004 SimulatorDispatcher tests.
//
// The SimulatorDispatcher is the relocated PhaseStateDispatcher. These tests
// mirror the contract tests from the original package inverter location and
// exercise the simulator path within the dispatch package so the coverage
// gate is met independently of production_test.go.

package dispatch

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// TestSimulatorDispatcher_RoutesDERControlList asserts field-value correctness:
// the fetcher receives the exact registered href; the cache contains exactly
// the returned controls.
func TestSimulatorDispatcher_RoutesDERControlList(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	ctrlA := mkDERControl("ctrl-sim-a")
	ctrlB := mkDERControl("ctrl-sim-b")

	fake := &fakeFetcher{
		returnVal: sep2.DERControlList{
			DERControl: []sep2.DERControl{ctrlA, ctrlB},
		},
	}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.calls(); got != 1 {
		t.Fatalf("expected 1 GetDERControlList call; got %d", got)
	}
	fake.mu.Lock()
	gotHref := fake.lastHref
	fake.mu.Unlock()
	if gotHref != listHref {
		t.Errorf("GetDERControlList called with href=%q; want %q", gotHref, listHref)
	}
	if got := cache.Len(); got != 2 {
		t.Fatalf("cache len=%d; want 2", got)
	}
	snap := cache.Snapshot()
	if _, ok := snap[ctrlA.MRID]; !ok {
		t.Errorf("cache missing mRID %q", ctrlA.MRID)
	}
	if _, ok := snap[ctrlB.MRID]; !ok {
		t.Errorf("cache missing mRID %q", ctrlB.MRID)
	}
}

// TestSimulatorDispatcher_Status1_CancelHook asserts the cancel hook fires
// with the exact subscribedResource and no GET is triggered.
func TestSimulatorDispatcher_Status1_CancelHook(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	const subscribedRes = "/edev/1/fsa"

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	var mu sync.Mutex
	var gotHrefs []string
	d.RegisterCancelHook(func(h string) {
		mu.Lock()
		gotHrefs = append(gotHrefs, h)
		mu.Unlock()
	})

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: subscribedRes},
		SubscribedResource: subscribedRes,
		Status:             sep2.NotificationStatusSubscripted,
	})

	mu.Lock()
	defer mu.Unlock()
	if len(gotHrefs) != 1 {
		t.Fatalf("CancelHook fired %d times; want 1", len(gotHrefs))
	}
	if gotHrefs[0] != subscribedRes {
		t.Errorf("CancelHook href=%q; want %q", gotHrefs[0], subscribedRes)
	}
	if got := fake.calls(); got != 0 {
		t.Errorf("status=1 must not trigger GET; got %d calls", got)
	}
}

// TestSimulatorDispatcher_Status1_NoHook asserts clean log+drop when no hook
// is registered.
func TestSimulatorDispatcher_Status1_NoHook(t *testing.T) {
	t.Parallel()

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, "/edev/1/derp/1/derc"); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		SubscribedResource: "/edev/1/fsa",
		Status:             sep2.NotificationStatusSubscripted,
	})

	if got := fake.calls(); got != 0 {
		t.Errorf("status=1 without hook must not GET; got %d calls", got)
	}
}

// TestSimulatorDispatcher_UnknownHref_ZeroFetches asserts unknown hrefs log
// and drop without triggering any GET.
func TestSimulatorDispatcher_UnknownHref_ZeroFetches(t *testing.T) {
	t.Parallel()

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, "/edev/1/derp/1/derc"); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: "/edev/1/mup/42"},
		SubscribedResource: "/edev/1/mup",
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.calls(); got != 0 {
		t.Errorf("unknown href must not trigger GET; got %d calls", got)
	}
}

// TestSimulatorDispatcher_NotYetRegistered_Drops asserts clean log+drop
// before RegisterDERControlList is called. The fetcher is never called and
// the cache remains empty (data-invariants Rule 1: no silent mutation).
func TestSimulatorDispatcher_NotYetRegistered_Drops(t *testing.T) {
	t.Parallel()

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()

	// Construct dispatcher WITHOUT calling Register: client/cache/href stay nil/empty.
	d := NewSimulatorDispatcher()
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: "/edev/1/derp/1/derc"},
		SubscribedResource: "/edev/1/derp/1/derc",
		Status:             sep2.NotificationStatusChanged,
	})

	// The unregistered dispatcher must not call the external fetcher.
	if got := fake.calls(); got != 0 {
		t.Errorf("unregistered dispatcher: fetcher called %d times; want 0", got)
	}
	// The cache we created independently must remain empty: the dispatcher
	// dropped before it had any cache to write to.
	if got := cache.Len(); got != 0 {
		t.Errorf("sentinel cache Len = %d after unregistered dispatch; want 0", got)
	}
}

// TestSimulatorDispatcher_EmptyHrefs_Drops asserts a notification with all
// href fields empty drops cleanly.
func TestSimulatorDispatcher_EmptyHrefs_Drops(t *testing.T) {
	t.Parallel()

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, "/edev/1/derp/1/derc"); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Status: sep2.NotificationStatusChanged, // all href fields empty
	})

	if got := fake.calls(); got != 0 {
		t.Errorf("empty-href notification must not GET; got %d calls", got)
	}
}

// TestSimulatorDispatcher_GetError_LoggedNotFatal asserts a GET error does
// not crash and the cache remains untouched.
func TestSimulatorDispatcher_GetError_LoggedNotFatal(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{
		returnErr: fmt.Errorf("GET failed: simulated error"),
	}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.calls(); got != 1 {
		t.Fatalf("expected 1 call; got %d", got)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("cache must be untouched on GET error; got len %d", got)
	}
}

// TestSimulatorDispatcher_ContextCancellation asserts the dispatcher returns
// cleanly when the context is cancelled mid-flight.
func TestSimulatorDispatcher_ContextCancellation(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	block := make(chan struct{})
	fake := &fakeFetcherBlocking{
		block:     block,
		returnVal: sep2.DERControlList{DERControl: []sep2.DERControl{mkDERControl("ctrl-ctx")}},
	}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Dispatch(ctx, sep2.Notification{
			Resource:           sep2.Resource{Href: listHref},
			SubscribedResource: listHref,
			Status:             sep2.NotificationStatusChanged,
		})
		close(done)
	}()

	cancel()
	close(block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after ctx cancel within 2s")
	}
}

// TestSimulatorDispatcher_ContextCancelWrappedError exercises the
// errors.Is(err, context.Canceled) branch inside refreshDERControlList when
// the fetcher returns a WRAPPED context.Canceled error. This path must be
// handled gracefully (log+return) without touching the cache.
func TestSimulatorDispatcher_ContextCancelWrappedError(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{
		// Wrap context.Canceled so errors.Is unwrapping is exercised.
		returnErr: fmt.Errorf("fetch: %w", context.Canceled),
	}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	// The fetcher was called once; the errors.Is(Canceled) branch must not
	// mutate the cache.
	if got := fake.calls(); got != 1 {
		t.Errorf("expected 1 fetcher call; got %d", got)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("cache must be untouched on wrapped Canceled error; got len %d", got)
	}
}

// TestSimulatorDispatcher_RegisterCancelHook_NilClears asserts passing nil
// clears a previously registered hook.
func TestSimulatorDispatcher_RegisterCancelHook_NilClears(t *testing.T) {
	t.Parallel()

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, "/edev/1/derp/1/derc"); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	var fired atomic.Int32
	d.RegisterCancelHook(func(_ string) { fired.Add(1) })
	d.RegisterCancelHook(nil) // clear

	d.Dispatch(context.Background(), sep2.Notification{
		SubscribedResource: "/edev/1/fsa",
		Status:             sep2.NotificationStatusSubscripted,
	})

	if got := fired.Load(); got != 0 {
		t.Errorf("cleared hook must not fire; fired %d times", got)
	}
}

// TestSimulatorDispatcher_ConcurrentDispatch_Safe exercises the race detector.
func TestSimulatorDispatcher_ConcurrentDispatch_Safe(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{
		returnVal: sep2.DERControlList{
			DERControl: []sep2.DERControl{mkDERControl("ctrl-concurrent")},
		},
	}
	cache := inverter.NewDERControlCache()
	d := NewSimulatorDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			d.Dispatch(context.Background(), sep2.Notification{
				Resource:           sep2.Resource{Href: listHref},
				SubscribedResource: listHref,
				Status:             sep2.NotificationStatusChanged,
			})
		}()
	}
	wg.Wait()

	if got := fake.calls(); got != N {
		t.Errorf("expected %d calls; got %d", N, got)
	}
}

// fakeFetcherBlocking is a blocking variant for context-cancellation tests.
type fakeFetcherBlocking struct {
	block     chan struct{}
	returnVal sep2.DERControlList
}

func (f *fakeFetcherBlocking) GetDERControlList(ctx context.Context, _ string) (sep2.DERControlList, string, error) {
	select {
	case <-f.block:
	case <-ctx.Done():
		return sep2.DERControlList{}, "", ctx.Err()
	}
	return f.returnVal, "", nil
}

// TestChangedResourceHref_PreferenceOrder tests the href preference ordering.
func TestChangedResourceHref_PreferenceOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		n    sep2.Notification
		want string
	}{
		{"href wins", sep2.Notification{Resource: sep2.Resource{Href: "/a"}, NewResourceURI: "/b", SubscribedResource: "/c"}, "/a"},
		{"new wins over subscribed", sep2.Notification{NewResourceURI: "/b", SubscribedResource: "/c"}, "/b"},
		{"subscribed fallback", sep2.Notification{SubscribedResource: "/c"}, "/c"},
		{"all empty", sep2.Notification{}, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := changedResourceHref(tc.n); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

// TestResourceKindFor_PrefixTolerance tests prefix matching for child hrefs.
func TestResourceKindFor_PrefixTolerance(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	cases := []struct {
		name    string
		changed string
		list    string
		want    resourceKind
	}{
		{"exact match", listHref, listHref, resourceKindDERControlList},
		{"per-mRID child", listHref + "/abc", listHref, resourceKindDERControlList},
		{"trailing slash on list", listHref + "/abc", listHref + "/", resourceKindDERControlList},
		{"unrelated href", "/edev/1/mup", listHref, resourceKindUnknown},
		{"empty changed", "", listHref, resourceKindUnknown},
		{"empty list", listHref, "", resourceKindUnknown},
		{"prefix-but-not-child", listHref + "extra", listHref, resourceKindUnknown},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resourceKindFor(tc.changed, tc.list); got != tc.want {
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}
