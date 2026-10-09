package inverter_test

import (
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
)

// redirectListener answers every request with a 301 to location and counts
// the requests it received.
func redirectListener(t *testing.T, env *ccmTestEnv, location string) (string, *atomic.Int32) {
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
	hits := &atomic.Int32{}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusMovedPermanently)
	})}
	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})
	return "https://" + tlsL.Addr().String(), hits
}

// A withdrawal that the server redirects to another host is refused: the body
// and the client certificate go to the configured server only.
func TestFlowReservation_WithdrawalRefusesA301ToAnotherHost(t *testing.T) {
	env := newCCMTestEnv(t)
	otherURL, otherSeen := frqListener(t, env)
	serverURL, hits := redirectListener(t, env, otherURL+"/edev/1/frq/5")
	client := newGuardTestClient(t, serverURL, env, "aggregator")
	ctx := t.Context()

	err := client.PutFlowReservationRequest(ctx, "/edev/1/frq/5", sep2.FlowReservationRequest{MRID: "REQ1"})
	if err == nil {
		t.Fatal("a 301 to another host was followed")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("configured server saw %d requests, want 1 (the PUT, not a retry)", got)
	}
	if n := len(otherSeen.posts); n != 0 {
		t.Errorf("%d writes reached the other host, want 0", n)
	}
}

// A 301 to a path on the configured server is still followed once.
func TestFlowReservation_WithdrawalFollowsA301OnTheConfiguredServer(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, hits := redirectListener(t, env, "/edev/1/frq/6")
	client := newGuardTestClient(t, serverURL, env, "aggregator")

	// The listener redirects every request, so the followed PUT is redirected
	// again: the second 301 surfaces as the error, after exactly two requests.
	err := client.PutFlowReservationRequest(t.Context(), "/edev/1/frq/5", sep2.FlowReservationRequest{MRID: "REQ1"})
	if err == nil {
		t.Fatal("a second 301 was accepted")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("configured server saw %d requests, want 2 (the PUT and one follow)", got)
	}
}
