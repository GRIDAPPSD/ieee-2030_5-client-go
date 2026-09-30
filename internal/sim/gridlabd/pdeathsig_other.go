//go:build !linux

package gridlabd

import "syscall"

// deathSigAttr is a no-op outside Linux: Pdeathsig is a Linux-only
// mechanism. An orphaned sidecar on another OS is left to Supervisor.Stop
// or the operator.
func deathSigAttr() *syscall.SysProcAttr {
	return nil
}
