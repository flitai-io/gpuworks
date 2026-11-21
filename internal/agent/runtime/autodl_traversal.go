package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/nodeenv"
)

// PrepareAutoDLTraversal opens only /root's search permission for the runtime
// identity on AutoDL. Other operator-owned ancestors retain their permissions.
func PrepareAutoDLTraversal(name string, owner MountOwner) error {
	hostname, _ := os.Hostname()
	return prepareAutoDLRootTraversal(name, owner, hostname, "/root", os.Geteuid())
}

func prepareAutoDLRootTraversal(name string, owner MountOwner, hostname, rootDir string, agentUID int) error {
	if agentUID != 0 || owner.UID == 0 || !nodeenv.IsAutoDLContainer(hostname) {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return fmt.Errorf("resolve AutoDL runtime path: %w", err)
	}
	insideRoot := func(path string) bool {
		rel, err := filepath.Rel(rootDir, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !insideRoot(name) && !insideRoot(resolved) {
		return nil
	}
	rootInfo, err := os.Lstat(rootDir)
	if err != nil {
		return fmt.Errorf("stat AutoDL root directory: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	// Do not follow a replaced /root or chmod a different object after checking.
	f, err := os.OpenFile(rootDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open AutoDL root directory: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// Mode bits decide the repair, not the kernel: this branch must stay
	// reachable to grant search when the owner's class lacks it.
	if hasPermission(info, owner, 0o1) {
		return nil
	}
	uid, gid, ok := FileOwner(info)
	if !ok || uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return traversalError(rootDir, info, owner, "must be root-owned and not writable by group or others for AutoDL traversal repair")
	}
	bit := os.FileMode(0o001)
	if gid == owner.GID {
		bit = 0o010
	}
	mode := info.Mode() | bit
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("grant AutoDL runtime traversal: %w", err)
	}
	updated, err := f.Stat()
	if err != nil {
		return err
	}
	if !hasPermission(updated, owner, 0o1) {
		return traversalError(rootDir, updated, owner, "is still not traversable after AutoDL repair")
	}
	slog.Warn("granted AutoDL root directory traversal for runtime user", "dir", rootDir,
		"run_uid", owner.UID, "run_gid", owner.GID,
		"old_mode", fmt.Sprintf("%04o", info.Mode().Perm()), "new_mode", fmt.Sprintf("%04o", updated.Mode().Perm()))
	return nil
}
