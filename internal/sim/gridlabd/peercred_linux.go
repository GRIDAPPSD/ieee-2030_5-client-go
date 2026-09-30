//go:build linux

package gridlabd

import (
	"fmt"
	"net"
	"syscall"
)

// verifyPeerPID checks that the process on the other end of conn is pid,
// using SO_PEERCRED: the kernel's own record of which process holds the
// peer file descriptor, which the peer cannot spoof by anything it writes
// to the socket. Without this, a connection at the expected path proves
// only that something accepted it, not that it was our own child (the
// reproduced case: a foreign listener bound after a stale-socket cleanup
// and before our own child bound, so dialing it succeeded and looked
// healthy).
func verifyPeerPID(conn net.Conn, pid int) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("peer credential check: not a Unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer credential check: %w", err)
	}
	var cred *syscall.Ucred
	var credErr error
	if ctrlErr := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); ctrlErr != nil {
		return fmt.Errorf("peer credential check: %w", ctrlErr)
	}
	if credErr != nil {
		return fmt.Errorf("peer credential check: %w", credErr)
	}
	if int(cred.Pid) != pid {
		return fmt.Errorf("peer credential check: connected process is pid %d, want %d", cred.Pid, pid)
	}
	return nil
}
