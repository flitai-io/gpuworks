package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelcache"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func cacheKey(storageID, nodeID uint) string { return fmt.Sprintf("%d/%d", storageID, nodeID) }

// cacheEvents only enqueues work. Slow node RPCs belong to the independent
// worker, never the deployment worker or bus subscriber.
func (c *Controller) cacheEvent(ev bus.Event) {
	if ev.Reason == bus.ReasonProgress {
		return
	}
	switch ev.Topic {
	case store.StorageCacheTopic:
		if ev.Reason == "requested" {
			c.cacheQ.add(ev.Key + "/0")
		}
	case "storage":
		if ev.Reason == "created" || ev.Reason == "updated" {
			c.cacheQ.add("config/" + ev.Key)
		} else if ev.Reason == "node_state" {
			c.cacheQ.add(ev.Key + "/0")
		}
	case "node":
		if ev.Reason != store.NodeChangeStatus {
			c.cacheQ.add("node/" + ev.Key)
		} else {
			c.cacheQ.add("reported/" + ev.Key)
		}
	case "deployment":
		c.cacheReferencesChanged(ev)
	}
}

// cacheModelFileChanged refreshes the cache that holds a changed model file.
// Model-file events are resolved at the routing tick (flushEventRoutes), once
// per key, rather than here for every event.
func (c *Controller) cacheModelFileChanged(row *store.ModelFile) {
	if row.NodeID != 0 {
		c.cacheQ.add("refresh/" + cacheKey(row.StorageID, row.NodeID))
	} else {
		c.cacheQ.add("refresh-storage/" + strconv.FormatUint(uint64(row.StorageID), 10))
	}
}

func (c *Controller) cacheWorker(ctx context.Context) {
	for {
		key, ok := c.cacheQ.next()
		if !ok {
			return
		}
		if ctx.Err() != nil {
			continue
		}
		if err := c.cacheWork(ctx, key); err != nil {
			c.log.Warn("cache reconciliation failed", "key", key, "err", err)
		}
	}
}

func (c *Controller) cacheWork(ctx context.Context, key string) error {
	if key == "waiting" {
		ops, err := c.store.WaitingCacheOperations(nil)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.State == "waiting" || op.State == "pending" {
				c.cacheQ.add(cacheKey(op.StorageID, op.NodeID))
			}
		}
		return nil
	}
	if strings.HasPrefix(key, "reported/") {
		node, err := c.store.NodeByUUID(strings.TrimPrefix(key, "reported/"))
		if err != nil {
			return err
		}
		ops, err := c.store.WaitingCacheOperationsForNode(node.ID)
		if err != nil {
			return err
		}
		for _, op := range ops {
			c.cacheQ.add(cacheKey(op.StorageID, op.NodeID))
		}
		return nil
	}
	if strings.HasPrefix(key, "node/") {
		node, err := c.store.NodeByUUID(strings.TrimPrefix(key, "node/"))
		if err != nil {
			return err
		}
		storages, err := c.store.StoragesForNode(node)
		if err != nil {
			return err
		}
		for _, eff := range storages {
			if eff.Storage.Type == proto.StorageNFSCache {
				c.cacheQ.add("refresh/" + cacheKey(eff.Storage.ID, node.ID))
			}
		}
		return nil
	}
	if strings.HasPrefix(key, "config/") || strings.HasPrefix(key, "refresh-storage/") {
		policyChange := strings.HasPrefix(key, "config/")
		n, _ := strconv.ParseUint(strings.SplitN(key, "/", 2)[1], 10, 64)
		storage, err := c.store.StorageByID(uint(n))
		if err != nil {
			return err
		}
		if storage.Type != proto.StorageNFSCache {
			return nil
		}
		nodes, err := c.store.NodesForStorage(storage)
		if err != nil {
			return err
		}
		ids := make([]uint, 0, len(nodes))
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		if len(ids) == 0 {
			return nil
		}
		if policyChange {
			return c.store.RequestCachePolicyTrim(storage.ID, ids)
		}
		return c.store.RequestCacheOperations(storage.ID, ids, "trim", false)
	}
	if strings.HasPrefix(key, "refresh/") {
		ids := strings.Split(strings.TrimPrefix(key, "refresh/"), "/")
		if len(ids) != 2 {
			return nil
		}
		sid, _ := strconv.ParseUint(ids[0], 10, 64)
		nid, _ := strconv.ParseUint(ids[1], 10, 64)
		storage, err := c.store.StorageByID(uint(sid))
		if err != nil {
			return err
		}
		if storage.Type != proto.StorageNFSCache {
			return nil
		}
		return c.store.RequestCacheOperations(uint(sid), []uint{uint(nid)}, "trim", false)
	}
	ids := strings.Split(key, "/")
	if len(ids) != 2 {
		return nil
	}
	sid, _ := strconv.ParseUint(ids[0], 10, 64)
	nid, _ := strconv.ParseUint(ids[1], 10, 64)
	if nid == 0 {
		ops, err := c.store.CacheOperations(uint(sid))
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.State == "pending" || op.State == "waiting" {
				c.cacheQ.add(cacheKey(op.StorageID, op.NodeID))
			}
		}
		return nil
	}
	return c.reconcileStorageCache(ctx, uint(sid), uint(nid))
}

func (c *Controller) reconcileStorageCache(ctx context.Context, storageID, nodeID uint) error {
	op, err := c.store.CacheOperation(storageID, nodeID)
	if err != nil {
		return err
	}
	if op.State != "pending" && op.State != "waiting" {
		return nil
	}
	finish := func(state string, err error) error {
		message := ""
		if err != nil {
			message = err.Error()
		}
		_, saveErr := c.store.SetCacheOperationResult(op, state, message)
		return saveErr
	}
	storage, err := c.store.StorageByID(storageID)
	if err != nil {
		return finish("failed", err)
	}
	node, err := c.store.NodeByID(nodeID)
	if err != nil {
		return finish("failed", err)
	}
	eff, applies, err := c.store.EffectiveStorageForNode(storage, node)
	if err != nil {
		return finish("failed", err)
	}
	if storage.Type != proto.StorageNFSCache || !applies {
		return finish("waiting", errors.New("cache storage is unavailable for this node"))
	}
	if !c.hub.Online(node.UUID) {
		return finish("waiting", errors.New("node is offline"))
	}
	state, err := c.store.StorageNodeStateFor(storageID, nodeID)
	if err != nil || state.State != proto.StorageStateReady || state.CachePath == "" || filepath.Clean(state.CachePath) != filepath.Clean(eff.CachePath) {
		return finish("waiting", errors.New("cache directory is not ready"))
	}
	// A launch pins ready files until its reader references are persisted.
	// Reserve exclusively before inspecting candidates and keep the reservation
	// through the remote deletion and index update; never wait behind a launch.
	release, reserved := c.store.TryEvictNodeCache(nodeID)
	if !reserved {
		return finish("waiting", errors.New("cache is being prepared for a launch"))
	}
	defer release()
	// Resume issued deletions separately. Including their bytes in a fresh
	// trim plan would evict healthy files even when completing the pending
	// removal alone satisfies the cap.
	pending, err := c.store.PendingCacheDeletions(storageID, nodeID)
	if err != nil {
		return finish("failed", err)
	}
	if len(pending) > 0 {
		if ok, err := c.store.SetCacheOperationResult(op, "running", ""); err != nil || !ok {
			return err
		}
		for i, row := range pending {
			if i == 16 {
				break
			}
			latest, err := c.store.CacheOperation(storageID, nodeID)
			if err != nil {
				return finish("failed", err)
			}
			if latest.Generation != op.Generation {
				c.cacheQ.add(cacheKey(storageID, nodeID))
				return nil
			}
			if err := c.deleteCacheFile(ctx, node, &row); err != nil {
				if ctx.Err() != nil {
					return finish("pending", ctx.Err())
				}
				return finish("failed", err)
			}
		}
		if err := finish("pending", nil); err != nil {
			return err
		}
		c.cacheQ.add(cacheKey(storageID, nodeID))
		return nil
	}
	entries, err := c.store.NodeCacheEntries(storageID, nodeID)
	if err != nil {
		return finish("failed", err)
	}
	targets, err := c.store.StorageNodes(storageID)
	if err != nil {
		return finish("failed", err)
	}
	in, diskKnown, _ := store.CacheBudget(storage, node, targets, entries, c.store.StoragePolicy(), op.DiskInvalidatedAt)
	plan := modelcache.Trim(in)
	if op.Mode == "clear" {
		plan.Evict = nil
		for _, e := range entries {
			if e.Evictable() {
				plan.Evict = append(plan.Evict, e.ID)
			}
		}
	}
	if ok, err := c.store.SetCacheOperationResult(op, "running", ""); err != nil || !ok {
		return err
	}
	// Work from one snapshot. Do not repeatedly subtract disk usage from stale
	// statfs reports after each removal. Re-evaluate only after this plan ends.
	evicted := false
	const maxCacheEvictionsPerPass = 16
	selected := plan.Evict
	if len(selected) > maxCacheEvictionsPerPass {
		selected = selected[:maxCacheEvictionsPerPass]
	}
	for _, id := range selected {
		if err := ctx.Err(); err != nil {
			return finish("pending", err)
		}
		latest, err := c.store.CacheOperation(storageID, nodeID)
		if err != nil {
			return err
		}
		if latest.Generation != op.Generation {
			c.cacheQ.add(cacheKey(storageID, nodeID))
			return nil
		}
		current, err := c.store.StorageByID(storageID)
		if err != nil {
			return finish("failed", err)
		}
		freshNode, err := c.store.NodeByID(nodeID)
		if err != nil {
			return finish("failed", err)
		}
		effective, ok, err := c.store.EffectiveStorageForNode(current, freshNode)
		if err != nil {
			return finish("failed", err)
		}
		if !ok || effective.CachePath != eff.CachePath || effective.CacheMaxBytes != eff.CacheMaxBytes {
			return finish("waiting", errors.New("cache configuration changed during cleanup"))
		}
		row, err := c.store.ModelFileByID(id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return finish("failed", err)
		}
		if err := c.deleteCacheFile(ctx, node, row); err != nil {
			if ctx.Err() != nil {
				return finish("pending", ctx.Err())
			}
			return finish("failed", err)
		}
		evicted = true

	}
	if len(plan.Evict) > len(selected) {
		if err := finish("pending", nil); err != nil {
			return err
		}
		c.cacheQ.add(cacheKey(storageID, nodeID))
		return nil
	}
	if op.Mode == "trim" && plan.Refusal != "" {
		return finish("waiting", errors.New(plan.Refusal))
	}
	if op.Mode == "trim" && (!diskKnown || evicted) {
		return finish("waiting", errors.New("disk_unverified"))
	}
	if op.Mode == "clear" {
		entries, err = c.store.NodeCacheEntries(storageID, nodeID)
		if err != nil {
			return finish("failed", err)
		}
		for _, entry := range entries {
			if !entry.Evictable() {
				return finish("completed", errors.New("protected_entries_remain"))
			}
		}
	}
	return finish("completed", nil)
}
