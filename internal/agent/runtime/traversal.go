package runtime

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// PrepareTraversal lets a dedicated runtime identity traverse dir, the
// agent-owned state directory that holds the runtime's own files (patched
// PRoot, runtime mounts) next to private agent state such as identity.json.
// A state directory the agent creates itself is 0700: the logger makes it
// before any runtime is provisioned.
//
// The grant is search (x) only, never listing or writing, on the narrowest
// class that works:
//   - dir is already traversable for owner: unchanged;
//   - dir grants its group nothing, or its group is already the runtime's
//     primary group: the group becomes the runtime's and gains x (0700 -> 0710);
//   - dir's group holds permissions that a group change would revoke or hand
//     to the runtime: others gain x instead (0750 -> 0751).
//
// dir must belong to the agent and not be writable by group or others;
// anything else is reported, not repaired. Except for AutoDL's /root search
// permission, ancestors are left to CheckAncestorTraversal: whether one is fatal depends
// on what the caller is about to do. Callers run this before each use so that a
// state directory reset after provisioning is repaired again; the traversable
// case costs a stat.
func PrepareTraversal(dir string, owner MountOwner) error {
	if err := PrepareAutoDLTraversal(dir, owner); err != nil {
		return err
	}
	// The state path is agent configuration and may itself be a symlink:
	// resolve it, then change only the opened directory.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve runtime state directory %s: %w", dir, err)
	}
	return grantTraversal(resolved, owner)
}

func grantTraversal(dir string, owner MountOwner) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open runtime state directory %s: %w", dir, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat runtime state directory %s: %w", dir, err)
	}
	// The mode-bit rule decides the repair here, not the kernel: the branch
	// below must stay reachable to hold the ownership contract (a capable
	// agent passes any directory, and granting traversal on a directory it
	// does not own would skip that contract silently).
	if hasPermission(info, owner, 0o1) {
		return nil
	}
	uid, gid, ok := FileOwner(info)
	mode := info.Mode().Perm()
	if !ok || uid != os.Geteuid() || mode&0o022 != 0 {
		return traversalError(dir, info, owner, "must belong to the agent and not be writable by group or others to grant runtime traversal")
	}
	if gid != owner.GID && mode&0o070 != 0 {
		slog.Warn("runtime state directory group holds its own permissions; granting traversal to others instead",
			"dir", dir, "mode", fmt.Sprintf("%04o", mode), "gid", gid, "run_gid", owner.GID)
		if err := f.Chmod(info.Mode() | 0o001); err != nil {
			return fmt.Errorf("grant runtime traversal on %s: %w", dir, err)
		}
		return nil
	}
	if gid != owner.GID {
		if err := f.Chown(-1, owner.GID); err != nil {
			return fmt.Errorf("set runtime group on %s: %w", dir, err)
		}
	}
	if err := f.Chmod(info.Mode() | 0o010); err != nil {
		if gid != owner.GID {
			if rollback := f.Chown(-1, gid); rollback != nil {
				return fmt.Errorf("grant runtime traversal on %s: %w; restore group %d: %v", dir, err, gid, rollback)
			}
		}
		return fmt.Errorf("grant runtime traversal on %s: %w", dir, err)
	}
	return nil
}

// TraversalError is a directory the runtime identity cannot search, with the
// facts that decide it. CheckAncestorTraversal returns it for that case (and
// plain errors for a path it could not stat), so every caller that reports the
// condition -- the launch-time warning, the node's doctor -- reads one
// verdict.
type TraversalError struct {
	// Dir is the blocking directory.
	Dir string
	// Reason is why it blocks, in the words of Error.
	Reason string
	// Owner is the runtime identity that cannot pass.
	Owner MountOwner
	// DirUID, DirGID and Mode are Dir's owner and permission bits.
	DirUID, DirGID int
	Mode           os.FileMode
}

func (e *TraversalError) Error() string {
	return fmt.Sprintf("%s %s (run_uid=%d run_gid=%d owner_uid=%d owner_gid=%d mode=%04o)", e.Dir, e.Reason, e.Owner.UID, e.Owner.GID, e.DirUID, e.DirGID, e.Mode)
}

// FixCommand is the narrowest repair for an operator directory that blocks
// the runtime: search for others, which opens no listing and no write.
func (e *TraversalError) FixCommand() string {
	return "chmod o+x " + shellQuote(e.Dir)
}

// shellQuote leaves a plain path as it is and single-quotes anything a shell
// would split or expand.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CheckAncestorTraversal reports the first directory from name up to / that
// owner cannot traverse, as a *TraversalError. It never changes anything.
func CheckAncestorTraversal(name string, owner MountOwner) error {
	for current := filepath.Clean(name); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return fmt.Errorf("stat parent %s: %w", current, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent %s is not a directory", current)
		}
		if !OwnerCanAccess(current, info, owner, 0o1) {
			return traversalError(current, info, owner, "is not traversable")
		}
		if next := filepath.Dir(current); next == current {
			return nil
		}
	}
}

// isCallingIdentity reports whether owner is the identity this
// process runs as: the only case where this process's access
// answers for the owner's.
func isCallingIdentity(owner MountOwner) bool {
	return owner.UID == os.Geteuid() && owner.GID == os.Getegid()
}

// OwnerCanAccess reports whether owner can reach path with need (0o4
// read, 0o2 write, 0o1 search/execute). When owner is the calling
// identity the kernel answers by opening the path, capabilities
// included -- the mode bits alone cannot see them, and faccessat's
// userspace fallback answers "root can read and write anything"
// from stat alone on kernels before 5.8 and under seccomp profiles
// that block faccessat2, so a root-squashed NFS mount would pass
// here and fail at container start. Two needs have no honest probe
// and fall back to the mode bits even for the calling identity:
// write on a directory (create and delete inside it) and execute on
// a file (decided by running it) -- no caller asks either today.
// For any other owner the mode bits through the owner's user,
// primary group or other class are the only answer this process can
// give; a switched identity is probed for real by
// checkAccessAsOwner's runuser calls.
func OwnerCanAccess(path string, info os.FileInfo, owner MountOwner, need os.FileMode) bool {
	if !isCallingIdentity(owner) {
		return hasPermission(info, owner, need)
	}
	// Mode bits that allow the calling identity are conclusive -- the
	// kernel grants what the matching class grants -- so the probe only
	// runs where the bits deny and capabilities could still carry it.
	if info != nil && hasPermission(info, owner, need) {
		return true
	}
	if info != nil && info.IsDir() && need&0o2 != 0 {
		// Write on a directory is create and delete inside it: no
		// honest probe without mutating the directory. No caller
		// asks it today; until one does, the mode bits answer.
		return hasPermission(info, owner, need)
	}
	return kernelCanAccess(path, info, need)
}

// kernelCanAccess opens path the way need asks: resolving its "."
// entry for search (reaching path only proved search on its
// ancestors), reading one entry of a directory for read, opening a
// file O_WRONLY when need asks only write or O_RDWR when it asks
// read and write, always O_NONBLOCK so a FIFO cannot hang the probe.
// An error answers false; there is no fallback to simulate. A nil
// info is stat'd here so a missing path answers false instead of
// panicking on the file branch.
func kernelCanAccess(path string, info os.FileInfo, need os.FileMode) bool {
	if info == nil {
		var err error
		info, err = os.Lstat(path)
		if err != nil {
			return false
		}
	}
	if info.IsDir() {
		if need&0o1 != 0 {
			if _, err := os.Lstat(path + "/."); err != nil {
				return false
			}
		}
		if need&0o4 == 0 {
			return true
		}
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		_, err = f.Readdirnames(1)
		f.Close()
		return err == nil || errors.Is(err, io.EOF)
	}
	if need&0o1 != 0 {
		// Execute on a file is decided by running it; no caller
		// asks it here. The mode bits answer until one does.
		return hasPermission(info, MountOwner{UID: os.Geteuid(), GID: os.Getegid()}, need)
	}
	flags := os.O_RDONLY
	if need&0o2 != 0 {
		flags = os.O_WRONLY
		if need&0o4 != 0 {
			flags = os.O_RDWR
		}
	}
	flags |= syscall.O_NONBLOCK
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// hasPermission reports whether owner holds every bit of need (0o4 read,
// 0o2 write, 0o1 search/execute) on info through its user, primary group or
// other class, the way the kernel picks exactly one class. The repair
// sites decide on these bits; verdict sites ask OwnerCanAccess instead.
func hasPermission(info os.FileInfo, owner MountOwner, need os.FileMode) bool {
	uid, gid, ok := FileOwner(info)
	if !ok {
		return false
	}
	mode := info.Mode().Perm()
	var bits os.FileMode
	switch {
	case owner.UID == uid:
		bits = (mode >> 6) & 0o7
	case owner.GID == gid:
		bits = (mode >> 3) & 0o7
	default:
		bits = mode & 0o7
	}
	return bits&need == need
}

// FileOwner returns the uid and gid recorded in info.
func FileOwner(info os.FileInfo) (int, int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}

func traversalError(name string, info os.FileInfo, owner MountOwner, reason string) error {
	uid, gid, _ := FileOwner(info)
	return &TraversalError{Dir: name, Reason: reason, Owner: owner, DirUID: uid, DirGID: gid, Mode: info.Mode().Perm()}
}
