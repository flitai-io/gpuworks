package app

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (n *NodeRuntime) StreamModelArchive(ctx context.Context, path string, w io.Writer) error {
	root := n.resolveModelPath(path)
	if _, err := validateModelDir(root); err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		openPath := path
		if entry.Type()&os.ModeSymlink != 0 {
			openPath, err = filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			info, err = os.Stat(openPath)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("model archive does not support symlinked directories: %s", rel)
			}
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("model archive contains unsupported file type: %s", rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		f, err := os.Open(openPath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeErr := tw.Close()
	if walkErr != nil {
		return walkErr
	}
	return closeErr
}

// ReceiveModelArchive installs an archive streamed by a peer at path, relative
// to the default models root (or absolute).
func (n *NodeRuntime) ReceiveModelArchive(ctx context.Context, path string, r io.Reader) error {
	root, err := resolveStorageTarget(n.ModelsRoot(), legacyTaskTarget(path))
	if err != nil {
		return err
	}
	return n.receiveModelArchiveAtomic(ctx, root, r)
}

// receiveModelArchiveAtomic extracts the archive next to root and renames it
// into place. root is the final, absolute directory: the caller resolves it
// against the right storage (see resolveStorageTarget).
func (n *NodeRuntime) receiveModelArchiveAtomic(ctx context.Context, root string, r io.Reader) error {
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(root)+".gpuworks-sync-")
	if err != nil {
		return err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := extractModelArchive(ctx, stage, r); err != nil {
		return err
	}
	// The tar end marker is not the end of the transfer: a sender whose walk
	// fails still closes its tar writer, so a well-formed archive can precede
	// an aborted stream. Only reading the stream to its own end proves the
	// whole archive arrived, and that must hold before root is replaced.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("model archive stream: %w", err)
	}
	if _, err := validateModelDir(stage); err != nil {
		return err
	}
	// MkdirTemp creates the stage 0700 and the rename keeps it: a model
	// directory synced from a peer would then be unreadable to an engine
	// that does not run as root, unlike one downloaded here (found on a
	// multi-node rank whose follower's /model could not be read).
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	// An immutable revision that another transfer finished meanwhile is left
	// alone. Only a completion marker says "finished": a leftover partial
	// directory is non-empty too, and is exactly what this swap replaces.
	if _, err := os.Stat(filepath.Join(root, modelCompleteMarker)); err == nil {
		if _, err := validateModelDir(root); err == nil {
			return nil
		}
	}
	backup := ""
	if _, err := os.Lstat(root); err == nil {
		backup, err = unusedSiblingPath(parent, "."+filepath.Base(root)+".gpuworks-old-")
		if err != nil {
			return err
		}
		if err := os.Rename(root, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, root); err != nil {
		if backup != "" {
			_ = os.Rename(backup, root)
		}
		return err
	}
	keepStage = true
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func extractModelArchive(ctx context.Context, root string, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		dst, err := safeTarPath(root, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("model archive contains unsupported entry type %d: %s", hdr.Typeflag, hdr.Name)
		}
	}
	return nil
}

func unusedSiblingPath(parent, pattern string) (string, error) {
	f, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func safeTarPath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("invalid tar path: %s", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid tar path: %s", name)
	}
	return filepath.Join(root, clean), nil
}
