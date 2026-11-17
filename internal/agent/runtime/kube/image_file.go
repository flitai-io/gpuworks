package kube

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// CheckImageFile follows the same host paths and symlinks as nerdctl load.
func (d *Driver) CheckImageFile(ctx context.Context, path string) (int64, error) {
	out, err := HostExec(ctx, "stat", "-L", "-c", "%f %s", "--", path)
	if err != nil {
		return 0, fmt.Errorf("stat host image archive: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, fmt.Errorf("unexpected host image archive stat output: %q", out)
	}
	mode, modeErr := strconv.ParseUint(fields[0], 16, 32)
	size, sizeErr := strconv.ParseInt(fields[1], 10, 64)
	if modeErr != nil || sizeErr != nil || size < 0 {
		return 0, fmt.Errorf("invalid host image archive stat output: %q", out)
	}
	if mode&syscall.S_IFMT != syscall.S_IFREG {
		return 0, runtime.ErrImageFileNotRegular
	}
	return size, nil
}

var _ runtime.ImageFileChecker = (*Driver)(nil)
