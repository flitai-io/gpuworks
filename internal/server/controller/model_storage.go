package controller

import (
	"context"
	"errors"
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var errCacheEvictionBusy = errors.New("cache is being prepared for a launch")

// deploymentModels is what one launch on one node mounts: the primary model's
// location and each template component's.
type deploymentModels struct {
	Primary    store.ModelLocation
	Components map[uint]store.ModelLocation
}

// paths is where the files are on the node, as the launch mounts them; nil
// files (a preview before the model is there) give nil.
func (f *deploymentModels) paths() *planning.ModelPaths {
	if f == nil {
		return nil
	}
	out := &planning.ModelPaths{Primary: f.Primary.HostPath, Components: make(map[uint]string, len(f.Components))}
	for id, loc := range f.Components {
		out.Components[id] = loc.HostPath
	}
	return out
}

// ensureModelReady makes a model readable on a node from wherever the node
// can read it (store.ModelSourceOn). When it is not ready yet, it starts what
// the chosen storage needs: a local storage gets the model from a peer or the
// public source, an nfs_cache storage makes room and copies it into the node
// cache, and a shared storage being written has nothing to start.
func (c *Controller) ensureModelReady(ctx context.Context, node *store.Node, dep *store.Deployment, model *store.Model, rules store.SourceRules) (store.ModelLocation, bool, error) {
	src, err := c.store.ModelSourceOn(model.ID, node, rules)
	if err != nil {
		return src.Location, false, err
	}
	loc, storage := src.Location, src.Location.Storage
	switch src.Action {
	case store.SourceReady:
		return loc, true, nil
	case store.SourceAcquire:
		return loc, false, c.acquireLocalModel(ctx, node, dep, &storage, model, loc.File, rules.Pinned)
	case store.SourceFillCache:
		return loc, false, c.fillNodeCache(ctx, node, dep, &storage, model, src)
	case store.SourceWaitShared:
		return loc, false, nil
	}
	if refusal := planning.SourceRefusal(src, node, model); refusal != nil {
		c.recordUnschedulable(dep, *refusal)
	}
	return loc, false, nil
}

// sourceRules are what a deployment says about where its models come from.
func (c *Controller) sourceRules(dep *store.Deployment) (store.SourceRules, error) {
	return c.store.SourceRulesFor(dep)
}

// acquireLocalModel is the node-local path that predates storages: an absent
// row becomes a pending task, which a mounted shared storage among pinned, a
// peer or the public source serves.
func (c *Controller) acquireLocalModel(ctx context.Context, node *store.Node, dep *store.Deployment, storage *store.Storage, model *store.Model, mf *store.ModelFile, pinned []uint) error {
	if mf == nil {
		created, err := c.store.UpsertModelFile(&store.ModelFile{
			ModelID: model.ID, StorageID: storage.ID, NodeID: node.ID,
			Path: relModelPath(model), State: proto.MFPending, Acquisition: proto.ModelAcquireAuto,
		})
		if err != nil {
			return err
		}
		mf = created
		if err := c.store.SetDownloadDeployment(mf.ID, mf.TransferID, dep.ID); err != nil {
			return err
		}
		mf.DownloadDeploymentID = dep.ID
	}
	handled, refused := c.syncModelWithin(ctx, node, model, mf, pinned)
	if !handled {
		// A definitive download failure (already reported by the agent,
		// internal/agent/app/modelfiles.go) will never resolve by blindly
		// repeating the identical request: stop pushing and surface it once per
		// reconcile pass instead of hammering the same failing download.
		if mf.State == proto.MFFailed {
			c.log.Warn("deployment blocked: model file failed", "dep", dep.ID, "model", model.ID, "node", node.UUID, "message", mf.Message)
		} else {
			refused = c.pushModelFile(node, mf, model)
		}
	}
	// The download is all that is left for this node and the rules forbid
	// it: a refusal the deployment shows, re-evaluated on the next pass (and
	// at once when the rules change, onEvent).
	if reason, ok := planning.WeightsFetchRefusal(refused, node, model.DisplayName()); ok {
		c.recordUnschedulable(dep, reason)
	}
	return nil
}

// fillNodeCache copies a model from an nfs_cache storage into a node's cache,
// evicting least recently used cache entries first when the cache cap or the
// disk reserve requires it (planning.CachePlan, the same decision admission
// made).
func (c *Controller) fillNodeCache(ctx context.Context, node *store.Node, dep *store.Deployment, storage *store.Storage, model *store.Model, src store.ModelSource) error {
	loc := src.Location
	ref := store.FileRef{ModelID: model.ID, StorageID: storage.ID, NodeID: node.ID}
	c.cacheCopies.claim(dep.ID, ref)
	resuming := loc.File != nil && loc.File.Acquisition == proto.ModelAcquireCopy && loc.File.State == proto.MFAbsent
	if loc.File != nil && !resuming {
		if refusal := planning.SourceRefusal(src, node, model); refusal != nil {
			c.recordUnschedulable(dep, *refusal)
			return nil
		}
		switch {
		case loc.File.Ready():
			return nil
		default:
			// Copying; re-push so a reconnected agent resumes it.
			_ = c.pushModelFile(node, loc.File, model)
			return nil
		}
	}
	eff, applies, err := c.store.EffectiveStorageForNode(storage, node)
	if err != nil || !applies {
		return err
	}
	need := loc.SizeBytes
	if need <= 0 {
		need = model.WeightBytes
	}
	if resuming {
		// Only entries in the current directory reserve its budget. A
		// suspended copy in an old directory must pay the full new size.
		entries, err := c.store.NodeCacheEntries(storage.ID, node.ID)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.ID == loc.File.ID {
				need = max(0, need-entry.SizeBytes)
				break
			}
		}
	}
	// An earlier admission may have lost its removal acknowledgement. Finish
	// that durable intent before asking the planner to budget a replacement.
	pending, err := c.store.PendingCacheDeletions(storage.ID, node.ID)
	if err != nil {
		return err
	}
	for _, deleting := range pending {
		if err := c.evictCacheRow(ctx, node, deleting.ID); err != nil {
			c.q.addAfter(depKey(dep.ID), c.retryBackoff)
			return nil //nolint:nilerr // The failed eviction is already scheduled for retry with retryBackoff.
		}
	}
	plan, err := planning.CachePlan(c.store, storage, eff, node, need)
	if err != nil {
		return err
	}
	if plan.Refusal != "" {
		c.recordUnschedulable(dep, planning.CacheRefusal(storage, node, plan, need, eff.CacheMaxBytes))
		return nil
	}
	for _, id := range plan.Evict {
		c.evictions.Set(ref, prepobs.PhaseEvicting, c.now())
		if err := c.evictCacheRow(ctx, node, id); err != nil {
			if errors.Is(err, errCacheEvictionBusy) {
				c.q.addAfter(depKey(dep.ID), groupPollInterval)
				return nil
			}
			c.evictions.Set(ref, prepobs.PhaseEvictionFailed, c.now())
			c.log.Warn("node cache eviction failed", "node", node.UUID, "model_file", id, "err", err)
			// No model-file event follows a failed eviction; try again
			// after the usual backoff rather than wait for an unrelated one.
			c.q.addAfter(depKey(dep.ID), c.retryBackoff)
			return nil
		}
	}
	c.evictions.Set(ref, "", c.now())
	row, err := c.store.PrepareModelCopy(ref, relModelPath(model))
	if err != nil {
		return err
	}
	_ = c.pushModelFile(node, row, model)
	return nil
}

// evictCacheRow removes one node-cache entry from disk and from the index.
// The entry is checked again right before (the plan that chose it may be a
// moment old), and an entry on a node that is not connected is left alone:
// dropping only its row would orphan the files.
func (c *Controller) evictCacheRow(ctx context.Context, node *store.Node, id uint) error {
	release, reserved := c.store.TryEvictNodeCache(node.ID)
	if !reserved {
		return errCacheEvictionBusy
	}
	defer release()
	row, err := c.store.ModelFileByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.deleteCacheFile(ctx, node, row)
}

// ensureDeploymentModelsPresent makes the deployment's primary model and every
// template component readable on the node, each from wherever the node can
// read it.
func (c *Controller) ensureDeploymentModelsPresent(ctx context.Context, node *store.Node, dep *store.Deployment, primary *store.Model) (deploymentModels, bool, error) {
	out := deploymentModels{Components: map[uint]store.ModelLocation{}}
	models, err := c.deploymentModels(dep, primary)
	if err != nil {
		return out, false, err
	}
	rules, err := c.sourceRules(dep)
	if err != nil {
		return out, false, err
	}
	for _, model := range models {
		loc, ready, err := c.ensureModelReady(ctx, node, dep, model, rules)
		if err != nil || !ready {
			return out, false, err
		}
		out.Components[model.ID] = loc
	}
	out.Primary = out.Components[primary.ID]
	return out, true, nil
}

// reads are the model files a launch with these locations mounts, for the
// instance's record.
func (f *deploymentModels) reads() []store.InstanceModelFile {
	if f == nil {
		return nil
	}
	ids := make([]uint, 0, len(f.Components))
	for id := range f.Components {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]store.InstanceModelFile, 0, len(ids))
	for _, id := range ids {
		loc := f.Components[id]
		if loc.File == nil {
			continue
		}
		out = append(out, store.InstanceModelFile{ModelID: id, ModelFileID: loc.File.ID, HostPath: loc.HostPath})
	}
	return out
}
