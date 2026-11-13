package controller

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/sourceurl"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// peerSource is one directory on an online node that can serve a peer copy:
// a node-local model file, or a shared storage's model file through that
// node's mount point.
type peerSource struct {
	node      *store.Node
	path      string
	sizeBytes int64
	// rateLimitKiB is the cap of the sync groups the two nodes share, for an
	// automatic copy (0 = none).
	rateLimitKiB int64
}

// trySyncModelFromPeer starts getting a model into a node's local storage
// from wherever it is cheapest: a shared storage mounted on the node, else a
// peer. Any shared storage may serve; see syncModelWithin.
func (c *Controller) trySyncModelFromPeer(ctx context.Context, dstNode *store.Node, model *store.Model, dstMF *store.ModelFile) bool {
	handled, _ := c.syncModelWithin(ctx, dstNode, model, dstMF, nil)
	return handled
}

// syncModelWithin is trySyncModelFromPeer for a deployment limited to some
// storages (store.SourceRules.Pinned): a shared storage outside them is not
// copied from. Nil or empty allows every storage. The error is a download
// the pull source rules forbid, when that is all that was left to try
// (fallbackModelDownload).
func (c *Controller) syncModelWithin(ctx context.Context, dstNode *store.Node, model *store.Model, dstMF *store.ModelFile, pinned []uint) (bool, error) {
	if dstMF == nil {
		return false, nil
	}
	if dstMF.State == proto.MFCanceled {
		return true, nil
	}
	if dstMF.TransferSourceNodeID != 0 && (dstMF.State == proto.MFFailed || dstMF.State == proto.MFDownloadRequired) {
		return true, nil // Only an explicit retry may restart a failed selected-source task.
	}
	// A pending download represents an explicit public choice. Once that
	// attempt has failed, however, a peer is a valid recovery path.
	if dstMF.Acquisition == proto.ModelAcquireDownload && dstMF.State != proto.MFFailed {
		return false, nil
	}
	// A same-node copy is already decided; it only needs (re)pushing.
	if dstMF.Acquisition == proto.ModelAcquireCopy && dstMF.State != proto.MFFailed {
		return false, nil
	}
	if dstMF.State == proto.MFDownloadRequired && dstMF.TransferSourceNodeID == 0 {
		return true, nil
	}
	if dstMF.State == proto.MFFailed && dstMF.Acquisition == proto.ModelAcquirePeer && dstMF.TransferSourceNodeID == 0 {
		return c.fallbackModelDownload(model, dstNode, dstMF)
	}
	key := store.ModelFileKey(dstMF.ID)
	if dstMF.State == proto.MFSyncing {
		c.syncMu.Lock()
		_, active := c.modelSyncs[key]
		c.syncMu.Unlock()
		if active {
			return true, nil
		}
	}
	// Without an explicit source, a shared storage mounted on the target
	// node is the cheapest source of all: a copy on the node itself.
	if dstMF.TransferSourceNodeID == 0 {
		if storageID, ok := c.mountedSharedSource(model.ID, dstNode, pinned); ok {
			mf, err := c.store.PrepareModelCopy(refOf(dstMF), peerSyncTargetPath(dstMF.Path, relModelPath(model)))
			if err == nil {
				if err := c.store.SetModelCopySource(mf.ID, storageID, dstNode.ID); err == nil {
					mf, _ = c.store.ModelFileByID(mf.ID)
					_ = c.pushModelFile(dstNode, mf, model)
					return true, nil
				}
			}
		}
	}
	sources, err := c.peerSources(model.ID, dstNode.ID, dstMF.TransferSourceNodeID)
	if err != nil {
		return true, nil //nolint:nilerr // An unavailable peer lookup leaves this acquisition pending for subsequent reconciliation.
	}
	// Without an explicit source choice a holder of the target's sync groups
	// may serve the copy, and a failed attempt falls back to a public
	// download.
	automatic := dstMF.TransferSourceNodeID == 0
	if automatic {
		sources = c.syncGroupSources(dstNode, sources)
	}
	for _, src := range sources {
		// A holder that shares no storage-sync network with the target cannot
		// serve it; the public download below is the way instead.
		path, err := c.storagePath(src.node, dstNode)
		if err != nil {
			continue
		}
		c.syncMu.Lock()
		if _, active := c.modelSyncs[key]; active {
			c.syncMu.Unlock()
			return true, nil
		}
		transferID := dstMF.TransferID
		if transferID == "" || dstMF.State != proto.MFSyncing || dstMF.TransferErrorCode == messages.ErrorPeerRelayBusy {
			transferID = auth.NewUUID()
		}
		copyCtx, cancel := context.WithCancel(ctx)
		run := &modelSyncRun{transferID: transferID, cancel: cancel, automatic: automatic}
		c.modelSyncs[key] = run
		var beginErr error
		if dstMF.State == proto.MFSyncing && dstMF.TransferErrorCode == messages.ErrorPeerRelayBusy {
			var changed bool
			changed, beginErr = c.store.RestartModelSyncIfActive(refOf(dstMF), dstMF.TransferID, transferID, src.node.ID, src.sizeBytes, dstMF.RateLimit)
			if beginErr == nil && !changed {
				beginErr = errModelSyncOwnerChanged
			}
		} else {
			beginErr = c.store.BeginModelSync(refOf(dstMF), src.node.ID, transferID, src.sizeBytes, copyRateLimit(dstMF.RateLimit, src.rateLimitKiB))
		}
		if beginErr != nil {
			delete(c.modelSyncs, key)
			c.syncMu.Unlock()
			run.cancel()
			return true, nil //nolint:nilerr // Failed peer setup clears the active sync and leaves acquisition for reconciliation.
		}
		c.syncMu.Unlock()
		c.log.Info("starting peer model sync", "model", model.ID, "src", src.node.UUID, "dst", dstNode.UUID, "transfer", transferID)
		go c.runModelSync(copyCtx, key, run, src, dstNode, dstMF, path)
		return true, nil
	}
	if dstMF.TransferSourceNodeID != 0 {
		_ = c.store.SetModelSyncDownloadRequired(refOf(dstMF), dstMF.TransferID, "selected source node is offline, no longer has a complete model, or shares no storage-sync network with this node")
		return true, nil
	}
	return c.fallbackModelDownload(model, dstNode, dstMF)
}

// syncGroupSources keeps the holders the target may copy from automatically:
// nodes it shares a sync group with, whose pair has not failed recently, in
// the order given, each with the groups' rate cap.
func (c *Controller) syncGroupSources(target *store.Node, sources []peerSource) []peerSource {
	if len(sources) == 0 {
		return nil
	}
	now := c.now()
	nodes := make([]store.Node, 0, len(sources))
	for _, src := range sources {
		if c.peerCool.cooling(src.node.ID, target.ID, now) {
			continue
		}
		nodes = append(nodes, *src.node)
	}
	peers, err := planning.SyncPeersFor(c.store, c.networks.Resolve, target, nodes)
	if err != nil {
		c.log.Warn("sync groups unreadable; no automatic copy", "node", target.UUID, "err", err)
		return nil
	}
	rates := make(map[uint]int64, len(peers))
	for _, peer := range peers {
		rates[peer.NodeID] = peer.RateLimitKiB
	}
	out := make([]peerSource, 0, len(peers))
	for _, src := range sources {
		rate, ok := rates[src.node.ID]
		if !ok {
			continue
		}
		src.rateLimitKiB = rate
		out = append(out, src)
	}
	return out
}

// copyRateLimit is the cap a copy runs at: the one recorded on the target's
// row (an operator's own sync) when set, else its sync groups'.
func copyRateLimit(recorded int, group int64) int {
	if recorded > 0 {
		return recorded
	}
	return int(group)
}

// mountedSharedSource finds a shared storage that holds a complete copy of
// the model and is ready on the node, among pinned when any are named.
func (c *Controller) mountedSharedSource(modelID uint, node *store.Node, pinned []uint) (uint, bool) {
	locations, err := c.store.ModelLocations(modelID, node.ID)
	if err != nil {
		return 0, false
	}
	for _, loc := range locations {
		if len(pinned) > 0 && !slices.Contains(pinned, loc.Storage.ID) {
			continue
		}
		if loc.Storage.Type != proto.StorageLocal && loc.Shared.Ready() && loc.NodeState != nil &&
			loc.NodeState.State == proto.StorageStateReady {
			return loc.Storage.ID, true
		}
	}
	return 0, false
}

// peerSources lists the online nodes that can serve a model to dstNodeID. An
// explicitly chosen source is the only candidate: silently falling back to a
// different node would contradict the user's recorded choice, and the
// caller's public-download fallback is the honest failure path instead.
func (c *Controller) peerSources(modelID, dstNodeID, explicitSourceNodeID uint) ([]peerSource, error) {
	files, err := c.store.ModelFilesForModel(modelID)
	if err != nil {
		return nil, err
	}
	var out []peerSource
	seen := map[uint]bool{}
	add := func(nodeID uint, path string, size int64) {
		if nodeID == dstNodeID || seen[nodeID] || path == "" {
			return
		}
		if explicitSourceNodeID != 0 && nodeID != explicitSourceNodeID {
			return
		}
		node, err := c.store.NodeByID(nodeID)
		if err != nil || !c.hub.Online(node.UUID) {
			return
		}
		seen[nodeID] = true
		out = append(out, peerSource{node: node, path: path, sizeBytes: size})
	}
	for _, file := range files {
		if file.State != proto.MFPresent && file.State != proto.MFRegistered {
			continue
		}
		if file.NodeID != 0 {
			storage, err := c.store.StorageByID(file.StorageID)
			if err != nil || storage.Type != proto.StorageLocal {
				continue // node caches are not offered as peers
			}
			add(file.NodeID, file.Path, file.SizeBytes)
			continue
		}
		states, err := c.store.StorageNodeStates(file.StorageID)
		if err != nil {
			continue
		}
		for _, state := range states {
			if state.State != proto.StorageStateReady {
				continue
			}
			if host, ok := store.SharedHostPath(state.Path, file.Path); ok {
				add(state.NodeID, host, file.SizeBytes)
			}
		}
	}
	return out, nil
}

// automaticSyncFallback starts the public download in place of a failed copy
// from a peer the controller chose itself: a peer that shares a storage-sync
// network on paper can still be unreachable in fact (seen between rented GPU
// hosts whose management addresses do not route to each other). It reports
// whether it did; a model with no public source, or one the failed transfer
// no longer owns, is left to the caller's download-required record.
func (c *Controller) automaticSyncFallback(node *store.Node, dstMF *store.ModelFile, transferID string) bool {
	model, err := c.store.GetModel(dstMF.ModelID)
	if err != nil || strings.TrimSpace(model.Source) == "" || strings.TrimSpace(model.SourceID) == "" {
		return false
	}
	current, err := c.store.ModelFileByID(dstMF.ID)
	if err != nil || current.TransferID != transferID || proto.ModelFileStateReady(current.State) {
		return false
	}
	if _, err := planning.ModelWeightsFetch(c.store, c.networks.Resolve, node, model); err != nil {
		c.log.Warn("no public download after failed peer sync", "model", model.ID, "node", node.UUID, "err", err)
		return false
	}
	mf, err := c.store.PrepareModelDownload(refOf(current), peerSyncTargetPath(current.Path, relModelPath(model)))
	if err != nil {
		c.log.Warn("public download after failed peer sync", "model", model.ID, "node", node.UUID, "err", err)
		return false
	}
	if err := c.store.SetDownloadDeployment(mf.ID, mf.TransferID, current.DownloadDeploymentID); err != nil {
		return false
	}
	c.log.Info("peer sync failed, falling back to public download", "model", model.ID, "node", node.UUID)
	_ = c.pushModelFile(node, mf, model)
	return true
}

// fallbackModelDownload turns an unavailable peer acquisition into a public
// download when the model itself has a configured upstream source. Models with
// no source remain explicitly blocked instead of surfacing context cancellation.
// A download the pull source rules forbid on node is not started and its
// planning error returned: the row stays as it is, so a rule change lets the
// next pass start it.
func (c *Controller) fallbackModelDownload(model *store.Model, node *store.Node, current *store.ModelFile) (bool, error) {
	if current != nil && current.State == proto.MFFailed {
		return true, nil
	}
	if strings.TrimSpace(model.Source) != "" && strings.TrimSpace(model.SourceID) != "" {
		if _, err := planning.ModelWeightsFetch(c.store, c.networks.Resolve, node, model); errors.Is(err, sourceurl.ErrNoAllowedSource) {
			return true, err
		}
	}
	ref := store.FileRef{ModelID: model.ID, StorageID: c.store.DefaultLocalStorageID(), NodeID: node.ID}
	if current != nil {
		ref = refOf(current)
	}
	if strings.TrimSpace(model.Source) != "" && strings.TrimSpace(model.SourceID) != "" {
		path := relModelPath(model)
		if current != nil {
			path = peerSyncTargetPath(current.Path, path)
		}
		mf, err := c.store.PrepareModelDownload(ref, path)
		if err == nil {
			if current != nil {
				if err := c.store.SetDownloadDeployment(mf.ID, mf.TransferID, current.DownloadDeploymentID); err != nil {
					return true, err
				}
			}
			return true, c.pushModelFile(node, mf, model)
		}
		_ = c.store.SetModelSyncDownloadRequired(ref, "", "public model download unavailable: "+err.Error())
		return true, nil
	}
	_ = c.store.SetModelSyncDownloadRequired(ref, "", "source_required: no online peer has this model and no public source is configured")
	return true, nil
}

// SyncModelToNode explicitly starts or retries peer acquisition for one model.
func (c *Controller) SyncModelToNode(ctx context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error) {
	return c.syncModelToNode(ctx, modelID, nodeUUID, "", -1)
}

// SyncModelToNodeWithOptions starts a strictly source-selected peer transfer.
// rateLimitKiB is per target transfer; -1 means use the stored retry snapshot.
func (c *Controller) SyncModelToNodeWithOptions(ctx context.Context, modelID uint, nodeUUID, sourceUUID string, rateLimitKiB int) (*store.ModelFile, error) {
	return c.syncModelToNode(ctx, modelID, nodeUUID, sourceUUID, rateLimitKiB)
}

func (c *Controller) ProbeModelSync(ctx context.Context, modelID uint, targetUUID, sourceUUID string) error {
	model, err := c.store.GetModel(modelID)
	if err != nil {
		return err
	}
	target, err := c.store.NodeByUUID(targetUUID)
	if err != nil {
		return err
	}
	source, err := c.store.NodeByUUID(sourceUUID)
	if err != nil {
		return err
	}
	sources, err := c.peerSources(model.ID, target.ID, source.ID)
	if err != nil || len(sources) == 0 {
		return errors.New("selected source node no longer holds a complete model")
	}
	dst, err := c.store.ModelFileByModelNode(model.ID, target.ID)
	if err != nil && errors.Is(err, store.ErrNotFound) {
		dst = &store.ModelFile{Path: relModelPath(model)}
	} else if err != nil {
		return err
	}
	// The path the receiver is asked for is the one a real sync would use: a
	// row left by an earlier registration can hold an absolute path, which the
	// receiver refuses.
	dst.Path = peerSyncTargetPath(dst.Path, relModelPath(model))
	path, err := c.storagePath(source, target)
	if err != nil {
		return err
	}
	prober, ok := c.hub.(interface {
		ProbeModelPath(context.Context, proto.ModelSyncCopyRequest) error
	})
	if !ok {
		return errors.New("model peer probe is not configured")
	}
	return prober.ProbeModelPath(ctx, proto.ModelSyncCopyRequest{TransferID: auth.NewUUID(), Key: "probe-" + auth.NewUUID(), SourceNodeUUID: source.UUID, SourcePath: sources[0].path, TargetNodeUUID: target.UUID, TargetPath: dst.Path, TargetStorageID: dst.StorageID, SourceIP: path.SourceIP, TargetIP: path.TargetIP, ExpectedFileBytes: sources[0].sizeBytes, AllowRelay: network.PeerRelayAllowed(source, target)})
}

func (c *Controller) syncModelToNode(ctx context.Context, modelID uint, nodeUUID, sourceUUID string, rateLimitKiB int) (*store.ModelFile, error) {
	model, err := c.store.GetModel(modelID)
	if err != nil {
		return nil, err
	}
	node, err := c.store.NodeByUUID(nodeUUID)
	if err != nil {
		return nil, err
	}
	if !c.hub.Online(node.UUID) {
		return nil, errors.New("target node is offline")
	}
	mf, err := c.store.ModelFileByModelNode(model.ID, node.ID)
	if err == nil && proto.ModelFileStateReady(mf.State) {
		return mf, nil
	}
	if err == nil && mf.State == proto.MFSyncing {
		return mf, nil
	}
	if err == nil && (mf.State == proto.MFDownloading || (mf.State == proto.MFPending && mf.Acquisition == proto.ModelAcquireDownload)) {
		return nil, errors.New("public model download is active; wait for it to finish before starting peer sync")
	}
	// A retry keeps the source recorded on the failed task. Initial syncs still
	// require an explicit source and are rejected below when no such task exists.
	if strings.TrimSpace(sourceUUID) == "" && err == nil && mf.TransferSourceNodeID != 0 && (mf.State == proto.MFFailed || mf.State == proto.MFDownloadRequired) {
		source, sourceErr := c.store.NodeByID(mf.TransferSourceNodeID)
		if sourceErr != nil {
			return nil, errors.New("failed model sync has no usable source node")
		}
		sourceUUID = source.UUID
		if rateLimitKiB < 0 {
			rateLimitKiB = mf.LastRateLimit
		}
	}
	if strings.TrimSpace(sourceUUID) == "" {
		return nil, errors.New("source node is required for peer model sync")
	}
	path := relModelPath(model)
	// A sync the models page starts without a row lands in the default local
	// storage, which must take new files.
	ref := store.FileRef{ModelID: model.ID, StorageID: c.store.DefaultLocalStorageID(), NodeID: node.ID}
	if err == nil {
		path = peerSyncTargetPath(mf.Path, path)
		ref = refOf(mf)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	} else if storage, storageErr := c.store.StorageByID(ref.StorageID); storageErr == nil && !storage.Enabled {
		return nil, store.ErrStorageDisabled
	}
	mf, err = c.store.PrepareModelPeerSync(ref, path)
	if err != nil {
		return nil, err
	}
	ref = refOf(mf)
	source, sourceErr := c.store.NodeByUUID(sourceUUID)
	if sourceErr != nil {
		return nil, errors.New("selected source node does not exist")
	}
	if source.ID == node.ID {
		return nil, errors.New("source and target nodes must be different")
	}
	if err := c.store.SetModelSyncSource(ref, source.ID); err != nil {
		return nil, err
	}
	if rateLimitKiB >= 0 {
		if err := c.store.SetModelSyncRate(ref, rateLimitKiB); err != nil {
			return nil, err
		}
	}
	mf, _ = c.store.ModelFileAt(ref)
	c.trySyncModelFromPeer(context.WithoutCancel(ctx), node, model, mf)
	return c.store.ModelFileAt(ref)
}

// peerSyncTargetPath converts persisted agent status back into the relative
// desired-state contract. Agents report absolute resolved paths after a public
// download, but a different target node can have a different storage root. A
// peer receiver must therefore never inherit the source/previous node's
// absolute filesystem path.
func peerSyncTargetPath(existing, fallback string) string {
	existing = filepath.Clean(strings.TrimSpace(existing))
	if existing == "." || filepath.IsAbs(existing) || existing == ".." || strings.HasPrefix(existing, ".."+string(filepath.Separator)) {
		return fallback
	}
	return existing
}

// CancelModelSync stops both the local coordinator and the target listener.
func (c *Controller) CancelModelSync(ctx context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error) {
	node, err := c.store.NodeByUUID(nodeUUID)
	if err != nil {
		return nil, err
	}
	c.syncMu.Lock()
	mf, err := c.store.SyncingModelFile(modelID, node.ID)
	if err != nil {
		c.syncMu.Unlock()
		return nil, errors.New("model sync is not active")
	}
	key := store.ModelFileKey(mf.ID)
	run := c.modelSyncs[key]
	if run != nil {
		run.canceled.Store(true)
		run.cancel()
	}
	message := "Peer sync canceled. Retry peer sync or choose public download."
	err = c.store.SetModelSyncDownloadRequiredCode(refOf(mf), mf.TransferID, message, messages.ErrorPeerTransferCanceled)
	c.syncMu.Unlock()
	if err != nil {
		return nil, err
	}
	if stopErr := c.hub.StopModelSync(ctx, node.UUID, key, mf.TransferID); stopErr != nil {
		message = "Peer sync cancellation requested; target stop was not confirmed: " + boundedModelSyncError(stopErr)
		if err := c.store.SetModelSyncDownloadRequiredCode(refOf(mf), mf.TransferID, message, messages.ErrorPeerTransferCanceled); err != nil {
			return nil, err
		}
	}
	return c.store.ModelFileByID(mf.ID)
}

func (c *Controller) runModelSync(ctx context.Context, key string, run *modelSyncRun, src peerSource, dstNode *store.Node, dstMF *store.ModelFile, path netplan.StoragePath) {
	retainUntilTargetStatus := false
	defer func() {
		if !retainUntilTargetStatus {
			c.clearModelSync(key, run)
		}
	}()
	result, err := c.copyModelWithRelayRetry(ctx, key, run, src, dstNode, dstMF, path)
	if err != nil {
		if run.canceled.Load() || errors.Is(err, errModelSyncOwnerChanged) || ctx.Err() != nil {
			// An operator canceled it: CancelModelSync records the outcome, and
			// an automatic copy must not turn the cancel into a public download.
			c.log.Info("peer sync canceled", "model_file", key, "transfer", run.transferID)
			return
		}
		// The target can finish validating and report its terminal state before
		// the source-side RPC response reaches the controller. That terminal
		// report releases this run and cancels ctx; treating the resulting
		// context.Canceled as a transfer failure would regress a verified model
		// back to download_required. The target's persisted terminal state is
		// authoritative, regardless of which peer confirmation wins the race.
		if current, statusErr := c.store.ModelFileByID(dstMF.ID); statusErr == nil &&
			proto.ModelFileStateReady(current.State) {
			c.log.Info("ignoring peer RPC error after target completion", "model_file", key, "transfer", run.transferID, "err", err)
			return
		}
		message := boundedModelSyncError(err)
		c.log.Warn("peer model sync failed", modelSyncDiagnosticAttrs(key, run, src, dstNode, path, err)...)
		if run.automatic {
			c.peerCool.cool(src.node.ID, dstNode.ID, c.now())
		}
		if run.automatic && c.automaticSyncFallback(dstNode, dstMF, run.transferID) {
			return
		}
		_ = c.store.SetModelSyncDownloadRequired(refOf(dstMF), run.transferID, message)
		return
	}
	if result.InProgress {
		if !c.adoptActiveModelSync(key, run, src, dstMF, result.TransferID) {
			return
		}
		c.log.Info("model sync already active on target", "model_file", key, "transfer", result.TransferID)
	} else {
		c.log.Info("peer model sync peer-confirmed", "model_file", key, "transfer", run.transferID, "bytes", result.Bytes, "sha256", result.SHA256, "transport", result.Transport)
		_ = c.hub.SyncNode(dstNode.UUID)
	}
	retainUntilTargetStatus = true
	time.AfterFunc(modelSyncConfirmationTimeout, func() { c.expireModelSyncConfirmation(key, run, dstMF, src, dstNode, path) })
}

func (c *Controller) clearModelSync(key string, run *modelSyncRun) {
	c.syncMu.Lock()
	if c.modelSyncs[key] == run {
		delete(c.modelSyncs, key)
	}
	c.syncMu.Unlock()
	run.cancel()
}

func boundedModelSyncError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 480 {
		message = message[:480]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

// storagePath is the network a copy between two nodes uses; see
// network.Service.StoragePath.
func (c *Controller) storagePath(source, target *store.Node) (netplan.StoragePath, error) {
	path, err := c.networks.StoragePath(source.ID, target.ID)
	if errors.Is(err, netplan.ErrNoStoragePath) {
		return path, messages.NoStoragePath(source.DisplayName(), target.DisplayName())
	}
	return path, err
}
