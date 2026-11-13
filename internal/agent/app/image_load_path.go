package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (n *NodeRuntime) rpcImageLoadPath(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImageLoadPathParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	p.CanonicalRef = strings.TrimSpace(p.CanonicalRef)
	p.ArchivePath = strings.TrimSpace(p.ArchivePath)
	if p.CanonicalRef == "" || strings.Contains(p.CanonicalRef, "@sha256:") {
		return nil, errors.New("image load requires a tagged canonical_ref")
	}
	if !filepath.IsAbs(p.ArchivePath) {
		return nil, errors.New("archive_path must be an absolute file path on the target node")
	}
	info, err := os.Stat(p.ArchivePath)
	if err != nil {
		return nil, fmt.Errorf("read node image archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("archive_path must name a regular file, not a directory or device")
	}
	d := n.runtimeDriver()
	loader, ok := d.(runtime.ImageFileLoader)
	if !ok {
		return nil, fmt.Errorf("runtime %s cannot load an image archive from a file", n.RuntimeDriverName())
	}
	loadCtx := context.Background()
	if ctx != nil {
		loadCtx = context.WithoutCancel(ctx)
	}
	loadCtx, cancel := context.WithTimeout(loadCtx, imageOperationTimeout)
	unregister := n.beginImageOperation(p.CanonicalRef, p.TransferID, cancel)
	go func() {
		defer cancel()
		defer unregister()
		// Runtime loaders can be silent for longer than the server's transfer
		// watchdog. Keep this attempt alive through queueing and layer expansion.
		stopHeartbeat := make(chan struct{})
		heartbeatDone := make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopHeartbeat:
					return
				case <-loadCtx.Done():
					return
				case <-ticker.C:
					n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "syncing", TransferID: p.TransferID, SizeBytes: info.Size()})
				}
			}
		}()
		n.lockImageOperation(p.CanonicalRef, "syncing")
		defer n.imagePullMu.Unlock()
		n.reportImageNode(proto.ImageNodeStatus{
			ImageRef: p.CanonicalRef, State: "syncing", TransferID: p.TransferID,
			SizeBytes: info.Size(), Message: "loading image archive from node file",
		})
		status, err := loadImageArchive(loadCtx, d, imageArchiveLoad{
			CanonicalRef: p.CanonicalRef,
			RuntimeName:  n.RuntimeDriverName(),
			Loaded:       "image archive loaded from node file",
			Load: func(ctx context.Context) (string, error) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if reporter, ok := loader.(runtime.LoadedRefReporter); ok {
					return reporter.LoadImageFileRef(ctx, p.CanonicalRef, p.ArchivePath)
				}
				return "", loader.LoadImageFile(ctx, p.CanonicalRef, p.ArchivePath)
			},
		})
		if err != nil {
			status = proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "failed", Message: imageOperationError(loadCtx, err)}
		} else {
			status.TransferredBytes = info.Size()
		}
		status.TransferID = p.TransferID
		status.SizeBytes = info.Size()
		close(stopHeartbeat)
		<-heartbeatDone
		n.reportImageNode(status)
	}()
	return proto.ImageLoadPathResult{State: "syncing", CanonicalRef: p.CanonicalRef}, nil
}
