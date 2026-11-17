package storagemount

import "strings"

// ClassifyMountError maps the text printed by a failed mount command to a
// Reason code. External text: mount(8) and its mount.nfs/mount.cifs helpers
// report failures only in words; mountFailure calls this on the output of the
// command it ran.
func ClassifyMountError(output string) string {
	s := strings.ToLower(output)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(s, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("only root can do that", "must be superuser", "must be root"):
		return ReasonNotRoot
	case has("reason given by server: no such file", "mount point does not exist", "does not exist"):
		return ReasonPathMissing
	case has("access denied by server", "permission denied", "operation not permitted"):
		return ReasonPermission
	case has("connection timed out", "no route to host", "connection refused", "failed to resolve server",
		"name or service not known", "network is unreachable", "operation timed out", "timed out"):
		return ReasonUnreachable
	case has("bad option; for several filesystems", "wrong fs type", "helper program", "unknown filesystem type",
		"command not found", "mount.nfs: not found"):
		return ReasonMissingClient
	}
	return ReasonOther
}

// IsNetworkFS reports whether a statfs filesystem magic number denotes a network or FUSE
// filesystem: NFS, CIFS, SMB2, FUSE, Ceph or Lustre.
func IsNetworkFS(fsType uint64) bool {
	switch fsType {
	case 0x6969, 0xFF534D42, 0xFE534D42, 0x65735546, 0x00C36400, 0x0BD00BD0:
		return true
	}
	return false
}
