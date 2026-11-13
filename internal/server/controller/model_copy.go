package controller

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// CopyModelFile copies a model's files from one location to another. Both
// ends are a storage plus, for a local storage, a node. The agent has two
// primitives and this picks one: a copy on one node when a single node can
// read the source and write the target (a local directory or a mounted NFS
// on that node), and the node-to-node direct transfer otherwise. A write into
// a shared storage is executed by one node, the writer.
func (c *Controller) CopyModelFile(ctx context.Context, modelID uint, src, dst store.FileRef, rateLimitKiB int) (*store.ModelFile, error) {
	model, err := c.store.GetModel(modelID)
	if err != nil {
		return nil, err
	}
	srcStorage, err := c.store.StorageByID(src.StorageID)
	if err != nil {
		return nil, fmt.Errorf("source storage: %w", err)
	}
	dstStorage, err := c.store.StorageByID(dst.StorageID)
	if err != nil {
		return nil, fmt.Errorf("target storage: %w", err)
	}
	if err := c.store.StorageAcceptsWrites(dstStorage); err != nil {
		return nil, err
	}
	if err := c.checkCopySource(model, srcStorage, src); err != nil {
		return nil, err
	}
	if src.StorageID == dst.StorageID && (proto.StorageShared(srcStorage.Type) || src.NodeID == dst.NodeID) {
		return nil, errors.New("source and target are the same location")
	}
	if proto.StorageShared(dstStorage.Type) {
		return c.copyIntoShared(ctx, model, srcStorage, src, dstStorage, rateLimitKiB)
	}
	return c.copyIntoLocal(ctx, model, srcStorage, src, dstStorage, dst, rateLimitKiB)
}

func (c *Controller) checkCopySource(model *store.Model, storage *store.Storage, src store.FileRef) error {
	if proto.StorageShared(storage.Type) {
		row, err := c.store.ModelFileAt(store.FileRef{ModelID: model.ID, StorageID: storage.ID})
		if err != nil || row.State != proto.MFPresent {
			return errors.New("the source storage does not hold a complete copy of the model")
		}
		return nil
	}
	if src.NodeID == 0 {
		return errors.New("a local source storage needs a source node")
	}
	row, err := c.store.ModelFileAt(store.FileRef{ModelID: model.ID, StorageID: storage.ID, NodeID: src.NodeID})
	if err != nil || (row.State != proto.MFPresent && row.State != proto.MFRegistered) {
		return errors.New("the source node does not hold a complete copy of the model")
	}
	return nil
}

// readableOn reports whether a node can read the source location directly.
func (c *Controller) readableOn(storage *store.Storage, src store.FileRef, nodeID uint) bool {
	if !proto.StorageShared(storage.Type) {
		return src.NodeID == nodeID
	}
	state, err := c.store.StorageNodeStateFor(storage.ID, nodeID)
	return err == nil && state.State == proto.StorageStateReady
}

func (c *Controller) copyIntoLocal(ctx context.Context, model *store.Model, srcStorage *store.Storage, src store.FileRef, dstStorage *store.Storage, dst store.FileRef, rateLimitKiB int) (*store.ModelFile, error) {
	if dst.NodeID == 0 {
		return nil, errors.New("a local target storage needs a target node")
	}
	node, err := c.store.NodeByID(dst.NodeID)
	if err != nil {
		return nil, err
	}
	if _, applies, err := c.store.EffectiveStorageForNode(dstStorage, node); err != nil || !applies {
		return nil, errors.New("the target storage does not apply to the target node")
	}
	if !c.hub.Online(node.UUID) {
		return nil, errors.New("target node is offline")
	}
	ref := store.FileRef{ModelID: model.ID, StorageID: dstStorage.ID, NodeID: node.ID}
	if current, err := c.store.ModelFileAt(ref); err == nil && proto.ModelFileStateReady(current.State) {
		return current, nil
	}
	if c.readableOn(srcStorage, src, node.ID) {
		row, err := c.store.PrepareModelCopy(ref, relModelPath(model))
		if err != nil {
			return nil, err
		}
		if err := c.store.SetModelCopySource(row.ID, srcStorage.ID, node.ID); err != nil {
			return nil, err
		}
		if rateLimitKiB >= 0 {
			if err := c.store.SetModelCopyRate(ref, rateLimitKiB); err != nil {
				return nil, err
			}
		}
		row, _ = c.store.ModelFileByID(row.ID)
		_ = c.pushModelFile(node, row, model)
		return row, nil
	}
	sourceNode := src.NodeID
	if proto.StorageShared(srcStorage.Type) {
		ready, err := c.store.ReadyStorageNodes(srcStorage.ID, false)
		if err != nil {
			return nil, err
		}
		sourceNode = c.firstOnline(ready, 0)
		if sourceNode == 0 {
			return nil, errors.New("no online node can read the source storage")
		}
	}
	_, err = c.store.PrepareModelPeerSync(ref, relModelPath(model))
	if err != nil {
		return nil, err
	}
	if err := c.store.SetModelSyncSource(ref, sourceNode); err != nil {
		return nil, err
	}
	if rateLimitKiB >= 0 {
		if err := c.store.SetModelSyncRate(ref, rateLimitKiB); err != nil {
			return nil, err
		}
	}
	row, _ := c.store.ModelFileAt(ref)
	c.trySyncModelFromPeer(context.WithoutCancel(ctx), node, model, row)
	return c.store.ModelFileAt(ref)
}

// copyIntoShared writes a model into a shared storage through one writer
// node: a node that can read the source and write the target copies on its
// own disk; otherwise a writer receives it from a node that can read the
// source.
func (c *Controller) copyIntoShared(ctx context.Context, model *store.Model, srcStorage *store.Storage, src store.FileRef, dstStorage *store.Storage, rateLimitKiB int) (*store.ModelFile, error) {
	ref := store.FileRef{ModelID: model.ID, StorageID: dstStorage.ID}
	if current, err := c.store.ModelFileAt(ref); err == nil && current.State == proto.MFPresent {
		return current, nil
	}
	writers, err := c.store.ReadyStorageNodes(dstStorage.ID, true)
	if err != nil {
		return nil, err
	}
	rel := store.SharedModelRelPath(model)
	for _, writer := range writers {
		node, err := c.store.NodeByID(writer)
		if err != nil || !c.hub.Online(node.UUID) || !c.readableOn(srcStorage, src, writer) {
			continue
		}
		row, err := c.store.PrepareModelCopy(ref, rel)
		if err != nil {
			return nil, err
		}
		if err := c.store.SetModelCopySource(row.ID, srcStorage.ID, writer); err != nil {
			return nil, err
		}
		if err := c.store.SetModelFileWriter(row.ID, writer); err != nil {
			return nil, err
		}
		if rateLimitKiB >= 0 {
			if err := c.store.SetModelCopyRate(ref, rateLimitKiB); err != nil {
				return nil, err
			}
		}
		row, _ = c.store.ModelFileByID(row.ID)
		_ = c.pushModelFile(node, row, model)
		return row, nil
	}
	writer := c.firstOnline(writers, 0)
	if writer == 0 {
		return nil, errors.New("no online node can write to the target storage")
	}
	node, err := c.store.NodeByID(writer)
	if err != nil {
		return nil, err
	}
	sourceNode := src.NodeID
	if proto.StorageShared(srcStorage.Type) {
		ready, err := c.store.ReadyStorageNodes(srcStorage.ID, false)
		if err != nil {
			return nil, err
		}
		sourceNode = c.firstOnline(ready, writer)
	}
	if sourceNode == 0 {
		return nil, errors.New("no online node can read the source")
	}
	row, err := c.store.PrepareModelPeerSync(ref, rel)
	if err != nil {
		return nil, err
	}
	if err := c.store.SetModelFileWriter(row.ID, writer); err != nil {
		return nil, err
	}
	if err := c.store.SetModelSyncSource(ref, sourceNode); err != nil {
		return nil, err
	}
	if rateLimitKiB >= 0 {
		if err := c.store.SetModelSyncRate(ref, rateLimitKiB); err != nil {
			return nil, err
		}
	}
	row, _ = c.store.ModelFileAt(ref)
	c.trySyncModelFromPeer(context.WithoutCancel(ctx), node, model, row)
	return c.store.ModelFileAt(ref)
}

// firstOnline is the first connected node of ids other than except.
func (c *Controller) firstOnline(ids []uint, except uint) uint {
	for _, id := range ids {
		if id == except {
			continue
		}
		node, err := c.store.NodeByID(id)
		if err == nil && c.hub.Online(node.UUID) {
			return id
		}
	}
	return 0
}
