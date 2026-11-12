package hub

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A node-side transfer — a registry pull, or a download-and-load of an archive
// — has no server-side progress source: the server knows it is alive only
// through the statuses the agent sends. When those stop, nothing else does:
// the row would stay in progress forever with its last reported numbers, the
// operator would see a frozen progress bar instead of a retry button, and the
// next transfer would be refused because the row still looks active.
//
// These are variables rather than constants so a test can shrink them.
var (
	// imageDirectStallCheck is how often a node-side transfer is looked at.
	imageDirectStallCheck = 30 * time.Second
	// imageDirectStallAfter is how much silence makes a transfer abandoned.
	//
	// It is applied to transfers that report continuously while they work — a
	// download-and-load of an archive reports every second. A registry pull is
	// deliberately excluded: docker prints one line per state change, so a
	// large layer can be silent for many minutes while it is downloading
	// perfectly well. Silence from a pull only means "not obviously alive",
	// which is enough to let a retry through, not enough to call it dead.
	imageDirectStallAfter = 15 * time.Minute
)

// watchDirectImageTransfer abandons a node-side transfer that stopped
// reporting, so the row becomes retryable instead of frozen.
func (h *Hub) watchDirectImageTransfer(imageID, nodeID uint) {
	// Snapshot the knobs before the goroutine starts: tests shrink them around
	// this call and restore them in cleanup, and the watcher, which outlives
	// the caller, must not read them after that point.
	checkEvery, silentAfter := imageDirectStallCheck, imageDirectStallAfter
	initial, err := h.store.ImageNode(imageID, nodeID)
	if err != nil || initial.State != store.ImageStateSyncing {
		return
	}
	key, transferID := [2]uint{imageID, nodeID}, initial.TransferID
	h.mu.Lock()
	if _, exists := h.imageWatches[key][transferID]; exists {
		h.mu.Unlock()
		return
	}
	if h.imageWatches == nil {
		h.imageWatches = make(map[[2]uint]map[string]struct{})
	}
	if h.imageWatches[key] == nil {
		h.imageWatches[key] = make(map[string]struct{})
	}
	h.imageWatches[key][transferID] = struct{}{}
	h.mu.Unlock()
	h.log.Info("watching image transfer progress", "image_id", imageID, "node_id", nodeID, "transfer", transferID)
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.imageWatches[key], transferID)
			if len(h.imageWatches[key]) == 0 {
				delete(h.imageWatches, key)
			}
			h.mu.Unlock()
		}()
		ticker := time.NewTicker(checkEvery)
		defer ticker.Stop()
		for range ticker.C {
			row, err := h.store.ImageNode(imageID, nodeID)
			if err != nil {
				return
			}
			if row.State != store.ImageStateSyncing || row.TransferID != transferID {
				return // finished, or something else already moved it on
			}
			if (row.TransferErrorCode == messages.ErrorPeerRelayBusy && h.ownsImageSync(transferID)) || time.Since(row.UpdatedAt) < silentAfter {
				continue
			}
			h.abandonDirectImageTransfer(imageID, nodeID, row)
		}
	}()
}

// imageRowActive reports whether a row still describes work that is plausibly
// in progress. Both node-side entry points ask this before deciding that a
// request is already being served: a row that stopped reporting is the
// leftover of an attempt that ended without saying so, and treating it as
// active would refuse every retry — silently, without ever reaching a node.
func imageRowActive(row *store.ImageNode) bool {
	switch row.State {
	case store.ImageStatePresent:
		return true
	case store.ImageStatePulling, store.ImageStateSyncing:
		return time.Since(row.UpdatedAt) < imageDirectStallAfter
	default:
		return false
	}
}

// abandonDirectImageTransfer decides what a silent transfer turned into. It
// asks the node first, because a transfer that finished while its last report
// was lost should be repaired rather than thrown away. Otherwise it stops the
// operation on the node, so the node's image slot is freed instead of staying
// busy until the runtime's own cap, and fails the row: that is what gives the
// operator a retry.
func (h *Hub) abandonDirectImageTransfer(imageID, nodeID uint, row *store.ImageNode) {
	if image, err := h.store.GetImage(imageID); err == nil {
		if node, nodeErr := h.store.NodeByID(nodeID); nodeErr == nil && h.Online(node.UUID) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if exists, existsErr := h.ImageExists(ctx, image.ImageRef, node.UUID); existsErr == nil && exists {
				_ = h.store.ApplyImageNodeStatus(imageID, nodeID, proto.ImageNodeStatus{
					ImageRef: image.ImageRef, State: store.ImageStatePresent, TransferID: row.TransferID,
					SizeBytes: row.SizeBytes, TransferredBytes: row.SizeBytes,
					Message: "the image was found on the node after this transfer stopped reporting",
				})
				return
			}
		}
	}
	const message = "this transfer stopped reporting progress, so it was cancelled (agent restart, lost connection, or a runtime that never returned). Retry it."
	changed, err := h.store.FailImageNodeIfUnchanged(imageID, nodeID, row.TransferID, row.UpdatedAt, message)
	if err != nil || !changed {
		return
	}
	h.log.Warn("image transfer stopped reporting progress; cancelling attempt", "image_id", imageID, "node_id", nodeID, "source_node_id", row.SourceNodeID, "transfer", row.TransferID, "transferred_bytes", row.TransferredBytes, "last_progress", row.UpdatedAt)
	image, imageErr := h.store.GetImage(imageID)
	node, nodeErr := h.store.NodeByID(nodeID)
	if imageErr == nil && nodeErr == nil {
		h.stopImageTransfer(context.Background(), image, node, row)
	}
}

// WatchImageTransfer restores monitoring for a persisted transfer after a
// server restart. Repeated reconciles share one watcher for the same attempt.
func (h *Hub) WatchImageTransfer(imageID, nodeID uint) {
	row, err := h.store.ImageNode(imageID, nodeID)
	if err == nil && row.State == store.ImageStateSyncing && row.TransferErrorCode == messages.ErrorPeerRelayBusy && !h.ownsImageSync(row.TransferID) {
		source, e := h.store.NodeByID(row.SourceNodeID)
		target, te := h.store.NodeByID(nodeID)
		if e == nil && te == nil {
			_, _ = h.syncImageDirect(context.Background(), imageID, target.UUID, source.UUID, row.RateLimitKiB, false)
		}
	}
	h.watchDirectImageTransfer(imageID, nodeID)
}

func (h *Hub) ownsImageSync(id string) bool {
	h.imageSyncMu.Lock()
	defer h.imageSyncMu.Unlock()
	return h.imageSyncOwners[id] != nil
}
