package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

var modelInstallPermissions = storagemount.PathPermissions

// The caller reports the original failure first. Diagnostics must neither
// replace that error nor hold a writer indefinitely on an unresponsive share.
func logModelInstallPermission(n *NodeRuntime, mf proto.ModelFileTask, target string, cause error) {
	if !errors.Is(cause, os.ErrPermission) {
		return
	}
	parent := filepath.Dir(target)
	attrs := []slog.Attr{
		slog.String("key", mf.Key), slog.String("transfer_id", mf.TransferID),
		slog.Uint64("storage_id", uint64(mf.StorageID)), slog.String("target_parent", parent),
		slog.Any("err", cause),
	}
	var permissions []slog.Attr
	readPermissions := modelInstallPermissions
	err := n.storageDo(mf.StorageID, "permissions/"+parent, n.storageTimeouts().marker, func() error {
		var err error
		permissions, err = readPermissions(parent)
		return err
	})
	if err != nil {
		attrs = append(attrs, slog.String("permissions", "unknown"), slog.Any("permissions_error", err))
	} else {
		attrs = append(attrs, permissions...)
	}
	n.log.LogAttrs(context.Background(), slog.LevelError, "model installation permission denied", attrs...)
}
