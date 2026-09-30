package guard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// countingTransport is a Transport whose three methods make a real HTTP
// call against a test server, so a refused Action can be proven to have
// sent literally nothing: the server's own request counter stays at 0.
type countingTransport struct {
	client *http.Client
	base   string
}

func (c *countingTransport) Get(ctx context.Context, path string, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return "", nil
}

func (c *countingTransport) Post(ctx context.Context, path string, body any) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	return "", "", nil
}

func (c *countingTransport) Put(ctx context.Context, path string, body any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return "", nil
}

func newCountingServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestGuarded_RefusedActionSendsNothing is the issue #71 invariant: "An
// off-list request in the aggregator role is refused before any byte is
// sent." The control (TestGuarded_AllowedActionReachesServer) proves the
// same test server does receive a request when the guard allows one, so
// a 0 count here is not a check that could never fire.
func TestGuarded_RefusedActionSendsNothing(t *testing.T) {
	t.Parallel()
	srv, requests := newCountingServer(t)

	g := New(RoleAggregator, testSelf, fakeManagedSet{})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	_, _, err := guarded.Post(context.Background(), Action{Kind: KindMirrorPost, TargetLFDI: testUnmanaged}, "/mup", nil)
	if err == nil {
		t.Fatal("want refusal error, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("test server saw %d requests, want 0 (refused action must not reach the network)", got)
	}
}

// TestGuarded_AllowedActionReachesServer is the control for the test
// above: it proves the same wiring can and does reach the network when
// the guard allows the action, so the 0 count above is not a check that
// could never fire.
func TestGuarded_AllowedActionReachesServer(t *testing.T) {
	t.Parallel()
	srv, requests := newCountingServer(t)

	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	if _, _, err := guarded.Post(context.Background(), Action{Kind: KindMirrorPost, TargetLFDI: testManaged}, "/mup", nil); err != nil {
		t.Fatalf("allowed action: want nil error, got %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("test server saw %d requests, want 1", got)
	}
}

// TestGuarded_MislabelledPUT_SendsNothing reproduces fix-round-1 finding 1
// at the Guarded layer: a PUT to /edev/7 labelled KindEndDeviceRead (a
// read-only Kind) must never reach the network. RED before this fix: the
// same call reached the server (1 request).
func TestGuarded_MislabelledPUT_SendsNothing(t *testing.T) {
	t.Parallel()
	srv, requests := newCountingServer(t)

	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	_, err := guarded.Put(context.Background(), Action{Kind: KindEndDeviceRead, TargetLFDI: testManaged}, "/edev/7", nil)
	if err == nil {
		t.Fatal("PUT /edev/7 labelled KindEndDeviceRead: want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("test server saw %d requests, want 0", got)
	}
}

// TestGuarded_ZeroValueAction_SendsNothing reproduces fix-round-1 finding 1
// at the Guarded layer: a zero-value Action{} used for a POST must never
// reach the network. RED before this fix: the same call reached the
// server (1 request).
func TestGuarded_ZeroValueAction_SendsNothing(t *testing.T) {
	t.Parallel()
	srv, requests := newCountingServer(t)

	g := New(RoleAggregator, testSelf, fakeManagedSet{})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	_, _, err := guarded.Post(context.Background(), Action{}, "/edev/9/dderc", nil)
	if err == nil {
		t.Fatal("zero-value Action POST: want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("test server saw %d requests, want 0", got)
	}
}

// TestGuarded_Get_RefusedSendsNothing is fix-round-1 finding 5: Get's
// refusal check had no dedicated test (only Post did), so removing the
// `if err := g.guard.Allow(...)` line inside Guarded.Get would have
// survived. This and the Put test below close that gap.
func TestGuarded_Get_RefusedSendsNothing(t *testing.T) {
	t.Parallel()
	srv, requests := newCountingServer(t)

	g := New(RoleAggregator, testSelf, fakeManagedSet{})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	_, err := guarded.Get(context.Background(), Action{Kind: KindEndDeviceRead, TargetLFDI: testUnmanaged}, "/edev/7", nil)
	if err == nil {
		t.Fatal("GET for unmanaged device: want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("test server saw %d requests, want 0", got)
	}
}

// TestGuarded_Put_RefusedSendsNothing is the Put half of the gap
// TestGuarded_Get_RefusedSendsNothing closes for Get.
func TestGuarded_Put_RefusedSendsNothing(t *testing.T) {
	t.Parallel()
	srv, requests := newCountingServer(t)

	g := New(RoleAggregator, testSelf, fakeManagedSet{})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	_, err := guarded.Put(context.Background(), Action{Kind: KindDERResourceWrite, TargetLFDI: testUnmanaged}, "/edev/7/der/1/dercap", nil)
	if err == nil {
		t.Fatal("PUT for unmanaged device: want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("test server saw %d requests, want 0", got)
	}
}

// fakeLFDIBearing is a minimal LFDIBearing body double, proving the body
// cross-check mechanism itself works (production sep2 types do not yet
// implement it; wiring them is #72's per-device mirror/response work).
type fakeLFDIBearing struct{ lfdi string }

func (f fakeLFDIBearing) ActingForLFDI() string { return f.lfdi }

// TestGuarded_BodyLFDIMismatch_Refused proves the third classification
// signal the design names: a body that names a different EndDevice than
// the declared Action target is refused even though method and Kind both
// check out.
func TestGuarded_BodyLFDIMismatch_Refused(t *testing.T) {
	t.Parallel()
	srv, requests := newCountingServer(t)

	g := New(RoleAggregator, testSelf, fakeManagedSet{testManaged: true, testUnmanaged: true})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	// Declared target is testManaged, but the body itself claims a
	// different (also managed) device: the mismatch must still refuse.
	_, _, err := guarded.Post(context.Background(), Action{Kind: KindMirrorPost, TargetLFDI: testManaged}, "/mup", fakeLFDIBearing{lfdi: testUnmanaged})
	if err == nil {
		t.Fatal("body naming a different EndDevice than the declared target: want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("test server saw %d requests, want 0", got)
	}

	// Control: a matching body still goes through.
	if _, _, err := guarded.Post(context.Background(), Action{Kind: KindMirrorPost, TargetLFDI: testManaged}, "/mup", fakeLFDIBearing{lfdi: testManaged}); err != nil {
		t.Fatalf("matching body: want allow, got %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("test server saw %d requests, want 1 after the matching call", got)
	}
}
