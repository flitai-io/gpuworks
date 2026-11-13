package native

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// errSkipEntry marks a tar entry that carries no filesystem content.
var errSkipEntry = errors.New("skip entry")

// copyBufferSize is a compromise: large enough that the write syscall count
// stays negligible against multi-gigabyte layers, small enough that dozens of
// concurrent layer workers do not add up to a meaningful resident set.
const copyBufferSize = 512 << 10

var openRegularFile = os.OpenFile

// writeEntry materializes one tar entry below root. The caller has already
// claimed the path, so this function owns it for the duration.
//
// Ownership and permissions follow the flags udocker passes to tar:
// --no-same-owner (everything belongs to the unpacking identity, not to the
// uid recorded in the archive) and --no-same-permissions (recorded modes are
// masked by the process umask).
func (u *Unpacker) writeEntry(header *tar.Header, reader io.Reader, root, name string, buf []byte) error {
	target := filepath.Join(root, filepath.FromSlash(name))
	if err := u.ensureParent(root, name); err != nil {
		return err
	}
	mode := u.fileMode(header)

	switch header.Typeflag {
	case tar.TypeReg:
		return u.writeRegular(target, header, reader, mode, buf)
	case tar.TypeSymlink:
		return u.replace(target, func(tmp string) error { return os.Symlink(header.Linkname, tmp) })
	case tar.TypeLink:
		source := filepath.Join(root, filepath.FromSlash(filepath.Clean(header.Linkname)))
		if err := u.replace(target, func(tmp string) error { return os.Link(source, tmp) }); err != nil {
			// The target may belong to a layer still being unpacked; the
			// caller retries these once every layer has finished.
			return &pendingLinkError{source: source, target: target, err: err}
		}
		return u.chown(target)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Device nodes need privileges udocker deliberately never has, and its
		// own create excludes them. Recording them would only fail later.
		return nil
	case tar.TypeXGlobalHeader, tar.TypeXHeader:
		return errSkipEntry
	default:
		return errSkipEntry
	}
}

func (u *Unpacker) writeRegular(target string, header *tar.Header, reader io.Reader, mode os.FileMode, buf []byte) error {
	if err := removeExisting(target); err != nil {
		return err
	}
	// Restricted hosts can reject special bits at creation. Restore them only
	// after writing and chown, which can clear setuid/setgid bits themselves.
	file, err := openRegularFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(file, io.LimitReader(reader, header.Size), buf); err != nil {
		file.Close()
		os.Remove(target)
		return err
	}
	if u.Owner != nil {
		_ = file.Chown(u.Owner.UID, u.Owner.GID)
	}
	// Retain the image's special flags when the host permits them.
	if mode&os.ModeSetuid|mode&os.ModeSetgid|mode&os.ModeSticky != 0 {
		_ = file.Chmod(mode)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if !header.ModTime.IsZero() {
		_ = os.Chtimes(target, header.ModTime, header.ModTime)
	}
	return nil
}

// replace creates a non-regular entry atomically: build it under a temporary
// name in the same directory, then rename it over any existing path. A plain
// remove-then-create would leave the path missing if the process died between
// the two, and symlink() cannot overwrite.
func (u *Unpacker) replace(target string, create func(tmp string) error) error {
	dir := filepath.Dir(target)
	tmp := filepath.Join(dir, fmt.Sprintf(".gpw-%d-%d", os.Getpid(), nextTempID()))
	_ = os.Remove(tmp)
	if err := create(tmp); err != nil {
		return err
	}
	// rename() cannot replace a directory with a non-directory, and images do
	// relocate whole trees this way (a layer turning /lib into a symlink to
	// /usr/lib). tar --overwrite removes the directory first.
	if info, err := os.Lstat(target); err == nil && info.IsDir() {
		if err := os.RemoveAll(target); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if u.Owner != nil {
		_ = os.Lchown(tmp, u.Owner.UID, u.Owner.GID)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ensureParent creates the directories leading to name. Layers routinely omit
// parent entries, and tar creates them implicitly.
func (u *Unpacker) ensureParent(root, name string) error {
	parent := filepath.Dir(filepath.Join(root, filepath.FromSlash(name)))
	if parent == root {
		return nil
	}
	if info, err := os.Lstat(parent); err == nil {
		if info.IsDir() {
			return nil
		}
		// A lower layer left a file (or a symlink) where this layer needs a
		// directory. tar --overwrite replaces it.
		if err := os.RemoveAll(parent); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	u.adoptNewDirs(root, parent)
	return nil
}

// mkdirEntry creates a directory for an explicit tar entry. The final mode is
// deferred: extracting into a directory that the archive marks read-only would
// fail, which is why udocker passes tar --delay-directory-restore.
func (u *Unpacker) mkdirEntry(header *tar.Header, root, name string) error {
	target := filepath.Join(root, filepath.FromSlash(name))
	if info, err := os.Lstat(target); err == nil && !info.IsDir() {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if u.Owner != nil {
		_ = os.Lchown(target, u.Owner.UID, u.Owner.GID)
	}
	u.deferDirMeta(target, u.fileMode(header), header.ModTime)
	return nil
}

// adoptNewDirs hands implicitly created parents to the unpacking identity.
func (u *Unpacker) adoptNewDirs(root, dir string) {
	if u.Owner == nil {
		return
	}
	for dir != root && len(dir) > len(root) {
		if err := os.Lchown(dir, u.Owner.UID, u.Owner.GID); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func (u *Unpacker) chown(target string) error {
	if u.Owner == nil {
		return nil
	}
	_ = os.Lchown(target, u.Owner.UID, u.Owner.GID)
	return nil
}

// fileMode applies the umask the way tar --no-same-permissions does.
func (u *Unpacker) fileMode(header *tar.Header) os.FileMode {
	mode := header.FileInfo().Mode().Perm()
	mode &^= os.FileMode(u.umask)
	return mode | header.FileInfo().Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
}

func removeExisting(target string) error {
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return nil // O_TRUNC handles it without an extra unlink
	}
	return os.RemoveAll(target)
}

type pendingLinkError struct {
	source string
	target string
	err    error
}

func (e *pendingLinkError) Error() string {
	return fmt.Sprintf("link %s -> %s: %v", e.target, e.source, e.err)
}

// dirMeta is a directory's final mode and timestamp, applied once every layer
// has been unpacked.
type dirMeta struct {
	path    string
	mode    os.FileMode
	modTime time.Time
}

func currentUmask() int {
	// Reading the umask requires setting it; restore it immediately. The agent
	// is single-purpose here and this runs once per unpack.
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return mask
}
