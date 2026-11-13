package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// reconcileStorageCache persists a fixed set of requests before returning.
// The controller consumes the store event; no HTTP request owns node RPCs.
func (a *apiHandlers) reconcileStorageCache(w http.ResponseWriter, r *http.Request) {
	storage, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if storage.Type != proto.StorageNFSCache {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "only an nfs_cache storage has node caches", nil)
		return
	}
	var req dto.StorageCacheReconcileRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Mode != "trim" && req.Mode != "clear" || len(req.NodeUUIDs) == 0 || len(req.NodeUUIDs) > 10000 {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "mode must be trim or clear and node_uuids must contain 1 to 10000 nodes", nil)
		return
	}
	nodes, err := a.d.Store.NodesForStorage(storage)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	eligible := map[string]uint{}
	for _, node := range nodes {
		eligible[node.UUID] = node.ID
	}
	ids := make([]uint, 0, len(req.NodeUUIDs))
	seen := map[uint]bool{}
	for _, uuid := range req.NodeUUIDs {
		id := eligible[strings.TrimSpace(uuid)]
		if id == 0 {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "a requested node is unknown or outside this storage's scope", map[string]any{"node": uuid})
			return
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if err := a.d.Store.RequestCacheOperations(storage.ID, ids, req.Mode, true); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": len(ids)})
}
