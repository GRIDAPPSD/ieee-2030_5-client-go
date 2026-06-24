// IEEE-052 Phase 8 ticket 4 of 4 (final) : integration test for the
// /notify listener to SimulatorDispatcher to CancelHook chain.
//
// IEEESIM-004: updated to use dispatch.SimulatorDispatcher in place of the
// removed inverter.PhaseStateDispatcher. Behavior and assertions are unchanged.
//
// Exercises the full stack: a real ccmTestEnv-backed NotifyReceiver
// listening on TLS, a dispatch.SimulatorDispatcher wired with a
// CancelHook that mutates a tiny inline registry, and a Notification
// POSTed with status=1 (CSIP V1.2 CORE-019 step 13: "subscription
// cancelled by server"). The assertion is that the registry entry for
// the SubscribedResource is removed after the POST returns 204.

package inverter_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter/dispatch"
)

// inlineRegistry is a minimal mutex-guarded subscription tracker used
// only by this test. The production version lives in cmd/inverterclient
// (subscriptionRegistry) : keeping the test self-contained avoids a
// cross-package import from internal/inverter into cmd/.
type inlineRegistry struct {
	mu      sync.Mutex
	entries map[string]string
}

func newInlineRegistry() *inlineRegistry {
	return &inlineRegistry{entries: map[string]string{}}
}

func (r *inlineRegistry) Add(sub, server string) {
	r.mu.Lock()
	r.entries[sub] = server
	r.mu.Unlock()
}

func (r *inlineRegistry) Has(sub string) bool {
	r.mu.Lock()
	_, ok := r.entries[sub]
	r.mu.Unlock()
	return ok
}

func (r *inlineRegistry) Len() int {
	r.mu.Lock()
	n := len(r.entries)
	r.mu.Unlock()
	return n
}

func (r *inlineRegistry) cancel(sub string) {
	r.mu.Lock()
	delete(r.entries, sub)
	r.mu.Unlock()
}

// TestNotifyHandler_Status1_CancelHookMutatesRegistry is the IEEE-052
// integration anchor: an inbound Notification with status=1 must result
// in the CancelHook firing with the SubscribedResource href, and the
// caller's subscription-tracking state must be cleaned up : observable
// here as registry.Len going from N to N-1.
func TestNotifyHandler_Status1_CancelHookMutatesRegistry(t *testing.T) {
	t.Parallel()

	env := newCCMTestEnv(t)

	// Pre-seed registry as if IEEE-050 had registered two subscriptions.
	registry := newInlineRegistry()
	registry.Add("/edev/0/fsa", "/edev/0/sub/1") // sampleNotificationXML uses /edev/0/fsa
	registry.Add("/edev/0/der", "/edev/0/sub/2")
	if got := registry.Len(); got != 2 {
		t.Fatalf("seed registry len = %d, want 2", got)
	}

	dispatcher := dispatch.NewSimulatorDispatcher()
	dispatcher.RegisterCancelHook(func(subscribedHref string) {
		registry.cancel(subscribedHref)
	})

	rcv, _ := newReceiverFromEnv(t, env, dispatcher.Dispatch)
	client := notifyClientForEnv(t, env)

	// status=1 → cancellation per CORE-019 step 13. SubscribedResource
	// in the sample is "/edev/0/fsa" (see sampleNotificationXML).
	body := sampleNotificationXML(t, 1, "/edev/0/fsa")
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, notifyURL(t, rcv, "/notify"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/sep+xml")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", resp.StatusCode)
	}

	// The IEEE-049 handler invokes the dispatcher synchronously on the
	// handler goroutine before writing 204 (see notify.go), so by the
	// time we observe the 204 the CancelHook has fired.
	if registry.Has("/edev/0/fsa") {
		t.Errorf("cancelled subscription still in registry; entries: %d", registry.Len())
	}
	// Untouched entry must remain.
	if !registry.Has("/edev/0/der") {
		t.Errorf("untouched subscription removed by mistake")
	}
	if got := registry.Len(); got != 1 {
		t.Errorf("registry len after cancel = %d, want 1", got)
	}
}
