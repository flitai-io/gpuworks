package api

import (
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fillStorageCaches loads shared/reference facts once for all rendered nodes.
// On read failure leave cache absent; unknown must never masquerade as zero.
func (a *apiHandlers) fillStorageCaches(out *dto.Storage, storage *store.Storage, nodes []store.Node) {
	if storage.Type != proto.StorageNFSCache {
		return
	}
	ids := make([]uint, 0, len(nodes))
	byUUID := map[string]*store.Node{}
	for i := range nodes {
		ids = append(ids, nodes[i].ID)
		byUUID[nodes[i].UUID] = &nodes[i]
	}
	entries, err := a.d.Store.NodeCacheEntriesForNodes(storage.ID, ids)
	if err != nil {
		return
	}
	// Admission reserves the complete size of transfers. The displayed cache
	// usage instead follows model-file details: only bytes already downloaded
	// count until a file is ready. Load these facts once for the entire fleet.
	files, err := a.d.Store.ModelFilesInStorage(storage.ID)
	if err != nil {
		return
	}
	bytesByFile := make(map[uint]int64, len(files))
	for _, file := range files {
		size := file.Downloaded
		if file.Ready() {
			size = file.SizeBytes
		}
		bytesByFile[file.ID] = max(size, 0)
	}
	targets, err := a.d.Store.StorageNodes(storage.ID)
	if err != nil {
		return
	}
	ops, err := a.d.Store.CacheOperations(storage.ID)
	if err != nil {
		return
	}
	byNode := map[uint]store.StorageCacheOperation{}
	for _, op := range ops {
		byNode[op.NodeID] = op
	}
	policy := a.d.Store.StoragePolicy()
	fill := func(status *dto.StorageNodeStatus) {
		node := byUUID[status.NodeUUID]
		if node == nil {
			return
		}
		op := byNode[node.ID]
		in, known, applies := store.CacheBudget(storage, node, targets, entries[node.ID], policy, op.DiskInvalidatedAt)
		if !known {
			status.CacheTotalBytes = 0
			status.CacheAvailableBytes = 0
		}
		cache := &dto.StorageNodeCache{LimitBytes: in.CapBytes, State: "satisfied"}
		hasProtected := false
		for _, e := range entries[node.ID] {
			size, exists := bytesByFile[e.ID]
			if !exists {
				// A concurrent removal invalidated this projection; do not
				// render a missing observation as zero bytes.
				return
			}
			cache.UsedBytes += size
			if e.Evictable() {
				cache.ReclaimableBytes += size
			} else {
				hasProtected = true
				cache.ProtectedBytes += size
			}
		}
		needsCleanup, refusal := store.CacheBudgetStatus(in)
		switch {
		case !applies:
			cache.State, cache.Reason = "unavailable", "storage_unavailable"
		case status.State == "offline":
			cache.State, cache.Reason = "offline", "node_offline"
		case status.State != proto.StorageStateReady:
			cache.State, cache.Reason = "unavailable", "storage_not_ready"
		case op.State == "failed":
			cache.State, cache.Reason, cache.Message = "failed", "cleanup_failed", op.Error
		case op.State == "running":
			cache.State = "running"
		case !cachePathMatches(storage, targets, node, status.CachePath):
			cache.State, cache.Reason = "pending", "directory_unverified"
		case !known:
			cache.State, cache.Reason = "pending", "disk_unverified"
		case op.Mode == "clear" && op.State == "completed" && op.Error == store.CacheOperationProtectedEntriesRemain && hasProtected:
			cache.State, cache.Reason = "blocked", "cache_full_in_use"
		case op.State == "pending":
			cache.State = "pending"
		case needsCleanup:
			cache.State, cache.Reason = "pending", "cleanup_required"
		case refusal != "":
			cache.State, cache.Reason = "blocked", refusal
		}
		if op.State == "waiting" && cache.State == "pending" && op.Error != "" {
			cache.Message = op.Error
		}
		status.Cache = cache
	}
	for i := range out.Nodes.All {
		fill(&out.Nodes.All[i])
	}
	for i := range out.Nodes.Problems {
		fill(&out.Nodes.Problems[i])
	}
}

func cachePathMatches(storage *store.Storage, targets []store.StorageNode, node *store.Node, actual string) bool {
	expected := storage.CachePath
	for _, t := range targets {
		if t.NodeID == node.ID && t.CachePath != "" {
			expected = t.CachePath
		}
	}
	return expected != "" && actual != "" && filepath.Clean(expected) == filepath.Clean(actual)
}
