//go:build !linux

package gridlabd

import (
	"fmt"
	"os"
)

// ownedByCurrentUser has no portable implementation here (syscall.Stat_t's
// Uid field is platform-specific). Fail closed rather than silently skip
// the check.
func ownedByCurrentUser(os.FileInfo) (bool, error) {
	return false, fmt.Errorf("owner check: not implemented on this platform")
}
