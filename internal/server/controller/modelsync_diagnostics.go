package controller

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// modelSyncDiagnosticAttrs uses the addresses selected for this attempt,
// rather than re-resolving networks after a failure or configuration change.
func modelSyncDiagnosticAttrs(key string, run *modelSyncRun, src peerSource, dst *store.Node, path netplan.StoragePath, err error) []any {
	attrs := []any{"model_file", key, "transfer", run.transferID, "observation_scope", "control_plane",
		"source_node", src.node.UUID, "target_node", dst.UUID, "source_ip", path.SourceIP, "target_ip", path.TargetIP}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	return attrs
}

func (c *Controller) expireModelSyncConfirmation(key string, run *modelSyncRun, dstMF *store.ModelFile, src peerSource, dst *store.Node, path netplan.StoragePath) {
	c.syncs.mu.Lock()
	active := c.syncs.byKey[key] == run
	c.syncs.mu.Unlock()
	if !active {
		return
	}
	mf, err := c.store.ModelFileByID(dstMF.ID)
	if err == nil && mf.State == proto.MFSyncing && mf.TransferID == run.transferID {
		c.log.Warn("model sync target confirmation timed out", modelSyncDiagnosticAttrs(key, run, src, dst, path, nil)...)
		_ = c.store.SetModelSyncDownloadRequired(refOf(dstMF), run.transferID,
			"Peer transfer was confirmed, but the target did not report completion. Retry peer verification or choose public download.")
	}
	c.clearModelSync(key, run)
}
