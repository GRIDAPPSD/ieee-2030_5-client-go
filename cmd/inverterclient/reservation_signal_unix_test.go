//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
	"time"
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
