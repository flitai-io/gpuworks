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
	// ProbeErr is the error the probe returned for the calling identity
	// where the mode bits denied it; nil when nothing was probed.
	ProbeErr error
}

func (e *TraversalError) Error() string {
	verdict := fmt.Sprintf("%s %s (run_uid=%d run_gid=%d owner_uid=%d owner_gid=%d mode=%04o)", e.Dir, e.Reason, e.Owner.UID, e.Owner.GID, e.DirUID, e.DirGID, e.Mode)
	if e.ProbeErr != nil {
		return verdict + ": " + e.ProbeErr.Error()
	}
	return verdict
}

// Unwrap is the probe's error, so a caller can match it with errors.Is.
func (e *TraversalError) Unwrap() error { return e.ProbeErr }

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
		if ok, probeErr := OwnerCanAccess(current, info, owner, 0o1); !ok {
			blocked := traversalError(current, info, owner, "is not traversable")
			// The error the probe returned after the bits denied: nil
			// unless the kernel was asked, which happens for the calling
			// identity alone.
			blocked.ProbeErr = probeErr
			return blocked
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
// read, 0o2 write, 0o1 search/execute). A false answer carries its
// reason: the probe's error where the probe ran and failed -- the
// kernel denying, or the path gone before it was opened -- the stat
// error where info was nil and path could not be stat'd, and nil
// where the mode bits alone decided.
//
// When owner is the calling identity the mode bits grant as before,
// and only where they deny is the path opened for real -- a
// directory's "." entry for search, open plus one readdir for read --
// because opening sees the capabilities the bits cannot: a root agent
// on a 0711 state directory another user owns. Where the bits allow,
// their answer stands and nothing is probed, so a filesystem that
// squashes root, an ACL mask or an LSM can still refuse what the bits
// grant, exactly as before the probe existed; root squash is caught
// only where the bits deny and the squashed identity then fails the
// probe. The probe is not faccessat: called with flags, x/sys/unix
// emulates it from stat alone wherever faccessat2 answers ENOSYS or
// EPERM -- kernels before 5.8, a seccomp profile that blocks it -- and
// can then answer "root can read and write anything" for a
// root-squashed path that an open refuses.
// Two needs have no honest probe and keep the mode-bit verdict for the
// calling identity too: write on a directory (create and delete inside
// it) and execute on a file (decided by running it) -- no caller asks
// either today.
// For any other owner the mode bits through the owner's user,
// primary group or other class are the only answer this process can
// give, and no probe runs: the kernel's answer for this process says
// nothing about a switched identity, which checkAccessAsOwner's
// runuser calls probe for real.
// info describes path; a nil info is replaced by a stat of path,
// symlinks resolved the way the probe itself resolves them.
func OwnerCanAccess(path string, info os.FileInfo, owner MountOwner, need os.FileMode) (bool, error) {
	if info == nil {
		var err error
		if info, err = os.Stat(path); err != nil {
			return false, err
		}
	}
	if !isCallingIdentity(owner) {
		return hasPermission(info, owner, need), nil
	}
	// Bits that allow the calling identity keep the mode-bit verdict;
	// the probe only runs where they deny and capabilities could still
	// carry the access.
	if hasPermission(info, owner, need) {
		return true, nil
	}
	if info.IsDir() && need&0o2 != 0 {
		// Write on a directory is create and delete inside it: no
		// honest probe without mutating the directory. No caller
		// asks it today; until one does, the mode bits answer.
		return false, nil
	}
	return kernelCanAccess(path, info, need)
}

// kernelCanAccess opens path the way need asks: resolving its "."
// entry for search (reaching path only proved search on its
// ancestors), reading one entry of a directory for read, opening a
// file O_WRONLY when need asks only write or O_RDWR when it asks
// read and write, always O_NONBLOCK so a FIFO cannot hang the probe.
// An error answers false with it; there is no fallback to simulate.
// info is path's own and never nil: OwnerCanAccess fills it in.
func kernelCanAccess(path string, info os.FileInfo, need os.FileMode) (bool, error) {
	if info.IsDir() {
		if need&0o1 != 0 {
			if _, err := os.Lstat(path + "/."); err != nil {
				return false, err
			}
		}
		if need&0o4 == 0 {
			return true, nil
		}
		f, err := os.Open(path)
		if err != nil {
			return false, err
		}
		_, err = f.Readdirnames(1)
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		return true, nil
	}
	if need&0o1 != 0 {
		// Execute on a file is decided by running it; no caller asks
		// it here. The mode bits answered above and denied.
		return false, nil
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
		return false, err
	}
	f.Close()
	return true, nil
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

func traversalError(name string, info os.FileInfo, owner MountOwner, reason string) *TraversalError {
	uid, gid, _ := FileOwner(info)
	return &TraversalError{Dir: name, Reason: reason, Owner: owner, DirUID: uid, DirGID: gid, Mode: info.Mode().Perm()}
}
