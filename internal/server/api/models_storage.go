package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Model operations that name a storage: download into it, copy between two
// locations, delete from it. Every write first asks the storage whether it
// accepts writes (store.StorageAcceptsWrites), which is what "disabled" means.

// targetStorage resolves the storage a request names; zero is the default
// storage, falling back to the default local storage.
func (a *apiHandlers) targetStorage(id uint) (*store.Storage, error) {
	if id != 0 {
		return a.d.Store.StorageByID(id)
	}
	if row, err := a.d.Store.DefaultStorage(); err == nil {
		return row, nil
	}
	return a.d.Store.StorageByID(a.d.Store.DefaultLocalStorageID())
}

// localStorageForNode is the local storage a per-node request without a
// storage means: the requested one, the node's existing copy's, or the
// default local storage.
func (a *apiHandlers) localStorageForNode(modelID uint, node *store.Node, requested uint) (*store.Storage, error) {
	if requested != 0 {
		return a.d.Store.StorageByID(requested)
	}
	if current, err := a.d.Store.ModelFileByModelNode(modelID, node.ID); err == nil {
		return a.d.Store.StorageByID(current.StorageID)
	}
	return a.d.Store.StorageByID(a.d.Store.DefaultLocalStorageID())
}

func writeStorageWriteErr(w http.ResponseWriter, storage *store.Storage, err error) {
	switch {
	case errors.Is(err, store.ErrStorageDisabled):
		writeProductError(w, http.StatusConflict, ErrorStorageDisabled, "storage "+storage.Name+" is read-only (disabled)", map[string]any{"storage": storage.Name})
	case errors.Is(err, store.ErrStorageNotWritable):
		writeProductError(w, http.StatusConflict, ErrorStorageNotWritable, "no node can write to storage "+storage.Name, map[string]any{"storage": storage.Name})
	default:
		writeStoreErr(w, err)
	}
}

// downloadModel starts a public download into a storage: onto each named node
// for a local storage, or once through a writer node for a shared storage.
func (a *apiHandlers) downloadModel(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	var req struct {
		NodeUUID  string   `json:"node_uuid"`
		NodeUUIDs []string `json:"node_uuids"`
		StorageID uint     `json:"storage_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	m, err := a.d.Store.GetModel(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if strings.EqualFold(strings.TrimSpace(m.Source), "local") {
		writeProductError(w, http.StatusBadRequest, ErrorLocalModelPublicDownload, "local models cannot be downloaded from a public repository", nil)
		return
	}
	uuids := req.NodeUUIDs
	if strings.TrimSpace(req.NodeUUID) != "" {
		uuids = append([]string{strings.TrimSpace(req.NodeUUID)}, uuids...)
	}
	var storage *store.Storage
	if req.StorageID == 0 && len(uuids) == 1 {
		node, err := a.d.Store.NodeByUUID(uuids[0])
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		storage, err = a.localStorageForNode(m.ID, node, 0)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
	} else if storage, err = a.targetStorage(req.StorageID); err != nil {
		writeStoreErr(w, err)
		return
	}
	if err := a.d.Store.StorageAcceptsWrites(storage); err != nil {
		writeStorageWriteErr(w, storage, err)
		return
	}
	if proto.StorageShared(storage.Type) {
		if len(uuids) > 1 {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "choose exactly one writer for a shared storage", map[string]any{"field": "node_uuids", "detail": "choose exactly one writer for a shared storage"})
			return
		}
		writerUUID := ""
		if len(uuids) == 1 {
			writerUUID = strings.TrimSpace(uuids[0])
			if writerUUID == "" {
				writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "writer node cannot be empty", map[string]any{"field": "node_uuids", "detail": "writer node cannot be empty"})
				return
			}
		}
		a.downloadIntoShared(w, m, storage, writerUUID)
		return
	}
	if len(uuids) == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorModelPathRequired, "choose the nodes to download onto", nil)
		return
	}
	for _, uuid := range uuids {
		node, err := a.d.Store.NodeByUUID(uuid)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if _, applies, err := a.d.Store.EffectiveStorageForNode(storage, node); err != nil || !applies {
			writeProductError(w, http.StatusConflict, ErrorStorageUnavailableOnNode, "storage "+storage.Name+" does not apply to node "+nodeDisplayName(node), map[string]any{"node": node.UUID, "storage": storage.Name})
			return
		}
		if err := deployment.AdmitModelDownloadNode(m, node); err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		if _, err := planning.ModelWeightsFetch(a.d.Store, a.networks().Resolve, node, m); err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		ref := store.FileRef{ModelID: m.ID, StorageID: storage.ID, NodeID: node.ID}
		current, err := a.d.Store.ModelFileAt(ref)
		if err == nil && current.State == proto.MFSyncing {
			writeProductError(w, http.StatusConflict, ErrorPeerSyncActive, "cancel the active peer sync before starting public download", nil)
			return
		}
		// Every preparation names a new transfer, which would restart a
		// download already under way; asking again leaves that one running.
		if err == nil && current.Acquisition == proto.ModelAcquireDownload && store.TransferUnderWay(current) {
			if err := a.d.Store.SetDownloadDeployment(current.ID, current.TransferID, 0); err != nil {
				writeStoreErr(w, err)
				return
			}
			continue
		}
		if _, err := a.d.Store.PrepareModelDownload(ref, relPath(m)); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if a.d.NodeSync != nil {
			if err := a.d.NodeSync.SyncNode(node.UUID); err != nil {
				writeErr(w, http.StatusServiceUnavailable, err)
				return
			}
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "downloading"})
}

// downloadIntoShared downloads a model once into a shared storage through the
// selected node, or the first online writer for older clients.
func (a *apiHandlers) downloadIntoShared(w http.ResponseWriter, m *store.Model, storage *store.Storage, writerUUID string) {
	started, err := a.deployments().StartSharedDownloadOn(m, storage, writerUUID)
	if err != nil {
		writeUseCaseErr(w, err)
		return
	}
	if started == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "present"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "downloading", "writer": started.UUID})
}

type locationRequest struct {
	StorageID uint   `json:"storage_id"`
	NodeUUID  string `json:"node_uuid,omitempty"`
}

func (a *apiHandlers) locationRef(modelID uint, loc locationRequest) (store.FileRef, error) {
	ref := store.FileRef{ModelID: modelID, StorageID: loc.StorageID}
	if strings.TrimSpace(loc.NodeUUID) != "" {
		node, err := a.d.Store.NodeByUUID(strings.TrimSpace(loc.NodeUUID))
		if err != nil {
			return ref, err
		}
		ref.NodeID = node.ID
	}
	if ref.StorageID == 0 {
		return ref, productError(ErrorFieldInvalid, "storage_id is required", map[string]any{"field": "storage_id", "detail": "storage_id is required"})
	}
	return ref, nil
}

// copyModel copies a model's files between two locations.
func (a *apiHandlers) copyModel(w http.ResponseWriter, r *http.Request) {
	if a.d.PeerSync.CopyModel == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorModelSyncUnavailable, "model copy is not configured", nil)
		return
	}
	modelID := parseUint(chi.URLParam(r, "id"))
	var req struct {
		Source       locationRequest `json:"source"`
		Target       locationRequest `json:"target"`
		RateLimitKiB *int            `json:"rate_limit_kib_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	src, err := a.locationRef(modelID, req.Source)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dst, err := a.locationRef(modelID, req.Target)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if src.StorageID == dst.StorageID {
		storage, err := a.d.Store.StorageByID(src.StorageID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		if proto.StorageShared(storage.Type) || src.NodeID == dst.NodeID {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "source and target are the same location", map[string]any{"field": "target", "detail": "source and target are the same location"})
			return
		}
	}
	rate := -1
	if req.RateLimitKiB != nil {
		if *req.RateLimitKiB < 0 {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "rate_limit_kib_s must be non-negative", map[string]any{"field": "rate_limit_kib_s", "detail": "rate_limit_kib_s must be non-negative"})
			return
		}
		rate = *req.RateLimitKiB
	}
	mf, err := a.d.PeerSync.CopyModel(r.Context(), modelID, src, dst, rate)
	if err != nil {
		err = deployment.ModelFilePathProductError(err)
		if errors.Is(err, store.ErrStorageDisabled) || errors.Is(err, store.ErrStorageNotWritable) || errors.Is(err, store.ErrNotFound) {
			writeStoreErr(w, err)
			return
		}
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"state": mf.State, "acquisition": mf.Acquisition, "message": mf.Message})
}

// evictModelNode removes a model's files from one node: its copy in a local
// storage, or its cache entry of an nfs_cache storage.
func (a *apiHandlers) evictModelNode(w http.ResponseWriter, r *http.Request) {
	modelID := parseUint(chi.URLParam(r, "id"))
	if modelID == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorModelIDInvalid, "invalid model id", nil)
		return
	}
	if _, err := a.d.Store.GetModel(modelID); err != nil {
		writeStoreErr(w, err)
		return
	}
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	storage, err := a.localStorageForNode(modelID, node, storageRefFromQuery(r))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !store.NodeFilesDeletable(storage) {
		writeStorageWriteErr(w, storage, store.ErrStorageDisabled)
		return
	}
	ref := store.FileRef{ModelID: modelID, StorageID: storage.ID, NodeID: node.ID}
	mf, err := a.d.Store.ModelFileAt(ref)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.CheckModelFileEviction(ref); err != nil {
		if errors.Is(err, store.ErrReferenced) {
			writeModelFileReferenced(w, err)
		} else if errors.Is(err, store.ErrCacheDeletionPending) {
			writeStoreErr(w, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	if mf != nil && mf.State == proto.MFSyncing && a.d.CancelModelSync != nil {
		if _, err := a.d.CancelModelSync(r.Context(), modelID, node.UUID); err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
	}
	if mf != nil && a.d.CallNode != nil {
		if _, err := a.d.CallNode(r.Context(), node.UUID, proto.RPCModelEvict, map[string]string{"path": mf.Path}); err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
	}
	result, err := a.d.Store.EvictModelFile(ref)
	if err != nil {
		if errors.Is(err, store.ErrReferenced) {
			writeModelFileReferenced(w, err)
		} else if errors.Is(err, store.ErrCacheDeletionPending) {
			writeStoreErr(w, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// deleteModelInStorage removes a model from a shared storage: the one copy
// every node reads, and, for nfs_cache, every node's cache of it. Another
// gpuworks using the same storage is warned about and needs force.
func (a *apiHandlers) deleteModelInStorage(w http.ResponseWriter, r *http.Request) {
	modelID := parseUint(chi.URLParam(r, "id"))
	storage, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "storage_id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !proto.StorageShared(storage.Type) {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "a local storage's files are deleted per node", nil)
		return
	}
	ref := store.FileRef{ModelID: modelID, StorageID: storage.ID}
	row, err := a.d.Store.ModelFileAt(ref)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if err := a.d.Store.StorageAcceptsWrites(storage); err != nil {
		writeStorageWriteErr(w, storage, err)
		return
	}
	// The storage's copy and, for nfs_cache, every node's cache of it: an
	// instance reads the cache, not the authoritative row.
	if err := a.d.Store.CheckModelFilesInStorage(modelID, storage.ID); err != nil {
		if errors.Is(err, store.ErrReferenced) {
			writeModelFileReferenced(w, err)
		} else if errors.Is(err, store.ErrCacheDeletionPending) {
			writeStoreErr(w, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	force := r.URL.Query().Get("force") == "1" || strings.EqualFold(r.URL.Query().Get("force"), "true")
	peers, err := a.storagePeers(r.Context(), storage, false)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var inUse []map[string]any
	for _, peer := range peers {
		if peer.Fresh {
			inUse = append(inUse, map[string]any{"id": peer.ID, "name": peer.Name, "last_seen_seconds": peer.LastSeenSeconds})
		}
	}
	if len(inUse) > 0 && !force {
		writeProductError(w, http.StatusConflict, ErrorStorageSharedByOtherControlPlanes,
			"other gpuworks installations use this storage; delete with force to remove the files anyway",
			map[string]any{"control_planes": inUse, "storage": storage.Name})
		return
	}
	// Node caches of an nfs_cache storage go first: they are this control
	// plane's own and must not outlive the authoritative copy.
	if storage.Type == proto.StorageNFSCache {
		if files, err := a.d.Store.ModelFilesForModel(modelID); err == nil {
			for _, file := range files {
				if file.StorageID != storage.ID || file.NodeID == 0 {
					continue
				}
				if node, err := a.d.Store.NodeByID(file.NodeID); err == nil && a.d.CallNode != nil && file.Path != "" {
					_, _ = a.d.CallNode(r.Context(), node.UUID, proto.RPCModelEvict, map[string]string{"path": file.Path})
				}
				_ = a.d.Store.DeleteModelFileRow(file.ID)
			}
		}
	}
	node, err := a.storageWriteNode(storage)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := a.d.CallNode(ctx, node.UUID, proto.RPCStorageRemove, proto.StorageRemoveParams{StorageID: storage.ID, RelPath: row.Path}); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err := a.d.Store.DeleteModelFileRow(row.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": row.Path, "size_bytes": row.SizeBytes})
}

func (a *apiHandlers) storageWriteNode(storage *store.Storage) (*store.Node, error) {
	if a.d.CallNode == nil {
		return nil, productError(ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
	}
	return a.pickStorageNode(storage, true, nil)
}
