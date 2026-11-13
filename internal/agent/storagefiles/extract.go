package storagefiles

import (
	"archive/tar"
	"archive/zip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// extractor unpacks one archive into a fresh directory. It writes only
// directories and regular files, with names that cannot climb out (no
// absolute path, no "..", no backslash), and keeps the totals inside its
// limits. Since it never creates a link, nothing it creates can redirect a
// later entry, so joining a checked name onto dir is enough.
type extractor struct {
	dir        string
	maxBytes   int64 // < 0: no bound
	maxEntries int

	bytes   int64
	entries int
	files   int
}

func extractFailed(format string, args ...any) error {
	return refuse(proto.StorageFilesErrExtractFailed, format, args...)
}

// fsFailed marks a filesystem error met while writing the extracted tree: the
// archive was fine, the storage was not.
func fsFailed(err error) error {
	return refuse(proto.StorageFilesErrFailed, "%v", err)
}

// dest checks an entry name and returns where it goes; "" means the entry is
// the archive's own root ("./") and is skipped.
func (e *extractor) dest(name string) (string, error) {
	e.entries++
	if e.entries > e.maxEntries {
		return "", extractFailed("the archive has more than %d entries", e.maxEntries)
	}
	switch {
	case name == "":
		return "", extractFailed("an archive entry has no name")
	case strings.ContainsRune(name, 0):
		return "", extractFailed("archive entry %q contains a NUL byte", name)
	case strings.Contains(name, `\`):
		return "", extractFailed("archive entry %q contains a backslash", name)
	case strings.HasPrefix(name, "/"):
		return "", extractFailed("archive entry %q is an absolute path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", extractFailed("archive entry %q climbs out with ..", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	return filepath.Join(e.dir, filepath.FromSlash(clean)), nil
}

func (e *extractor) mkdir(name string) error {
	dst, err := e.dest(name)
	if err != nil || dst == "" {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fsFailed(err)
	}
	return nil
}

// file writes one regular file. declared is the size the archive states
// (-1 unknown); the bytes actually written are what the bound checks.
func (e *extractor) file(name string, mode fs.FileMode, declared int64, r io.Reader) error {
	dst, err := e.dest(name)
	if err != nil {
		return err
	}
	if dst == "" {
		return extractFailed("archive entry %q is a file at the archive root", name)
	}
	left := int64(-1)
	if e.maxBytes >= 0 {
		left = e.maxBytes - e.bytes
		if declared > left {
			return e.tooLarge()
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fsFailed(err)
	}
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return extractFailed("archive entry %q appears twice", name)
		}
		return fsFailed(err)
	}
	src := r
	if left >= 0 {
		src = io.LimitReader(r, left+1)
	}
	// A write error is the storage's; a read error is the archive's or the
	// stream's, which only the caller can tell apart.
	n, copyErr := io.Copy(fsWriter{f}, src)
	closeErr := f.Close()
	e.bytes += n
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return fsFailed(closeErr)
	}
	if left >= 0 && n > left {
		return e.tooLarge()
	}
	e.files++
	return nil
}

func (e *extractor) tooLarge() error {
	return extractFailed("the archive unpacks to more than %d bytes, the free space of the storage less a reserve", e.maxBytes)
}

// extractTar unpacks a tar stream. Only directories and regular files are
// accepted: links, devices and FIFOs fail the whole upload rather than being
// dropped silently.
func (e *extractor) extractTar(ctx context.Context, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = e.mkdir(hdr.Name)
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // old archivers still write TypeRegA
			err = e.file(hdr.Name, fs.FileMode(hdr.Mode), hdr.Size, tr)
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			err = extractFailed("archive entry %q is a link", hdr.Name)
		default:
			err = extractFailed("archive entry %q is not a regular file or directory (type %q)", hdr.Name, hdr.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

// extractZip unpacks a zip file that is already on disk (zip needs random
// access to its central directory).
func (e *extractor) extractZip(ctx context.Context, archive string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return extractFailed("not a zip archive: %v", err)
	}
	defer zr.Close()
	if len(zr.File) > e.maxEntries {
		return extractFailed("the archive has more than %d entries", e.maxEntries)
	}
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		mode := f.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			err = extractFailed("archive entry %q is a link", f.Name)
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			err = e.mkdir(f.Name)
		case mode&fs.ModeType != 0:
			err = extractFailed("archive entry %q is not a regular file or directory", f.Name)
		default:
			err = e.zipFile(f, mode)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *extractor) zipFile(f *zip.File, mode fs.FileMode) error {
	if f.UncompressedSize64 > 1<<62 {
		return e.tooLarge()
	}
	declared := int64(f.UncompressedSize64)
	rc, err := f.Open()
	if err != nil {
		return extractFailed("archive entry %q: %v", f.Name, err)
	}
	defer rc.Close()
	if err := e.file(f.Name, mode, declared, rc); err != nil {
		var fe *proto.StorageFilesError
		if errors.As(err, &fe) {
			return err
		}
		// Reading a zip that is on local disk fails on the archive
		// (checksum, bad compression), not on a stream.
		return extractFailed("archive entry %q: %v", f.Name, err)
	}
	return nil
}

// describeExtract keeps an archive-format error distinct from a filesystem
// or stream one.
func describeExtract(format string, err error) error {
	var fe *proto.StorageFilesError
	if errors.As(err, &fe) {
		return err
	}
	return extractFailed("%s archive: %v", format, err)
}

// fsWriter marks write errors as the storage's.
type fsWriter struct{ w io.Writer }

func (f fsWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, fsFailed(err)
	}
	return n, nil
}
