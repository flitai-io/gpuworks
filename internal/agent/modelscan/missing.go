package modelscan

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// MissingDirectory says the files at path are gone rather than the filesystem
// gone from under them. A mount that is dropped, or did not come up, leaves
// its mount point an empty directory on the root filesystem, and everything
// below it then reads as absent while the files are fine. So path is missing
// only when the nearest directory that does exist still holds something: an
// empty one is what such a mount point looks like. stat picks the directory
// the caller checks paths with, so a test sees the same view of the disk.
func MissingDirectory(stat func(string) (os.FileInfo, error), path string) bool {
	if stat == nil {
		stat = os.Stat
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := stat(dir)
		if err == nil {
			if !info.IsDir() {
				return false
			}
			entries, err := os.ReadDir(dir)
			return err == nil && len(entries) > 0
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false
		}
	}
}
