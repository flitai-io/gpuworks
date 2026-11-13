package hub

import (
	"context"
	"errors"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const imageArchiveMaxBytes int64 = 500 << 30

// SyncImage starts a direct source-to-target image transfer. A source is
// mandatory; a relay preserves the selected source and target nodes.
func (h *Hub) SyncImage(ctx context.Context, imageID uint, targetNodeUUID string) (*store.ImageNode, error) {
	return nil, errors.New("source node is required for direct image sync")
}

// SyncImageFrom starts the same relay while honoring the source node selected
// for this deployment.
func (h *Hub) SyncImageFrom(ctx context.Context, imageID uint, targetNodeUUID, sourceNodeUUID string) (*store.ImageNode, error) {
	return h.SyncImageFromRate(ctx, imageID, targetNodeUUID, sourceNodeUUID, 0)
}

func (h *Hub) SyncImageFromRate(ctx context.Context, imageID uint, targetNodeUUID, sourceNodeUUID string, rateLimitKiB int64) (*store.ImageNode, error) {
	return h.syncImageDirect(ctx, imageID, targetNodeUUID, sourceNodeUUID, rateLimitKiB, false)
}

func (h *Hub) syncImageDirect(ctx context.Context, imageID uint, targetNodeUUID, requestedSourceNodeUUID string, rateLimitKiB int64, force bool) (*store.ImageNode, error) {
	requestedSourceNodeUUID = strings.TrimSpace(requestedSourceNodeUUID)
	if requestedSourceNodeUUID == "" {
		return nil, errors.New("source node is required for direct image sync")
	}
	if rateLimitKiB < 0 {
		return nil, errors.New("image sync rate limit must be non-negative")
	}
	image, err := h.store.GetImage(imageID)
	if err != nil {
		return nil, err
	}
	target, err := h.store.NodeByUUID(targetNodeUUID)
	if err != nil {
		return nil, err
	}
	if !h.Online(target.UUID) {
		return nil, errors.New("target node is offline")
	}
	if existing, err := h.store.ImageNode(image.ID, target.ID); err == nil {
		if existing.State == store.ImageStatePresent && !force {
			return existing, nil
		}
		if existing.State == store.ImageStateSyncing && !force && !(existing.TransferErrorCode == messages.ErrorPeerRelayBusy && !h.ownsImageSync(existing.TransferID)) {
			h.watchDirectImageTransfer(image.ID, target.ID)
			sourceNode, sourceErr := h.store.NodeByID(existing.SourceNodeID)
			if sourceErr != nil || sourceNode.UUID != requestedSourceNodeUUID {
				return nil, errors.New("target is already syncing this image from a different source node")
			}
			return existing, nil
		}
		if existing.State == store.ImageStateSyncing && force && (h.ownsImageSync(existing.TransferID) || (existing.TransferErrorCode != messages.ErrorPeerRelayBusy && time.Since(existing.UpdatedAt) < 2*time.Minute)) {
			// A reconnect can race an archive that is still alive. Keep the
			// existing transfer and let its original completion update the row.
			h.watchDirectImageTransfer(image.ID, target.ID)
			return existing, nil
		}
	}
	var source *store.ImageNode
	rows, err := h.store.ImageNodesForImage(image.ID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].NodeID == target.ID || rows[i].State != store.ImageStatePresent {
			continue
		}
		node, nodeErr := h.store.NodeByID(rows[i].NodeID)
		if nodeErr == nil && h.Online(node.UUID) && node.UUID == requestedSourceNodeUUID {
			source = &rows[i]
			break
		}
	}
	if source == nil {
		return nil, errors.New("selected source node is offline or no longer has this image")
	}
	sourceNode, err := h.store.NodeByID(source.NodeID)
	if err != nil {
		return nil, err
	}
	if sourceNode.ID == target.ID {
		return nil, errors.New("source and target nodes must be different")
	}
	if !h.Online(sourceNode.UUID) {
		return nil, errors.New("selected source node is offline")
	}
	transferID := auth.NewUUID()
	sizeBytes := source.SizeBytes
	copyCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.imageSyncMu.Lock()
	// Recheck after source validation: another caller may have started this slot.
	if current, e := h.store.ImageNode(image.ID, target.ID); e == nil && (current.State == store.ImageStateSyncing || current.State == store.ImageStatePulling) && (!force || h.imageSyncOwners[current.TransferID] != nil) && !(current.TransferErrorCode == messages.ErrorPeerRelayBusy && h.imageSyncOwners[current.TransferID] == nil) {
		h.imageSyncMu.Unlock()
		cancel()
		return current, nil
	}
	if h.imageSyncOwners == nil {
		h.imageSyncOwners = make(map[string]context.CancelFunc)
	}
	h.imageSyncOwners[transferID] = cancel
	targetRow, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, State: store.ImageStateSyncing, SizeBytes: sizeBytes, TransferID: transferID, SourceNodeID: source.NodeID, Source: "peer", RateLimitKiB: rateLimitKiB})
	if err == nil {
		err = h.store.BeginDirectImageSync(image.ID, target.ID, source.NodeID, transferID, sizeBytes, rateLimitKiB)
	}
	if err != nil {
		delete(h.imageSyncOwners, transferID)
		cancel()
	}
	h.imageSyncMu.Unlock()
	if err != nil {
		return nil, err
	}
	h.watchDirectImageTransfer(image.ID, target.ID)
	go h.runDirectImageSync(copyCtx, image, target, sourceNode, target.ID, transferID, sizeBytes, rateLimitKiB)
	return targetRow, nil
}

// RetryImageSync resets a failed transfer and starts it again. Docker save is
// not seekable, so retries always restart from byte zero.
func (h *Hub) RetryImageSync(ctx context.Context, imageID uint, targetNodeUUID string) (*store.ImageNode, error) {
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
	if row.SourceNodeID == 0 {
		return nil, errors.New("failed image sync has no source node to retry")
	}
	source, err := h.store.NodeByID(row.SourceNodeID)
	if err != nil {
		return nil, err
	}
	return h.syncImageDirect(ctx, imageID, targetNodeUUID, source.UUID, row.LastRateLimitKiB, true)
}

func (h *Hub) runDirectImageSync(ctx context.Context, image *store.Image, target, source *store.Node, nodeID uint, transferID string, sizeBytes, rateLimitKiB int64) {
	defer func() {
		h.imageSyncMu.Lock()
		cancel := h.imageSyncOwners[transferID]
		delete(h.imageSyncOwners, transferID)
		h.imageSyncMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()
	var result proto.ImageSyncCopyResult
	for {
		path, err := h.storagePath(source, target)
		if err != nil {
			_ = h.store.FailImageSyncIfActive(image.ID, nodeID, transferID, err.Error())
			return
		}
		result, err = h.copyImagePath(ctx, proto.ImageSyncCopyRequest{TransferID: transferID, ImageRef: image.ImageRef, SourceNodeUUID: source.UUID, TargetNodeUUID: target.UUID, SourceIP: path.SourceIP, TargetIP: path.TargetIP, ExpectedBytes: sizeBytes, RateLimitKBs: int(rateLimitKiB), AllowRelay: network.PeerRelayAllowed(source, target)}, func() {
			_, _ = h.store.MarkImageSyncWaiting(image.ID, nodeID, transferID, messages.PeerRelayBusy().Error(), messages.ErrorPeerRelayBusy)
		})
		if !errors.Is(err, messages.ErrPeerRelayBusy) {
			if err != nil {
				_ = h.store.FailImageSyncIfActive(image.ID, nodeID, transferID, err.Error())
				return
			}
			break
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		freshSource, e := h.store.NodeByID(source.ID)
		if e != nil {
			_ = h.store.FailImageSyncIfActive(image.ID, nodeID, transferID, "selected source is unavailable")
			return
		}
		freshTarget, e := h.store.NodeByID(target.ID)
		sourceRow, rowErr := h.store.ImageNode(image.ID, source.ID)
		if e != nil || rowErr != nil || sourceRow.State != store.ImageStatePresent || !h.Online(freshSource.UUID) || !h.Online(freshTarget.UUID) {
			_ = h.store.FailImageSyncIfActive(image.ID, nodeID, transferID, "selected peer is no longer available")
			return
		}
		source, target = freshSource, freshTarget
		nextID := auth.NewUUID()
		h.imageSyncMu.Lock()
		cancel := h.imageSyncOwners[transferID]
		if cancel == nil || ctx.Err() != nil {
			h.imageSyncMu.Unlock()
			return
		}
		changed, e := h.store.RestartImageSyncIfActive(image.ID, nodeID, transferID, nextID, source.ID, sizeBytes, rateLimitKiB)
		if changed {
			delete(h.imageSyncOwners, transferID)
			h.imageSyncOwners[nextID] = cancel
			transferID = nextID
		}
		h.imageSyncMu.Unlock()
		if e != nil || !changed {
			return
		}
		h.watchDirectImageTransfer(image.ID, nodeID)
	}
	if result.InProgress {
		// The target agent already runs another receiver for this image, so
		// nothing will ever report on the transfer id we just wrote. Leaving
		// the row syncing would strand it with no timer to fail it.
		if result.TransferID != transferID {
			_ = h.store.FailImageSyncIfActive(image.ID, nodeID, transferID, "target node already has an active transfer for this image; retry after it finishes")
		}
		return
	}
	h.log.Info("peer image archive transferred", "transfer_id", transferID, "transport", result.Transport)
	if result.Bytes > imageArchiveMaxBytes {
		_ = h.store.FailImageSyncIfActive(image.ID, nodeID, transferID, "image archive exceeded 500 GiB transfer limit")
		return
	}
	time.AfterFunc(2*time.Minute, func() {
		_ = h.store.FailImageSyncIfActive(image.ID, nodeID, transferID, "image archive transferred, but target did not confirm completion")
	})
}

// trackImageTransfer runs one server-relayed transfer in the background and
// owns the whole ledger state machine around it: throttled progress while the
// stream is live, a failure recorded with whatever the copy reported, and the
// guard that refuses to leave a row syncing forever when the target never
// confirms its load. Every relay — peer save/load, offline blob, archive URL —
// goes through here, so the three cannot drift apart.
func (h *Hub) trackImageTransfer(ctx context.Context, imageID, nodeID uint, transferID string, sizeBytes int64, copy func(context.Context, func(int64)) (int64, error)) {
	go func() {
		started := time.Now()
		var progressLimiter imageSyncProgressLimiter
		persistProgress := func(done int64, force bool) {
			elapsed := time.Since(started)
			if !progressLimiter.allow(elapsed, force) {
				return
			}
			seconds := elapsed.Seconds()
			var rate int64
			if seconds > 0 && !force {
				rate = int64(float64(done) / seconds)
			}
			_, _ = h.store.UpdateImageSyncProgressIfActive(imageID, nodeID, transferID, sizeBytes, done, rate)
		}
		copyCtx := context.Background()
		if ctx != nil {
			copyCtx = context.WithoutCancel(ctx)
		}
		bytes, copyErr := copy(copyCtx, func(done int64) { persistProgress(done, false) })
		if copyErr != nil {
			persistProgress(bytes, true)
			_ = h.store.FailImageSyncIfActive(imageID, nodeID, transferID, copyErr.Error())
			return
		}
		// The target agent reports present/failed only after its load returns.
		// Keep the ledger syncing here so io.Copy completion is not mistaken for
		// a successful image import.
		persistProgress(bytes, true)
		time.AfterFunc(2*time.Minute, func() {
			_ = h.store.FailImageSyncIfActive(imageID, nodeID, transferID, "image archive transferred, but target docker load did not confirm completion")
		})
	}()
}
