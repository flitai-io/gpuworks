package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// cancelModelDownload names the exact storage location, including the shared
// row (no node). It remains available when a storage has been made read-only.
func (a *apiHandlers) cancelModelDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StorageID uint   `json:"storage_id"`
		NodeUUID  string `json:"node_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	modelID := parseUint(chi.URLParam(r, "id"))
	if modelID == 0 {
		writeErr(w, http.StatusBadRequest, store.ErrNotFound)
		return
	}
	if _, err := a.d.Store.GetModel(modelID); err != nil {
		writeStoreErr(w, err)
		return
	}
	var storage *store.Storage
	var err error
	if req.StorageID == 0 && strings.TrimSpace(req.NodeUUID) != "" {
		node, nodeErr := a.d.Store.NodeByUUID(req.NodeUUID)
		if nodeErr != nil {
			writeStoreErr(w, nodeErr)
			return
		}
		storage, err = a.localStorageForNode(modelID, node, 0)
	} else {
		storage, err = a.targetStorage(req.StorageID)
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	ref := store.FileRef{ModelID: modelID, StorageID: storage.ID}
	if !proto.StorageShared(storage.Type) {
		if strings.TrimSpace(req.NodeUUID) == "" {
			writeErr(w, http.StatusBadRequest, store.ErrModelDownloadInactive)
			return
		}
		node, err := a.d.Store.NodeByUUID(req.NodeUUID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		ref.NodeID = node.ID
	}
	row, err := a.d.Store.ModelFileAt(ref)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	canceled, err := a.d.Store.CancelModelDownload(row.ID, row.TransferID, "Download canceled by user")
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	executor := canceled.NodeID
	if executor == 0 {
		executor = canceled.WriterNodeID
	}
	if executor != 0 && a.d.CallNode != nil && row.TransferID != "" {
		if node, err := a.d.Store.NodeByID(executor); err == nil {
			_, _ = a.d.CallNode(r.Context(), node.UUID, proto.RPCModelDownloadStop, proto.ModelCopyStopParams{Key: store.ModelFileKey(row.ID), TransferID: row.TransferID})
		}
	}
	if executor != 0 && a.d.NodeSync != nil {
		if node, err := a.d.Store.NodeByID(executor); err == nil {
			// Intent is already durable. An offline writer receives it on reconnect.
			_ = a.d.NodeSync.SyncNode(node.UUID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": canceled.State, "message": canceled.Message})
}
