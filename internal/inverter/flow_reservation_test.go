package inverter_test

// The aggregator's own flow reservation calls (GRIDAPPSD/ieee-2030_5-client-go#73)
// on the real client and guard: they reach the network for the aggregator's
// own EndDevice whatever target the context carries, and the der role cannot
// send them.

import (
	"context"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
)

type frqSeen struct {
	mu    sync.Mutex
	posts map[string][]string // path -> bodies
	gets  []string            // request URIs
}

func frqListener(t *testing.T, env *ccmTestEnv) (string, *frqSeen) {
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
	seen := &frqSeen{posts: map[string][]string{}}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		if r.Method == http.MethodGet {
			seen.gets = append(seen.gets, r.URL.RequestURI())
			w.Header().Set("Content-Type", "application/sep+xml")
			_ = xml.NewEncoder(w).Encode(&sep2.FlowReservationResponseList{
				FlowReservationResponse: []sep2.FlowReservationResponse{{Subject: "REQ1"}},
			})
			return
		}
		b, _ := io.ReadAll(r.Body)
		seen.posts[r.URL.Path] = append(seen.posts[r.URL.Path], string(b))
		w.Header().Set("Location", "/edev/1/frq/1")
		w.WriteHeader(http.StatusCreated)
	})}
	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})
	return "https://" + tlsL.Addr().String(), seen
}

// A context that names a managed device does not change who the reservation
// is for: the request is posted as the aggregator's own and reaches the
// server, carrying the request it was given.
func TestFlowReservation_AggregatorPostsOwnWhateverTheContextTargets(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, seen := frqListener(t, env)
	client := newGuardTestClient(t, serverURL, env, "aggregator")
	client.SetManagedSet(guard.NewStaticManagedSet(managedLFDI))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := sep2.FlowReservationRequest{
		MRID:            "REQ1",
		EnergyRequested: &sep2.SignedRealEnergy{Value: 6000},
		PowerRequested:  &sep2.ActivePower{Value: 3000},
	}
	loc, err := client.PostFlowReservationRequest(inverter.WithTarget(ctx, managedLFDI), "/edev/1/frq", req)
	if err != nil {
		t.Fatalf("PostFlowReservationRequest: %v", err)
	}
	if loc != "/edev/1/frq/1" {
		t.Errorf("Location = %q, want /edev/1/frq/1", loc)
	}
	bodies := seen.posts["/edev/1/frq"]
	if len(bodies) != 1 {
		t.Fatalf("%d bodies posted to /edev/1/frq, want 1", len(bodies))
	}
	var got sep2.FlowReservationRequest
	if err := xml.Unmarshal([]byte(bodies[0]), &got); err != nil {
		t.Fatalf("posted body does not decode: %v", err)
	}
	if got.MRID != "REQ1" || got.EnergyRequested.Value != 6000 || got.PowerRequested.Value != 3000 {
		t.Errorf("posted request = mRID %q energy %v power %v, want REQ1 6000 3000", got.MRID, got.EnergyRequested, got.PowerRequested)
	}

	if _, err := client.GetFlowReservationResponses(inverter.WithTarget(ctx, managedLFDI), "/edev/1/frp"); err != nil {
		t.Fatalf("GetFlowReservationResponses: %v", err)
	}
	if len(seen.gets) != 1 || seen.gets[0] != "/edev/1/frp?l=255" {
		t.Errorf("GET URIs = %q, want [/edev/1/frp?l=255]", seen.gets)
	}
}

func TestFlowReservation_AcknowledgementIsPostedWithTheResponseMRIDAsSubject(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, seen := frqListener(t, env)
	client := newGuardTestClient(t, serverURL, env, "aggregator")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	status := sep2.ResponseStatusEventReceived
	err := client.PostFlowReservationResponseResponse(ctx, "/rsps/9/rsp", sep2.FlowReservationResponseResponse{
		Response: sep2.Response{Status: &status, Subject: "RESP-MRID"},
	})
	if err != nil {
		t.Fatalf("PostFlowReservationResponseResponse: %v", err)
	}
	bodies := seen.posts["/rsps/9/rsp"]
	if len(bodies) != 1 || !strings.Contains(bodies[0], "<FlowReservationResponseResponse") {
		t.Fatalf("posted %q, want one FlowReservationResponseResponse", bodies)
	}
	var got sep2.FlowReservationResponseResponse
	if err := xml.Unmarshal([]byte(bodies[0]), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Subject != "RESP-MRID" || got.Status == nil || *got.Status != sep2.ResponseStatusEventReceived {
		t.Errorf("acknowledgement subject %q status %v, want RESP-MRID received", got.Subject, got.Status)
	}
}

// The der role has no fleet to reserve for and no reservation to
// acknowledge: both calls are refused before anything reaches the server.
func TestFlowReservation_DERRoleRefusedBeforeSend(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, seen := frqListener(t, env)
	client := newGuardTestClient(t, serverURL, env, "der")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.PostFlowReservationRequest(ctx, "/edev/1/frq", sep2.FlowReservationRequest{MRID: "X"}); err == nil {
		t.Error("der role posted a FlowReservationRequest")
	}
	if err := client.PostFlowReservationResponseResponse(ctx, "/rsps/9/rsp", sep2.FlowReservationResponseResponse{}); err == nil {
		t.Error("der role posted a FlowReservationResponseResponse")
	}
	if len(seen.posts) != 0 {
		t.Errorf("%d POSTs reached the server, want 0", len(seen.posts))
	}
}

func TestNewFlowReservationRequestMRID_IsFreshHex128(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a, err := inverter.NewFlowReservationRequestMRID("LFDI", now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := inverter.NewFlowReservationRequestMRID("LFDI", now)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two requests with the same LFDI and instant share mRID %s", a)
	}
	for _, m := range []string{a, b} {
		if len(m) != 32 || strings.Trim(m, "0123456789ABCDEF") != "" {
			t.Errorf("mRID %q is not 32 upper-case hex characters", m)
		}
	}
}
