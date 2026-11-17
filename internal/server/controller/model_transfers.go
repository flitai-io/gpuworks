package controller

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ModelTransfers are the model file transfers an operator asks for: a peer
// sync to a node, its probe and its cancel, and a copy between storages. They
// run on the controller's transfer machinery, the one the controller's own
// peer syncs use, so an operator's transfer and an automatic one share the
// run table and cannot both own a model file.
type ModelTransfers struct{ c *Controller }

// Transfers is the operator's view of the controller's model transfers.
func (c *Controller) Transfers() ModelTransfers { return ModelTransfers{c: c} }

// Sync starts or retries a peer transfer of the model to the node. An empty
// sourceUUID lets the controller choose the source; rateLimitKiB is per
// target transfer, and -1 keeps the rate the row last recorded.
func (t ModelTransfers) Sync(ctx context.Context, modelID uint, nodeUUID, sourceUUID string, rateLimitKiB int) (*store.ModelFile, error) {
	return t.c.syncModelToNode(ctx, modelID, nodeUUID, sourceUUID, rateLimitKiB)
}

// Probe asks the target node to accept the transfer a peer sync of the model
// from the source would start (same paths and addresses) without copying:
// the source must still hold the whole model.
func (t ModelTransfers) Probe(ctx context.Context, modelID uint, targetUUID, sourceUUID string) error {
	return t.c.probeModelSync(ctx, modelID, targetUUID, sourceUUID)
}

// Cancel stops the model's peer transfer to the node: the coordinator here
// and the listener on the target.
func (t ModelTransfers) Cancel(ctx context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error) {
	return t.c.cancelModelSync(ctx, modelID, nodeUUID)
}

// Copy copies the model's files from one location to another (copyModelFile).
func (t ModelTransfers) Copy(ctx context.Context, modelID uint, src, dst store.FileRef, rateLimitKiB int) (*store.ModelFile, error) {
	return t.c.copyModelFile(ctx, modelID, src, dst, rateLimitKiB)
}
