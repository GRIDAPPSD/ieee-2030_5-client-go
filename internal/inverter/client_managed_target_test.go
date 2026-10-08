package inverter_test

// The guard target (GRIDAPPSD/ieee-2030_5-client-go#72): a request made with
// a WithTarget context is judged as an action for that device, on the real
// client, before anything reaches the network.

import (
	"context"
	"encoding/xml"
	"errors"
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

const (
	managedLFDI   = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	unmanagedLFDI = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

// recorded is what a recordingListener saw.
type recorded struct {
	mu       sync.Mutex
	requests int
	posts    []string
}

func (r *recorded) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

func (r *recorded) postBodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.posts...)
}

// recordingListener is a gotls HTTPS server that counts every request on
// any path and keeps each POST body. GET answers an empty FSA list; POST
// and PUT answer 201 with a Location.
func recordingListener(t *testing.T, env *ccmTestEnv) (string, *recorded) {
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

	rec := &recorded{}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests++
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			rec.posts = append(rec.posts, string(b))
		}
		rec.mu.Unlock()
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/sep+xml")
			_ = xml.NewEncoder(w).Encode(&sep2.FunctionSetAssignmentsList{})
			return
		}
		w.Header().Set("Location", "/mup/1")
		w.WriteHeader(http.StatusCreated)
	})}
	go func() { _ = srv.Serve(tlsL) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = tlsL.Close()
	})
	return "https://" + tlsL.Addr().String(), rec
}

func TestManagedTarget_AggregatorMirrorForManagedDeviceIsSent(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, rec := recordingListener(t, env)
	client := newGuardTestClient(t, serverURL, env, "aggregator")
	client.SetManagedSet(guard.NewStaticManagedSet(managedLFDI))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	loc, err := client.CreateMirrorUsagePoint(inverter.WithTarget(ctx, managedLFDI), "/mup", inverter.DeviceLFDI(managedLFDI), sep2.MirrorUsagePoint{})
	if err != nil {
		t.Fatalf("mirror for a managed device: %v", err)
	}
	if loc != "/mup/1" {
		t.Errorf("Location = %q, want /mup/1", loc)
	}
	got := rec.postBodies()
	if len(got) != 1 || !strings.Contains(got[0], managedLFDI) {
		t.Errorf("posted bodies = %q, want one naming %s", got, managedLFDI)
	}
}

func TestManagedTarget_UnmanagedLFDIIsRefusedBeforeSend(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, rec := recordingListener(t, env)
	client := newGuardTestClient(t, serverURL, env, "aggregator")
	client.SetManagedSet(guard.NewStaticManagedSet(managedLFDI))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	other := inverter.WithTarget(ctx, unmanagedLFDI)

	var refusal *guard.RefusalError
	calls := map[string]func() error{
		"mirror": func() error {
			_, err := client.CreateMirrorUsagePoint(other, "/mup", inverter.DeviceLFDI(unmanagedLFDI), sep2.MirrorUsagePoint{})
			return err
		},
		"read": func() error { _, _, err := client.GetFSAList(other, "/fsa"); return err },
		"put":  func() error { return client.PutDERStatus(other, "/ders", sep2.DERStatus{}) },
		"response": func() error {
			return client.PostResponse(other, "/rsps/1/rsp", sep2.DERControlResponse{})
		},
	}
	for name, call := range calls {
		err := call()
		if !errors.As(err, &refusal) {
			t.Errorf("%s for an unmanaged LFDI: err = %v, want a guard refusal", name, err)
			continue
		}
		if !strings.EqualFold(refusal.TargetLFDI, unmanagedLFDI) {
			t.Errorf("%s refusal names target %q, want %s", name, refusal.TargetLFDI, unmanagedLFDI)
		}
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("server saw %d requests for unmanaged LFDIs, want 0", got)
	}
}

func TestManagedTarget_NoManagedSetRefusesEveryOtherDevice(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, rec := recordingListener(t, env)
	client := newGuardTestClient(t, serverURL, env, "aggregator")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, _, err := client.GetFSAList(inverter.WithTarget(ctx, managedLFDI), "/fsa"); err == nil {
		t.Error("read for another device with no managed set: want refusal, got nil")
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("server saw %d requests, want 0", got)
	}
}

// The der role never acts as a manager, so a target other than itself is
// refused for it however the context got there.
func TestManagedTarget_DERRoleRefusesAnotherDevice(t *testing.T) {
	env := newCCMTestEnv(t)
	serverURL, rec := recordingListener(t, env)
	client := newGuardTestClient(t, serverURL, env, "der")
	client.SetManagedSet(guard.NewStaticManagedSet(managedLFDI))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, _, err := client.GetFSAList(inverter.WithTarget(ctx, managedLFDI), "/fsa"); err == nil {
		t.Error("der-role read for another device: want refusal, got nil")
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("server saw %d requests, want 0", got)
	}
	// Control: the same call for its own EndDevice is sent.
	if _, _, err := client.GetFSAList(inverter.WithTarget(ctx, client.LFDI()), "/fsa"); err != nil {
		t.Errorf("der-role read for itself: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Errorf("server saw %d requests after the self read, want 1", got)
	}
}
