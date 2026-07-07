// IEEESIM-004 ProductionDispatcher tests.
//
// Field-value assertions per data-invariants Rule 1: tests assert the data
// written to the cache and received by hooks, not just "no crash."

package dispatch

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// fakeFetcher stubs GetDERControlList. Counts calls and records the href
// argument so tests can assert the exact href the dispatcher passed.
type fakeFetcher struct {
	mu        sync.Mutex
	callCount int32
	lastHref  string
	returnVal sep2.DERControlList
	returnErr error
}

func (f *fakeFetcher) GetDERControlList(_ context.Context, href string) (sep2.DERControlList, string, error) {
	atomic.AddInt32(&f.callCount, 1)
	f.mu.Lock()
	f.lastHref = href
	f.mu.Unlock()
	if f.returnErr != nil {
		return sep2.DERControlList{}, "", f.returnErr
	}
	return f.returnVal, "", nil
}

func (f *fakeFetcher) calls() int { return int(atomic.LoadInt32(&f.callCount)) }

func mkDERControl(mrid string) sep2.DERControl {
	var c sep2.DERControl
	c.MRID = mrid
	return c
}

// TestProductionDispatcher_RoutesDERControlList asserts:
// - the fetcher received EXACTLY the registered href
// - the cache contains EXACTLY the controls returned by the fetcher
// This is the data-invariants Rule 1 discipline: field-value correctness, not
// just "no crash."
func TestProductionDispatcher_RoutesDERControlList(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	ctrl1 := mkDERControl("ctrl-alpha")
	ctrl2 := mkDERControl("ctrl-beta")

	fake := &fakeFetcher{
		returnVal: sep2.DERControlList{
			DERControl: []sep2.DERControl{ctrl1, ctrl2},
		},
	}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	// Assert fetcher was called with the exact registered href.
	if got := fake.calls(); got != 1 {
		t.Fatalf("expected 1 GetDERControlList call; got %d", got)
	}
	fake.mu.Lock()
	gotHref := fake.lastHref
	fake.mu.Unlock()
	if gotHref != listHref {
		t.Errorf("GetDERControlList called with href=%q; want %q", gotHref, listHref)
	}

	// Assert cache contains exactly the two controls with the exact MRIDs.
	if got := cache.Len(); got != 2 {
		t.Fatalf("cache len = %d; want 2", got)
	}
	snap := cache.Snapshot()
	if _, ok := snap[ctrl1.MRID]; !ok {
		t.Errorf("cache missing mRID %q", ctrl1.MRID)
	}
	if _, ok := snap[ctrl2.MRID]; !ok {
		t.Errorf("cache missing mRID %q", ctrl2.MRID)
	}
}

// TestProductionDispatcher_Status1_CancelHook asserts that a status=1
// notification invokes the registered CancelHook with the EXACT
// subscribedResource from the notification, and does NOT trigger a GET.
func TestProductionDispatcher_Status1_CancelHook(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	const subscribedRes = "/edev/1/fsa"

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
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
		Status:             sep2.NotificationStatusSubscripted, // status=1
	})

	// Assert hook fired exactly once with the exact subscribedResource.
	mu.Lock()
	defer mu.Unlock()
	if len(gotHrefs) != 1 {
		t.Fatalf("CancelHook fired %d times; want 1", len(gotHrefs))
	}
	if gotHrefs[0] != subscribedRes {
		t.Errorf("CancelHook href=%q; want %q", gotHrefs[0], subscribedRes)
	}
	// Assert no GET was triggered.
	if got := fake.calls(); got != 0 {
		t.Errorf("status=1 must not trigger GET; got %d calls", got)
	}
}

// TestProductionDispatcher_UnknownHref_ZeroFetches asserts that a notification
// for an unregistered href does not trigger any GET and the cache remains
// untouched.
func TestProductionDispatcher_UnknownHref_ZeroFetches(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
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
	if got := cache.Len(); got != 0 {
		t.Errorf("cache must remain empty; got len %d", got)
	}
}

// TestProductionDispatcher_RegisterDERControlList_Validation asserts the three
// invalid-input cases each return a non-nil error.
func TestProductionDispatcher_RegisterDERControlList_Validation(t *testing.T) {
	t.Parallel()

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()

	cases := []struct {
		name   string
		client DERControlListFetcher
		cache  *inverter.DERControlCache
		href   string
	}{
		{"nil client", nil, cache, "/x"},
		{"nil cache", fake, nil, "/x"},
		{"empty href", fake, cache, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewProductionDispatcher()
			if err := d.RegisterDERControlList(tc.client, tc.cache, tc.href); err == nil {
				t.Fatal("expected non-nil error; got nil")
			}
		})
	}
}

// TestProductionDispatcher_NotYetRegistered_Drops asserts the dispatcher
// logs and drops before RegisterDERControlList is called. The fetcher is
// never called and the cache remains empty (data-invariants Rule 1).
func TestProductionDispatcher_NotYetRegistered_Drops(t *testing.T) {
	t.Parallel()

	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()

	// Construct dispatcher WITHOUT calling Register.
	d := NewProductionDispatcher()
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: "/edev/1/derp/1/derc"},
		SubscribedResource: "/edev/1/derp/1/derc",
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.calls(); got != 0 {
		t.Errorf("unregistered dispatcher: fetcher called %d times; want 0", got)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("sentinel cache Len = %d after unregistered dispatch; want 0", got)
	}
}

// TestProductionDispatcher_GetError_LoggedNotFatal asserts a GET error does
// not crash and the cache remains untouched.
func TestProductionDispatcher_GetError_LoggedNotFatal(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{
		returnErr: fmt.Errorf("GET failed: simulated production error"),
	}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
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

// TestProductionDispatcher_ChildHref_Routed asserts that a per-mRID child
// href (e.g. /edev/1/derp/1/derc/abc) is routed to the DERControlList fetcher.
func TestProductionDispatcher_ChildHref_Routed(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{
		returnVal: sep2.DERControlList{
			DERControl: []sep2.DERControl{mkDERControl("ctrl-child")},
		},
	}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		// Per-mRID child href: the notification carries a specific control
		// href under the list. The dispatcher must still route to the list.
		Resource:           sep2.Resource{Href: listHref + "/ctrl-child"},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.calls(); got != 1 {
		t.Errorf("child href must route to fetcher; got %d calls", got)
	}
	// The fetcher is called with the REGISTERED list href, not the child href.
	fake.mu.Lock()
	gotHref := fake.lastHref
	fake.mu.Unlock()
	if gotHref != listHref {
		t.Errorf("fetcher called with href=%q; want list href %q", gotHref, listHref)
	}
}

// TestProductionDispatcher_Status1_NoHook asserts that a status=1
// (subscription cancelled by server) notification with no registered cancel
// hook logs and drops without calling the fetcher or mutating the cache.
func TestProductionDispatcher_Status1_NoHook(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}
	// Intentionally do NOT register a cancel hook.

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusSubscripted,
	})

	// status=1 must NOT trigger a GET.
	if got := fake.calls(); got != 0 {
		t.Errorf("status=1 no-hook: fetcher called %d times; want 0", got)
	}
	// Cache must be untouched.
	if got := cache.Len(); got != 0 {
		t.Errorf("cache must remain empty on status=1 no-hook; got len %d", got)
	}
}

// TestProductionDispatcher_EmptyHrefs_Drops asserts a notification with all
// href fields empty drops cleanly without calling the fetcher.
func TestProductionDispatcher_EmptyHrefs_Drops(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	// All href fields empty: changedResourceHref returns ""; dispatcher drops.
	d.Dispatch(context.Background(), sep2.Notification{
		SubscribedResource: "",
		NewResourceURI:     "",
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.calls(); got != 0 {
		t.Errorf("empty-href notification: fetcher called %d times; want 0", got)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("cache must be untouched on empty-href notification; got len %d", got)
	}
}

// TestProductionDispatcher_ContextCancelWrappedError exercises the
// errors.Is(err, context.Canceled) branch inside refreshDERControlList when
// the fetcher returns a WRAPPED context.Canceled error. The cache must
// remain untouched (data-invariants Rule 1).
func TestProductionDispatcher_ContextCancelWrappedError(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeFetcher{
		returnErr: fmt.Errorf("transport: %w", context.Canceled),
	}
	cache := inverter.NewDERControlCache()
	d := NewProductionDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.calls(); got != 1 {
		t.Errorf("expected 1 fetcher call; got %d", got)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("cache must be untouched on wrapped Canceled error; got len %d", got)
	}
}
