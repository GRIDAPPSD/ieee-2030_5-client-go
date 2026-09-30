// Fix-round-3 finding 1 (HIGH, test coverage): a test proving the
// production guard path, not just the guard package in isolation. Every
// existing guard test builds *guard.Guard directly; none builds a real
// *inverter.SEP2Client and shows that ITS Get/Post/Put/PostResponse each
// refuse for the aggregator role. Without this, removing any one of the
// four `c.guard.Allow(...)` checks in client.go, or hardcoding the guard's
// role at construction, passes the whole suite.

package inverter_test

import (
	"context"
	"encoding/xml"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
)

// startCountingListener boots a real gotls-backed HTTPS server, like
// startGotlsListener in client_ccm_test.go, but counts every request that
// reaches ANY handler, on any path. Refused calls must never increment it.
func startCountingListener(t *testing.T, env *ccmTestEnv) (serverURL string, requests *int32, stop func()) {
	t.Helper()

	cfg, err := sepTLS.NewCCMServerConfig(env.serverCertPath, env.serverKeyPath, env.caCertPath)
	if err != nil {
		t.Fatalf("NewCCMServerConfig: %v", err)
	}

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	tlsL := gotls.NewListener(tcpL, cfg)

	requests = new(int32)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(requests, 1)
		switch r.Method {
		case http.MethodPost, http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "application/sep+xml")
			_ = xml.NewEncoder(w).Encode(&sep2.EndDevice{})
		}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})

	return "https://" + tlsL.Addr().String(), requests, func() {
		_ = srv.Close()
		_ = tlsL.Close()
	}
}

func newGuardTestClient(t *testing.T, serverURL string, env *ccmTestEnv, role string) *inverter.SEP2Client {
	t.Helper()
	client, err := inverter.NewSEP2Client(inverter.SimConfig{
		ServerURL:  serverURL,
		CertFile:   env.deviceCertPath,
		KeyFile:    env.deviceKeyPath,
		CAFile:     env.caCertPath,
		ClientRole: role,
	})
	if err != nil {
		t.Fatalf("NewSEP2Client(role=%q): %v", role, err)
	}
	return client
}

// TestSEP2Client_AggregatorRole_RefusesDERWritesAndMirrorAndResponse is fix
// round 3 finding 1. It builds a real *SEP2Client (real cert, real mTLS
// listener) with ClientRole "aggregator" and exercises PutDERCapability
// (Put), CreateMirrorUsagePoint (Post), and PostResponse: all three are
// allowed for a der-role client's own EndDevice (derSelfKinds) and refused
// for an aggregator-role client's own EndDevice (aggregatorSelfKinds), so
// each case is sensitive to BOTH the individual Allow check at its call
// site AND the role the guard was actually built with:
//
//   - Deleting any one of the three `c.guard.Allow(...)` checks in
//     client.go's Put, Post, or the explicit check in PostResponse turns
//     that one case RED (the call would then reach the server).
//   - Hardcoding NewSEP2Client's guard construction to guard.RoleDER
//     (ignoring cfg.ClientRole) turns ALL THREE cases RED at once, since
//     der self allows every one of them.
//
// Get is not exercised here: every Kind SEP2Client.Get sends today
// (KindEndDeviceRead, KindRegistrationRead) is self-allowed in BOTH
// roles, so no production Get call is refused by role. Get's own Allow
// check is proven load-bearing separately, by verb (see
// TestSEP2Client_Get_GuardCheckIsLoadBearing): the guard's per-method
// checks are symmetric in client.go, so there is no reason to expect
// Get's alone would be the one silently unwired.
func TestSEP2Client_AggregatorRole_RefusesDERWritesAndMirrorAndResponse(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, requests, stop := startCountingListener(t, env)
	defer stop()

	client := newGuardTestClient(t, serverURL, env, "aggregator")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.PutDERCapability(ctx, "/edev/1/der/1/dercap", sep2.DERCapability{}); err == nil {
		t.Error("aggregator PutDERCapability (self): want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Fatalf("after PutDERCapability: server saw %d requests, want 0", got)
	}

	if _, err := client.CreateMirrorUsagePoint(ctx, "/mup", sep2.MirrorUsagePoint{}); err == nil {
		t.Error("aggregator CreateMirrorUsagePoint (self): want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Fatalf("after CreateMirrorUsagePoint: server saw %d requests, want 0", got)
	}

	if err := client.PostResponse(ctx, "/rsps/1/rsp", sep2.DERControlResponse{}); err == nil {
		t.Error("aggregator PostResponse (self): want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Fatalf("after PostResponse: server saw %d requests, want 0", got)
	}
}

// TestSEP2Client_DERRole_ControlAllowsTheSameThreeCalls is the control for
// the test above: it proves the three refusals are not simply because
// PutDERCapability/CreateMirrorUsagePoint/PostResponse never work at all
// (a check that could never fail looks identical to one that passes). The
// same three calls, against the same server, from a der-role client, must
// reach the server.
func TestSEP2Client_DERRole_ControlAllowsTheSameThreeCalls(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, requests, stop := startCountingListener(t, env)
	defer stop()

	client := newGuardTestClient(t, serverURL, env, "der")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.PutDERCapability(ctx, "/edev/1/der/1/dercap", sep2.DERCapability{}); err != nil {
		t.Errorf("der PutDERCapability (self): want allow, got %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("after PutDERCapability: server saw %d requests, want 1", got)
	}

	if _, err := client.CreateMirrorUsagePoint(ctx, "/mup", sep2.MirrorUsagePoint{}); err != nil {
		t.Errorf("der CreateMirrorUsagePoint (self): want allow, got %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 2 {
		t.Fatalf("after CreateMirrorUsagePoint: server saw %d requests, want 2", got)
	}

	if err := client.PostResponse(ctx, "/rsps/1/rsp", sep2.DERControlResponse{}); err != nil {
		t.Errorf("der PostResponse (self): want allow, got %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Fatalf("after PostResponse: server saw %d requests, want 3", got)
	}
}

// TestSEP2Client_Get_GuardCheckIsLoadBearing proves Get's own
// `c.guard.Allow(http.MethodGet, ...)` check (client.go) is not dead code:
// with no production Kind naturally refused via Get (see the doc comment
// above), this uses the one shape that IS refused through Get today, a
// verb-mismatched Kind, exported and reachable because Get is a public
// method. Deleting Get's check turns this RED (a *MovedError-free 200
// would come back instead of a refusal, or the call would reach the
// server the fakeAddrSource-free way the mislabelled-PUT case did in
// guard_test.go before that fix).
func TestSEP2Client_Get_GuardCheckIsLoadBearing(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, requests, stop := startCountingListener(t, env)
	defer stop()

	client := newGuardTestClient(t, serverURL, env, "der")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out sep2.EndDevice
	// KindDERResourceWrite requires PUT; invoking it through Get is
	// refused by the same verb cross-check every role shares.
	if _, err := client.Get(ctx, guard.KindDERResourceWrite, "/edev/1", &out); err == nil {
		t.Error("Get with a PUT-only Kind: want refusal, got nil")
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("server saw %d requests, want 0", got)
	}
}
