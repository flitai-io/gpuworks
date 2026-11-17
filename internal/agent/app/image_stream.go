package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// StreamImage relays an image archive save/load through the server's yamux
// stream. Runtimes without runtime.ImageArchiver are rejected here.
func (n *NodeRuntime) StreamImage(ctx context.Context, imageRef, transferID string, rw io.ReadWriter, load bool) error {
	return n.streamImage(ctx, imageRef, transferID, rw, load, nil)
}

// streamImage is StreamImage with a hook that runs immediately before the
// terminal (present/failed) report is sent. The peer receiver uses it to free
// its per-image slot first: once the server sees a terminal row a retry may
// arrive at once, and it must not find the finished transfer still registered.
func (n *NodeRuntime) streamImage(ctx context.Context, imageRef, transferID string, rw io.ReadWriter, load bool, beforeTerminal func()) error {
	d := n.runtimeDriver()
	archiver, ok := d.(runtime.ImageArchiver)
	if !ok {
		return fmt.Errorf("runtime %s does not support image save/load", n.RuntimeDriverName())
	}
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return fmt.Errorf("image_ref is required")
	}
	if load {
		n.log.Info("loading image archive", "image", imageRef, "transfer", transferID, "driver", n.RuntimeDriverName())
		n.reportImageNode(proto.ImageNodeStatus{ImageRef: imageRef, State: "syncing", TransferID: transferID})
		// A relayed archive was published by someone else, so it can carry a
		// reference other than the catalog one just as a downloaded archive
		// can. Both paths run loadImageArchive, which loads, confirms and tags.
		status, err := loadImageArchive(ctx, d, imageArchiveLoad{
			CanonicalRef: imageRef,
			RuntimeName:  n.RuntimeDriverName(),
			Loaded:       "image archive loaded",
			Load: func(loadCtx context.Context) (string, error) {
				if reporter, ok := archiver.(runtime.StreamedRefReporter); ok {
					return reporter.LoadImageRef(loadCtx, imageRef, rw)
				}
				return "", archiver.LoadImage(loadCtx, imageRef, rw)
			},
		})
		if err != nil {
			n.log.Warn("image archive load failed", "image", imageRef, "transfer", transferID, "err", err)
			if beforeTerminal != nil {
				beforeTerminal()
			}
			n.reportImageNode(proto.ImageNodeStatus{ImageRef: imageRef, State: "failed", TransferID: transferID, Message: imageOperationError(ctx, err)})
			return err
		}
		status.TransferID = transferID
		if beforeTerminal != nil {
			beforeTerminal()
		}
		n.reportImageNode(status)
		n.log.Info("image archive loaded", "image", imageRef, "transfer", transferID, "message", status.Message)
		return nil
	}
	return archiver.SaveImage(ctx, imageRef, rw)
}
