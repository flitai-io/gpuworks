package runtime

import (
	"context"
	"errors"
	"os"
)

var ErrImageFileNotRegular = errors.New("archive_path must name a regular file, not a directory or device")

// ImageFileChecker checks an archive in the filesystem used by LoadImageFile
// and returns its size. Runtimes sharing the agent's filesystem use os.Stat.
type ImageFileChecker interface {
	CheckImageFile(ctx context.Context, path string) (int64, error)
}

func CheckImageFile(ctx context.Context, loader ImageFileLoader, path string) (int64, error) {
	if checker, ok := loader.(ImageFileChecker); ok {
		return checker.CheckImageFile(ctx, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, ErrImageFileNotRegular
	}
	return info.Size(), nil
}
