package hub

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// beginImageFetch keeps the acquisition policy here while the store atomically
// claims the observed row. Force only bypasses a finished, present image.
func (h *Hub) beginImageFetch(ctx context.Context, next *store.ImageNode, force bool) (*store.ImageNode, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		current, err := h.store.ImageNode(next.ImageID, next.NodeID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, false, err
		}
		if errors.Is(err, store.ErrNotFound) {
			current = nil
		}
		if current != nil && imageRowActive(current) && !(force && current.State == store.ImageStatePresent) {
			return current, false, nil
		}
		row, started, err := h.store.TryStartImageTransfer(ctx, current, next)
		if err != nil || started {
			return row, started, err
		}
		// Someone changed the row after our read. Reapply the policy to that
		// new state; a failed comparison is never permission to send the RPC.
	}
}

// ownImagePullFallback lets an explicit cancel stop a legacy agent's candidate
// watcher as well as the node operation. The existing mutex protects this
// in-memory cancel registry only; admission is a conditional database write.
func (h *Hub) ownImagePullFallback(ctx context.Context, transferID string) (context.Context, func()) {
	pullCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.imageSyncMu.Lock()
	if h.imageSyncOwners == nil {
		h.imageSyncOwners = make(map[string]context.CancelFunc)
	}
	h.imageSyncOwners[transferID] = cancel
	h.imageSyncMu.Unlock()
	return pullCtx, func() {
		cancel()
		h.imageSyncMu.Lock()
		delete(h.imageSyncOwners, transferID)
		h.imageSyncMu.Unlock()
	}
}

func imagePullAttemptCurrent(row *store.ImageNode, transferID string) bool {
	return row.TransferID == transferID || (row.TransferID == "" && row.State == store.ImageStateFailed && row.LastTransferID == transferID)
}
