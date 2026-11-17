package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// Storage operations that reach a node: probing a definition, the control
// plane heartbeats on a shared storage, clearing node caches, and the per-node
// disk usage view, plus the platform-wide storage settings.

func (a *apiHandlers) probeStorage(w http.ResponseWriter, r *http.Request) {
	var req dto.StorageProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Without a named node the probe runs on any online node: the form has
	// not saved its targets yet. This trial does not establish access from
	// any other node.
	row := &store.Storage{Name: "probe", Type: strings.TrimSpace(req.Type), Path: strings.TrimSpace(req.Path),
		Export: strings.TrimSpace(req.Export), MountOptions: strings.TrimSpace(req.MountOptions), AllNodes: true}
	// A shared storage the form has not given a path yet is tried where a
	// create would mount it.
	if name := strings.TrimSpace(req.Name); row.Path == "" && proto.StorageShared(row.Type) && name != "" {
		row.Path = store.DefaultMountPoint(name)
	}
	if err := validateStorage(row); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.validateStoragePaths(row, nil); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := a.probeOnNode(r.Context(), req.NodeUUID, row, []store.StorageNode{{Existing: req.Existing}})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// probeOnNode runs storage.probe on the named node, or on the first online
// node the storage would apply to.
func (a *apiHandlers) probeOnNode(ctx context.Context, nodeUUID string, row *store.Storage, targets []store.StorageNode) (dto.StorageProbeResult, error) {
	if a.d.CallNode == nil {
		return dto.StorageProbeResult{}, productError(ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
	}
	node, err := a.probeNode(nodeUUID, row, targets)
	if err != nil {
		return dto.StorageProbeResult{}, err
	}
	existing := false
	for _, target := range targets {
		if target.NodeID == node.ID || target.NodeID == 0 {
			existing = existing || target.Existing
		}
	}
	params := proto.StorageProbeParams{Type: row.Type, Path: row.Path, Export: row.Export, MountOptions: row.MountOptions, Existing: existing}
	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, err := a.d.CallNode(callCtx, node.UUID, proto.RPCStorageProbe, params)
	if err != nil {
		return dto.StorageProbeResult{}, wrapProductError(ErrorStorageProbeFailed, "probe on "+nodeDisplayName(node)+" failed: "+err.Error(), map[string]any{"node": node.UUID}, err)
	}
	var result proto.StorageProbeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return dto.StorageProbeResult{}, err
	}
	checkedAt := time.Now().UTC()
	target := row.Path
	// Only a new NFS mount probes the export; existing mounts and directory
	// storage are checked through the configured node path.
	if !existing && (row.Type == proto.StorageNFS || row.Type == proto.StorageNFSCache) {
		target = row.Export
	}
	return dto.StorageProbeResult{
		Target: target, CheckedAt: &checkedAt,
		NodeUUID: node.UUID, NodeName: nodeDisplayName(node), OK: result.OK, Reason: result.Reason,
		Message: result.Message, Output: result.Output, Version: result.Version, Options: result.Options,
		Readable: result.Readable, Writable: result.Writable, TotalBytes: result.TotalBytes, AvailableBytes: result.AvailableBytes,
	}, nil
}

func (a *apiHandlers) probeNode(nodeUUID string, row *store.Storage, targets []store.StorageNode) (*store.Node, error) {
	if uuid := strings.TrimSpace(nodeUUID); uuid != "" {
		node, err := a.d.Store.NodeByUUID(uuid)
		if err != nil {
			return nil, err
		}
		if a.d.Online != nil && !a.d.Online(node.UUID) {
			return nil, productError(ErrorStorageProbeFailed, "the probe node is offline", map[string]any{"node": uuid})
		}
		return node, nil
	}
	nodes, err := a.d.Store.FleetNodes()
	if err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for i := range nodes {
		node := &nodes[i]
		if a.d.Online != nil && !a.d.Online(node.UUID) {
			continue
		}
		if row.AllNodes || len(targets) == 0 {
			return node, nil
		}
		labels, _ := store.NodeLabels(node.Labels)
		for _, target := range targets {
			if target.NodeID == node.ID {
				return node, nil
			}
			if target.NodeID == 0 && target.LabelKey != "" {
				if value, ok := labels[target.LabelKey]; ok && (target.LabelValue == "" || target.LabelValue == value) {
					return node, nil
				}
			}
		}
	}
	return nil, productError(ErrorStorageProbeFailed, "no online node to probe on", nil)
}

// onlineStorageNodes are the connected nodes a storage is ready on - the ones
// it is writable from when write is set - in node id order: the nodes an
// action on a shared storage can run through (planning.OnlineStorageNodes).
func (a *apiHandlers) onlineStorageNodes(row *store.Storage, write bool) ([]*store.Node, error) {
	return planning.OnlineStorageNodes(a.d.Store, a.d.Online, row, write)
}

// pickStorageNode is planning.PickStorageNode over the connected nodes.
func (a *apiHandlers) pickStorageNode(row *store.Storage, write bool, accept func(*store.Node) bool) (*store.Node, error) {
	return planning.PickStorageNode(a.d.Store, a.d.Online, row, write, accept)
}

// storagePeers refreshes this control plane's heartbeat on a shared storage
// and lists the other control planes using it. remove deletes the heartbeat.
func (a *apiHandlers) storagePeers(ctx context.Context, row *store.Storage, remove bool) ([]dto.StorageControlPlane, error) {
	writers, err := a.onlineStorageNodes(row, true)
	if err != nil {
		return nil, err
	}
	return a.storagePeersOn(ctx, row, remove, writers)
}

// storagePeersOn is storagePeers through the first of writers that answers,
// for a caller that read them before the storage's node states went away.
func (a *apiHandlers) storagePeersOn(ctx context.Context, row *store.Storage, remove bool, writers []*store.Node) ([]dto.StorageControlPlane, error) {
	if a.d.CallNode == nil {
		return nil, productError(ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
	}
	var identity store.ControlPlaneIdentity
	if remove {
		var err error
		identity, err = a.d.Store.EnsureControlPlaneIdentity()
		if err != nil {
			return nil, err
		}
	} else {
		identity = a.d.Store.ControlPlaneIdentity()
	}
	var removalErrors error
	for _, node := range writers {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		raw, err := a.d.CallNode(callCtx, node.UUID, proto.RPCStoragePeers, proto.StoragePeersParams{
			StorageID: row.ID, ControlPlaneID: identity.ID, ControlPlaneName: identity.Name, Version: version.Version,
			FreshSeconds: int(store.StorageHeartbeatFresh.Seconds()), Remove: remove,
		})
		cancel()
		if err != nil {
			if remove {
				removalErrors = errors.Join(removalErrors, fmt.Errorf("remove storage heartbeat through node %s: %w", node.UUID, err))
			}
			continue
		}
		var result proto.StoragePeersResult
		if err := json.Unmarshal(raw, &result); err != nil {
			if remove {
				return nil, errors.Join(removalErrors, fmt.Errorf("decode storage heartbeat removal from node %s: %w", node.UUID, err))
			}
			return nil, err
		}
		out := make([]dto.StorageControlPlane, 0, len(result.Peers))
		for _, peer := range result.Peers {
			out = append(out, dto.StorageControlPlane{ID: peer.ID, Name: peer.Name, Version: peer.Version, LastSeenSeconds: peer.LastSeenSeconds, Fresh: peer.Fresh})
		}
		return out, nil
	}
	if removalErrors != nil {
		return nil, removalErrors
	}
	return nil, productError(ErrorStorageNotWritable, "no online node can write to this storage", nil)
}

func (a *apiHandlers) storageControlPlanes(w http.ResponseWriter, r *http.Request) {
	row, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !proto.StorageShared(row.Type) {
		writeJSON(w, http.StatusOK, dto.NewList([]dto.StorageControlPlane{}))
		return
	}
	peers, err := a.storagePeers(r.Context(), row, false)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(peers))
}

func (a *apiHandlers) getStorageSettings(w http.ResponseWriter, r *http.Request) {
	identity := a.d.Store.ControlPlaneIdentity()
	defaults := store.DefaultStorageDirs()
	writeJSON(w, http.StatusOK, dto.StorageSettings{ControlPlaneID: identity.ID, ControlPlaneName: identity.Name,
		ReserveFreeBytes: a.d.Store.StoragePolicy().ReserveFreeBytes,
		Defaults:         dto.StorageDefaults{LocalPath: defaults.LocalPath, MountRoot: defaults.MountRoot, CacheRoot: defaults.CacheRoot}})
}

func (a *apiHandlers) patchStorageSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ControlPlaneName *string `json:"control_plane_name"`
		ReserveFreeBytes *int64  `json:"reserve_free_bytes"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.ControlPlaneName != nil {
		if !validateName(w, *req.ControlPlaneName) {
			return
		}
		if _, err := a.d.Store.SetControlPlaneName(*req.ControlPlaneName); err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorStorageNameInvalid, err.Error(), nil)
			return
		}
	}
	if req.ReserveFreeBytes != nil {
		policy := a.d.Store.StoragePolicy()
		policy.ReserveFreeBytes = *req.ReserveFreeBytes
		if err := a.d.Store.SetStoragePolicy(policy); err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, err.Error(), map[string]any{"field": "reserve_free_bytes", "detail": err.Error()})
			return
		}
	}
	a.getStorageSettings(w, r)
}

// clearStorageCache evicts an nfs_cache storage's unused node caches, on one
// node or on all; caches an instance or an active deployment reads stay.
func (a *apiHandlers) clearStorageCache(w http.ResponseWriter, r *http.Request) {
	row, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if row.Type != proto.StorageNFSCache {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "only an nfs_cache storage has node caches", nil)
		return
	}
	nodes, err := a.d.Store.NodesForStorage(row)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	only := strings.TrimSpace(r.URL.Query().Get("node_uuid"))
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	var freed int64
	removed := 0
	failures := []map[string]string{}
	matched := only == ""
	for i := range nodes {
		node := &nodes[i]
		if only != "" && node.UUID != only {
			continue
		}
		matched = true
		if a.d.Online != nil && !a.d.Online(node.UUID) {
			// An offline node with nothing cached here has nothing to clear;
			// failing on it would stop a switch the server would accept.
			entries, err := a.d.Store.NodeCacheEntries(row.ID, node.ID)
			if err == nil {
				var pending []store.ModelFile
				pending, err = a.d.Store.PendingCacheDeletions(row.ID, node.ID)
				if err == nil && len(entries) == 0 && len(pending) == 0 {
					continue
				}
			}
			message := "node is offline"
			if err != nil {
				message = err.Error()
			}
			failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": message})
			continue
		}
		func() {
			release, reserved := a.d.Store.TryEvictNodeCache(node.ID)
			if !reserved {
				failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": "cache is being prepared for a launch; retry later"})
				return
			}
			defer release()
			entries, err := a.d.Store.NodeCacheEntries(row.ID, node.ID)
			if err != nil {
				failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": err.Error()})
				return
			}
			pending, err := a.d.Store.PendingCacheDeletions(row.ID, node.ID)
			if err != nil {
				failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": err.Error()})
				return
			}
			ids := make([]uint, 0, len(entries)+len(pending))
			for _, entry := range entries {
				if entry.Evictable() {
					ids = append(ids, entry.ID)
				}
			}
			for _, file := range pending {
				ids = append(ids, file.ID)
			}
			for _, id := range ids {
				file, err := a.d.Store.ModelFileByID(id)
				if err != nil {
					failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": err.Error()})
					continue
				}
				if err := a.d.Store.InvalidateCacheDisk(row.ID, node.ID); err != nil {
					failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": err.Error()})
					continue
				}
				file, err = a.d.Store.BeginCacheDeletion(file)
				if err != nil {
					failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": err.Error()})
					continue
				}
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				raw, err := a.d.CallNode(ctx, node.UUID, proto.RPCCacheEvict, map[string]string{"path": file.Path, "eviction_id": file.TransferID})
				cancel()
				if err == nil {
					var result struct {
						Removed    bool   `json:"removed"`
						Path       string `json:"path"`
						EvictionID string `json:"eviction_id"`
					}
					if decodeErr := json.Unmarshal(raw, &result); decodeErr != nil || !result.Removed || result.Path != filepath.Clean(file.Path) || result.EvictionID != file.TransferID {
						err = errors.New("node did not confirm cache removal")
					}
				}
				if err != nil {
					failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": err.Error()})
					continue
				}
				if err := a.d.Store.InvalidateCacheDisk(row.ID, node.ID); err != nil {
					failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": err.Error()})
					continue
				}
				if a.d.Store.CompleteCacheDeletion(file) == nil {
					freed += max(file.SizeBytes, file.Downloaded)
					removed++
				} else {
					failures = append(failures, map[string]string{"node_uuid": node.UUID, "node_name": node.DisplayName(), "message": "cache index deletion failed"})
				}
			}
		}()
	}
	if !matched {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "node is unknown or outside storage scope", nil)
		return
	}
	body := map[string]any{"removed": removed, "freed_bytes": freed, "failed": failures}
	if len(failures) == 0 {
		writeJSON(w, http.StatusOK, body)
		return
	}
	// A failed clear is a product error too, so a client shows why. The body
	// keeps the counts and the per-file failures it always returned; the
	// error counts nodes, since one node can fail on several files.
	failedNodes := map[string]bool{}
	for _, f := range failures {
		failedNodes[f["node_uuid"]] = true
	}
	first := failures[0]
	params := map[string]any{"failed": len(failedNodes), "node": first["node_name"]}
	body["code"] = messages.ErrorStorageCacheClearFailed
	body["error"] = fmt.Sprintf("cache could not be cleared on %d node(s); first %s: %s", len(failedNodes), first["node_name"], first["message"])
	body["params"] = params
	w.Header().Set(usageErrorHeader, messages.ErrorStorageCacheClearFailed)
	noteProductError(w, messages.ErrorStorageCacheClearFailed, params)
	writeJSON(w, http.StatusConflict, body)
}

// storageUsage combines control-plane managed bytes with each node's statfs
// snapshot of its models disk. A shared storage is one row of its own and is
// not counted on any node.
func (a *apiHandlers) storageUsage(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.d.Store.FleetNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	modelBytesByNode, imageBytesByNode, err := a.d.Store.NodeDiskUsage()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.StorageNodeUsage, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		modelBytes, imageBytes := modelBytesByNode[node.ID], imageBytesByNode[node.ID]
		var paths []proto.PathUsage
		_ = json.Unmarshal([]byte(node.PathUsage), &paths)
		total, available := modelsDiskUsage(paths)
		used := max(total-available, 0)
		other := max(used-modelBytes-imageBytes, 0)
		managed := make([]dto.ManagedPathUsage, 0, len(paths))
		for _, p := range paths {
			managed = append(managed, dto.ManagedPathUsage{Kind: p.Kind, StorageID: p.StorageID, Path: p.Path, TotalBytes: p.TotalBytes, AvailableBytes: p.AvailableBytes})
		}
		out = append(out, dto.StorageNodeUsage{NodeUUID: node.UUID, Hostname: node.Hostname, CapacityKnown: total > 0, ObservedAt: node.PathUsageObservedAt, TotalBytes: total, AvailableBytes: available, UsedBytes: used, ModelBytes: modelBytes, ImageBytes: imageBytes, OtherBytes: other, PathUsage: managed})
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

// modelsDiskUsage is the node's models disk: the models entry an agent
// reports. It never falls through to the docker data root, which is a
// different filesystem on most nodes.
func modelsDiskUsage(paths []proto.PathUsage) (int64, int64) {
	for _, p := range paths {
		if p.Kind == "models" && p.TotalBytes > 0 {
			return p.TotalBytes, p.AvailableBytes
		}
	}
	return 0, 0
}

func storageRefFromQuery(r *http.Request) uint {
	return parseUint(r.URL.Query().Get("storage_id"))
}
