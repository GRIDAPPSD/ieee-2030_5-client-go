//go:build !unix

package main

import "os"

// reserveSignals returns a channel that never delivers: there is no SIGUSR1
// on this platform, so a flow reservation cannot be triggered by signal.
func reserveSignals() <-chan os.Signal { return nil }

// withdrawSignals returns a channel that never delivers: there is no SIGUSR2
// on this platform, so a reservation cannot be withdrawn by signal.
func withdrawSignals() <-chan os.Signal { return nil }
