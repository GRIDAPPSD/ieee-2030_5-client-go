//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// reserveSignals delivers SIGUSR1, the operator's request to post a flow
// reservation. It is registered before the aggregator finishes starting so
// that a signal arriving early is held, not fatal (SIGUSR1's default
// action ends the process).
func reserveSignals() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	return ch
}

// withdrawSignals delivers SIGUSR2, the operator's request to withdraw the
// flow reservation being followed. Registered with reserveSignals, for the
// same reason.
func withdrawSignals() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR2)
	return ch
}
