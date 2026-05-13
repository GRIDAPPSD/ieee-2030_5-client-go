// IEEE-052 Phase 8 ticket 4 of 4 (final) unit tests: subscriptionRegistry.
//
// Covers Add / Cancel / Lookup / Len / Snapshot / CancelHookFunc semantics
// plus a race-detector exercise so concurrent Add+Cancel from independent
// goroutines is observably safe.

package main

import (
	"sync"
	"testing"
)

func TestSubscriptionRegistry_AddAndLookup(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(nil)
	if r.Len() != 0 {
		t.Fatalf("empty registry Len = %d, want 0", r.Len())
	}
	r.Add("/edev/1/fsa", "/edev/1/sub/sub-FSA")
	if r.Len() != 1 {
		t.Fatalf("after Add Len = %d, want 1", r.Len())
	}
	got, ok := r.Lookup("/edev/1/fsa")
	if !ok || got != "/edev/1/sub/sub-FSA" {
		t.Errorf("Lookup(fsa) = (%q, %v), want (/edev/1/sub/sub-FSA, true)", got, ok)
	}
	if _, ok := r.Lookup("/missing"); ok {
		t.Errorf("Lookup(/missing) ok = true, want false")
	}
}

func TestSubscriptionRegistry_AddEmptySubscribedHrefIgnored(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(nil)
	r.Add("", "/edev/1/sub/anything")
	if r.Len() != 0 {
		t.Errorf("empty subscribed href should be ignored; Len = %d", r.Len())
	}
}

func TestSubscriptionRegistry_AddIdempotent(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(nil)
	r.Add("/edev/1/fsa", "/edev/1/sub/A")
	r.Add("/edev/1/fsa", "/edev/1/sub/B") // server reassigned
	if r.Len() != 1 {
		t.Fatalf("Len after re-Add = %d, want 1", r.Len())
	}
	got, _ := r.Lookup("/edev/1/fsa")
	if got != "/edev/1/sub/B" {
		t.Errorf("re-Add did not replace value: got %q, want /edev/1/sub/B", got)
	}
}

func TestSubscriptionRegistry_CancelRemovesEntry(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(map[string]string{
		"/edev/1/fsa": "/edev/1/sub/sub-FSA",
		"/edev/1/der": "/edev/1/sub/sub-DER",
	})
	prev := r.Cancel("/edev/1/fsa")
	if prev != "/edev/1/sub/sub-FSA" {
		t.Errorf("Cancel returned %q, want /edev/1/sub/sub-FSA", prev)
	}
	if r.Len() != 1 {
		t.Errorf("after Cancel Len = %d, want 1", r.Len())
	}
	if _, ok := r.Lookup("/edev/1/fsa"); ok {
		t.Errorf("cancelled entry still present")
	}
	if _, ok := r.Lookup("/edev/1/der"); !ok {
		t.Errorf("untouched entry was removed")
	}
}

func TestSubscriptionRegistry_CancelUnknownHrefNoop(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(map[string]string{
		"/edev/1/fsa": "/edev/1/sub/sub-FSA",
	})
	prev := r.Cancel("/not-in-registry")
	if prev != "" {
		t.Errorf("Cancel(unknown) returned %q, want \"\"", prev)
	}
	if r.Len() != 1 {
		t.Errorf("Cancel(unknown) mutated registry; Len = %d, want 1", r.Len())
	}
}

func TestSubscriptionRegistry_CancelEmptyHrefNoop(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(map[string]string{
		"/edev/1/fsa": "/edev/1/sub/sub-FSA",
	})
	prev := r.Cancel("")
	if prev != "" {
		t.Errorf("Cancel(\"\") returned %q, want \"\"", prev)
	}
	if r.Len() != 1 {
		t.Errorf("Cancel(\"\") mutated registry; Len = %d", r.Len())
	}
}

func TestSubscriptionRegistry_Snapshot_Stable(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(map[string]string{
		"/edev/1/der": "/edev/1/sub/sub-DER",
		"/edev/1/fsa": "/edev/1/sub/sub-FSA",
		"/edev/1/aaa": "/edev/1/sub/sub-AAA",
	})
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	// Sort key is subscribed href: aaa < der < fsa lexicographically.
	want := [3]string{"/edev/1/aaa", "/edev/1/der", "/edev/1/fsa"}
	for i := range want {
		if snap[i][0] != want[i] {
			t.Errorf("Snapshot[%d][0] = %q, want %q", i, snap[i][0], want[i])
		}
	}
}

func TestSubscriptionRegistry_CancelHookFunc_DelegatesToCancel(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(map[string]string{
		"/edev/1/fsa": "/edev/1/sub/sub-FSA",
	})
	hook := r.CancelHookFunc()
	hook("/edev/1/fsa")
	if r.Len() != 0 {
		t.Errorf("CancelHookFunc did not remove entry; Len = %d", r.Len())
	}
}

// TestSubscriptionRegistry_ConcurrentAddCancel exercises the race
// detector: many goroutines Add and Cancel against the same registry
// concurrently. The race-clean expectation is that `go test -race`
// reports no data race; the assertion below is a soft sanity check.
func TestSubscriptionRegistry_ConcurrentAddCancel(t *testing.T) {
	t.Parallel()
	r := newSubscriptionRegistry(nil)
	const n = 200
	var wg sync.WaitGroup
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			r.Add(href(i), serverHref(i))
		}(i)
		go func(i int) {
			defer wg.Done()
			r.Cancel(href(i))
		}(i)
	}
	wg.Wait()
	// Sanity: registry size should be ≤ n. We can't assert an exact
	// count because Add and Cancel race; the point of the test is the
	// race detector, not the count.
	if got := r.Len(); got > n {
		t.Errorf("registry grew past n; Len = %d, max %d", got, n)
	}
}

func href(i int) string       { return "/edev/1/sub-target-" + itoa(i) }
func serverHref(i int) string { return "/edev/1/sub/sub-" + itoa(i) }

// itoa avoids importing strconv into a tiny test helper. The values
// don't need to round-trip — they only need to be unique.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
