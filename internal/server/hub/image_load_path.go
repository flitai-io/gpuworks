package hub

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (h *Hub) loadImageArchivePath(ctx context.Context, image *store.Image, target *store.Node, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
	path := strings.TrimSpace(fetch.ArchivePath)
	if !filepath.IsAbs(path) {
		return nil, errors.New("archive_path must be an absolute file path on the target node")
	}
	if row, err := h.store.ImageNode(image.ID, target.ID); err == nil && imageRowActive(row) {
		return row, nil
	}
	transferID := auth.NewUUID()
	row, err := h.store.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID,
		State: store.ImageStateSyncing, Source: imagecatalog.FetchSourceLocal,
		TransferID: transferID, Message: "loading image archive from node file",
	})
	if err != nil {
		return nil, err
	}
	if _, err := h.CallNode(ctx, target.UUID, proto.RPCImageLoadPath, proto.ImageLoadPathParams{
		CanonicalRef: image.ImageRef, ArchivePath: path, TransferID: transferID,
	}); err != nil {
		message := "node image archive could not be loaded: " + err.Error()
		if strings.Contains(strings.ToLower(err.Error()), "unknown rpc method: "+proto.RPCImageLoadPath) {
			message = "the target node is running an older agent that cannot import a local image archive; upgrade the agent first"
		}
		_, _ = h.store.UpsertImageNode(&store.ImageNode{
			ID: row.ID, ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID,
			State: store.ImageStateFailed, Source: imagecatalog.FetchSourceLocal,
			TransferID: transferID, Message: boundedPullMessage(message),
		})
		return nil, errors.New(message)
	}
	h.watchDirectImageTransfer(image.ID, target.ID)
	return row, nil
}
