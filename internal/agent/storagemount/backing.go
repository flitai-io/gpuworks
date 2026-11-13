package storagemount

import (
	"context"
	"fmt"
	"strings"
)

// checkBacking confirms that a storage this agent does not mount itself (a shared_dir, or an
// nfs/nfs_cache marked Existing) is really backed by the network share and not by a bare
// directory on the root disk. A mount that dropped leaves exactly such a directory behind; without
// this check the storage would stay ready and downloads and heartbeats would be written to the
// local disk.
//
//   - nfs, nfs_cache (Existing): mountinfo must list a mount at the path whose fstype is nfs*.
//   - shared_dir: the path must be a mount point in mountinfo, or sit on a network or FUSE
//     filesystem according to statfs (a directory inside a mounted share is not a mount point).
//
// A non-empty reason means the storage is not backed (msg says why); err is a time out or an
// unsupported platform and is to be mapped with failErr. key is the guard key for the statfs call.
func (m *Manager) checkBacking(ctx context.Context, sp Spec, key string) (reason, msg string, err error) {
	entries, readErr := m.readMounts()
	if sp.Type == TypeNFS || sp.Type == TypeNFSCache {
		if readErr != nil {
			return ReasonOther, "cannot read mountinfo: " + readErr.Error(), nil //nolint:nilerr // A mount observation failure is a domain reason; err is reserved for guard/platform failures.
		}
		e, ok := FindMount(entries, sp.Path)
		switch {
		case !ok:
			return ReasonNotMounted, fmt.Sprintf("nothing is mounted at %s; the export is not mounted there, "+
				"and using the directory would write to the local disk", sp.Path), nil
		case !strings.HasPrefix(e.FSType, "nfs"):
			return ReasonNotMounted, fmt.Sprintf("%s is mounted as %s from %s, not as NFS", sp.Path, e.FSType, e.Source), nil
		}
		return "", "", nil
	}
	if readErr == nil {
		if _, ok := FindMount(entries, sp.Path); ok {
			return "", "", nil
		}
	}
	u, err := m.statfs(ctx, key, sp.Path)
	if err != nil {
		return "", "", err
	}
	if IsNetworkFS(u.FSType) {
		return "", "", nil
	}
	return ReasonNotMounted, fmt.Sprintf("%s is neither a mount point nor on a network filesystem; "+
		"the share is not mounted there, and using the directory would write to the local disk", sp.Path), nil
}
