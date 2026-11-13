package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// uninstallSnapshot avoids even SQLite's WAL read-mark writes to the installed
// database's shared-memory sidecar. SQLite may recover/update only the private
// copy. Compare both sources again after copying: a changing database is unknown,
// never an empty or stale inventory. No schema or journal settings are changed.
const maxUninstallSnapshotFileBytes = 256 << 20

func uninstallSnapshot(ctx context.Context, source string) (path string, cleanup func(), err error) {
	if err := checkUninstallRollbackJournal(source); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "gpuworks-uninstall-inventory-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "inventory.db")
	var fingerprints [2]string
	for idx, suffix := range []string{"", "-wal"} {
		fingerprints[idx], err = copyUninstallInventoryFile(ctx, source+suffix, path+suffix)
		if err != nil {
			cleanup()
			return "", nil, err
		}
	}
	for idx, suffix := range []string{"", "-wal"} {
		current, readErr := copyUninstallInventoryFile(ctx, source+suffix, "")
		if readErr != nil || current != fingerprints[idx] {
			cleanup()
			return "", nil, errors.New("database changed while collecting uninstall inventory")
		}
	}
	if err := checkUninstallRollbackJournal(source); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func copyUninstallInventoryFile(ctx context.Context, source, destination string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	input, err := os.Open(source)
	if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(source, "-wal") {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > maxUninstallSnapshotFileBytes {
		return "", errors.New("inventory source exceeds snapshot size budget")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("inventory source is not a regular file")
	}
	digest := sha256.New()
	var output *os.File
	var writer io.Writer = digest
	if destination != "" {
		output, err = os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return "", err
		}
		writer = io.MultiWriter(output, digest)
	}
	copyErr := copyUninstallSnapshot(ctx, writer, input)
	if output != nil {
		copyErr = errors.Join(copyErr, output.Close())
	}
	if copyErr != nil {
		return "", copyErr
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%s", info.Size(), hex.EncodeToString(digest.Sum(nil))), nil
}

// Check cancellation between bounded reads; a growing file cannot exceed the
// snapshot budget even when it grows after the initial stat.
func copyUninstallSnapshot(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		total += int64(count)
		if total > maxUninstallSnapshotFileBytes {
			return errors.New("inventory source grew beyond snapshot size budget")
		}
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			if writeErr != nil {
				return writeErr
			}
			if written != count {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// A rollback-journal transaction can spill uncommitted pages into the main
// database. A copied main file without its hot journal is not a safe snapshot.
func checkUninstallRollbackJournal(source string) error {
	if _, err := os.Lstat(source + "-journal"); !errors.Is(err, os.ErrNotExist) {
		return errors.New("rollback journal prevents a consistent uninstall inventory")
	}
	return nil
}
