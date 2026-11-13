package api

import (
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) patchStorageNodeOverrides(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	storage, err := a.d.Store.StorageByID(id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if storage.Type != proto.StorageNFSCache {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "only an nfs_cache storage has cache overrides", nil)
		return
	}
	var req dto.StorageNodeOverridesRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTargetsRequired, "request must contain one JSON object", nil)
		return
	}
	if len(req.NodeUUIDs) == 0 || (!req.CachePath.Present && !req.CacheMaxBytes.Present) {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTargetsRequired, "choose nodes and at least one cache field", nil)
		return
	}
	patch := store.StorageNodeOverridePatch{SetCacheMaxBytes: req.CacheMaxBytes.Present, CacheMaxBytes: req.CacheMaxBytes.Value}
	if patch.CacheMaxBytes != nil && *patch.CacheMaxBytes < 0 {
		writeProductError(w, http.StatusBadRequest, ErrorStorageCacheLimitNegative, "cache_max_bytes must be non-negative", nil)
		return
	}
	if req.CachePath.Present {
		value := ""
		if req.CachePath.Value != nil {
			value = strings.TrimSpace(*req.CachePath.Value)
			if value == "" {
				writeProductError(w, http.StatusBadRequest, ErrorStoragePathRequired, "use null to inherit the cache directory", nil)
				return
			}
		}
		if value != "" && !path.IsAbs(value) {
			writeProductError(w, http.StatusBadRequest, ErrorStoragePathInvalid, "paths must be absolute", map[string]any{"path": value})
			return
		}
		patch.CachePath = &value
		if value != "" {
			if err := a.validateStoragePaths(&store.Storage{CachePath: value}, nil); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		}
	}
	seen := map[string]bool{}
	nodeIDs := make([]uint, 0, len(req.NodeUUIDs))
	nodeUUIDs := make([]string, 0, len(req.NodeUUIDs))
	for _, raw := range req.NodeUUIDs {
		uuid := strings.TrimSpace(raw)
		if uuid == "" || seen[uuid] {
			writeProductError(w, http.StatusBadRequest, ErrorStorageTargetsRequired, "node UUIDs must be unique and nonempty", nil)
			return
		}
		seen[uuid] = true
		node, err := a.d.Store.NodeByUUID(uuid)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorStorageTargetsRequired, "unknown node "+uuid, map[string]any{"node": uuid})
			return
		}
		nodeIDs = append(nodeIDs, node.ID)
		nodeUUIDs = append(nodeUUIDs, node.UUID)
	}
	updated, err := a.d.Store.PatchStorageNodeOverrides(id, nodeIDs, patch)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	a.syncNodesIfOnline(nodeUUIDs...)
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.storageDTO(updated, nodes, true))
}
