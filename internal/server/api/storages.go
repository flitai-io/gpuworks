package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Storage handlers: a storage is where model files live (local directory on
// each node, an NFS export the agents mount, or an NFS export plus a per-node
// cache). The API validates shapes; whether a definition actually mounts is
// answered by an agent through storage.probe.

func (a *apiHandlers) listStorages(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.ListStorages()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	nodes, _ := a.d.Store.ListNodes()
	out := make([]dto.Storage, 0, len(rows))
	for i := range rows {
		out = append(out, a.storageDTO(&rows[i], nodes, false))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func (a *apiHandlers) getStorage(w http.ResponseWriter, r *http.Request) {
	row, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	nodes, _ := a.d.Store.ListNodes()
	writeJSON(w, http.StatusOK, a.storageDTO(row, nodes, r.URL.Query().Get("all_nodes") == "1"))
}

// storageDTO renders a storage with its node summary: how many of the nodes it
// applies to have it ready, and only the problem nodes unless all is set, so a
// 1000-node fleet does not render 1000 rows by default. nodes are every node,
// read once by the caller for all the storages it renders.
func (a *apiHandlers) storageDTO(row *store.Storage, nodes []store.Node, all bool) dto.Storage {
	out := dto.Storage{
		UpdatedAt: &row.UpdatedAt,
		ID:        row.ID, Name: row.Name, Type: row.Type, Enabled: row.Enabled, IsDefault: row.IsDefault,
		AllNodes: row.AllNodes, Path: row.Path, Export: row.Export, MountOptions: row.MountOptions,
		CachePath: row.CachePath, CacheMaxBytes: row.CacheMaxBytes,
		TotalBytes: row.TotalBytes, AvailableBytes: row.AvailableBytes,
		Targets: []dto.StorageTarget{}, Nodes: dto.StorageNodes{Problems: []dto.StorageNodeStatus{}},
		DeploymentIDs: []uint{},
	}
	byID := map[uint]*store.Node{}
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	targets, _ := a.d.Store.StorageNodes(row.ID)
	for _, target := range targets {
		item := dto.StorageTarget{OverrideOnly: target.OverrideOnly, LabelKey: target.LabelKey, LabelValue: target.LabelValue, Path: target.Path,
			CachePath: target.CachePath, CacheMaxBytes: target.CacheMaxBytes, Existing: target.Existing}
		if node := byID[target.NodeID]; node != nil {
			item.NodeUUID, item.NodeName = node.UUID, nodeDisplayName(node)
		}
		out.Targets = append(out.Targets, item)
	}
	states := map[uint]store.StorageNodeState{}
	if rows, err := a.d.Store.StorageNodeStates(row.ID); err == nil {
		for _, state := range rows {
			states[state.NodeID] = state
		}
	}
	out.Writable = row.Type == proto.StorageLocal
	for i := range nodes {
		node := &nodes[i]
		if !store.StorageAppliesTo(row, targets, node) {
			continue
		}
		status := dto.StorageNodeStatus{NodeUUID: node.UUID, NodeName: nodeDisplayName(node), State: "unknown"}
		if state, ok := states[node.ID]; ok {
			status.NeedsOwnDirectory = store.NodeNeedsOwnDirectory(row.Type, &state)
			updated := state.UpdatedAt
			status.State, status.Reason, status.Message = state.State, state.Reason, state.Message
			status.Path, status.CachePath, status.Writable, status.UpdatedAt = state.Path, state.CachePath, state.Writable, &updated
			if state.State == proto.StorageStateReady && state.Writable {
				out.Writable = true
			}
		}
		fillStorageNodeCapacity(&status, node, row.ID)
		// A node that is not connected reports nothing: its last state is stale
		// and a node that never reported is not "unknown" but offline.
		if node.State == store.NodeStateOffline || node.State == store.NodeStateConnecting {
			status.State, status.NeedsOwnDirectory = "offline", false
		}
		out.Nodes.Total++
		if status.State == proto.StorageStateReady {
			out.Nodes.Ready++
		} else {
			out.Nodes.Problems = append(out.Nodes.Problems, status)
		}
		if all {
			out.Nodes.All = append(out.Nodes.All, status)
		}
	}
	out.ModelCount, _ = a.d.Store.CountModelsInStorage(row.ID)
	if inUse, err := a.d.Store.StorageUsers(row.ID, storageRemovalExcludedStates); err == nil {
		out.DeploymentIDs = append(out.DeploymentIDs, inUse.DeploymentIDs...)
	}
	a.fillStorageCaches(&out, row, nodes)
	return out
}

// fillStorageNodeCapacity copies the capacity the node last reported for the
// storage (and its cache) from the node's path usage.
func fillStorageNodeCapacity(status *dto.StorageNodeStatus, node *store.Node, storageID uint) {
	var paths []proto.PathUsage
	if json.Unmarshal([]byte(node.PathUsage), &paths) != nil {
		return
	}
	for _, p := range paths {
		if p.StorageID != storageID {
			continue
		}
		switch p.Kind {
		case proto.PathKindStorage:
			status.TotalBytes, status.AvailableBytes = p.TotalBytes, p.AvailableBytes
		case proto.PathKindStorageCache:
			status.CacheTotalBytes, status.CacheAvailableBytes = p.TotalBytes, p.AvailableBytes
		}
	}
}

func writeStoreErr(w http.ResponseWriter, err error) {
	var inUse *store.StorageInUseError
	var cacheNotEmpty *store.StorageCacheNotEmptyError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrCacheDeletionPending):
		writeErr(w, http.StatusConflict, err)
	case errors.As(err, &inUse):
		writeProductError(w, http.StatusConflict, ErrorStorageInUse, err.Error(), map[string]any{
			"deployment_ids": inUse.DeploymentIDs, "instance_uuids": inUse.InstanceUUIDs,
		})
	case errors.As(err, &cacheNotEmpty):
		writeProductError(w, http.StatusConflict, messages.ErrorStorageCacheNotEmpty, err.Error(), map[string]any{
			"nodes": len(cacheNotEmpty.NodeIDs), "entries": cacheNotEmpty.Entries, "transferring": cacheNotEmpty.Transferring,
		})
	case errors.Is(err, store.ErrStorageIsDefault):
		writeProductError(w, http.StatusConflict, ErrorStorageIsDefault, "the default storage cannot be deleted until another storage is set as the default", nil)
	case errors.Is(err, store.ErrStorageExportTaken):
		writeProductError(w, http.StatusConflict, ErrorStorageExportTaken, err.Error(), nil)
	case errors.Is(err, store.ErrStorageNameTaken):
		writeProductError(w, http.StatusConflict, ErrorStorageNameTaken, err.Error(), nil)
	case errors.Is(err, store.ErrStoragePathRequired):
		writeProductError(w, http.StatusBadRequest, ErrorStoragePathRequired, err.Error(), nil)
	case errors.Is(err, store.ErrStorageConfigChanged):
		writeProductError(w, http.StatusConflict, ErrorStorageConfigChanged, "Storage configuration changed; reload and try again.", nil)
	case errors.Is(err, store.ErrStorageDisabled):
		writeProductError(w, http.StatusConflict, ErrorStorageDisabled, err.Error(), nil)
	case errors.Is(err, store.ErrStorageNotWritable):
		writeProductError(w, http.StatusConflict, ErrorStorageNotWritable, err.Error(), nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// storageFromRequest validates a create request into a storage row and its
// node targets.
func (a *apiHandlers) storageFromRequest(req dto.StorageRequest) (*store.Storage, []store.StorageNode, error) {
	row := &store.Storage{Enabled: true, AllNodes: true}
	if req.Name != nil {
		row.Name = strings.TrimSpace(*req.Name)
	}
	if req.Type != nil {
		row.Type = strings.TrimSpace(*req.Type)
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	if req.IsDefault != nil {
		row.IsDefault = *req.IsDefault
	}
	if req.AllNodes != nil {
		row.AllNodes = *req.AllNodes
	}
	if req.Path != nil {
		row.Path = strings.TrimSpace(*req.Path)
	}
	if req.Export != nil {
		row.Export = strings.TrimSpace(*req.Export)
	}
	if req.MountOptions != nil {
		row.MountOptions = strings.TrimSpace(*req.MountOptions)
	}
	if req.CachePath != nil {
		row.CachePath = strings.TrimSpace(*req.CachePath)
	}
	if req.CacheMaxBytes != nil {
		row.CacheMaxBytes = *req.CacheMaxBytes
	}
	if row.Path == "" && row.Type != proto.StorageLocal && row.Name != "" {
		row.Path = store.DefaultMountPoint(row.Name)
	}
	if err := validateStorage(row); err != nil {
		return nil, nil, err
	}
	var targets []store.StorageNode
	if req.Targets != nil {
		var err error
		if targets, err = a.storageTargets(*req.Targets); err != nil {
			return nil, nil, err
		}
	}
	if !row.AllNodes && !store.StorageHasSelectors(targets) {
		return nil, nil, productError(ErrorStorageTargetsRequired, "choose the nodes or labels this storage applies to", nil)
	}
	return row, targets, nil
}

var storageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validateStorage(row *store.Storage) error {
	if !storageNamePattern.MatchString(row.Name) {
		return productError(ErrorStorageNameInvalid, "storage name must be 1-63 lowercase letters, digits or hyphens", map[string]any{"name": row.Name})
	}
	switch row.Type {
	case proto.StorageLocal, proto.StorageSharedDir:
	case proto.StorageNFS, proto.StorageNFSCache:
		// The agent applies the same rules when it mounts (store.CheckNFSSpec).
		switch err := store.CheckNFSSpec(row.Export, row.MountOptions); {
		case errors.Is(err, store.ErrStorageExportInvalid):
			return productError(ErrorStorageExportInvalid, "export must be host:/path", map[string]any{"export": row.Export})
		case err != nil:
			return productError(ErrorStorageOptionsInvalid, "mount options must be a comma separated list of allowed options: "+err.Error(), map[string]any{"detail": err.Error()})
		}
	default:
		return productError(ErrorStorageTypeInvalid, "type must be local, nfs, nfs_cache or shared_dir", map[string]any{"type": row.Type})
	}
	for _, p := range []string{row.Path, row.CachePath} {
		if p != "" && !path.IsAbs(p) {
			return productError(ErrorStoragePathInvalid, "paths must be absolute", map[string]any{"path": p})
		}
		// Only the form is checked here: a PATCH validates the whole row, and a
		// stored path a later denylist covers must not block unrelated edits.
		// validateStoragePaths applies the denylist to submitted paths.
		if p != "" {
			if err := store.CheckStoragePathForm(p); err != nil {
				return productError(ErrorStoragePathInvalid, err.Error(), map[string]any{"path": p})
			}
		}
	}
	if row.Type == proto.StorageSharedDir && row.Path == "" {
		return productError(ErrorStoragePathRequired, "a shared directory needs its path", nil)
	}
	if row.Type == proto.StorageLocal && row.Path == "" {
		// Never the agent's own directory: see store.DefaultLocalStoragePath.
		return productError(ErrorStoragePathRequired, "a local storage needs its directory", nil)
	}
	if row.CacheMaxBytes < 0 {
		return productError(ErrorStorageCacheLimitNegative, "cache_max_bytes must be non-negative", nil)
	}
	return nil
}

func (a *apiHandlers) storageTargets(in []dto.StorageTarget) ([]store.StorageNode, error) {
	out := make([]store.StorageNode, 0, len(in))
	seenNodes := map[uint]bool{}
	for _, target := range in {
		row := store.StorageNode{OverrideOnly: target.OverrideOnly, LabelKey: strings.TrimSpace(target.LabelKey), LabelValue: strings.TrimSpace(target.LabelValue),
			Path: strings.TrimSpace(target.Path), CachePath: strings.TrimSpace(target.CachePath),
			CacheMaxBytes: target.CacheMaxBytes, Existing: target.Existing}
		if row.CacheMaxBytes != nil && *row.CacheMaxBytes < 0 {
			return nil, productError(ErrorStorageCacheLimitNegative, "cache_max_bytes must be non-negative", nil)
		}
		if strings.TrimSpace(target.NodeUUID) != "" && (row.LabelKey != "" || row.LabelValue != "") {
			return nil, productError(ErrorStorageTargetsRequired, "a target must name either a node or a label", nil)
		}
		if strings.TrimSpace(target.NodeUUID) == "" && (row.OverrideOnly || row.Path != "" || row.CachePath != "" || row.CacheMaxBytes != nil || row.Existing) {
			return nil, productError(ErrorStorageTargetsRequired, "node overrides require a node", nil)
		}
		if uuid := strings.TrimSpace(target.NodeUUID); uuid != "" {
			node, err := a.d.Store.NodeByUUID(uuid)
			if err != nil {
				return nil, productError(ErrorStorageTargetsRequired, "unknown node "+uuid, map[string]any{"node": uuid})
			}
			row.NodeID = node.ID
			if seenNodes[node.ID] {
				return nil, productError(ErrorStorageTargetsRequired, "a node may appear only once in targets", map[string]any{"node": node.UUID})
			}
			seenNodes[node.ID] = true
		} else if row.LabelKey == "" {
			return nil, productError(ErrorStorageTargetsRequired, "a target needs a node or a label", nil)
		}
		for _, p := range []string{row.Path, row.CachePath} {
			if p != "" && !path.IsAbs(p) {
				return nil, productError(ErrorStoragePathInvalid, "paths must be absolute", map[string]any{"path": p})
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// createStorage adds a storage. A shared storage is probed on one of its
// nodes first, so a storage that cannot mount is never saved, and one no node
// can write to is saved disabled (read-only), as the plan requires.
func (a *apiHandlers) createStorage(w http.ResponseWriter, r *http.Request) {
	var req dto.StorageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	row, targets, err := a.storageFromRequest(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.validateStoragePaths(row, targets); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if row.Type != proto.StorageLocal {
		probe, err := a.probeOnNode(r.Context(), req.ProbeNode, row, targets)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		if !probe.OK {
			writeProductError(w, http.StatusConflict, ErrorStorageProbeFailed, probe.Message, map[string]any{"probe": probe})
			return
		}
		if !probe.Writable {
			row.Enabled = false
			row.IsDefault = false
		}
		row.TotalBytes, row.AvailableBytes = probe.TotalBytes, probe.AvailableBytes
	}
	created, err := a.d.Store.CreateStorage(row, targets)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	a.syncStorageNodes(created)
	nodes, _ := a.d.Store.ListNodes()
	writeJSON(w, http.StatusCreated, a.storageDTO(created, nodes, false))
}

func (a *apiHandlers) patchStorage(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	var req dto.StorageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	current, err := a.d.Store.StorageByID(id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if req.Export != nil && strings.TrimSpace(*req.Export) != current.Export {
		writeProductError(w, http.StatusBadRequest, ErrorStorageExportInvalid, "an export cannot be changed; add a new storage instead", nil)
		return
	}
	next := *current
	patch := store.StoragePatch{ExpectedUpdatedAt: req.ExpectedUpdatedAt, Name: req.Name, Enabled: req.Enabled, IsDefault: req.IsDefault, AllNodes: req.AllNodes,
		Path: req.Path, MountOptions: req.MountOptions, CachePath: req.CachePath, CacheMaxBytes: req.CacheMaxBytes}
	if req.Name != nil {
		next.Name = strings.TrimSpace(*req.Name)
	}
	if req.Type != nil && *req.Type != current.Type {
		// Only the two NFS types switch: the authoritative copy stays where it is.
		shared := map[string]bool{proto.StorageNFS: true, proto.StorageNFSCache: true}
		if !shared[*req.Type] || !shared[current.Type] {
			writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "only nfs and nfs_cache switch into each other", nil)
			return
		}
		next.Type = *req.Type
		patch.Type = req.Type
	}
	if req.Path != nil {
		next.Path = strings.TrimSpace(*req.Path)
	}
	if req.CachePath != nil {
		next.CachePath = strings.TrimSpace(*req.CachePath)
	}
	if req.MountOptions != nil {
		next.MountOptions = strings.TrimSpace(*req.MountOptions)
	}
	if req.CacheMaxBytes != nil {
		next.CacheMaxBytes = *req.CacheMaxBytes
	}
	if err := validateStorage(&next); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Enabled != nil && *req.Enabled && proto.StorageShared(current.Type) {
		if writers, _ := a.d.Store.ReadyStorageNodes(current.ID, true); len(writers) == 0 {
			writeProductError(w, http.StatusConflict, ErrorStorageNotWritable, "no node can write to this storage, so it stays read-only", nil)
			return
		}
	}
	if req.Targets != nil {
		targets, err := a.storageTargets(*req.Targets)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		patch.Targets = &targets
	}
	var nextTargets []store.StorageNode
	if patch.Targets != nil {
		nextTargets = *patch.Targets
	}
	pathsToValidate := &store.Storage{}
	if req.Path != nil {
		pathsToValidate.Path = next.Path
	}
	if req.CachePath != nil {
		pathsToValidate.CachePath = next.CachePath
	}
	if err := a.validateStoragePaths(pathsToValidate, nextTargets); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if (req.AllNodes != nil && !*req.AllNodes) || patch.Targets != nil {
		allNodes := current.AllNodes
		if req.AllNodes != nil {
			allNodes = *req.AllNodes
		}
		if !allNodes {
			var selected []store.StorageNode
			if patch.Targets != nil {
				selected = *patch.Targets
			} else {
				var err error
				selected, err = a.d.Store.StorageNodes(id)
				if err != nil {
					writeStoreErr(w, err)
					return
				}
			}
			if !store.StorageHasSelectors(selected) {
				writeProductError(w, http.StatusBadRequest, ErrorStorageTargetsRequired, "choose the nodes or labels this storage applies to", nil)
				return
			}
		}
	}
	nodesBefore, _ := a.d.Store.NodesForStorage(current)
	updated, err := a.d.Store.UpdateStorage(id, patch)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	a.syncNodes(nodesBefore)
	a.syncStorageNodes(updated)
	nodes, _ := a.d.Store.ListNodes()
	writeJSON(w, http.StatusOK, a.storageDTO(updated, nodes, false))
}

func (a *apiHandlers) deleteStorage(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	row, err := a.d.Store.StorageByID(id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	models, inUse, err := a.storageRemovalScope(id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if len(inUse.DeploymentIDs) > 0 || len(inUse.InstanceUUIDs) > 0 {
		writeStoreErr(w, inUse)
		return
	}
	nodes, err := a.d.Store.NodesForStorage(row)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var writers []*store.Node
	if proto.StorageShared(row.Type) {
		ids, err := a.d.Store.ReadyStorageNodes(id, true)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		byID := make(map[uint]*store.Node, len(nodes))
		for i := range nodes {
			byID[nodes[i].ID] = &nodes[i]
		}
		for _, nodeID := range ids {
			if node := byID[nodeID]; node != nil && (a.d.Online == nil || a.d.Online(node.UUID)) {
				writers = append(writers, node)
			}
		}
	}
	if err := a.d.Store.DeleteStorage(id, models...); err != nil {
		writeStoreErr(w, err)
		return
	}
	var cleanupErr error
	if len(writers) > 0 {
		_, err := a.storagePeersOn(r.Context(), row, true, writers)
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if a.d.NodeSync != nil {
		for _, node := range nodes {
			if a.d.Online == nil || a.d.Online(node.UUID) {
				if err := a.d.NodeSync.SyncNode(node.UUID); err != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync storage state to node %s: %w", node.UUID, err))
				}
			}
		}
	}
	if cleanupErr != nil && a.d.Log != nil {
		a.d.Log.Warn("storage node cleanup failed", "storage_id", id, "error", cleanupErr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// These states provide neither a readable copy nor an unfinished operation to
// preserve. Display and deletion pass the same source policy to their queries.
var storageRemovalExcludedStates = []string{
	proto.MFMissing, proto.MFAbsent, proto.MFFailed,
	proto.MFDownloadRequired, proto.MFInsufficientDisk,
}

func (a *apiHandlers) storageRemovalScope(id uint) ([]uint, *store.StorageInUseError, error) {
	refs, err := a.d.Store.StorageModelReferences(id, storageRemovalExcludedStates)
	if err != nil {
		return nil, nil, err
	}
	keep := make(map[uint]bool)
	for _, modelID := range refs.OtherModelIDs {
		keep[modelID] = true
	}
	models := slices.DeleteFunc(refs.ModelIDs, func(id uint) bool { return keep[id] })
	inUse := refs.DirectUsers
	for _, user := range refs.ModelUsers {
		if !keep[user.ModelID] {
			inUse.DeploymentIDs = append(inUse.DeploymentIDs, user.ID)
		}
	}
	slices.Sort(inUse.DeploymentIDs)
	inUse.DeploymentIDs = slices.Compact(inUse.DeploymentIDs)
	return models, &inUse, nil
}

// syncStorageNodes resends the desired state to every node a storage applies
// to; storage changes are rare, so a full state sync is the whole protocol.
func (a *apiHandlers) syncStorageNodes(row *store.Storage) {
	nodes, err := a.d.Store.NodesForStorage(row)
	if err != nil {
		return
	}
	a.syncNodes(nodes)
}

func (a *apiHandlers) syncNodes(nodes []store.Node) {
	if a.d.NodeSync == nil {
		return
	}
	for _, node := range nodes {
		if a.d.Online == nil || a.d.Online(node.UUID) {
			_ = a.d.NodeSync.SyncNode(node.UUID)
		}
	}
}
