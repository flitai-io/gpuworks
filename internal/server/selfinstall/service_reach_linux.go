//go:build linux

package selfinstall

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// statDirAccess reads a directory's owner, group, mode and whether an access
// ACL is attached to it.
func statDirAccess(path string) (dirAccess, error) {
	info, err := os.Stat(path)
	if err != nil {
		return dirAccess{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return dirAccess{}, errUnsupportedStat
	}
	access := dirAccess{uid: st.Uid, gid: st.Gid, mode: info.Mode()}
	if _, err := unix.Getxattr(path, "system.posix_acl_access", nil); err == nil {
		access.acl = true
	}
	return access, nil
}
