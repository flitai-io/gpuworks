package hub

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// CancelImageTransfer stops a node-side image transfer.
//
// The agent ends the runtime call it is running (the docker CLI is killed
// through its context), and the row is failed here as well: the node may have
// finished already, or lost the operation with its last report, and in every
// case the operator who asked for a stop has to end up with a row that can be
// retried. Cancelling is therefore both an RPC and a state change, best effort
// on the first and definite on the second.
func (h *Hub) CancelImageTransfer(ctx context.Context, imageID uint, targetNodeUUID string) (*store.ImageNode, error) {
	image, err := h.store.GetImage(imageID)
	if err != nil {
		return nil, err
	}
	target, err := h.store.NodeByUUID(targetNodeUUID)
	if err != nil {
		return nil, err
	}
	row, err := h.store.ImageNode(image.ID, target.ID)
	if err != nil {
		return nil, err
	}
	return h.cancelImageTransferRow(ctx, image, target, row, "the transfer was cancelled")
}

// cancelImageTransferRow is the shared half: stop the runtime call on the node,
// then fail the row with the caller's reason.
func (h *Hub) cancelImageTransferRow(ctx context.Context, image *store.Image, target *store.Node, row *store.ImageNode, message string) (*store.ImageNode, error) {
	if row.State != store.ImageStatePulling && row.State != store.ImageStateSyncing {
		return row, nil // nothing is running; there is nothing to stop
	}
	h.imageSyncMu.Lock()
	current, err := h.store.ImageNode(image.ID, target.ID)
	if err != nil {
		h.imageSyncMu.Unlock()
		return nil, err
	}
	if current.TransferID != row.TransferID {
		h.imageSyncMu.Unlock()
		return current, nil
	}
	if cancel := h.imageSyncOwners[row.TransferID]; cancel != nil {
		cancel()
		delete(h.imageSyncOwners, row.TransferID)
	}
	// A peer receiver reports the load the stop kills; the code keeps that
	// report from replacing the reason. A pull or URL fetch has no such code:
	// the node reports its own cancellation for those.
	code := ""
	if row.Source == "peer" {
		code = messages.ErrorPeerTransferCanceled
	}
	_, failErr := h.store.CancelImageNodeTransferIfActive(image.ID, target.ID, row.TransferID, message, code)
	h.imageSyncMu.Unlock()
	if failErr != nil {
		return nil, failErr
	}
	h.CancelPeerTransfer(row.TransferID)
	h.stopImageTransfer(ctx, image, target, row)
	return h.store.ImageNode(image.ID, target.ID)
}

// stopImageTransfer addresses both ends of a peer copy by attempt id. Closing
// only the receiver cannot interrupt a source runtime stuck before its next write.
func (h *Hub) stopImageTransfer(ctx context.Context, image *store.Image, target *store.Node, row *store.ImageNode) {
	if h.Online(target.UUID) {
		// The id names the attempt: the same image can have a stuck transfer and
		// a queued retry at the same time.
		method := proto.RPCImageCancel
		params := any(proto.ImageCancelParams{CanonicalRef: image.ImageRef, TransferID: row.TransferID})
		if row.Source == "peer" {
			// A peer transfer is owned by the direct receiver, not by the
			// runtime image operation registry. Stop that receiver explicitly so
			// its Receive context (and the docker load it is running) is cancelled.
			method = proto.RPCImageSyncStop
			params = proto.ImageSyncStopParams{TransferID: row.TransferID}
		}
		_, callErr := h.CallNode(ctx, target.UUID, method, params)
		if callErr != nil {
			h.log.Warn("image cancellation RPC failed",
				"image", image.ImageRef, "node", target.UUID, "err", callErr)
		}
	}
	if row.Source == "peer" && row.SourceNodeID != 0 && row.TransferID != "" {
		if source, err := h.store.NodeByID(row.SourceNodeID); err == nil && h.Online(source.UUID) {
			if _, err := h.CallNode(ctx, source.UUID, proto.RPCImageSyncStop, proto.ImageSyncStopParams{TransferID: row.TransferID}); err != nil {
				h.log.Warn("image source cancellation failed", "image", image.ImageRef, "node", source.UUID, "transfer", row.TransferID, "err", err)
			}
		}
	}
}
