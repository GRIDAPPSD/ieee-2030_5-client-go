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

// TestGuarded_RefusedActionSendsNothing is the issue #71 invariant: "An
// off-list request in the aggregator role is refused before any byte is
// sent." The control (TestGuarded_AllowedActionReachesServer) proves the
// same test server does receive a request when the guard allows one, so
// a 0 count here is not a check that could never fire.
func TestGuarded_RefusedActionSendsNothing(t *testing.T) {
	t.Parallel()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const self = "AAAA000000000000000000000000000000AAAA"
	const unmanaged = "CCCC000000000000000000000000000000CCCC"
	g := New(RoleAggregator, self, fakeManagedSet{})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	_, _, err := guarded.Post(context.Background(), Action{Kind: KindMirrorPost, TargetLFDI: unmanaged}, "/mup", nil)
	if err == nil {
		t.Fatal("want refusal error, got nil")
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Errorf("test server saw %d requests, want 0 (refused action must not reach the network)", got)
	}
}

// TestGuarded_AllowedActionReachesServer is the control for the test
// above: it proves the same wiring can and does reach the network when
// the guard allows the action, so the 0 count above is not a check that
// could never fire.
func TestGuarded_AllowedActionReachesServer(t *testing.T) {
	t.Parallel()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const self = "AAAA000000000000000000000000000000AAAA"
	const managed = "BBBB000000000000000000000000000000BBBB"
	g := New(RoleAggregator, self, fakeManagedSet{managed: true})
	guarded := NewGuarded(&countingTransport{client: srv.Client(), base: srv.URL}, g)

	if _, _, err := guarded.Post(context.Background(), Action{Kind: KindMirrorPost, TargetLFDI: managed}, "/mup", nil); err != nil {
		t.Fatalf("allowed action: want nil error, got %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("test server saw %d requests, want 1", got)
	}
}
