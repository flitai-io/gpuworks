package hub

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (h *Hub) copyRuntimeImage(ctx context.Context, image *store.Image, node *store.Node, p proto.RuntimeImageSelection) (*store.ImageNode, error) {
	if image.ImageRef != p.Ref {
		return nil, errors.New("runtime selection no longer matches the catalog version")
	}
	id := auth.NewUUID()
	// Preflight verified that the target is missing. Replace a stale present
	// observation, but never dispatch over a live transfer.
	row, started, err := h.beginImageFetch(ctx, &store.ImageNode{ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID, State: store.ImageStateSyncing, TransferID: id, Source: proto.RuntimeImageCopySource}, true)
	if err != nil {
		return nil, err
	}
	if !started {
		// The active attempt may refer to another source namespace or identity.
		return nil, messages.NewProductError(proto.ErrorLocalImageBusy, "another transfer is active", nil)
	}
	_, err = h.CallNode(ctx, node.UUID, proto.RPCImageRuntimeCopy, proto.RuntimeImageCopyRequest{RuntimeImageSelection: p, TransferID: id})
	if err != nil {
		code := proto.ErrorLocalImageUnavailable
		var rpc *proto.RPCError
		if errors.Is(err, proto.ErrRPCUnknownMethod) {
			code = messages.ErrorAgentUpgradeRequired
		} else if errors.As(err, &rpc) && rpc.Code == proto.ErrorLocalImageBusy {
			code = rpc.Code
		}
		_ = h.store.FailImageTransferIfActive(image.ID, node.ID, id, code)
		return nil, messages.NewProductError(code, "the node could not start the image copy", nil)
	}
	h.watchDirectImageTransfer(image.ID, node.ID)
	return row, nil
}
