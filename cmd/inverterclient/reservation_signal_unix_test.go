//go:build unix

package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
)

// SIGUSR1 reaches the channel the main loop reads, and does not end the
// test process (its default action would).
func TestReserveSignals_DeliversSIGUSR1(t *testing.T) {
	ch := reserveSignals()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case sig := <-ch:
		if sig != syscall.SIGUSR1 {
			t.Errorf("got %v, want SIGUSR1", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGUSR1 was not delivered to the channel")
	}
}

// An aggregator registers both operator signals and SIGUSR2 reaches the
// withdraw channel; a der process registers neither, so a nil channel never
// fires in its loop.
func TestOperatorSignals_AggregatorGetsSIGUSR2AndDERGetsNeither(t *testing.T) {
	der := inverter.SimConfig{ClientRole: string(guard.RoleDER)}
	if reserve, withdraw := operatorSignals(der); reserve != nil || withdraw != nil {
		t.Errorf("der role registered signal channels (reserve %v, withdraw %v), want none", reserve != nil, withdraw != nil)
	}

	agg := inverter.SimConfig{ClientRole: string(guard.RoleAggregator)}
	reserve, withdraw := operatorSignals(agg)
	if reserve == nil || withdraw == nil {
		t.Fatalf("aggregator role: reserve %v, withdraw %v, want both registered", reserve != nil, withdraw != nil)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	select {
	case sig := <-withdraw:
		if sig != syscall.SIGUSR2 {
			t.Errorf("got %v, want SIGUSR2", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGUSR2 was not delivered to the withdraw channel")
	}
	select {
	case sig := <-reserve:
		t.Errorf("SIGUSR2 also reached the reserve channel: %v", sig)
	default:
	}
}

// The SIGUSR2 handler withdraws the request being followed; with reservations
// off it does nothing and does not crash.
func TestWithdrawOnSignal_WithdrawsTheFollowedRequest(t *testing.T) {
	r, f, _ := startedReserver(t, nil)
	first := r.cur.mrid
	withdrawOnSignal(context.Background(), r)
	if len(f.puts) != 1 || f.puts[0].MRID != first || !r.cur.withdrawn {
		t.Errorf("PUTs %+v withdrawn %v, want the followed request %q withdrawn", f.puts, r.cur.withdrawn, first)
	}
	withdrawOnSignal(context.Background(), nil)
}
