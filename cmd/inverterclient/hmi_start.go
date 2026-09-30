// HMI dashboard bind. Extracted from main() so a test can drive the
// bind-failure path (an occupied port) without spinning up the full
// binary, matching the graceful-bypass seam pattern of
// startNotifyReceiver (notify_receiver.go).

package main

import "net"

// listenHMI binds addr (an http.Server.Addr shape, e.g. ":8080") and
// returns the listener. Callers log listener.Addr() rather than
// reconstructing the address from the configured port: the listener's own
// Addr() is the address that actually bound, not a guess.
func listenHMI(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
