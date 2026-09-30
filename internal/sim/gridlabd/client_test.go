package gridlabd

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func dialFake(t *testing.T, configure func(*fakeProcess)) (*Client, *fakeProcess) {
	t.Helper()
	reg := &fakeRegistry{}
	launch := newFakeLauncher(t, configure, reg)
	sockPath := filepath.Join(shortSockDir(t), "a.sock")
	proc, err := launch(context.Background(), []string{"--socket", sockPath, "--fleet-file", "unused.json"})
	if err != nil {
		t.Fatalf("launch fake: %v", err)
	}
	t.Cleanup(func() { _ = proc.Kill() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := waitForSocket(ctx, sockPath)
	if err != nil {
		t.Fatalf("dial fake: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, reg.list()[0]
}

func TestClient_Hello(t *testing.T) {
	client, _ := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if hello.Protocol != ProtocolVersion {
		t.Errorf("Protocol = %d, want %d", hello.Protocol, ProtocolVersion)
	}
}

func TestClient_Hello_RemoteError(t *testing.T) {
	client, _ := dialFake(t, func(fp *fakeProcess) {
		fp.helloErr = &wireErrorBody{Code: "gridlabd_error", Message: "version mismatch"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Hello(ctx)
	if err == nil {
		t.Fatal("Hello: want error, got nil")
	}
	remoteErr, ok := err.(*RemoteError)
	if !ok {
		t.Fatalf("Hello error type = %T, want *RemoteError", err)
	}
	if remoteErr.Code != "gridlabd_error" {
		t.Errorf("Code = %q, want gridlabd_error", remoteErr.Code)
	}
}

// TestClient_Set_Get_ReadsBackAppliedValue proves the set-then-get round
// trip: the value read back is the value that was actually applied on the
// far side of the protocol, not merely an echo the client itself invented.
func TestClient_Set_Get_ReadsBackAppliedValue(t *testing.T) {
	client, _ := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	setResults, err := client.Set(ctx, []SetItem{
		{Object: "inv0", Property: "P_Out", Value: FloatValue(2500)},
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := setResults[0].Value.Float64()
	if err != nil {
		t.Fatalf("Set result Float64: %v", err)
	}
	if got != 2500 {
		t.Errorf("Set applied value = %v, want 2500", got)
	}

	getResults, err := client.Get(ctx, []GetItem{{Object: "inv0", Property: "P_Out"}})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err = getResults[0].Value.Float64()
	if err != nil {
		t.Fatalf("Get result Float64: %v", err)
	}
	if got != 2500 {
		t.Errorf("Get after Set = %v, want 2500 (the value Set actually applied)", got)
	}
}

func TestClient_StepTo_AdvancesAndIsIdempotent(t *testing.T) {
	client, _ := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	target := time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC)
	reached, err := client.StepTo(ctx, target)
	if err != nil {
		t.Fatalf("StepTo: %v", err)
	}
	if !reached.Equal(target) {
		t.Errorf("StepTo reached = %v, want %v", reached, target)
	}

	// A step_to at or before the model's current time is a no-op: it must
	// return the current time, not move backwards or error.
	earlier := target.Add(-30 * time.Second)
	reached2, err := client.StepTo(ctx, earlier)
	if err != nil {
		t.Fatalf("StepTo (earlier): %v", err)
	}
	if !reached2.Equal(target) {
		t.Errorf("StepTo(earlier) = %v, want unchanged %v", reached2, target)
	}
}

func TestClient_Shutdown_ClosesConnection(t *testing.T) {
	client, fp := dialFake(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-fp.Wait():
	case <-time.After(2 * time.Second):
		t.Fatal("fake process did not exit after Shutdown")
	}
}
