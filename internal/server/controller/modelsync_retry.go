package controller

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var errModelSyncOwnerChanged = errors.New("model sync attempt is no longer owned")

// Capacity is temporary. Keep the selected source and explicit rate while each
// retry obtains fresh receiver credentials; it must not poison the peer pair.
func (c *Controller) copyModelWithRelayRetry(ctx context.Context, key string, run *modelSyncRun, src peerSource, target *store.Node, file *store.ModelFile, path netplan.StoragePath) (proto.ModelSyncCopyResult, error) {
	rate := copyRateLimit(file.RateLimit, src.rateLimitKiB)
	for {
		result, err := c.hub.CopyModelPath(ctx, proto.ModelSyncCopyRequest{
			TransferID: run.transferID, Key: key, SourceNodeUUID: src.node.UUID, SourcePath: src.path,
			TargetNodeUUID: target.UUID, TargetPath: file.Path, TargetStorageID: file.StorageID,
			SourceIP: path.SourceIP, TargetIP: path.TargetIP, ExpectedFileBytes: src.sizeBytes,
			RateLimitKBs: rate, AllowRelay: network.PeerRelayAllowed(src.node, target),
		})
		if !errors.Is(err, messages.ErrPeerRelayBusy) {
			return result, err
		}
		changed, markErr := c.store.MarkModelSyncWaiting(refOf(file), run.transferID, messages.PeerRelayBusy().Error(), messages.ErrorPeerRelayBusy)
		if markErr != nil {
			return result, markErr
		}
		if !changed {
			return result, errModelSyncOwnerChanged
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
		sources, checkErr := c.peerSources(file.ModelID, target.ID, src.node.ID)
		if checkErr != nil {
			return result, checkErr
		}
		if len(sources) == 0 || !c.hub.Online(target.UUID) {
			return result, errors.New("selected peer is no longer available")
		}
		freshTarget, checkErr := c.store.NodeByID(target.ID)
		if checkErr != nil {
			return result, checkErr
		}
		src.node, src.path, src.sizeBytes = sources[0].node, sources[0].path, sources[0].sizeBytes
		target = freshTarget
		path, checkErr = c.storagePath(src.node, target)
		if checkErr != nil {
			return result, checkErr
		}
		nextID := auth.NewUUID()
		c.syncMu.Lock()
		if c.modelSyncs[key] != run || run.canceled.Load() || ctx.Err() != nil {
			c.syncMu.Unlock()
			return result, errModelSyncOwnerChanged
		}
		changed, checkErr = c.store.RestartModelSyncIfActive(refOf(file), run.transferID, nextID, src.node.ID, src.sizeBytes, rate)
		if changed {
			run.transferID = nextID
		}
		c.syncMu.Unlock()
		if checkErr != nil {
			return result, checkErr
		}
		if !changed {
			return result, errModelSyncOwnerChanged
		}
	}
}
