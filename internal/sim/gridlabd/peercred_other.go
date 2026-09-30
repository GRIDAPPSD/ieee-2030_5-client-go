//go:build !linux

package gridlabd

import (
	"fmt"
	"net"
)

// verifyPeerPID has no portable implementation: SO_PEERCRED is
// Linux-specific (other platforms use a different mechanism, e.g.
// LOCAL_PEERCRED). Fail closed rather than silently skip the check.
func verifyPeerPID(net.Conn, int) error {
	return fmt.Errorf("peer credential check: not implemented on this platform")
}
