//go:build linux

package gridlabd

import (
	"fmt"
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info's file (a directory, in
// practice) is owned by this process's own uid. A RunDir owned by another
// local user is refused: SO_PEERCRED stops that user's process from
// impersonating our own child, but it does not stop them from reading or
// replacing whatever they own inside a directory they also own.
func ownedByCurrentUser(info os.FileInfo) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("owner check: unexpected Sys() type %T", info.Sys())
	}
	return stat.Uid == uint32(os.Getuid()), nil
}
