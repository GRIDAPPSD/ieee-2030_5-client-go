//go:build linux

package gridlabd

import "syscall"

// deathSigAttr makes a sidecar exit if this process dies uncleanly (no
// Stop call, e.g. a crash), instead of leaking an orphaned python process.
func deathSigAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
