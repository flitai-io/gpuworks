package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
)

// modelDownloadSafetyMargin leaves room for metadata, temporary files and
// filesystem accounting overhead. This is deliberately a fixed, small guard;
// downloads do not reserve capacity and concurrent transfers may still race.
const modelDownloadSafetyMargin int64 = 1 << 30

// errInsufficientDisk is the admission check refusing a model transfer the
// target filesystem cannot hold; the status report reads it as
// MFInsufficientDisk.
var errInsufficientDisk = errors.New("insufficient disk space")

// checkDownloadSpace is checkModelDownloadSpace for a path on a storage, which
// may be a hung network mount: the check runs under the storage's guard and a
// share that does not answer is reported as such instead of stalling the task.
func (n *NodeRuntime) checkDownloadSpace(storageID uint, target string, expectedBytes int64) error {
	err := n.storageDo(storageID, "space/"+target, n.storageTimeouts().space, func() error {
		return checkModelDownloadSpace(target, expectedBytes)
	})
	if errors.Is(err, storagemount.ErrUnresponsive) {
		return fmt.Errorf("the storage did not answer while checking free space at %s", target)
	}
	return err
}

func checkModelDownloadSpace(target string, expectedBytes int64) error {
	if expectedBytes < 0 {
		return fmt.Errorf("invalid expected download size %d", expectedBytes)
	}
	probe := target
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat model target %s: %w", target, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("model target filesystem is unavailable: %s", target)
		}
		probe = parent
	}
	usage := statManagedPath("models", probe)
	if usage.AvailableBytes <= 0 {
		return fmt.Errorf("%w at %s: available=%d required=%d", errInsufficientDisk, probe, usage.AvailableBytes, expectedBytes+modelDownloadSafetyMargin)
	}
	required := expectedBytes + modelDownloadSafetyMargin
	if required < expectedBytes || usage.AvailableBytes < required {
		return fmt.Errorf("%w at %s: available=%d required=%d (download=%d safety_margin=%d)", errInsufficientDisk, probe, usage.AvailableBytes, required, expectedBytes, modelDownloadSafetyMargin)
	}
	return nil
}

// modelDiskFull reports a model transfer that failed for lack of disk: the
// admission check refused it (errInsufficientDisk) or a write hit ENOSPC or
// the quota.
func modelDiskFull(err error) bool {
	return errors.Is(err, errInsufficientDisk) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
