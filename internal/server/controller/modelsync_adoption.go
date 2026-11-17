package controller

import "gpuworks.ai/gpuworks/internal/server/store"

// adoptActiveModelSync binds an existing target receiver to the run that
// still owns this model-file slot. Event handling observes the same lock, so
// it cannot mix a terminal row from one attempt with another run's identity.
func (c *Controller) adoptActiveModelSync(key string, run *modelSyncRun, src peerSource, dstMF *store.ModelFile, transferID string) bool {
	c.syncs.mu.Lock()
	defer c.syncs.mu.Unlock()
	if c.syncs.byKey[key] != run {
		return false
	}
	if transferID == "" || transferID == run.transferID {
		return true
	}
	if err := c.store.BeginModelSync(refOf(dstMF), src.node.ID, transferID, src.sizeBytes); err != nil {
		c.log.Warn("adopt active target model sync failed", "model_file", key, "transfer", transferID, "err", err)
		return false
	}
	run.transferID = transferID
	return true
}
