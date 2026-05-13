// IEEE-051 notification dispatcher unit tests.
//
// Stubs the SEP2 client via the derControlListFetcher seam so tests
// don't require a real TLS listener. End-to-end coverage through the
// /notify listener lives in notification_dispatcher_integration_test.go.

package inverter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-go/pkg/sep2"
)

// mkControl builds a DERControl with MRID set. MRID lives on the
// embedded Event so it can't be used as a struct-literal field name on
// the outer DERControl — this helper sidesteps that.
func mkControl(mrid string) sep2.DERControl {
	var c sep2.DERControl
	c.MRID = mrid
	return c
}

// mkControls builds an n-element DERControl slice with sequenced MRIDs.
func mkControls(mrids ...string) []sep2.DERControl {
	out := make([]sep2.DERControl, 0, len(mrids))
	for _, m := range mrids {
		out = append(out, mkControl(m))
	}
	return out
}

// fakeDERControlListFetcher stubs GetDERControlList. Counts calls,
// records args, returns a programmable result.
type fakeDERControlListFetcher struct {
	mu        sync.Mutex
	calls     int32
	lastHref  string
	returnVal sep2.DERControlList
	returnErr error
	// optional pause to test context cancellation
	block chan struct{}
}

func (f *fakeDERControlListFetcher) GetDERControlList(ctx context.Context, href string) (sep2.DERControlList, string, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.lastHref = href
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return sep2.DERControlList{}, "", ctx.Err()
		}
	}
	if f.returnErr != nil {
		return sep2.DERControlList{}, "", f.returnErr
	}
	return f.returnVal, "", nil
}

func (f *fakeDERControlListFetcher) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

func TestPhaseStateDispatcher_HappyPath_HrefRouting(t *testing.T) {
	t.Parallel()

	const listHref = "/edev/1/derp/1/derc"

	fake := &fakeDERControlListFetcher{
		returnVal: sep2.DERControlList{
			DERControl: mkControls("ctrl-1", "ctrl-2"),
		},
	}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("RegisterDERControlList: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.callCount(); got != 1 {
		t.Fatalf("expected 1 GetDERControlList call, got %d", got)
	}
	if cache.Len() != 2 {
		t.Fatalf("expected cache to contain 2 entries, got %d", cache.Len())
	}
}

func TestPhaseStateDispatcher_RoutingViaNewResourceURI(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{
		returnVal: sep2.DERControlList{DERControl: mkControls("ctrl-1")},
	}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Per-mRID child href in NewResourceURI; Href absent.
	d.Dispatch(context.Background(), sep2.Notification{
		SubscribedResource: "/edev/1/fsa", // subscribed to FSAList
		NewResourceURI:     "/edev/1/derp/1/derc/abc",
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.callCount(); got != 1 {
		t.Fatalf("expected 1 call (routed via NewResourceURI prefix), got %d", got)
	}
	if cache.Len() != 1 {
		t.Fatalf("expected cache len 1, got %d", cache.Len())
	}
}

func TestPhaseStateDispatcher_RoutingViaSubscribedResource(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{
		returnVal: sep2.DERControlList{DERControl: mkControls("ctrl-1")},
	}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Server publishes a notification with only SubscribedResource set —
	// it happens to equal the registered DERControlListHref.
	d.Dispatch(context.Background(), sep2.Notification{
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.callCount(); got != 1 {
		t.Fatalf("expected 1 call via SubscribedResource fallback, got %d", got)
	}
}

func TestPhaseStateDispatcher_UnknownResource_DropsCleanly(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Notification for a resource we didn't subscribe to.
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: "/edev/1/mup/42"},
		SubscribedResource: "/edev/1/mup",
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.callCount(); got != 0 {
		t.Fatalf("unknown resource should not trigger GET; got %d calls", got)
	}
}

func TestPhaseStateDispatcher_StatusCancellation_Drops(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
	}

	// status=1 → subscription cancelled by server. IEEE-052 territory;
	// IEEE-051 logs + drops.
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusSubscripted,
	})

	if got := fake.callCount(); got != 0 {
		t.Fatalf("status=1 cancellation should drop; got %d calls", got)
	}
}

func TestPhaseStateDispatcher_NotYetRegistered_Drops(t *testing.T) {
	t.Parallel()
	// Unregistered dispatcher → IEEE-049 no-op semantics.
	d := NewPhaseStateDispatcher()
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: "/edev/1/derp/1/derc"},
		SubscribedResource: "/edev/1/derp/1/derc",
		Status:             sep2.NotificationStatusChanged,
	})
	// No assertion needed beyond "did not panic". The log line is the
	// audit trail; the test exists so the unregistered path is exercised
	// by the race-detector + coverage tooling.
}

func TestPhaseStateDispatcher_EmptyHrefs_Drops(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Status: sep2.NotificationStatusChanged, // every href field empty
	})

	if got := fake.callCount(); got != 0 {
		t.Fatalf("empty-href notification should drop; got %d calls", got)
	}
}

func TestPhaseStateDispatcher_GetError_LoggedNotFatal(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{
		returnErr: fmt.Errorf("GET DERControlList: %w", ErrResponseTransient),
	}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Must not crash; cache remains untouched; method returns cleanly.
	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: listHref},
		SubscribedResource: listHref,
		Status:             sep2.NotificationStatusChanged,
	})

	if got := fake.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 call, got %d", got)
	}
	if cache.Len() != 0 {
		t.Fatalf("cache should be untouched on GET error; got len %d", cache.Len())
	}
}

func TestPhaseStateDispatcher_ContextCancellation(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	block := make(chan struct{})
	fake := &fakeDERControlListFetcher{
		block:     block,
		returnVal: sep2.DERControlList{DERControl: mkControls("ctrl-1")},
	}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
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

	// Cancel mid-flight; dispatch should return cleanly without crashing.
	cancel()
	close(block) // let the fetcher observe the cancelled ctx
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after ctx cancel within 2s — goroutine leak suspected")
	}
}

func TestPhaseStateDispatcher_ConcurrentDispatch_Safe(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{
		returnVal: sep2.DERControlList{DERControl: mkControls("ctrl-1")},
	}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatalf("register: %v", err)
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

	if got := fake.callCount(); got != N {
		t.Fatalf("expected %d concurrent calls, got %d", N, got)
	}
	if cache.Len() != 1 {
		t.Fatalf("cache should converge to len 1, got %d", cache.Len())
	}
}

func TestPhaseStateDispatcher_RegisterDERControlList_ArgValidation(t *testing.T) {
	t.Parallel()

	fake := &fakeDERControlListFetcher{}
	cache := NewDERControlCache()

	tests := []struct {
		name   string
		client derControlListFetcher
		cache  *DERControlCache
		href   string
	}{
		{"nil client", nil, cache, "/x"},
		{"nil cache", fake, nil, "/x"},
		{"empty href", fake, cache, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewPhaseStateDispatcher()
			err := d.RegisterDERControlList(tt.client, tt.cache, tt.href)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestPhaseStateDispatcher_RegisterDERControlList_Idempotent(t *testing.T) {
	t.Parallel()
	d := NewPhaseStateDispatcher()
	cache := NewDERControlCache()
	fake1 := &fakeDERControlListFetcher{returnVal: sep2.DERControlList{DERControl: mkControls("a")}}
	fake2 := &fakeDERControlListFetcher{returnVal: sep2.DERControlList{DERControl: mkControls("b", "c")}}
	if err := d.RegisterDERControlList(fake1, cache, "/h1"); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterDERControlList(fake2, cache, "/h2"); err != nil {
		t.Fatal(err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource:           sep2.Resource{Href: "/h2"},
		SubscribedResource: "/h2",
		Status:             sep2.NotificationStatusChanged,
	})

	if fake1.callCount() != 0 {
		t.Fatalf("fake1 should have been replaced; got %d calls", fake1.callCount())
	}
	if fake2.callCount() != 1 {
		t.Fatalf("fake2 should have been called once; got %d", fake2.callCount())
	}
}

// changedResourceHref + resourceKindFor are tested via Dispatch above,
// but a couple of direct cases keep the helpers honest under refactor.

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
		t.Run(tc.name, func(t *testing.T) {
			if got := changedResourceHref(tc.n); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

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
		{"prefix-but-not-child", listHref + "extra", listHref, resourceKindUnknown}, // not under listHref/
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceKindFor(tc.changed, tc.list); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestPhaseStateDispatcher_GetError_ContextCancelled_LogsAndReturns(t *testing.T) {
	t.Parallel()
	const listHref = "/edev/1/derp/1/derc"
	fake := &fakeDERControlListFetcher{
		returnErr: fmt.Errorf("GET DERControlList: %w", context.Canceled),
	}
	cache := NewDERControlCache()
	d := NewPhaseStateDispatcher()
	if err := d.RegisterDERControlList(fake, cache, listHref); err != nil {
		t.Fatal(err)
	}

	d.Dispatch(context.Background(), sep2.Notification{
		Resource: sep2.Resource{Href: listHref},
		Status:   sep2.NotificationStatusChanged,
	})
	if fake.callCount() != 1 {
		t.Fatalf("expected 1 call; got %d", fake.callCount())
	}
}
