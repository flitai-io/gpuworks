package planning

import (
	"encoding/json"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/modelcache"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// storageAdmission is what reading the deployment's models requires of a
// node: the bytes still to arrive on the node's disk, which disk that is, and
// a refusal when no storage can serve the node. It is the storage half of
// disk admission; the scheduler applies the result.
type storageAdmission struct {
	NeedBytes int64
	// AvailBytes replaces the node snapshot's models-disk figure when the
	// model is acquired into a storage on another disk; zero leaves it unknown.
	AvailBytes int64
	Refusal    *scheduler.Reason
}

// admitStorage asks store.ModelSourceOn, the rule launches follow, where the
// node would read each model from, and adds up what that costs: nothing for a
// copy it can read or a shared storage being written, the missing bytes for a
// model acquired into a local storage, and an eviction plan for an nfs_cache
// fill. A shared storage that lacks the model is not refused here - a create
// that downloads into it is previewed before the download starts; the
// controller reports it.
func admitStorage(st *store.Store, in Input, primaryBytes, componentBytes int64) storageAdmission {
	if st == nil || in.Node == nil {
		return storageAdmission{NeedBytes: modelDiskNeed(st, in.Model, in.Node, 0, primaryBytes) + componentDiskNeed(st, in.Node, 0, componentBytes, in.ComponentArtifacts)}
	}
	type modelNeed struct {
		id    uint
		bytes int64
	}
	var out storageAdmission
	var needs []modelNeed
	if in.Model != nil && in.Model.ID != 0 {
		needs = append(needs, modelNeed{in.Model.ID, primaryBytes})
	} else {
		out.NeedBytes += max(primaryBytes, 0)
	}
	if len(in.ComponentArtifacts) == 0 {
		out.NeedBytes += max(componentBytes, 0)
	}
	for _, artifact := range in.ComponentArtifacts {
		needs = append(needs, modelNeed{artifact.ModelID, artifact.StorageBytes})
	}
	rules := in.ModelRules
	cacheNeed := map[uint]int64{}
	cacheStorages := map[uint]store.Storage{}
	var cacheOrder []uint
	for _, need := range needs {
		src, err := st.ModelSourceOn(need.id, in.Node, rules)
		if err != nil {
			out.NeedBytes += max(need.bytes, 0)
			continue
		}
		storage := src.Location.Storage
		switch src.Action {
		case store.SourceAcquire:
			out.NeedBytes += modelDiskNeed(st, &store.Model{ID: need.id}, in.Node, storage.ID, need.bytes)
			if out.AvailBytes == 0 {
				out.AvailBytes = storageDiskAvailable(in.Node, &storage, false)
			}
		case store.SourceFillCache:
			if _, seen := cacheStorages[storage.ID]; !seen {
				cacheOrder = append(cacheOrder, storage.ID)
			}
			cacheStorages[storage.ID] = storage
			size := need.bytes
			if src.Location.SizeBytes > 0 {
				size = src.Location.SizeBytes
			}
			cacheNeed[storage.ID] += max(size, 0)
		case store.SourceRefused:
			if out.Refusal == nil && SourceBlocksAdmission(src) {
				out.Refusal = SourceRefusal(src, in.Node, nil)
			}
		}
	}
	for _, id := range cacheOrder {
		if out.Refusal != nil {
			break
		}
		storage := cacheStorages[id]
		eff, applies, err := st.EffectiveStorageForNode(&storage, in.Node)
		if err != nil || !applies {
			continue
		}
		plan, err := CachePlan(st, &storage, eff, in.Node, cacheNeed[id])
		if err != nil || plan.Refusal == "" {
			continue
		}
		refusal := CacheRefusal(&storage, in.Node, plan, cacheNeed[id], eff.CacheMaxBytes)
		out.Refusal = &refusal
	}
	return out
}

// CacheRefusal is the scheduler reason for a refused cache plan. Admission and
// the controller's cache fill both report it, so the user sees the same reason
// before and after the deployment is created.
func CacheRefusal(storage *store.Storage, node *store.Node, plan modelcache.Result, need, capBytes int64) scheduler.Reason {
	code := scheduler.ReasonInsufficientDisk
	switch plan.Refusal {
	case modelcache.RefuseCacheLimit:
		code = scheduler.ReasonModelExceedsCacheLimit
	case modelcache.RefuseCacheInUse:
		code = scheduler.ReasonCacheFullInUse
	}
	params := map[string]any{
		"storage": storage.Name, "storage_id": storage.ID, "storage_type": storage.Type,
		"model_bytes": need, "cache_max_bytes": capBytes,
		"need_disk_bytes": plan.NeedDiskBytes, "need_cap_bytes": plan.NeedCapBytes,
	}
	if node != nil {
		params["node"], params["node_uuid"] = node.DisplayName(), node.UUID
		if usage, ok := nodePathUsage(node, "storage_cache", storage.ID); ok {
			if path.IsAbs(usage.Path) {
				params["path"] = path.Clean(usage.Path)
			}
			if code == scheduler.ReasonInsufficientDisk && usage.AvailableBytes >= 0 && plan.NeedDiskBytes > 0 {
				required := usage.AvailableBytes + plan.NeedDiskBytes
				if required >= usage.AvailableBytes {
					params["required_bytes"], params["available_bytes"] = required, usage.AvailableBytes
				}
			}
		}
	}
	return scheduler.Reason{Code: code, Params: params}
}

// CachePlan decides what an nfs_cache storage evicts on a node to make room
// for needBytes more. The scheduler's admission and the controller's eviction
// both call it, so what one admits the other can make room for.
func CachePlan(st *store.Store, storage *store.Storage, eff store.EffectiveStorage, node *store.Node, needBytes int64) (modelcache.Result, error) {
	entries, err := st.NodeCacheEntries(storage.ID, node.ID)
	if err != nil {
		return modelcache.Result{}, err
	}
	usage, _ := nodePathUsage(node, "storage_cache", storage.ID)
	reserve := st.StoragePolicy().ReserveFreeBytes
	if reserve <= 0 {
		reserve = store.DefaultReserveFreeBytes(usage.TotalBytes)
	}
	free := usage.AvailableBytes
	if usage.TotalBytes <= 0 {
		// Unknown disk: only the cap can be enforced.
		free, reserve = needBytes, 0
	}
	return modelcache.Plan(modelcache.Input{
		NeedBytes: needBytes, FreeBytes: free, ReserveBytes: reserve,
		CapBytes: eff.CacheMaxBytes, Entries: entries,
	}), nil
}

// storageDiskAvailable is the free space of the disk a storage keeps its
// files on for this node, from the agent's last statfs snapshot. The local
// storage that uses the agent default falls back to the models entry older
// agents report.
func storageDiskAvailable(node *store.Node, storage *store.Storage, cache bool) int64 {
	kind := "storage"
	if cache {
		kind = "storage_cache"
	}
	if usage, ok := nodePathUsage(node, kind, storage.ID); ok {
		return usage.AvailableBytes
	}
	if storage.Type == proto.StorageLocal && storage.Path == "" {
		return modelDiskAvailable(node)
	}
	return 0
}

func nodePathUsage(node *store.Node, kind string, storageID uint) (proto.PathUsage, bool) {
	if node == nil || strings.TrimSpace(node.PathUsage) == "" {
		return proto.PathUsage{}, false
	}
	var paths []proto.PathUsage
	if err := json.Unmarshal([]byte(node.PathUsage), &paths); err != nil {
		return proto.PathUsage{}, false
	}
	for _, path := range paths {
		if path.Kind == kind && path.StorageID == storageID && path.TotalBytes > 0 {
			return path, true
		}
	}
	return proto.PathUsage{}, false
}
